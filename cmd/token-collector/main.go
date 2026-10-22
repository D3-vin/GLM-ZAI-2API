// token-collector — device-token collector on playwright-go.
//
// Replaced the chromedp version (2026-09-03): chromedp-collected tokens are
// always rejected by Aliyun (F001) in headless, playwright-go with flag-only
// stealth pass (T001). Stealth is FLAGS ONLY (no JS patches):
//
//   - Headless:false + --headless=new (the new engine: full browser, no window);
//   - Channel:"chrome" uses the installed system Chrome (no download);
//   - --user-agent kills the HeadlessChrome tell;
//   - --enable-automation stripped from Playwright defaults.
//
// First run bootstraps the playwright driver automatically (playwright-core +
// node, ~40 MB, one-time; browsers are NOT downloaded). Flow is the production
// one: chat page → "__" → send → wait z_um → batch getToken → tokens.sqlite.
//
// Usage:
//
//	token-collector.exe --count 8            # stealth headless=new
//	token-collector.exe --count 8 --headed   # visible window (control)
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
	_ "modernc.org/sqlite"
)

const (
	maxTokens           = 1250
	defaultTokens       = 750
	maxRetries          = 3
	tokenCollectTimeout = 90 * time.Second
	zaiURL              = "https://chat.z.ai"
	zumWaitTimeout      = 30 * time.Second
)

// collectorUA matches the bridge's zaiUserAgent (Chrome/146): the page must
// not see "HeadlessChrome" in navigator.userAgent — z_um bakes it into the
// device token and Aliyun rejects it (F001).
const collectorUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// oddiProxyURL is the single source of egress routes (oddi-pool-api) — the
// banned free-proxy-file pattern is not used for the collector. The API
// returns only status='working' routes already ordered stability-first
// (success_rate desc, fail_streak asc, latency asc), so the collector no
// longer needs its own probe/rotation.
//
// Auth: ODDI_PROXY_TOKEN wins; otherwise the token is read from the pool API's
// own token file (same box, root-only 0600). Falling back to the file keeps the
// collector self-sufficient — an operator running it by hand (or from a cron
// wrapper that forgets the env) still gets a routed run instead of a silent
// direct-egress one.
//
// ODDI_PROXY_DISABLE=1 turns routing off (direct egress).
const (
	// oct22: ask for a wider candidate list. With limit=5 the API returned only
	// the five freshest routes, and when all five were dead the collector fell
	// back to direct egress mid-run instead of looking further down the pool —
	// observed on 2026-10-22: five stale socks5 routes, the first live one
	// (87.192.231.122:3128) sat at position 6 and was never requested at all.
	// The pool holds hundreds of routes; requesting 40 costs one cheap call and
	// leaves the pre-flight probe to reject the dead ones.
	oddiProxyDefault = "http://127.0.0.1:9121/v1/proxies?limit=40"
	oddiTokenDefault = "/root/.hermes/scripts/oddi-pool-api/.token"
)

