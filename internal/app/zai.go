// zai.go — Z.AI session, HMAC-SHA256 signature, and SSE streaming.
// Ports main.js session logic + sendToZAI async generator into Go.
package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// baseURL is a var (not const) so tests can point it at an httptest server
// instead of the real API.
var baseURL = "https://chat.z.ai"

const (
	saltKey          = "key-@@@@)))()((9))-xxxx&&&%%%%%"
	defaultFEVersion = "prod-fe-1.0.185"
	sessionTTL       = 30 * time.Minute
)

// ── Global Z.AI session state ──

type ZaiSession struct {
	mu           sync.Mutex
	token        string
	userID       string
	userName     string
	chatID       string
	feVersion    string
	initialized  bool
	initializing bool
}

func newZaiSession() *ZaiSession {
	return &ZaiSession{
		userName:  "Guest",
		chatID:    uuidV4(),
		feVersion: defaultFEVersion,
	}
}

// ── Per-conversation state ──

type Conversation struct {
	chatID   string
	messages []map[string]interface{}
	lastUsed time.Time
}

type ConversationStore struct {
	mu       sync.Mutex
	sessions map[string]*Conversation
}

func newConversationStore() *ConversationStore {
	cs := &ConversationStore{sessions: make(map[string]*Conversation)}
	go cs.cleanup()
	return cs
}

func (cs *ConversationStore) getOrCreate(sessionID string, fresh bool) *Conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if fresh || cs.sessions[sessionID] == nil {
		cs.sessions[sessionID] = &Conversation{
			chatID:   uuidV4(),
			lastUsed: time.Now(),
		}
	}
	s := cs.sessions[sessionID]
	s.lastUsed = time.Now()
	return s
}

func (cs *ConversationStore) clear() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.sessions = make(map[string]*Conversation)
}

func (cs *ConversationStore) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.sessions)
}

func (cs *ConversationStore) totalMessages() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	total := 0
	for _, s := range cs.sessions {
		total += len(s.messages)
	}
	return total
}

func (cs *ConversationStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cs.mu.Lock()
		now := time.Now()
		for id, s := range cs.sessions {
			if now.Sub(s.lastUsed) > sessionTTL {
				delete(cs.sessions, id)
			}
		}
		cs.mu.Unlock()
	}
}

// ── Utility ──

func uuidV4() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// getMessageContent extracts text from an OpenAI message content field,
// which can be a string or an array of content parts.
func getMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of parts
	var parts []map[string]interface{}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if t, ok := p["type"].(string); ok && t != "text" {
				continue
			}
			if text, ok := p["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// messagesToPrompt flattens an OpenAI messages array → prompt string.
// Used ONLY for signature_prompt computation (HMAC signing).
// The actual messages array is forwarded as-is to Z.AI.
func messagesToPrompt(messages []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		content := getMessageContent(msg.Content)
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// ── Z.AI HMAC-SHA256 signature ──

func (s *ZaiSession) generateSignature(prompt, token, userID string) string {
	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	requestID := uuidV4()
	bucket := time.Now().UnixMilli() / 300000

	// wKey = HMAC-SHA256(saltKey, bucket) hex
	mac := hmac.New(sha256.New, []byte(saltKey))
	mac.Write([]byte(fmt.Sprintf("%d", bucket)))
	wKey := hex.EncodeToString(mac.Sum(nil))

	// sorted payload: requestId,timestamp,user_id
	type kv struct{ k, v string }
	sorted := []kv{
		{"requestId", requestID},
		{"timestamp", timestamp},
		{"user_id", userID},
	}
	var sp strings.Builder
	for i, kv := range sorted {
		if i > 0 {
			sp.WriteByte(',')
		}
		sp.WriteString(kv.k)
		sp.WriteByte(',')
		sp.WriteString(kv.v)
	}

	promptB64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(prompt)))
	dataToSign := sp.String() + "|" + promptB64 + "|" + timestamp

	mac2 := hmac.New(sha256.New, []byte(wKey))
	mac2.Write([]byte(dataToSign))
	return hex.EncodeToString(mac2.Sum(nil))
}

// ── Scrape fe_version from Z.AI homepage ──

