// proxy.go — rotating proxy pool for Z.AI requests (WAF evasion).
// Enabled via USE_PROXY=true; list lives in PROXY_FILE (default proxy.txt),
// one proxy per line: host:port, http://host:port, socks5://host:port.
// On an Aliyun WAF block the pool advances to the next proxy; existing
// retry loops (sendToZAI 401/WAF, anthropic WAF retries) then hit a new IP.
//
// Hot reload: the file is re-read when its mtime+size change (polled every
// proxyReloadInterval and checked again on rotation), so routes appended or
// pruned by the external pool watchdog take effect without a restart.
// A revision that parses to zero routes is ignored — the previous list stays.
//
// Dead-proxy tracking: routes that fail to connect are marked dead for
// deadTTL and skipped by rotation — otherwise every request re-walks the
// dead nodes (observed: 145 attempts burning 184 captcha device-tokens
// on a single request through a stale list).
package app

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	deadTTL = 10 * time.Minute
	// proxyReloadInterval is how often the proxy file is checked for changes.
	proxyReloadInterval = 30 * time.Second
)

type proxyPool struct {
	urls atomic.Pointer[[]*url.URL]
	idx  atomic.Uint64

	path  string     // proxy file, set once at load ("" = hot reload disabled)
	stamp string     // last revision seen (mtime+size), guarded by rmu
	rmu   sync.Mutex // serialises reloads

	mu   sync.Mutex
	dead map[string]time.Time // proxy URL string → marked-dead time
}

var proxies = &proxyPool{}

// list returns the active proxy URLs. The slice is replaced atomically on
// reload, never mutated in place, so callers may use it without locking.
func (p *proxyPool) list() []*url.URL {
	if l := p.urls.Load(); l != nil {
		return *l
	}
	return nil
}

// store atomically installs a new URL slice.
func (p *proxyPool) store(list []*url.URL) { p.urls.Store(&list) }

// parseProxyFile parses the proxy file. Bare host:port defaults to http://.
func parseProxyFile(path string) ([]*url.URL, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []*url.URL
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "://") {
			line = "http://" + line
		}
		u, err := url.Parse(line)
		if err != nil || u.Host == "" {
			log.Printf("[Proxy] skip invalid line: %q", line)
			continue
		}
		list = append(list, u)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no valid proxies in %s", path)
	}
	return list, nil
}

// loadProxyPool parses the proxy file and installs it as the active pool.
func loadProxyPool(path string) error {
	list, err := parseProxyFile(path)
	if err != nil {
		return err
	}
	proxies.path = path
	proxies.stamp = proxyFileStamp(path)
	proxies.store(list)
	return nil
}

// proxyFileStamp identifies the current revision of the proxy file
// ("" when the file is missing or unreadable).
func proxyFileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

// reload re-parses the proxy file and atomically swaps the pool when the file
// changed since the last check (or force is set). A revision that yields zero
// valid routes is ignored, so the bridge is never left without a route; the
// dead-route map is untouched. Logs one line per successful reload.
func (p *proxyPool) reload(force bool) {
	if p.path == "" {
		return
	}
	p.rmu.Lock()
	defer p.rmu.Unlock()
	stamp := proxyFileStamp(p.path)
	if stamp == "" || (!force && stamp == p.stamp) {
		return
	}
	p.stamp = stamp
	list, err := parseProxyFile(p.path)
	if err != nil {
		log.Printf("[Proxy] reload skipped: %v — keeping %d proxies", err, len(p.list()))
		return
	}
	p.store(list)
	log.Printf("[Proxy] reloaded %d proxies from %s", len(list), p.path)
}

// watchProxyFile polls the proxy file in the background and picks up routes
// appended or pruned by an external writer, without a service restart.
func watchProxyFile() {
	if proxies.path == "" {
		return
	}
	go func() {
		t := time.NewTicker(proxyReloadInterval)
		defer t.Stop()
		for range t.C {
			proxies.reload(false)
		}
	}()
}

func (p *proxyPool) enabled() bool { return len(p.list()) > 0 }

// isDead reports whether the proxy is currently marked dead (TTL-expired
// marks are forgotten so a recovered node gets another chance).
func (p *proxyPool) isDead(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead == nil {
		return false
	}
	t, ok := p.dead[key]
	if !ok {
		return false
	}
	if time.Since(t) > deadTTL {
		delete(p.dead, key)
		return false
	}
	return true
}

// markDead flags the proxy as unusable for deadTTL.
func (p *proxyPool) markDead(u *url.URL) {
	if u == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead == nil {
		p.dead = make(map[string]time.Time)
	}
	p.dead[u.String()] = time.Now()
}

// currentURL returns the active proxy (nil when disabled), advancing past
// dead entries.
func (p *proxyPool) currentURL() *url.URL {
	list := p.list()
	if len(list) == 0 {
		return nil
	}
	for i := 0; i < len(list); i++ {
		u := list[p.idx.Load()%uint64(len(list))]
		if !p.isDead(u.String()) {
			return u
		}
		p.idx.Add(1)
	}
	// everything marked dead — serve the current one rather than nil
	// (the caller's pre-check will re-verify and the TTL will expire)
	return list[p.idx.Load()%uint64(len(list))]
}

// rotate advances to the next live proxy; returns a human-readable label.
func (p *proxyPool) rotate() string {
	p.reload(false) // pick up routes appended/pruned since the last check
	list := p.list()
	if len(list) == 0 {
		return "none"
	}
	for i := 0; i < len(list); i++ {
		next := p.idx.Add(1) % uint64(len(list))
		u := list[next]
		if !p.isDead(u.String()) {
			return u.Redacted()
		}
	}
	return list[p.idx.Load()%uint64(len(list))].Redacted()
}
