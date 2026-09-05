// proxy.go — rotating proxy pool for Z.AI requests (WAF evasion).
// Enabled via USE_PROXY=true; list lives in PROXY_FILE (default proxy.txt),
// one proxy per line: host:port, http://host:port, socks5://host:port.
// On an Aliyun WAF block the pool advances to the next proxy; existing
// retry loops (sendToZAI 401/WAF, anthropic WAF retries) then hit a new IP.
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

const deadTTL = 10 * time.Minute

type proxyPool struct {
	urls []*url.URL
	idx  atomic.Uint64

	mu   sync.Mutex
	dead map[string]time.Time // proxy URL string → marked-dead time
}

var proxies = &proxyPool{}

// loadProxyPool parses the proxy file. Bare host:port defaults to http://.
func loadProxyPool(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
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
		proxies.urls = append(proxies.urls, u)
	}
	if len(proxies.urls) == 0 {
		return fmt.Errorf("no valid proxies in %s", path)
	}
	return nil
}

func (p *proxyPool) enabled() bool { return len(p.urls) > 0 }

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
	if !p.enabled() {
		return nil
	}
	for i := 0; i < len(p.urls); i++ {
		u := p.urls[p.idx.Load()%uint64(len(p.urls))]
		if !p.isDead(u.String()) {
			return u
		}
		p.idx.Add(1)
	}
	// everything marked dead — serve the current one rather than nil
	// (the caller's pre-check will re-verify and the TTL will expire)
	return p.urls[p.idx.Load()%uint64(len(p.urls))]
}

// rotate advances to the next live proxy; returns a human-readable label.
func (p *proxyPool) rotate() string {
	if !p.enabled() {
		return "none"
	}
	for i := 0; i < len(p.urls); i++ {
		next := p.idx.Add(1) % uint64(len(p.urls))
		u := p.urls[next]
		if !p.isDead(u.String()) {
			return u.Redacted()
		}
	}
	return p.urls[p.idx.Load()%uint64(len(p.urls))].Redacted()
}