// oddiToken resolves the oddi-pool-api bearer token ("" when none found).
func oddiToken() string {
	if tok := os.Getenv("ODDI_PROXY_TOKEN"); tok != "" {
		return tok
	}
	path := os.Getenv("ODDI_TOKEN_FILE")
	if path == "" {
		path = oddiTokenDefault
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// pickEgressRoute asks oddi-pool-api for working routes and returns the first
// one that actually reaches chat.z.ai as a Chrome --proxy-server value.
//
// Why probe: the pool's status='working' is a historical verdict from the last
// check, not a live guarantee — on 2026-10-18 six "working" routes in a row
// were dead (rc=000) and only the seventh answered 200. Handing Chrome a dead
// route costs a full browser launch plus a retry cycle, so we verify with a
// cheap pre-flight GET before committing. Returns "" when the pool is
// disabled/unreachable or no candidate answers — the caller then launches
// without a proxy rather than failing the run.
//
// Env: ODDI_PROXY_DISABLE=1 -> off; ODDI_PROXY_URL, ODDI_PROXY_TOKEN,
// ODDI_PROXY_TRIES (default 6), ODDI_PROXY_TIMEOUT (seconds, default 12).
func pickEgressRoute() string {
	if os.Getenv("ODDI_PROXY_DISABLE") == "1" {
		return ""
	}
	api := os.Getenv("ODDI_PROXY_URL")
	if api == "" {
		api = oddiProxyDefault
	}
	// oct22: was 6. The pool carries hundreds of routes and a large share are
	// dead at any moment (5 of the first 5 were dead on 2026-10-22), so six
	// probes routinely exhausted before reaching a live one. Each probe is a
	// 12s-bounded GET and a dead route usually fails fast (connection refused)
	// or burns the full timeout (dead tunnel); 12 leaves room for a mixed run
	// while staying inside the wrapper's 280s collector budget.
	tries := 12
	if v := os.Getenv("ODDI_PROXY_TRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tries = n
		}
	}
	timeout := 12 * time.Second
	if v := os.Getenv("ODDI_PROXY_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return ""
	}
	if tok := oddiToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("  oddi proxy API unreachable (%v) - direct egress\n", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("  oddi proxy API http=%d - direct egress\n", resp.StatusCode)
		return ""
	}
	var body struct {
		Proxies []struct {
			Protocol string `json:"protocol"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
		} `json:"proxies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Proxies) == 0 {
		fmt.Printf("  oddi proxy API returned no routes - direct egress\n")
		return ""
	}
	for i, p := range body.Proxies {
		if i >= tries {
			break
		}
		scheme := p.Protocol
		if scheme == "" {
			scheme = "http"
		}
		url := fmt.Sprintf("%s://%s:%d", scheme, p.Host, p.Port)
		if proxyReachesZAI(url, timeout) {
			fmt.Printf("  egress route: %s (from oddi-pool-api, verified)\n", url)
			return url
		}
		fmt.Printf("  route %s did not answer - trying next\n", url)
	}
	fmt.Printf("  no oddi route reached chat.z.ai - direct egress\n")
	return ""
}

// proxyReachesZAI pre-flights one route: does an HTTP GET to chat.z.ai through
// it come back with a status code? Any response proves the tunnel carries
// traffic (a WAF 403 still means the route works); a transport error does not.
//
// oct22: HEAD-first. A dead tunnel burns the full timeout waiting for headers
// that never arrive (measured: 12s per dead socks5 route), and with 12 tries
// that alone can exceed the wrapper's 280s collector budget. HEAD is enough to
// prove the tunnel carries traffic and is what a pre-flight needs; the browser
// does the real GET afterwards.
func proxyReachesZAI(proxyURL string, timeout time.Duration) bool {
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	client := &http.Client{Timeout: timeout, Transport: tr}
	resp, err := client.Head(zaiURL + "/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode > 0
}

func main() {
	var (
		count   int
		outPath string
		headed  bool
	)
	flag.IntVar(&count, "count", defaultTokens, fmt.Sprintf("Number of tokens to collect (max %d)", maxTokens))
	flag.StringVar(&outPath, "out", "tokens.sqlite", "Output SQLite database path")
	flag.BoolVar(&headed, "headed", false, "Run browser with visible window (default: stealth headless=new)")
	flag.Parse()

	if count <= 0 {
		count = defaultTokens
	}
	if count > maxTokens {
		fmt.Printf("Capping to max %d.\n", maxTokens)
		count = maxTokens
	}
	fmt.Printf("\nCollecting %d tokens (playwright-go, stealth-by-flags, %s)\n",
		count, map[bool]string{true: "HEADED", false: "headless=new"}[headed])

	pw, err := ensurePlaywright()
	if err != nil {
		log.Fatalf("playwright bootstrap: %v", err)
	}
	defer pw.Stop()

	route := pickEgressRoute()
	browser, err := launchBrowser(pw, headed, route)
	if err != nil {
		log.Fatalf("launch: %v", err)
	}

	var success bool
	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("\nAttempt %d of %d\n", attempt, maxRetries)
		err := tryCollect(browser, count, outPath)
		if err != nil {
			fmt.Printf("  Attempt %d failed: %v\n", attempt, err)
			if attempt == maxRetries {
				break
			}
			fmt.Println("  Retrying with a fresh page...")
			continue
		}
		success = true
		break
	}
	browser.Close()

	// oct22: direct-egress fallback. Routing through oddi-pool-api is the
	// preferred path, but the pool's "working" routes are volatile: on
	// 2026-10-22 every candidate in the first 40 either timed out or reset the
	// connection, and each routed browser run burned its full retry budget.
	// chat.z.ai answered directly (HTTP 200, 0.35s) the whole time, so a run
	// that fails with a proxy still has a good chance without one. Only when a
	// route was actually used — a run that started direct has nothing to fall
	// back from, and repeating it would just double the wall time.
	if !success && route != "" {
		fmt.Printf("\nAll %d attempts failed via %s - retrying with direct egress\n", maxRetries, route)
		browser, err = launchBrowser(pw, headed, "")
		if err != nil {
			log.Fatalf("launch (direct): %v", err)
		}
		defer browser.Close()
		for attempt := 1; attempt <= maxRetries; attempt++ {
			fmt.Printf("\nDirect attempt %d of %d\n", attempt, maxRetries)
			err := tryCollect(browser, count, outPath)
			if err != nil {
				fmt.Printf("  Direct attempt %d failed: %v\n", attempt, err)
				if attempt == maxRetries {
					break
				}
				fmt.Println("  Retrying with a fresh page...")
				continue
			}
			success = true
			break
		}
	}

	if !success {
		fmt.Println("Failed after maximum retries (proxy and direct).")
		waitForEnter()
		os.Exit(1)
	}
	fmt.Println("\nScript finished successfully.")
}

// ensurePlaywright makes playwright.Run() viable:
//  1. driver already installed → no-op;
//  2. driver half-installed (package/ present, node missing) → reuse the
//     system node via PLAYWRIGHT_NODEJS_PATH instead of re-downloading;
//  3. otherwise download once (playwright-core + node, ~40 MB, no browsers).
//     If the download fails through the ambient proxy (e.g. a local
//     TLS-intercepting mitmproxy whose CA Go does not trust), retry direct
//     by temporarily scrubbing the proxy environment variables.
func ensurePlaywright() (*playwright.Playwright, error) {
	pw, err := playwright.Run()
	if err == nil {
		return pw, nil
	}

	if os.Getenv("PLAYWRIGHT_NODEJS_PATH") == "" {
		if node, lookErr := exec.LookPath("node"); lookErr == nil {
			os.Setenv("PLAYWRIGHT_NODEJS_PATH", node)
			if pw, err = playwright.Run(); err == nil {
				return pw, nil
			}
		}
	}

	fmt.Println("First run: downloading playwright driver (one-time, ~40 MB; browsers are NOT downloaded)...")
	installErr := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true})
	if installErr != nil {
		scrubErr := retryInstallDirect()
		if scrubErr != nil {
			return nil, fmt.Errorf("driver install failed (proxy: %v / direct: %v)", installErr, scrubErr)
		}
	}
	return playwright.Run()
}

