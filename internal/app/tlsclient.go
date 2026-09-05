// tlsclient.go — browser-fingerprinted HTTP clients (bogdanfinn/tls-client).
// All outgoing traffic (Z.AI + Aliyun captcha) goes through a Chrome_146
// TLS profile so WAFs see a real browser fingerprint, not Go's default.
package app

import (
	"fmt"
	"log"
	"net/url"
	"sync"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// httpDoer is everything we need from an HTTP client.
type httpDoer interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}

// zaiUserAgent — latest Chrome UA matching the TLS profile below.
const zaiUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

var (
	clientCache   = map[string]httpDoer{}
	clientCacheMu sync.Mutex
)

// browserClient returns a Chrome-fingerprinted client, cached per
// (proxy, timeout). proxy "" = direct; timeoutSecs 0 = no timeout
// (streaming responses need long-lived connections).
func browserClient(proxy string, timeoutSecs int) (httpDoer, error) {
	// Proxied clients are NOT cached: pool rotation can touch thousands of
	// proxies and every cached tls-client keeps its own connection pool —
	// the accumulated sockets starve new dials (observed as instant write
	// timeouts after mass rotation). Per-attempt clients are cheap; the
	// direct clients (a couple of fixed keys) stay cached.
	if proxy != "" {
		return newBrowserClient(proxy, timeoutSecs)
	}
	key := fmt.Sprintf("%s|%d", proxy, timeoutSecs)
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c, nil
	}
	c, err := newBrowserClient(proxy, timeoutSecs)
	if err != nil {
		return nil, err
	}
	clientCache[key] = c
	log.Printf("[TLS] browser client ready (profile=chrome_146 proxy=%q timeout=%ds)", proxy, timeoutSecs)
	return c, nil
}

// newBrowserClient builds a fresh tls-client instance (uncached).
func newBrowserClient(proxy string, timeoutSecs int) (httpDoer, error) {
	// tls-client quirks, both handled by remapping 0 to a generous timeout:
	//  1) NOT passing the option at all defaults to a hard 30s whole-request
	//     deadline that silently kills thinking-heavy streams;
	//  2) passing 0 (documented as "unlimited") breaks PROXIED requests —
	//     the CONNECT phase does SetDeadline(now+0), an instantly-expired
	//     deadline, so every proxy write fails with an i/o timeout.
	// 600s clears every real bound (per-attempt ctx, idle watchdog).
	if timeoutSecs <= 0 {
		timeoutSecs = 600
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithTimeoutSeconds(timeoutSecs),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}
	c, err := tls_client.NewHttpClient(nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("tls client (proxy=%q): %w", proxy, err)
	}
	if proxy != "" {
		log.Printf("[TLS] browser client ready (profile=chrome_146 proxy=%q timeout=%ds)", proxy, timeoutSecs)
	}
	return c, nil
}

// zaiHTTP returns a Chrome-fingerprinted client for Z.AI traffic.
//
// Proxy policy (since "use proxies only in chat, only on blocks"):
//   - nil → direct connection. ALL auxiliary paths go direct: model catalog
//     fetch, session init/auths, config scraping (Aliyun captcha was always
//     direct).
//   - non-nil → that exact proxy is forced. Only the chat path
//     (doZAIRequest via sendToZAI) does this, and only on retries after a
//     WAF block rotated the pool.
func zaiHTTP(proxy *url.URL) (httpDoer, error) {
	p := ""
	if proxy != nil {
		p = proxy.String()
	}
	return browserClient(p, 0)
}
