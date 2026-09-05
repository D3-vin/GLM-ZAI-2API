// zai.go — Z.AI session, HMAC-SHA256 signature, and SSE streaming.
// Ports main.js session logic + sendToZAI async generator into Go.
package main

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
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	baseURL          = "https://chat.z.ai"
	saltKey          = "key-@@@@)))()((9))-xxxx&&&%%%%%"
	defaultFEVersion = "prod-fe-1.0.185"
	sessionTTL       = 30 * time.Minute
)

// ── Global Z.AI session state ──

type Features struct {
	WebSearch      bool `json:"webSearch"`
	AutoWebSearch  bool `json:"autoWebSearch"`
	Thinking       bool `json:"thinking"`
	ImageGen       bool `json:"imageGen"`
	PreviewMode    bool `json:"previewMode"`
	PersistHistory bool `json:"persistHistory"`
}

type ZaiSession struct {
	mu           sync.Mutex
	token        string
	userID       string
	userName     string
	chatID       string
	feVersion    string
	features     Features
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
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL, nil)
	resp, err := httpClient.Do(req)
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

	// Fast path: use hardcoded ZAI_TOKEN
	if zaiToken != "" {
		log.Println("[Session] Using hardcoded ZAI_TOKEN, skipping guest init.")
		s.mu.Lock()
		s.token = zaiToken
		s.mu.Unlock()
		if payload, err := jwtPayload(zaiToken); err == nil {
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
			log.Printf("[Session] Token user: %s... (%s)", uid, s.userName)
		} else {
			log.Println("[Session] Token decode failed, continuing with raw token.")
		}
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return nil
	}

	log.Println("[Session] Initializing Z.AI session...")

	s.scrapeConfig()

	headers := map[string]string{
		"Origin":       baseURL,
		"Referer":      baseURL + "/",
		"Content-Type": "application/json",
	}

	// POST /api/v1/auths/guest (warm up)
	postReq(ctx(15*time.Second), baseURL+"/api/v1/auths/guest", "{}", headers)

	// GET /api/v1/auths/
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	req, _ := http.NewRequestWithContext(ctx2, "GET", baseURL+"/api/v1/auths/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
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
		guestBody := postReq(ctx(15*time.Second), baseURL+"/api/v1/auths/guest", "{}", headers)
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

// helper: context with timeout
func ctx(d time.Duration) context.Context {
	c, _ := context.WithTimeout(context.Background(), d)
	return c
}

// postReq is a simple POST helper returning the response body as string.
func postReq(ctx context.Context, url, body string, headers map[string]string) string {
	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// ── Z.AI request body ──

type zaiFeatures struct {
	ImageGeneration bool     `json:"image_generation"`
	WebSearch       bool     `json:"web_search"`
	AutoWebSearch   bool     `json:"auto_web_search"`
	PreviewMode     bool     `json:"preview_mode"`
	Flags           []string `json:"flags"`
	EnableThinking  bool     `json:"enable_thinking"`
}

type zaiRequestBody struct {
	Model              string            `json:"model"`
	ChatID             string            `json:"chat_id"`
	Messages           []json.RawMessage `json:"messages"`
	SignaturePrompt    string            `json:"signature_prompt"`
	Stream             bool              `json:"stream"`
	CaptchaVerifyParam string            `json:"captcha_verify_param"`
	Features           zaiFeatures       `json:"features"`
}

// ── sendToZAI — streaming chat completions ──

type sendOpts struct {
	model          string
	webSearch      bool
	thinking       bool
	imageGen       bool
	previewMode    bool
	chatID         string
	messages       []map[string]interface{}
	clientMessages []json.RawMessage
}

// sendToZAI sends a chat request to Z.AI and calls onChunk for each
// content delta received from the SSE stream.
func (s *Server) sendToZAI(ctx context.Context, prompt string, opts sendOpts, onChunk func(string)) error {
	if !s.zaiSession.isInitialized() {
		if err := s.zaiSession.initialize(s.cfg.ZAIToken); err != nil {
			return err
		}
	}

	for retry := 0; retry < 2; retry++ {
		err := s.doZAIRequest(ctx, prompt, opts, onChunk)
		if err == errZAIUnauthorized && retry == 0 {
			log.Println("[ZAI] 401, re-initializing session...")
			s.zaiSession.markUninitialized()
			if initErr := s.zaiSession.initialize(s.cfg.ZAIToken); initErr != nil {
				return initErr
			}
			continue
		}
		return err
	}
	return fmt.Errorf("Z.AI request failed after retry")
}

var errZAIUnauthorized = fmt.Errorf("zai unauthorized")

func (s *Server) doZAIRequest(ctx context.Context, prompt string, opts sendOpts, onChunk func(string)) error {
	zs := s.zaiSession
	zs.mu.Lock()
	token := zs.token
	userID := zs.userID
	feVersion := zs.feVersion
	zs.mu.Unlock()

	signature := zs.generateSignature(prompt, token, userID)

	headers := map[string]string{
		"authorization": "Bearer " + token,
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

	// Get captcha param — now a direct in-process call, no named pipe
	captchaParam := computeCaptchaParam(s.tokens)
	if captchaParam == "" {
		return fmt.Errorf("captcha verification failed — check tokens.sqlite")
	}

	reqBody := zaiRequestBody{
		Model:              opts.model,
		ChatID:             opts.chatID,
		Messages:           forwardedMessages,
		SignaturePrompt:    prompt,
		Stream:             true,
		CaptchaVerifyParam: captchaParam,
		Features: zaiFeatures{
			ImageGeneration: opts.imageGen,
			WebSearch:       opts.webSearch,
			AutoWebSearch:   opts.webSearch,
			PreviewMode:     opts.previewMode,
			Flags:           []string{},
			EnableThinking:  opts.thinking,
		},
	}

	bodyBytes, err := jsonMarshal(reqBody)
	if err != nil {
		return err
	}

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI request body: %s", string(bodyBytes))
	}

	reqCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, "POST", baseURL+"/api/v2/chat/completions", bytes.NewReader(bodyBytes))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Z.AI connection error: %w", err)
	}
	defer resp.Body.Close()

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI response status: %d %s", resp.StatusCode, resp.Status)
	}

	if resp.StatusCode == 401 {
		return errZAIUnauthorized
	}

	if resp.StatusCode != 200 {
		errText, _ := io.ReadAll(resp.Body)
		errStr := string(errText)
		// HTML WAF pages → clean message instead of dumping raw HTML
		if strings.Contains(errStr, "<html") || strings.Contains(errStr, "<!doctype") {
			return fmt.Errorf("Z.AI error %d: blocked by Aliyun WAF (rate limit/security)", resp.StatusCode)
		}
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
		if s.cfg.LogLevel == "debug" {
			log.Printf("[DEBUG] Z.AI error body: %s", errStr)
		}
		return fmt.Errorf("Z.AI error %d: %s", resp.StatusCode, errStr)
	}

	// Parse SSE stream
	reader := bufio.NewReaderSize(resp.Body, 256*1024)
	var buffer string

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Z.AI response headers: %v", resp.Header)
	}

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			buffer += line
			lines := strings.Split(buffer, "\n")
			buffer = lines[len(lines)-1] // keep incomplete last line

			for _, l := range lines[:len(lines)-1] {
				trimmed := strings.TrimSpace(l)
				if trimmed != "" && s.cfg.LogLevel == "debug" {
					log.Printf("[DEBUG] Z.AI SSE line: %s", trimmed)
				}
				if err := processSSELine(l, onChunk, s.cfg.LogLevel); err != nil {
					if err == io.EOF {
						return nil // [DONE] — normal end
					}
					return err // actual error from Z.AI
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
					processSSELine(buffer, onChunk, s.cfg.LogLevel)
				}
			}
			break
		}
	}

	return nil
}