// retryInstallDirect re-runs the driver download bypassing any ambient HTTP
// proxy, for networks where the proxy MITMs TLS with an untrusted CA. Two
// layers are needed: unsetting the env vars is NOT enough because Go caches
// the env-derived proxy (sync.Once inside http.ProxyFromEnvironment), so the
// shared DefaultTransport's Proxy hook is also cleared for the retry.
// Returns nil on success.
func retryInstallDirect() error {
	proxyVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}
	var savedEnv [][2]string
	for _, k := range proxyVars {
		if v, ok := os.LookupEnv(k); ok {
			savedEnv = append(savedEnv, [2]string{k, v})
			os.Unsetenv(k)
		}
	}
	transport, haveTransport := http.DefaultTransport.(*http.Transport)
	if haveTransport {
		transport.Proxy = nil
	}
	defer func() {
		for _, kv := range savedEnv {
			os.Setenv(kv[0], kv[1])
		}
		if haveTransport {
			transport.Proxy = http.ProxyFromEnvironment
		}
	}()

	fmt.Println("  download failed via proxy, retrying direct (proxy bypassed)...")
	return playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true})
}

// launchBrowser — stealth by flags only:
//   - Headless:false is OUR baseline; real headless comes from --headless=new
//     (the new engine: full real browser minus the window);
//   - Channel:"chrome" uses the installed system Chrome (no download);
//   - --user-agent kills the HeadlessChrome tell;
//   - --enable-automation stripped from Playwright defaults.
func launchBrowser(pw *playwright.Playwright, headed bool, route string) (playwright.Browser, error) {
	base := []string{
		"--disable-blink-features=AutomationControlled",
		"--user-agent=" + collectorUA,
		"--window-size=1920,1080",
		"--lang=en-US",
		"--no-first-run",
		"--no-default-browser-check",
		"--password-store=basic",
		"--use-mock-keychain",
	}
	if route != "" {
		// Route the browser through the oddi-pool egress route. Chrome takes
		// scheme://host:port for --proxy-server (socks5:// included), which is
		// why pickEgressRoute returns the fully-qualified URL.
		base = append(base, "--proxy-server="+route)
	}
	launch := playwright.BrowserTypeLaunchOptions{
		Channel:           playwright.String("chrome"), // system Chrome, no download
		Headless:          playwright.Bool(false),      // OUR flag controls headlessness
		Args:              base,
		IgnoreDefaultArgs: []string{"--enable-automation"},
	}
	if !headed {
		launch.Args = append(append([]string{}, base...), "--headless=new")
	} else {
		launch.Args = base
	}
	return pw.Chromium.Launch(launch)
}