var feVersionRegex = regexp.MustCompile(`prod-fe-\d+\.\d+\.\d+`)

func (s *ZaiSession) scrapeConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := zaiHTTP(nil)
	if err != nil {
		log.Printf("[Config] http client: %v", err)
		return
	}
	req, _ := fhttp.NewRequestWithContext(ctx, "GET", baseURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Config] Scrape error: %v, using default feVersion", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := feVersionRegex.FindSubmatch(body); m != nil {
		s.feVersion = string(m[0])
		log.Printf("[Config] fe_version: %s", s.feVersion)
	}
}

// ── Session initialization ──

// jwtPayload decodes the payload section of a JWT (base64url, no padding).
func jwtPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// ponytail: original JS adds "==" padding and uses std base64.
		// RawURLEncoding should handle JWT correctly; fallback just in case.
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *ZaiSession) initialize(zaiToken string) error {
	s.mu.Lock()
	if s.initializing {
		s.mu.Unlock()
		// Wait for other goroutine to finish init
		for i := 0; i < 300; i++ {
			time.Sleep(100 * time.Millisecond)
			s.mu.Lock()
			if !s.initializing {
				s.mu.Unlock()
				return nil
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("session init timeout")
	}
	s.initializing = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.initializing = false
		s.mu.Unlock()
	}()

	// Fast path: provided Z.AI account(s) — validate against auths/ and pick
	// a live one (see zaitoken.go for the pool policy).
	if zaiToken != "" {
		if s.initFromAccounts(zaiToken) {
			return nil
		}
		log.Println("[Session] Provided ZAI account(s) unusable — falling back to guest session")
	}

	log.Println("[Session] Initializing Z.AI session (guest flow)...")

	s.scrapeConfig()

	headers := map[string]string{
		"Origin":       baseURL,
		"Referer":      baseURL + "/",
		"Content-Type": "application/json",
		"User-Agent":   zaiUserAgent,
	}

	// POST /api/v1/auths/guest (warm up)
	guestCtx, guestCancel := context.WithTimeout(context.Background(), 15*time.Second)
	postReq(guestCtx, baseURL+"/api/v1/auths/guest", "{}", headers)
	guestCancel()

	// GET /api/v1/auths/
	client, err := zaiHTTP(nil)
	if err != nil {
		return fmt.Errorf("http client: %w", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req, _ := fhttp.NewRequestWithContext(ctx2, "GET", baseURL+"/api/v1/auths/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth request failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("auth failed: %d", resp.StatusCode)
	}

	var authData struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &authData)
	token := authData.Token

	if token == "" {
		// Retry guest endpoint
		retryCtx, retryCancel := context.WithTimeout(context.Background(), 15*time.Second)
		guestBody := postReq(retryCtx, baseURL+"/api/v1/auths/guest", "{}", headers)
		retryCancel()
		var guestData struct {
			Token string `json:"token"`
		}
		json.Unmarshal([]byte(guestBody), &guestData)
		token = guestData.Token
	}

	if token == "" {
		return fmt.Errorf("no token received from Z.AI")
	}

	s.mu.Lock()
	s.token = token
	s.mu.Unlock()

	if payload, err := jwtPayload(token); err == nil {
		s.mu.Lock()
		if id, ok := payload["id"].(string); ok {
			s.userID = id
		}
		if email, ok := payload["email"].(string); ok && email != "" {
			s.userName = strings.Split(email, "@")[0]
		}
		s.mu.Unlock()
		uid := s.userID
		if len(uid) > 8 {
			uid = uid[:8]
		}
		log.Printf("[Session] Connected. UserID: %s... (%s)", uid, s.userName)
	} else {
		log.Println("[Session] Token decode failed, but continuing.")
	}

	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
	return nil
}

// initFromAccounts tries the configured Z.AI accounts (the caller's primary —
// typically the pool's current pick or the legacy single token — followed by
// the rest of the pool) against the live auths/ endpoint and adopts the first
// usable one. Reports true when the session fields are set.
func (s *ZaiSession) initFromAccounts(primary string) bool {
	zaiAccountsMu.Lock()
	pool := zaiAccounts
	zaiAccountsMu.Unlock()

	var extraTokens []string
	if pool != nil {
		extraTokens = pool.tokens // immutable after load; idx is the mutable part
	}

	var candidates []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			candidates = append(candidates, t)
		}
	}
	add(primary)
	for _, t := range extraTokens {
		add(t)
	}
	if len(candidates) == 0 {
		return false
	}

	for i, tok := range candidates {
		status, body := probeZaiAccount(tok)
		switch classifyAuthCheck(status, body) {
		case "alive":
			log.Printf("[Session] Account %d/%d accepted (HTTP %d)", i+1, len(candidates), status)
			s.setHardcodedAccount(tok, body)
			return true
		case "dead":
			log.Printf("[Session] Account %d/%d unusable (HTTP %d) — trying next", i+1, len(candidates), status)
		default:
			// Network/parse ambiguity: accept locally like the old offline-
			// tolerant fast path so a flaky network never bricks startup.
			log.Printf("[Session] Account %d/%d check ambiguous (HTTP %d) — accepting without live validation", i+1, len(candidates), status)
			s.setHardcodedAccount(tok, "")
			return true
		}
	}
	return false
}