// processSSELine parses a single SSE data line and calls onChunk.
// Returns io.EOF to signal [DONE], or a non-nil error if Z.AI reports an error.
func processSSELine(line string, onChunk func(string), logLevel string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "data: ") {
		return nil
	}
	dataStr := trimmed[6:]
	if dataStr == "[DONE]" {
		return io.EOF
	}

	var json_ map[string]interface{}
	if err := json.Unmarshal([]byte(dataStr), &json_); err != nil {
		if logLevel == "debug" {
			log.Printf("[DEBUG] Z.AI failed to parse SSE: %s", dataStr)
		}
		return nil
	}

	// Check for error in Z.AI response (e.g. "Model not available for current user level")
	if data, ok := json_["data"].(map[string]interface{}); ok {
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			if detail, _ := errObj["detail"].(string); detail != "" {
				return fmt.Errorf("Z.AI: %s", detail)
			}
		}
	}
	if errObj, ok := json_["error"].(map[string]interface{}); ok {
		if detail, _ := errObj["detail"].(string); detail != "" {
			return fmt.Errorf("Z.AI: %s", detail)
		}
	}

	// Extract content delta
	var chunk string
	if data, ok := json_["data"].(map[string]interface{}); ok {
		if delta, ok := data["delta_content"].(string); ok {
			chunk = delta
		}
		// Z.AI sends JSON/code output as edit_content instead of delta_content
		if chunk == "" {
			if edit, ok := data["edit_content"].(string); ok {
				chunk = edit
			}
		}
	}
	if chunk == "" {
		if choices, ok := json_["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok {
						chunk = content
					}
				}
			}
		}
	}

	if chunk != "" {
		onChunk(chunk)
	}
	return nil
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

func (s *ZaiSession) snapshot() (connected, userName, userID, feVersion string, features Features, init bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid := s.userID
	if len(uid) > 8 {
		uid = uid[:8] + "..."
	}
	return s.token, s.userName, uid, s.feVersion, s.features, s.initialized
}

func (s *ZaiSession) updateFeatures(f Features) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.features = f
}

func (s *ZaiSession) getFeatures() Features {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.features
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
