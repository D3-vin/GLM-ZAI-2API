// zaitoken.go — Z.AI account pool: ZAI_TOKENS_FILE with one JWT per line.
//
// Policy (user requirement): a single hardcoded ZAI_TOKEN may die mid-life
// (platform ban USER_BLOCKED, 401 expiry), killing every chat. With a pool,
// the bridge picks the next healthy account instead:
//   - at session init: candidates are validated against GET /api/v1/auths/
//     (direct, browser-fingerprinted). Definitively dead ones (401/403)
//     are skipped; the first alive one wins.
//   - if EVERY provided token dies, init falls back to the guest flow so the
//     service keeps working (limited model set) rather than refusing.
//   - mid-session 401/USER_BLOCKED chat errors rotate to the next account
//     (see sendToZAI's unauthorized branch).
package app

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

type zaiTokenPool struct {
	mu     sync.Mutex
	tokens []string
	idx    int // current candidate (round-robin across restarts of a process)
}

// zaiAccounts is the process-wide pool; nil until configured.
var zaiAccounts *zaiTokenPool

// loadZaiTokens parses the token file: one JWT per line, '#' comments and
// blank lines ignored, duplicates dropped, whitespace trimmed.
func loadZaiTokens(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	var tokens []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		tokens = append(tokens, line)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("no usable tokens in %s", path)
	}
	zaiAccountsMu.Lock()
	defer zaiAccountsMu.Unlock()
	zaiAccounts = &zaiTokenPool{tokens: tokens}
	log.Printf("[Session] Token pool loaded: %d accounts from %s", len(tokens), path)
	return nil
}

var zaiAccountsMu sync.Mutex // guards zaiAccounts swap in loader

// countPoolTokens reports how many usable (non-comment) lines the pool file
// has WITHOUT installing it as the active pool. Used for the hint logged
// when USE_ACCOUNT_POOL is off but tokens sit in the file.
func countPoolTokens(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	return n
}

func (p *zaiTokenPool) size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens)
}

func (p *zaiTokenPool) index() int {
	if p == nil || len(p.tokens) == 0 {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx
}

// current returns the selected candidate ("" for an absent pool).
func (p *zaiTokenPool) current() string {
	if p == nil || len(p.tokens) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens[p.idx%len(p.tokens)]
}

// rotate advances the round-robin pointer and returns the new candidate.
func (p *zaiTokenPool) rotate() string {
	if p == nil || len(p.tokens) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idx = (p.idx + 1) % len(p.tokens)
	return p.tokens[p.idx]
}

// nextZaiToken picks the account to (re)initialize with: the pool's current
// candidate, or the legacy single-ZAI_TOKEN config when no pool is loaded.
func nextZaiToken(single string) string {
	zaiAccountsMu.Lock()
	pool := zaiAccounts
	zaiAccountsMu.Unlock()
	if t := pool.current(); t != "" {
		return t
	}
	return single
}

// rotateZaiAccount advances past a dead account and returns who comes next
// (same single-token fallback rule).
func rotateZaiAccount(single string) string {
	zaiAccountsMu.Lock()
	pool := zaiAccounts
	zaiAccountsMu.Unlock()
	if t := pool.rotate(); t != "" {
		return t
	}
	return single
}

// classifyAuthCheck turns an auths/ probe result into an account verdict.
// Direct HTTP tests (tls-client) hit this endpoint fine even though plain
// curl gets WAF'd — a clean 2xx with a JSON id proves the account is alive;
// 401/403 mean it is definitively unusable; anything else (network flake,
// odd response) must NOT kill an otherwise-plausible token, so callers treat
// ambiguous verdicts as alive to preserve the historical offline-tolerant
// boot.
func classifyAuthCheck(status int, body string) string {
	switch {
	case status >= 200 && status < 300 && strings.Contains(body, "\"id\""):
		return "alive"
	case status == 401 || status == 403 || status == 410:
		return "dead"
	default:
		return "ambiguous"
	}
}

// shortToken makes a long JWT safe to print in logs.
func shortToken(t string) string {
	if len(t) > 16 {
		return t[:16] + "..."
	}
	return t
}
