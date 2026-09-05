// token-collector/main.go — Device-token collector for Aliyun Captcha.
// Go port of .assets/token-helper-js/index.js (Playwright → chromedp).
//
// Launches headless Chrome, navigates to chat.z.ai, triggers the token
// endpoint, then calls window.z_um.getToken() in a loop to collect device
// tokens. Saves them to tokens.sqlite for the captcha solver.
//
// Usage:
//
//	go run ./token-collector/
//	go build -o token-collector.exe ./token-collector/
//	token-collector.exe --headless=false        # visible browser
//	token-collector.exe --count 500 --out my_tokens.sqlite
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	_ "modernc.org/sqlite"
)

const (
	maxTokens           = 1250
	defaultTokens       = 750
	maxRetries          = 3
	tokenCollectTimeout = 90 * time.Second
	zaiURL              = "https://chat.z.ai"
)

func main() {
	var (
		count    int
		outPath  string
		headless bool
	)
	flag.IntVar(&count, "count", defaultTokens, fmt.Sprintf("Number of tokens to collect (max %d)", maxTokens))
	flag.StringVar(&outPath, "out", "tokens.sqlite", "Output SQLite database path")
	flag.BoolVar(&headless, "headless", true, "Run browser headless")
	flag.Parse()

	if count <= 0 {
		count = defaultTokens
	}
	if count > maxTokens {
		fmt.Printf("Capping to max %d.\n", maxTokens)
		count = maxTokens
	}

	// Interactive prompt if no --count flag given explicitly
	countChanged := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "count" {
			countChanged = true
		}
	})
	if !countChanged {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("How many tokens to collect? [default: %d, max: %d] ", defaultTokens, maxTokens)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if n, err := strconv.Atoi(line); err == nil && n > 0 {
				count = n
				if count > maxTokens {
					count = maxTokens
				}
			}
		}
	}

	fmt.Printf("\nCollecting %d tokens\n", count)

	// Chrome allocator options
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", headless),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// Listen for any JavaScript dialogs (like beforeunload) and automatically accept them
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if _, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			go chromedp.Run(ctx, page.HandleJavaScriptDialog(true))
		}
	})

	var success bool

	for attempt := 1; attempt <= maxRetries; attempt++ {
		fmt.Printf("\nAttempt %d of %d\n", attempt, maxRetries)

		// On retry, try to clear the chat input before navigating,
		// which acts as a secondary measure against "unsaved changes" warnings.
		if attempt > 1 {
			chromedp.Run(ctx, chromedp.Evaluate(`
				var input = document.querySelector("#chat-input");
				if (input) {
					input.value = "";
				}
			`, nil))
		}

		err := tryCollect(ctx, count, outPath)
		if err != nil {
			fmt.Printf("  Attempt %d failed: %v\n", attempt, err)
			if attempt == maxRetries {
				fmt.Println("  All retries exhausted.")
				log.Printf("Error: %v", err)
				waitForEnter()
				os.Exit(1)
			}
			fmt.Println("  Retrying with a fresh page load...")
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

func tryCollect(ctx context.Context, total int, outPath string) error {
	// Navigate to Z.AI
	fmt.Println("  Navigating to chat.z.ai...")
	if err := chromedp.Run(ctx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitVisible(`#chat-input`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("page load failed: %w", err)
	}
	fmt.Println("  Chat input found")

	// Fill textarea and click send
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(`#chat-input`, "__", chromedp.ByQuery),
		chromedp.Click(`#send-message-button`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("send click failed: %w", err)
	}
	fmt.Println("  Send clicked")

	// Wait for window.z_um.getToken to be ready — the page injects the captcha
	// SDK async after the first send, so a fixed sleep is flaky under load.
	fmt.Println("  Waiting for token endpoint...")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	if err := waitForZUM(waitCtx); err != nil {
		return fmt.Errorf("token endpoint not ready: %w", err)
	}

	// Collect tokens with timeout
	fmt.Println("  Collecting tokens...")
	collectCtx, cancel := context.WithTimeout(ctx, tokenCollectTimeout)
	defer cancel()

	t0 := time.Now()

	// Single batch JS evaluation — calls window.z_um.getToken() in a loop
	jsExpr := fmt.Sprintf(`(async () => {
		const out = new Array(%d);
		for (let i = 0; i < %d; i++) {
			const tok = window.z_um.getToken();
			out[i] = (tok && typeof tok.then === 'function') ? await tok : tok;
			if (i %% 50 === 0) await new Promise(r => setTimeout(r, 0));
		}
		return out;
	})()`, total, total)

	// Use runtime.Evaluate with awaitPromise + returnByValue
	var tokens []string
	err := chromedp.Run(collectCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			result, exception, err := runtime.Evaluate(jsExpr).
				WithAwaitPromise(true).
				WithReturnByValue(true).
				Do(ctx)
			if err != nil {
				return err
			}
			if exception != nil {
				return fmt.Errorf("JS exception: %s", exception.Text)
			}
			if result == nil || result.Value == nil {
				return fmt.Errorf("empty result from getToken")
			}
			return json.Unmarshal(result.Value, &tokens)
		}),
	)
	if err != nil {
		return fmt.Errorf("token collection failed: %w", err)
	}

	// Filter out empty or invalid tokens
	var validTokens []string
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok != "" && tok != "null" && tok != "undefined" {
			validTokens = append(validTokens, tok)
		}
	}

	if len(validTokens) == 0 {
		return fmt.Errorf("no valid tokens collected (received empty values)")
	}

	elapsed := time.Since(t0).Seconds()
	fmt.Printf("  Collected %d valid tokens in %.2fs\n", len(validTokens), elapsed)

	// Save to SQLite
	fmt.Println("  Building SQLite database...")
	if err := saveTokens(outPath, validTokens); err != nil {
		return fmt.Errorf("save failed: %w", err)
	}

	info, _ := os.Stat(outPath)
	sizeKB := 0.0
	if info != nil {
		sizeKB = float64(info.Size()) / 1024
	}
	fmt.Printf("  Saved: %s (%.1f KB)\n", outPath, sizeKB)

	return nil
}

// waitForZUM polls until window.z_um.getToken is defined. Z.AI injects the
// captcha SDK async after the first send; a fixed sleep misses it under load.
// ponytail: 500ms poll — z_um appears in ~2-5s; 30s deadline (caller) is the ceiling.
func waitForZUM(ctx context.Context) error {
	for {
		var ready bool
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(`typeof window.z_um !== 'undefined' && typeof window.z_um.getToken === 'function'`, &ready),
		); err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// saveTokens creates a SQLite database with the collected tokens.
func saveTokens(path string, tokens []string) error {
	// Remove existing file to start fresh
	os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE tokens (id INTEGER PRIMARY KEY, token TEXT);"); err != nil {
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
// console window (e.g. when the .exe is double-clicked on Windows).
// Skipped when stdin is piped/redirected, so automation isn't blocked.
func waitForEnter() {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) == 0 {
		return // not interactive (piped/redirected) — don't block
	}
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
