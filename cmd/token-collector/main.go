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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

	browser, err := launchBrowser(pw, headed)
	if err != nil {
		log.Fatalf("launch: %v", err)
	}
	defer browser.Close()

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
	if !success {
		fmt.Println("Failed after maximum retries.")
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
func launchBrowser(pw *playwright.Playwright, headed bool) (playwright.Browser, error) {
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

// saveTokens creates a SQLite database with the collected tokens.
func saveTokens(path string, tokens []string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot remove old %s (is the bridge running?): %w", path, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS tokens (id INTEGER PRIMARY KEY, token TEXT);"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO tokens (id, token) VALUES (?, ?);")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for i, tok := range tokens {
		if _, err := stmt.Exec(i, tok); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
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