// setHardcodedAccount installs a validated-or-trusted account into session
// state, deriving userID/email from the auths/ response first, then from the
// JWT payload as fallback.
func (s *ZaiSession) setHardcodedAccount(token, authBody string) {
	id, email := "", ""
	var respData struct {
		Data struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(authBody), &respData); err == nil {
		id, email = respData.Data.ID, respData.Data.Email
	}
	if id == "" || email == "" {
		if payload, perr := jwtPayload(token); perr == nil {
			if v, ok := payload["id"].(string); ok && id == "" {
				id = v
			}
			if v, ok := payload["email"].(string); ok && email == "" {
				email = v
			}
		}
	}

	name := ""
	if email != "" {
		name = strings.Split(email, "@")[0]
	}

	s.mu.Lock()
	s.token = token
	s.userID = id
	if name != "" {
		s.userName = name
	}
	s.initialized = true
	s.mu.Unlock()

	uid := id
	if len(uid) > 8 {
		uid = uid[:8]
	}
	log.Printf("[Session] Token user: %s... (%s)", uid, name)
}

// probeZaiAccount validates one account against GET /api/v1/auths/ (direct,
// browser-fingerprinted). Returns status + raw body for classification.
func probeZaiAccount(token string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := zaiHTTP(nil)
	if err != nil {
		return -1, "client error: " + err.Error()
	}
	req, _ := fhttp.NewRequestWithContext(ctx, "GET", baseURL+"/api/v1/auths/", nil)
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", zaiUserAgent)
	req.Header.Set("authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return -1, "request error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, string(body)
}

// proxiedRouteAlive verifies the route (proxy) can reach Z.AI with a cheap
// authenticated GET /api/models — no captcha device-token involved.
func proxiedRouteAlive(s *Server, proxy *url.URL) error {
	client, err := zaiHTTP(proxy)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := fhttp.NewRequestWithContext(ctx, "GET", baseURL+"/api/models", nil)
	token, _, _, _, _ := s.zaiSession.snapshot()
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("User-Agent", zaiUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return nil // reachable: 2xx-4xx means the route talks to Z.AI
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}

// postReq is a simple POST helper returning the response body as string.
func postReq(ctx context.Context, url, body string, headers map[string]string) string {
	client, err := zaiHTTP(nil)
	if err != nil {
		return ""
	}
	req, _ := fhttp.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// ── Z.AI request body ──

type zaiRequestBody struct {
	Model              string                 `json:"model"`
	ChatID             string                 `json:"chat_id"`
	Messages           []json.RawMessage      `json:"messages"`
	SignaturePrompt    string                 `json:"signature_prompt"`
	Stream             bool                   `json:"stream"`
	CaptchaVerifyParam string                 `json:"captcha_verify_param"`
	Features           map[string]interface{} `json:"features"`
	// Files carries already-uploaded Z.AI file entries (see vision.go) to
	// attach to the upstream completion request as the top-level "files"
	// array. nil for text-only requests (the field is omitted entirely).
	Files []map[string]interface{} `json:"files,omitempty"`
}

// ── sendToZAI — streaming chat completions ──

type sendOpts struct {
	model           string
	webSearch       *bool // nil = per-model default
	thinking        *bool // nil = per-model default
	reasoningEffort string
	chatID          string
	messages        []map[string]interface{}
	clientMessages  []json.RawMessage
	featuresMap     map[string]interface{} // resolved once in sendToZAI
	// routeProxy selects the outbound route for this attempt: nil = direct.
	// sendToZAI starts every logical request direct and only switches to a
	// pool proxy on retries after WAF blocks ("proxies only in chat, only
	// when blocked"). Auxiliary paths never set it.
	routeProxy *url.URL
	// files carries already-uploaded Z.AI file entries (vision.go) — sent
	// as the top-level "files" array on the completion request. nil for
	// text-only requests.
	files []map[string]interface{}
}

// sendToZAI sends a chat request to Z.AI, calling onContent for each
// content delta and onReasoning (may be nil) for each reasoning delta.
// Returns the AUTHORITATIVE final content/reasoning from the parser —
// buffered consumers must use these, NOT concatenated deltas: Z.AI's
// edit_content rewrites already-emitted text in place, so delta
// concatenation produces garbled mixes of pre- and post-edit fragments.
func (s *Server) sendToZAI(ctx context.Context, prompt string, opts sendOpts, onContent, onReasoning func(string)) (string, string, error) {
	if !s.zaiSession.isInitialized() {
		if err := s.zaiSession.initialize(s.cfg.ZAIToken); err != nil {
			return "", "", err
		}
	}

	// Resolve features dynamically from per-model state
	// (server capabilities + stored user overrides)
	featuresMap := s.resolveFeaturesForModel(opts.model)

	// Apply per-request overrides (highest precedence)
	if opts.webSearch != nil {
		if *opts.webSearch {
			featuresMap["auto_web_search"] = true
			featuresMap["web_search"] = true
		} else {
			delete(featuresMap, "auto_web_search")
			delete(featuresMap, "web_search")
		}
	}
	if opts.thinking != nil {
		featuresMap["enable_thinking"] = *opts.thinking
	}

	// ── reasoning_effort handling ──
	s.applyReasoningEffort(featuresMap, opts.model, opts.reasoningEffort)

	// Remove 'think' entirely; ALWAYS force image_generation to false
	delete(featuresMap, "think")
	featuresMap["image_generation"] = false

	opts.featuresMap = featuresMap

	// Route policy: start direct; after a WAF block, retries ride the proxy
	// pool (rotating per block) while the budget lasts.
	for attempt := 0; attempt < zaiSendAttempts(); attempt++ {
		if attempt > 0 && proxies.enabled() {
			opts.routeProxy = proxies.currentURL()
		} else {
			opts.routeProxy = nil
		}
		content, reasoning, err := s.doZAIRequest(ctx, prompt, opts, onContent, onReasoning)
		switch {
		case err == errZAIUnauthorized && attempt+1 < zaiSendAttempts():
			// Dead account or expired session: rotate to the next Z.AI
			// account (pool) and re-initialize before the next attempt.
			next := rotateZaiAccount(s.cfg.ZAIToken)
			log.Printf("[ZAI] unauthorized/blocked — rotating account, next candidate %s...", shortToken(next))
			s.zaiSession.markUninitialized()
			if initErr := s.zaiSession.initialize(next); initErr != nil {
				return "", "", initErr
			}
		case errors.Is(err, errZAIWAF) && attempt+1 < zaiSendAttempts() && proxies.enabled():
			// WAF blocks are per-IP — rotate through every proxy once
			log.Printf("[ZAI] WAF block — rotating proxy → %s", proxies.rotate())
		case err != nil && strings.Contains(err.Error(), "connection error") && attempt+1 < zaiSendAttempts() && proxies.enabled():
			// Dead/unreachable route (dial/write failures, proxy refused) —
			// mark it dead, rotate to the next proxy instead of failing the
			// request on the first dead node of the pool.
			proxies.markDead(opts.routeProxy)
			log.Printf("[ZAI] connection error on route — rotating proxy → %s", proxies.rotate())
		case errors.Is(err, errZAIStreamCut) && attempt+1 < zaiSendAttempts():
			// The route dropped the stream mid-flight (proxies often cut long
			// thinking phases). Retry on a fresh route; rotate the proxy when
			// a pool is configured, otherwise try direct.
			if proxies.enabled() {
				log.Printf("[ZAI] stream cut before done — rotating proxy → %s", proxies.rotate())
			} else {
				log.Printf("[ZAI] stream cut before done — retrying")
			}
		default:
			return content, reasoning, err
		}
	}
	return "", "", fmt.Errorf("Z.AI request failed after retry")
}

var errZAIUnauthorized = fmt.Errorf("zai unauthorized")
var errZAIWAF = fmt.Errorf("zai waf block")

// errZAIStreamCut marks a response stream that ended (connection closed by
// the route — typically a pool proxy dropping a long thinking phase) before
// Z.AI's explicit done-phase. Nothing usable was produced; sendToZAI retries
// on another route.
var errZAIStreamCut = fmt.Errorf("zai stream cut before done")

// zaiSendAttempts reports how many times one logical send may be retried:
// base 2 covers the direct route plus one retry after a 401 session re-init.
// WAF blocks are per-IP, so every configured proxy extends the budget by one
// — with WAF errors the loop "just rotates" through the whole list once
// before giving up.
func zaiSendAttempts() int {
	attempts := 2
	if n := len(proxies.urls); n > 0 {
		attempts += n
	}
	return attempts
}

// applyReasoningEffort applies the per-request reasoning_effort parameter to
// the feature map. Defensive semantics: stale values are always stripped
// first so unsupported models never receive a placeholder value (would cause
// malfunction); the parameter is re-added only when the model's capabilities
// declare "reasoning_effort": true AND the value is a valid level. When
// applied, enable_thinking is forced on.
func (s *Server) applyReasoningEffort(featuresMap map[string]interface{}, modelID, effort string) {
	delete(featuresMap, "reasoning_effort")
	if effort == "" {
		return
	}
	if !s.modelSupportsReasoningEffort(modelID) {
		log.Printf("[reasoning_effort] model=%s does not support reasoning_effort; parameter ignored", modelID)
		return
	}
	if !isValidReasoningEffort(effort) {
		log.Printf("[reasoning_effort] invalid value '%s' for model=%s (accepted: low, high, max); ignored",
			effort, modelID)
		return
	}
	featuresMap["reasoning_effort"] = effort
	featuresMap["enable_thinking"] = true
	log.Printf("[reasoning_effort] model=%s effort=%s enabled (enable_thinking forced true)",
		modelID, effort)
}

func (s *Server) doZAIRequest(ctx context.Context, prompt string, opts sendOpts, onContent, onReasoning func(string)) (string, string, error) {
	zs := s.zaiSession
	zs.mu.Lock()
	token := zs.token
	userID := zs.userID
	feVersion := zs.feVersion
	zs.mu.Unlock()

	signature := zs.generateSignature(prompt, token, userID)

	headers := map[string]string{
		"authorization": "Bearer " + token,
		"User-Agent":    zaiUserAgent,
		"content-type":  "application/json",
		"x-fe-Version":  feVersion,
		"x-region":      "overseas",
		"x-signature":   signature,
	}

	// Forward structured messages, NOT flattened prompt
	var forwardedMessages []json.RawMessage
	if len(opts.clientMessages) > 0 {
		forwardedMessages = opts.clientMessages
	} else {
		// Fallback: session messages + new user message
		for _, m := range opts.messages {
			raw, _ := json.Marshal(m)
			forwardedMessages = append(forwardedMessages, raw)
		}
		userMsg, _ := json.Marshal(map[string]string{"role": "user", "content": prompt})
		forwardedMessages = append(forwardedMessages, userMsg)
	}

	// Proxied routes get a cheap connectivity pre-check BEFORE the captcha:
	// every attempt burns a single-use device token, and walking a stale
	// pool must not cost hundreds of tokens (GET /api/models needs no
	// captcha). A failed pre-check marks the proxy dead and rotates.
	if opts.routeProxy != nil {
		if err := proxiedRouteAlive(s, opts.routeProxy); err != nil {
			proxies.markDead(opts.routeProxy)
			return "", "", fmt.Errorf("Z.AI connection error: route pre-check failed: %v", err)
		}
	}

	// Get captcha param — now a direct in-process call, no named pipe
	captchaParam := computeCaptchaParam(s, s.tokens)
	if captchaParam == "" {
		return "", "", fmt.Errorf("captcha verification failed — check tokens.sqlite")
	}

	// Features payload from dynamically resolved per-model features
	featuresPayload := make(map[string]interface{}, len(opts.featuresMap)+2)
	for k, v := range opts.featuresMap {
		featuresPayload[k] = v
	}
	delete(featuresPayload, "think")
	featuresPayload["flags"] = []interface{}{}
	featuresPayload["image_generation"] = false

	reqBody := zaiRequestBody{
		Model:              opts.model,
		ChatID:             opts.chatID,
		Messages:           forwardedMessages,
		SignaturePrompt:    prompt,
		Stream:             true,
		CaptchaVerifyParam: captchaParam,
		Features:           featuresPayload,
		Files:              opts.files,
	}

	bodyBytes, err := jsonMarshal(reqBody)
	if err != nil {
		return "", "", err
	}

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI request body: %s", string(bodyBytes))
	}

	// No per-attempt timeout here: thinking-heavy streams legitimately run
	// past 90s WITH data flowing (a hard 90s cap killed live streams at
	// exactly 90.00s — misclassified as proxy cuts). Bounds instead: the
	// outer request context (cfg.Timeout, default 300s) and the 120s idle
	// watchdog above (fires only when NO data arrives).
	reqCtx := ctx
	client, err := zaiHTTP(opts.routeProxy)
	if err != nil {
		return "", "", fmt.Errorf("http client: %w", err)
	}
	if opts.routeProxy != nil {
		log.Printf("[ZAI] chat via proxy %s", opts.routeProxy.Redacted())
	}
	req, _ := fhttp.NewRequestWithContext(reqCtx, "POST", baseURL+"/api/v2/chat/completions", bytes.NewReader(bodyBytes))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Z.AI connection error: %w", err)
	}
	defer resp.Body.Close()

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI response status: %d %s", resp.StatusCode, resp.Status)
	}

	if resp.StatusCode == 401 {
		return "", "", errZAIUnauthorized
	}

	if resp.StatusCode != 200 {
		errText, _ := io.ReadAll(resp.Body)
		errStr := string(errText)
		// HTML WAF pages → clean sentinel instead of dumping raw HTML
		if strings.Contains(errStr, "<html") || strings.Contains(errStr, "<!doctype") {
			return "", "", fmt.Errorf("%w: Z.AI error %d, blocked by Aliyun WAF (rate limit/security)", errZAIWAF, resp.StatusCode)
		}
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
		if s.cfg.LogLevel == "debug" {
			log.Printf("[DEBUG] Z.AI error body: %s", errStr)
		}
		// Account-level rejections behave like unauthorized so the retry
		// loop rotates to the next Z.AI account instead of hammering a dead one
		if strings.Contains(errStr, "USER_BLOCKED") || strings.Contains(errStr, "\"detail\"") && resp.StatusCode == 403 {
			return "", "", fmt.Errorf("%w: account rejected by platform (%d)", errZAIUnauthorized, resp.StatusCode)
		}
		return "", "", fmt.Errorf("Z.AI error %d: %s", resp.StatusCode, errStr)
	}

	// Parse SSE stream through the stateful parser (edit_index, details)
	reader := bufio.NewReaderSize(resp.Body, 256*1024)
	parser := newSSEParser(s.cfg.LogLevel)
	var buffer string

	// Track whether ANYTHING was forwarded to the client: a stream that dies
	// before the done-phase is retryable only while nothing was emitted
	// (retrying after partial deltas would duplicate output).
	emitted := false
	origContent, origReasoning := onContent, onReasoning
	clientContent, clientReasoning := origContent, origReasoning
	if origContent != nil {
		clientContent = func(chunk string) { emitted = true; origContent(chunk) }
	}
	if origReasoning != nil {
		clientReasoning = func(chunk string) { emitted = true; origReasoning(chunk) }
	}
	// Proxied routes NEVER stream live: pool proxies cut long streams
	// mid-flight, and a cut after live emission cannot be retried (duplicate
	// output). On a proxied attempt the parser buffers internally and the
	// final content/reasoning is flushed to the client in one piece after
	// the done-phase — every proxied cut stays retryable. Direct routes keep
	// true streaming.
	if opts.routeProxy != nil {
		onContent, onReasoning = nil, nil
	}
	flushClient := func() {
		if c := parser.content(); c != "" && clientContent != nil {
			clientContent(c)
		}
		if r := parser.reasoning(); r != "" && clientReasoning != nil {
			clientReasoning(r)
		}
	}
	var sawDone bool

	// Idle watchdog: Z.AI occasionally stalls mid-stream (no bytes, no
	// done-phase) and the SSE would hang until cfg.Timeout. Close the body
	// after 120s of silence — the blocked ReadString unblocks and we return
	// an error instead of keeping the client on hold.
	var watchdogFired atomic.Bool
	streamIdle := time.AfterFunc(120*time.Second, func() {
		watchdogFired.Store(true)
		log.Printf("[ZAI] stream idle >120s — aborting")
		resp.Body.Close()
	})
	defer streamIdle.Stop()

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI response headers: %v", resp.Header)
	}

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			streamIdle.Reset(120 * time.Second)
			buffer += line
			lines := strings.Split(buffer, "\n")
			buffer = lines[len(lines)-1] // keep incomplete last line

			for _, l := range lines[:len(lines)-1] {
				trimmed := strings.TrimSpace(l)
				if trimmed != "" && s.cfg.LogLevel == "debug" {
					log.Printf("[DEBUG] Z.AI SSE line: %s", trimmed)
				}
				if ferr := parser.feedLine(l, onContent, onReasoning); ferr != nil {
					if ferr == io.EOF {
						// [DONE] / phase done — normal end; release any
						// holdback-tail content before returning finals
						sawDone = true
						parser.FlushFinal(onContent, onReasoning)
						if opts.routeProxy != nil {
							flushClient()
						}
						return parser.content(), parser.reasoning(), nil
					}
					return "", "", ferr // actual error from Z.AI
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				// Process any remaining buffer
				if strings.TrimSpace(buffer) != "" {
					if s.cfg.LogLevel == "debug" {
						log.Printf("[DEBUG] Z.AI SSE line (final): %s", strings.TrimSpace(buffer))
					}
					parser.feedLine(buffer, onContent, onReasoning)
				}
			} else if watchdogFired.Load() {
				return "", "", fmt.Errorf("Z.AI stream stalled >120s — aborted by idle watchdog")
			}
			break
		}
	}

	// Stream ended without an explicit done-phase: the route dropped the
	// connection mid-flight (proxies cut long thinking phases; Z.AI itself
	// always sends a done-phase). Release held-back tail, then either retry
	// (nothing emitted yet) or surface the truncation to the client.
	parser.FlushFinal(onContent, onReasoning)
	if !sawDone {
		if !emitted {
			log.Printf("[ZAI] stream ended without done-phase (%d bytes content buffered) — treated as cut", len(parser.content()))
			return "", "", errZAIStreamCut
		}
		return "", "", fmt.Errorf("Z.AI stream cut before completion (partial output already delivered)")
	}
	if opts.routeProxy != nil {
		flushClient()
	}
	return parser.content(), parser.reasoning(), nil
}

// ── Accessors ──

func (s *ZaiSession) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *ZaiSession) markUninitialized() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized = false
}

func (s *ZaiSession) snapshot() (connected, userName, userID, feVersion string, init bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid := s.userID
	if len(uid) > 8 {
		uid = uid[:8] + "..."
	}
	return s.token, s.userName, uid, s.feVersion, s.initialized
}

func (s *ZaiSession) getFEVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feVersion
}

func (s *ZaiSession) getUserName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userName
}

func (s *ZaiSession) isConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

// logPrintf is a shared log helper.
func logPrintf(format string, args ...interface{}) {
	log.Printf(format, args...)
}