func tryCollect(browser playwright.Browser, total int, outPath string) error {
	page, err := browser.NewPage()
	if err != nil {
		return fmt.Errorf("new page: %w", err)
	}
	defer page.Close()

	fmt.Println("  Opening chat.z.ai...")
	if _, err := page.Goto(zaiURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return fmt.Errorf("goto: %w", err)
	}
	if err := page.Locator("#chat-input").WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(20000)},
	); err != nil {
		return fmt.Errorf("chat input not found: %w", err)
	}
	fmt.Println("  Chat input found")

	fmt.Println("  Sending \"__\" (device registration trigger)...")
	if err := page.Locator("#chat-input").Fill("__"); err != nil {
		return fmt.Errorf("fill: %w", err)
	}
	if err := page.Locator("#send-message-button").Click(
		playwright.LocatorClickOptions{Timeout: playwright.Float(10000)},
	); err != nil {
		return fmt.Errorf("send click: %w", err)
	}
	fmt.Println("  Send clicked")

	// z_um appears asynchronously after the first send (SDK injection).
	if err := waitForZUM(page); err != nil {
		return fmt.Errorf("token endpoint not ready: %w", err)
	}

	fmt.Println("  Collecting tokens...")
	ctx, cancel := context.WithTimeout(context.Background(), tokenCollectTimeout)
	defer cancel()

	type evalResult struct {
		val interface{}
		err error
	}
	ch := make(chan evalResult, 1)
	go func() {
		val, err := page.Evaluate(`async (total) => {
			const out = new Array(total);
			for (let i = 0; i < total; i++) {
				const tok = window.z_um.getToken();
				out[i] = (tok && typeof tok.then === 'function') ? await tok : tok;
				if (i % 50 === 0) await new Promise(r => setTimeout(r, 0));
			}
			return out;
		}`, total)
		ch <- evalResult{val, err}
	}()

	var tokens []string
	select {
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("evaluate: %w", res.err)
		}
		arr, ok := res.val.([]interface{})
		if !ok {
			return fmt.Errorf("unexpected evaluate result type: %T", res.val)
		}
		for _, v := range arr {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				tokens = append(tokens, s)
			}
		}
	case <-ctx.Done():
		return fmt.Errorf("token collection timed out after %s", tokenCollectTimeout)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("no valid tokens collected")
	}
	fmt.Printf("  Collected %d tokens\n", len(tokens))

	if err := saveTokens(outPath, tokens); err != nil {
		return fmt.Errorf("save failed: %w", err)
	}
	abs, _ := os.Getwd()
	info, _ := os.Stat(outPath)
	sizeKB := 0.0
	if info != nil {
		sizeKB = float64(info.Size()) / 1024
	}
	fmt.Printf("  Saved: %s (%.1f KB)\n  Bridge DB_PATH must point here (absolute: %s)\n",
		outPath, sizeKB, filepath.Join(abs, outPath))
	return nil
}

// waitForZUM polls until window.z_um.getToken is defined — the SDK is
// injected asynchronously after the first send.
func waitForZUM(page playwright.Page) error {
	deadline := time.Now().Add(zumWaitTimeout)
	for {
		ready, err := page.Evaluate(`typeof window.z_um !== 'undefined' && typeof window.z_um.getToken === 'function'`)
		if err == nil {
			if b, ok := ready.(bool); ok && b {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for z_um")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ensureTokenIndex guarantees UNIQUE(token), so INSERT OR IGNORE really dedupes.
//
// A UNIQUE index cannot be built while duplicate rows exist — the historical
// state of DBs written before this guard. Aborting on that error loses the whole
// collection run (fresh tokens are dropped and the pool keeps draining), so
// treat it as self-healing: drop duplicate rows keeping the lowest id, then
// create the index. Safe to call on every run.
func ensureTokenIndex(db *sql.DB) error {
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_token ON tokens(token);"); err == nil {
		return nil
	}
	if _, err := db.Exec("DELETE FROM tokens WHERE id NOT IN (SELECT MIN(id) FROM tokens GROUP BY token);"); err != nil {
		return err
	}
	_, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_token ON tokens(token);")
	return err
}

// saveTokens APPENDS the collected tokens to the SQLite store.
//
// It must NOT replace the file: the bridge keeps its SQLite connection open, so
// a replaced file (new inode) leaves the running process reading the old, empty
// database — freshly collected tokens would only appear after a bridge restart.
// Appending to the same inode makes a refill visible immediately.
func saveTokens(path string, tokens []string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout = 10000;"); err != nil {
		return err
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS tokens (id INTEGER PRIMARY KEY, token TEXT);"); err != nil {
		return err
	}
	if err := ensureTokenIndex(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT OR IGNORE INTO tokens (token) VALUES (?);")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	added := 0
	for _, tok := range tokens {
		res, err := stmt.Exec(tok)
		if err != nil {
			tx.Rollback()
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("  Appended %d new tokens to %s (same inode — bridge restart NOT needed)\n", added, path)
	return nil
}

// waitForEnter pauses before exit so fatal errors stay visible in the
// console window. Skipped when stdin is piped (automation).
func waitForEnter() {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
