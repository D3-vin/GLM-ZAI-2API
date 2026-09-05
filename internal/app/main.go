// main.go — HTTP server, config, OpenAI-compatible routes, dashboard.
// Replaces main.js (Express) entirely. Captcha logic is in-process (no pipe IPC).
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ── Config ──

type Config struct {
	Host           string
	Port           string
	AuthEnabled    bool
	AuthToken      string
	ZAIToken       string
	ZAITokensFile  string
	UseAccountPool bool
	LogLevel       string
	LogFormat      string
	Timeout        time.Duration
	DBPath         string
	CaptchaTimeout time.Duration
	UseProxy       bool
	ProxyFile      string
}

func loadConfig() *Config {
	cfg := &Config{
		Host:           envOr("HOST", "0.0.0.0"),
		Port:           envOr("PORT", "5082"),
		AuthEnabled:    true,
		AuthToken:      envOr("AUTH_TOKEN", "d3vin"),
		ZAIToken:       envOr("ZAI_TOKEN", ""),
		ZAITokensFile:  envOr("ZAI_TOKENS_FILE", "zai_tokens.txt"),
		UseAccountPool: envOr("USE_ACCOUNT_POOL", "") == "true",
		LogLevel:       envOr("LOG_LEVEL", "info"),
		LogFormat:      envOr("LOG_FORMAT", "text"),
		Timeout:        time.Duration(envIntOr("TIMEOUT", 300000)) * time.Millisecond,
		DBPath:         envOr("DB_PATH", "tokens.sqlite"),
		CaptchaTimeout: 90 * time.Second,
		UseProxy:       envOr("USE_PROXY", "") == "true",
		ProxyFile:      envOr("PROXY_FILE", "proxy.txt"),
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

// ── Shared HTTP client ──
// All outgoing traffic goes through browser-fingerprinted tls-client
// instances (see tlsclient.go): zaiHTTP(nil) = direct for every auxiliary
// path (model catalog, session init, config scrape); the chat path calls
// zaiHTTP(proxy) and only after WAF blocks, rotating through the pool.
// Aliyun captcha always uses browserClient("", 30), direct.

// ── Server ──

type Server struct {
	cfg           *Config
	zaiSession    *ZaiSession
	conversations *ConversationStore
	tokens        *TokenStore
}

// ── OpenAI response types ──

type openAIDelta struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Delta        *openAIDelta   `json:"delta,omitempty"`
	Message      *openAIMessage `json:"message,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

func newChunk(content, model, requestID string, finish *string) openAIResponse {
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Delta:        &openAIDelta{Content: content},
			FinishReason: finish,
		}},
	}
}

func newReasoningChunk(reasoning, model, requestID string) openAIResponse {
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Delta:        &openAIDelta{ReasoningContent: reasoning},
			FinishReason: nil,
		}},
	}
}

func newCompletion(content, reasoning, prompt, model, requestID string) openAIResponse {
	pt := estimateTokens(prompt)
	ct := estimateTokens(content)
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      &openAIMessage{Role: "assistant", Content: content, ReasoningContent: reasoning},
			FinishReason: strPtr("stop"),
		}},
		Usage: &openAIUsage{
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      pt + ct,
		},
	}
}

func formatOpenAIError(msg, errType string) map[string]interface{} {
	if errType == "" {
		errType = "api_error"
	}
	return map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    errType,
			"code":    nil,
			"param":   nil,
		},
	}
}

func strPtr(s string) *string { return &s }

// runeChunkEnd returns the largest chunk end ≤ max that does not split a
// UTF-8 rune (backs off up to 3 bytes to a rune start). Byte-sliced chunks
// would cut multi-byte characters (Cyrillic, emoji) in half.
func runeChunkEnd(s string, i, max int) int {
	end := max
	if end > len(s) {
		end = len(s)
	}
	for end < len(s) && end > i+1 && !utf8.RuneStart(s[end]) {
		end--
	}
	return end
}

// statusFromError maps a Z.AI/bridge error string to an HTTP status code.
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "429"):
		return 429
	case strings.Contains(errMsg, "400"):
		return 400
	default:
		return 500
	}
}

// ── Chat request ──

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []json.RawMessage `json:"messages"`
	Stream          *bool             `json:"stream"`
	DeepThink       *bool             `json:"deepThink"`
	Search          *bool             `json:"search"`
	WebSearch       *bool             `json:"webSearch"`
	Reasoning       *bool             `json:"reasoning"`
	Thinking        json.RawMessage   `json:"thinking"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
}

func (r *chatRequest) streamEnabled() bool {
	if r.Stream == nil {
		return true // default stream=true like the original
	}
	return *r.Stream
}

// ── Middleware ──

func (s *Server) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AuthEnabled {
			h(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		provided = strings.TrimSpace(provided)
		if provided != s.cfg.AuthToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "authentication_error",
					"message": "Invalid or missing authentication token",
				},
			})
			return
		}
		h(w, r)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id, X-Fresh-Session")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Routes ──

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/status", s.handleStatus)

	mux.HandleFunc("/v1/models", s.withAuth(s.handleV1Models))
	mux.HandleFunc("/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))

	mux.HandleFunc("/prompt", s.withAuth(s.handlePrompt))
	mux.HandleFunc("/features", s.withAuth(s.handleFeatures))
	mux.HandleFunc("/admin/stats", s.handleAdminStats)
	mux.HandleFunc("/admin/models", s.withAuth(s.handleAdminModels))
	mux.HandleFunc("/admin/health", s.handleAdminHealth)
	mux.HandleFunc("/admin/clients", s.handleAdminClients)
	mux.HandleFunc("/admin/session/clear", s.withAuth(s.handleSessionClear))
	mux.HandleFunc("/admin/clients/", s.withAuth(s.handleClientClear))

	mux.HandleFunc("/inject.js", s.handleInject)
	mux.HandleFunc("/stop", s.withAuth(s.handleStop))

	return corsMiddleware(mux)
}

// ── Handlers ──

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	host := r.Host
	if host == "" {
		host = "localhost:" + s.cfg.Port
	}
	html := dashboardHTML
	html = strings.ReplaceAll(html, "__HOST__", host)
	html = strings.ReplaceAll(html, "__AUTH_TOKEN__", s.cfg.AuthToken)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	_, userName, userID, feVersion, init := s.zaiSession.snapshot()
	if !init {
		userID = ""
	}
	// Dashboard-friendly view: resolved features for the default model
	features := s.resolveFeaturesForModel("glm-4.7")
	features["persistHistory"] = persistHistoryEnabled()
	zaiAccountsMu.Lock()
	pool := zaiAccounts
	zaiAccountsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":      init,
		"userName":       userName,
		"userId":         userID,
		"feVersion":      feVersion,
		"activeSessions": s.conversations.count(),
		"features":       features,
		"mode":           "direct",
		"port":           s.cfg.Port,
		"tokenCount":     s.tokens.Count(),
		// Z.AI account pool visibility for the dashboard
		"accountPoolSize":  pool.size(),
		"accountPoolIndex": pool.index(),
	})
}

func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := s.fetchModelsFromZAI()
	if claudeDesktopEnabled() {
		data := make([]map[string]interface{}, len(models))
		for i, m := range models {
			name := getClaudeName(m.ID)
			data[i] = map[string]interface{}{
				"id":           name,
				"object":       "model",
				"created":      now,
				"owned_by":     "anthropic",
				"display_name": name,
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   data,
		})
		return
	}
	data := make([]map[string]interface{}, len(models))
	for i, m := range models {
		data[i] = map[string]interface{}{
			"id":           m.ID,
			"object":       "model",
			"created":      now,
			"owned_by":     "z-ai",
			"display_name": m.Name,
			"description":  m.Description,
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.fetchModelsFromZAI()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	currentModel := "glm-4.7"
	if len(ids) > 0 {
		currentModel = ids[0]
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models":       ids,
		"currentModel": currentModel,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Limit body size (50mb like the original)
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError("invalid JSON body: "+err.Error(), "invalid_request_error"))
		return
	}

	if len(req.Messages) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError("messages is required and must be an array", "invalid_request_error"))
		return
	}

	model := req.Model
	if model == "" {
		model = "glm-4.7"
	}

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "default"
	}
	fresh := r.Header.Get("X-Fresh-Session") == "true"
	conv := s.conversations.getOrCreate(sessionID, fresh)

	requestID := generateID()

	// ── Vision: extract image_url parts, upload them to Z.AI, rewrite the
	// parts to file-id references. req.Messages is byte-identical when the
	// request carries no images (the common case).
	var visionFiles []map[string]interface{}
	if rawMsgs, merr := json.Marshal(req.Messages); merr == nil {
		cleaned, files, vErr := s.processVisionMessages(r.Context(), rawMsgs)
		if vErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(formatOpenAIError(vErr.Error(), "invalid_request_error"))
			return
		}
		if files != nil {
			if err := json.Unmarshal(cleaned, &req.Messages); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(formatOpenAIError("image processing: malformed messages", "invalid_request_error"))
				return
			}
			visionFiles = files
		}
	}
	prompt := messagesToPrompt(req.Messages)

	opts := sendOpts{
		model:           model,
		webSearch:       firstNonNilBool(req.WebSearch, req.Search),
		thinking:        s.thinkingOverride(req),
		reasoningEffort: req.ReasoningEffort,
		chatID:          conv.chatID,
		messages:        conv.messages,
		clientMessages:  req.Messages,
		files:           visionFiles,
	}

	// Tool calling: streaming path intercepts <<<TOOL_CALL>>> live,
	// non-streaming buffers and parses after the fact.
	if len(req.Tools) > 0 {
		if req.streamEnabled() {
			s.handleChatToolsStream(w, r, req, prompt, model, requestID, conv, opts)
		} else {
			s.handleChatWithTools(w, r, req, prompt, model, requestID, conv, opts)
		}
		return
	}

	if req.streamEnabled() {
		s.handleChatStream(w, r, prompt, model, requestID, opts, conv)
	} else {
		s.handleChatNonStream(w, r, prompt, model, requestID, opts, conv)
	}
}

// firstNonNilBool returns the first non-nil *bool, or nil.
func firstNonNilBool(vals ...*bool) *bool {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// thinkingOverride merges deepThink / reasoning / thinking:{"type":...}
// into a single enable_thinking override (nil = per-model default).
func (s *Server) thinkingOverride(req chatRequest) *bool {
	if req.DeepThink != nil {
		return req.DeepThink
	}
	if req.Reasoning != nil {
		return req.Reasoning
	}
	if len(req.Thinking) > 0 {
		var thinkCfg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(req.Thinking, &thinkCfg); err == nil {
			enabled := thinkCfg.Type == "enabled"
			return &enabled
		}
	}
	return nil
}

// handleChatWithTools processes non-streaming requests with tools.
// Buffers full response, parses for tool markers, returns either
// tool_calls or normal text in OpenAI format.
func (s *Server) handleChatWithTools(w http.ResponseWriter, r *http.Request, req chatRequest, prompt, model, requestID string, conv *Conversation, opts sendOpts) {
	// Convert messages: prepend tools system prompt, convert tool/assistant roles
	convertedMsgs := convertToolMessages(req.Messages, req.Tools)
	// Recompute prompt from converted messages for signature
	convertedPrompt := messagesToPrompt(convertedMsgs)
	opts.clientMessages = convertedMsgs

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	// Buffered path: take the authoritative parser content, not deltas
	// (edit_content rewrites would garble delta concatenation).
	content, _, err := s.sendToZAI(ctx, convertedPrompt, opts, nil, nil)

	if err != nil {
		log.Printf("[Tools] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	// Parse for tool calls
	toolCalls := parseToolCalls(content, req.Tools)
	if len(toolCalls) > 0 {
		cleanContent := stripToolContent(content)

		// Build tool call entries
		type tcFunc struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		type tcEntry struct {
			Index_   int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function tcFunc `json:"function"`
		}

		entries := make([]tcEntry, len(toolCalls))
		for i, c := range toolCalls {
			entries[i] = tcEntry{Index_: i, ID: c.ID, Type: c.Type}
			entries[i].Function.Name = c.Function.Name
			entries[i].Function.Arguments = c.Function.Arguments
		}

		if req.streamEnabled() {
			// SSE streaming format for tool_calls (what opencode/OpenAI clients expect)
			flusher, ok := w.(http.Flusher)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				writeToolCallsJSON(w, requestID, model, cleanContent, entries)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)

			// Chunk 1: role + tool_calls
			chunk1 := map[string]interface{}{
				"id":      "chatcmpl-" + requestID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]interface{}{
						"role":       "assistant",
						"content":    nil,
						"tool_calls": entries,
					},
					"finish_reason": nil,
				}},
			}
			data1, _ := json.Marshal(chunk1)
			fmt.Fprintf(w, "data: %s\n\n", data1)
			flusher.Flush()

			// Chunk 2: finish_reason
			finish := "tool_calls"
			chunk2 := map[string]interface{}{
				"id":      "chatcmpl-" + requestID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": finish,
				}},
			}
			data2, _ := json.Marshal(chunk2)
			fmt.Fprintf(w, "data: %s\n\n", data2)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		} else {
			// Non-streaming JSON
			w.Header().Set("Content-Type", "application/json")
			writeToolCallsJSON(w, requestID, model, cleanContent, entries)
		}
		log.Printf("[Tools] Returned %d tool calls", len(toolCalls))
		return
	}

	// No tool calls found — return content as-is (text or raw JSON).
	// Previously cleanFallbackContent replaced JSON with a generic message,
	// but that hid legitimate text responses from the user.
	// Don't leak raw contract markers though: strip complete blocks.
	if strings.Contains(content, agentToolCallStart) {
		preview := content
		if len(preview) > 1500 {
			preview = preview[:1500] + "...(truncated)"
		}
		log.Printf("[Tools] markers present but no valid tool call parsed — model output:\n%s", preview)
		content = stripAgentToolCallBlocks(content)
	}
	if req.streamEnabled() {
		// Client expects SSE — send text as stream chunks
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(newCompletion(content, "", prompt, model, requestID))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)

		writeSSE := func(v interface{}) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Split content into chunks to avoid oversized SSE events.
		// Rune-safe: a byte boundary would cut a multi-byte character
		// (Cyrillic, emoji) in half — json.Marshal renders the halves
		// as U+FFFD (��) on the client.
		const chunkSize = 2000
		for i := 0; i < len(content); {
			end := runeChunkEnd(content, i, i+chunkSize)
			writeSSE(newChunk(content[i:end], model, requestID, nil))
			i = end
		}

		// Final chunk with finish_reason
		writeSSE(newChunk("", model, requestID, strPtr("stop")))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// Non-streaming JSON
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newCompletion(content, "", prompt, model, requestID))
	}
	preview := content
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	log.Printf("[Tools] No tool calls in response, returning text (%d chars): %s", len(content), preview)
}

// writeToolCallsJSON writes a non-streaming JSON response with tool_calls.
func writeToolCallsJSON(w http.ResponseWriter, requestID, model, content string, entries interface{}) {
	resp := map[string]interface{}{
		"id":      "chatcmpl-" + requestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    content,
				"tool_calls": entries,
			},
			"finish_reason": "tool_calls",
		}},
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	var writeMu sync.Mutex
	writeSSE := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Initial chunk
	writeSSE(newChunk("", model, requestID, nil))

	// Keepalive ticker (stopped + joined before return — no writes after close)
	keepAliveStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The keepalive is non-essential: writing to a disconnecting client
		// races the handler teardown and can panic on a half-closed
		// connection — an unrecovered panic here kills the WHOLE process
		// (observed in production). Swallow and stop instead.
		defer func() { recover() }()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeSSE(newChunk("", model, requestID, nil))
			case <-keepAliveStop:
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	finalContent, _, err := s.sendToZAI(ctx, prompt, opts, func(chunk string) {
		writeSSE(newChunk(chunk, model, requestID, nil))
	}, func(chunk string) {
		writeSSE(newReasoningChunk(chunk, model, requestID))
	})

	if err != nil {
		log.Printf("[Stream] Error: %v", err)
		errJSON, _ := json.Marshal(map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		writeMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		writeMu.Unlock()
	}

	// Final chunk
	writeSSE(newChunk("", model, requestID, strPtr("stop")))
	writeMu.Lock()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	writeMu.Unlock()
	flusher.Flush()

	close(keepAliveStop)
	wg.Wait()

	// History persistence (authoritative parser content, reasoning excluded)
	if persistHistoryEnabled() && err == nil && finalContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": finalContent},
		)
	}
}

// handleChatToolsStream streams a tools request, intercepting
// <<<TOOL_CALL>>> blocks live and rewriting them into tool_calls deltas.
func (s *Server) handleChatToolsStream(w http.ResponseWriter, r *http.Request, req chatRequest, prompt, model, requestID string, conv *Conversation, opts sendOpts) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	convertedMsgs := convertToolMessages(req.Messages, req.Tools)
	convertedPrompt := messagesToPrompt(convertedMsgs)
	opts.clientMessages = convertedMsgs

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	var writeMu sync.Mutex
	writeSSE := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	emitToolCallDelta := func(tc map[string]interface{}) {
		writeSSE(map[string]interface{}{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{"tool_calls": []map[string]interface{}{tc}},
				"finish_reason": nil,
			}},
		})
	}

	writeSSE(newChunk("", model, requestID, nil))

	keepAliveStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The keepalive is non-essential: writing to a disconnecting client
		// races the handler teardown and can panic on a half-closed
		// connection — an unrecovered panic here kills the WHOLE process
		// (observed in production). Swallow and stop instead.
		defer func() { recover() }()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeSSE(newChunk("", model, requestID, nil))
			case <-keepAliveStop:
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	interceptor := newToolStreamInterceptor()
	toolCallEmitted := false

	finalContent, _, err := s.sendToZAI(ctx, convertedPrompt, opts, func(chunk string) {
		// edit_content shrinkage is absorbed by the SSE parser (no deltas
		// emitted until caught up), so the interceptor buffer can go stale
		// across a shrink — the end-of-stream parseToolCalls fallback below
		// recovers naked JSON, fences and truncated blocks.
		contentDelta, toolCalls, _ := interceptor.feed(chunk)
		if contentDelta != "" {
			writeSSE(newChunk(contentDelta, model, requestID, nil))
		}
		for _, tc := range toolCalls {
			emitToolCallDelta(tc)
			toolCallEmitted = true
		}
	}, func(chunk string) {
		writeSSE(newReasoningChunk(chunk, model, requestID))
	})

	if err != nil {
		log.Printf("[Stream Tools] Error: %v", err)
		writeSSE(formatOpenAIError(err.Error(), "api_error"))
		writeMu.Lock()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		writeMu.Unlock()
		close(keepAliveStop)
		wg.Wait()
		return
	}

	// Flush trailing text (only when no tool calls were emitted)
	if rem := interceptor.flushFinal(); rem != "" && !toolCallEmitted {
		writeSSE(newChunk(rem, model, requestID, nil))
	}

	// Safety net: full parser over the authoritative content — catches naked
	// JSON, fences and truncated marker blocks (the marker-only extractor
	// requires a complete end marker).
	if !toolCallEmitted {
		for _, tc := range parseToolCalls(finalContent, req.Tools) {
			emitToolCallDelta(map[string]interface{}{
				"index":    0,
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]interface{}{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
			toolCallEmitted = true
		}
	}

	if toolCallEmitted {
		writeSSE(map[string]interface{}{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			}},
		})
		log.Printf("[Stream Tools] tool_calls emitted")
	} else {
		writeSSE(newChunk("", model, requestID, strPtr("stop")))
	}
	writeMu.Lock()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	writeMu.Unlock()
	flusher.Flush()

	close(keepAliveStop)
	wg.Wait()
}

func (s *Server) handleChatNonStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	// Buffered path: authoritative parser content/reasoning, not deltas
	fullContent, fullReasoning, err := s.sendToZAI(ctx, prompt, opts, nil, nil)

	if err != nil {
		log.Printf("[API] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	if persistHistoryEnabled() && fullContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent},
		)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newCompletion(fullContent, fullReasoning, prompt, model, requestID))
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	var body struct {
		Prompt    string `json:"prompt"`
		Search    *bool  `json:"search"`
		DeepThink *bool  `json:"deepThink"`
		WebSearch *bool  `json:"webSearch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if body.Prompt == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Prompt is required"})
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "default"
	}
	fresh := r.Header.Get("X-Fresh-Session") == "true"
	conv := s.conversations.getOrCreate(sessionID, fresh)

	opts := sendOpts{
		webSearch: firstNonNilBool(body.WebSearch, body.Search),
		thinking:  body.DeepThink,
		chatID:    conv.chatID,
		messages:  conv.messages,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	fullContent, _, err := s.sendToZAI(ctx, body.Prompt, opts, nil, nil)

	if err != nil {
		log.Printf("[Prompt] Error: %v", err)
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	if persistHistoryEnabled() && fullContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": body.Prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent},
		)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"response": fullContent,
	})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	totalMsgs := s.conversations.totalMessages()
	_, _, _, _, init := s.zaiSession.snapshot()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"mode":           "direct",
		"totalClients":   boolToInt(init),
		"activeSessions": s.conversations.count(),
		"stats":          map[string]int{"totalRequests": totalMsgs / 2},
	})
}

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	healthy := s.zaiSession.isConnected()
	if !healthy {
		w.WriteHeader(503)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"healthy": healthy, "mode": "direct"})
}

func (s *Server) handleAdminClients(w http.ResponseWriter, r *http.Request) {
	var clients []map[string]string
	if s.zaiSession.isConnected() {
		clients = []map[string]string{{"id": "session", "status": "idle"}}
	} else {
		clients = []map[string]string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"clients": clients})
}

func (s *Server) handleSessionClear(w http.ResponseWriter, r *http.Request) {
	s.conversations.clear()
	log.Println("[Session] All session histories cleared.")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"message":        "All session histories cleared",
		"activeSessions": 0,
	})
}

func (s *Server) handleClientClear(w http.ResponseWriter, r *http.Request) {
	s.conversations.clear()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "History cleared"})
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Direct mode"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Stop acknowledged"})
}

// ── Helpers ──

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── .env file loader (minimal: KEY=VALUE per line, # comments, quotes stripped) ──

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // no .env file — that's fine
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "`\"'")
		if os.Getenv(key) == "" { // env vars take precedence over .env
			os.Setenv(key, val)
		}
	}
}

// ── Banner ──

func printBanner() {
	cyan := "\033[36m"
	yellow := "\033[33m"
	magenta := "\033[35m"
	green := "\033[32m"
	reset := "\033[0m"

	fmt.Println()
	fmt.Println(cyan + "   ██████╗ ██╗     ███╗   ███╗    ███████╗ █████╗ ██╗    ██████╗  █████╗ ██████╗ ██╗" + reset)
	fmt.Println(cyan + "  ██╔════╝ ██║     ████╗ ████║    ╚══███╔╝██╔══██╗██║    ╚════██╗██╔══██╗██╔══██╗██║" + reset)
	fmt.Println(cyan + "  ██║  ███╗██║     ██╔████╔██║      ███╔╝ ███████║██║     █████╔╝███████║██████╔╝██║" + reset)
	fmt.Println(cyan + "  ██║   ██║██║     ██║╚██╔╝██║     ███╔╝  ██╔══██║██║    ██╔═══╝ ██╔══██║██╔═══╝ ██║" + reset)
	fmt.Println(cyan + "  ╚██████╔╝███████╗██║ ╚═╝ ██║    ███████╗██║  ██║██║    ███████╗██║  ██║██║     ██║" + reset)
	fmt.Println(cyan + "   ╚═════╝ ╚══════╝╚═╝     ╚═╝    ╚══════╝╚═╝  ╚═╝╚═╝    ╚══════╝╚═╝  ╚═╝╚═╝     ╚═╝" + reset)
	fmt.Println()
	fmt.Println(yellow + "  📧 Telegram:" + reset + " https://t.me/D3_vin")
	fmt.Println(magenta + "  👤 Author:" + reset + " @D3vin_dev")
	fmt.Println(green + "  🔗 GitHub:" + reset + " https://github.com/D3-vin/GLM-ZAI-2API")
	fmt.Println(cyan + "  📦 Version:" + reset + " 1.1.0")
	fmt.Println()
}

// ── Main ──

func Main() {
	var (
		dbPath     string
		verbose    bool
		tokensFile string
	)
	flag.StringVar(&dbPath, "db-path", "", "Path to tokens.sqlite (overrides DB_PATH env)")
	flag.BoolVar(&verbose, "verbose", false, "Enable captcha verbose logging")
	flag.StringVar(&tokensFile, "zai-tokens", "", "Z.AI token file, one JWT per line (overrides ZAI_TOKENS_FILE)")
	flag.Parse()

	loadEnvFile(".env")

	cfg := loadConfig()
	if claudeDesktopEnabled() {
		log.Printf("[Gateway] Claude Desktop mode ON — /v1/messages + claude model aliases")
	}
	if dbPath != "" {
		cfg.DBPath = dbPath
	}
	if tokensFile != "" {
		cfg.ZAITokensFile = tokensFile
	}
	captchaVerbose = verbose

	// Z.AI account pool: only when USE_ACCOUNT_POOL=true (mirrors the
	// USE_PROXY/PROXY_FILE pattern). With the switch off the pool file is
	// completely ignored — single ZAI_TOKEN mode.
	if cfg.UseAccountPool {
		if err := loadZaiTokens(cfg.ZAITokensFile); err != nil {
			log.Printf("[Session] Token pool: %v — using single ZAI_TOKEN mode", err)
		}
	} else if n := countPoolTokens(cfg.ZAITokensFile); n > 0 {
		log.Printf("[Session] Pool file has %d token(s) but USE_ACCOUNT_POOL=false — ignoring it (single ZAI_TOKEN mode)", n)
	}

	// Proxy pool (Z.AI chat traffic only; captcha/Aliyun stays direct)
	if cfg.UseProxy {
		if err := loadProxyPool(cfg.ProxyFile); err != nil {
			log.Printf("[Proxy] %v — continuing without proxies", err)
		} else {
			log.Printf("[Proxy] %d proxies loaded from %s, active: %s",
				len(proxies.urls), cfg.ProxyFile, proxies.urls[0].Redacted())
		}
	}

	// Open token store
	var tokenCount int
	tokenStore, err := OpenTokenStore(cfg.DBPath)
	if err != nil {
		log.Printf("[Startup] Token store: %v — captcha will fail until tokens.sqlite is available", err)
		tokenStore = nil
	} else {
		defer tokenStore.Close()
		tokenCount = tokenStore.Count()
	}

	srv := &Server{
		cfg:           cfg,
		zaiSession:    newZaiSession(),
		conversations: newConversationStore(),
		tokens:        tokenStore,
	}

	// Startup banner
	printBanner()
	fmt.Printf(`
╔═══════════════════════════════════════════════════════════════╗
║           GLM-ZAI-2API  (Go)                                  ║
╠═══════════════════════════════════════════════════════════════╣
║  Mode:          DIRECT HTTP (no browser needed)               ║
║  Dashboard:     http://localhost:%s                          ║
║  OpenAI API:    http://localhost:%s/v1/chat/completions      ║
║  Token DB:      %s (%d tokens)
║  Auth Token:    %s
╚═══════════════════════════════════════════════════════════════╝
`, cfg.Port, cfg.Port, cfg.DBPath, tokenCount, cfg.AuthToken)

	// Initialize the session with the account pool's current candidate
	// (falls back to the single ZAI_TOKEN when no pool file exists);
	// non-blocking on failure — will retry on first request.
	go func() {
		if err := srv.zaiSession.initialize(nextZaiToken(cfg.ZAIToken)); err != nil {
			log.Printf("[Startup] Session init deferred — will retry on first request: %v", err)
		}
	}()

	// Keep the model catalog cache warm in the background (refreshes at the
	// modelsCacheTTL cadence; fetch failures fall back to the cached list)
	go srv.startModelRefresher()

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("Listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Printf("Server error: %v", err)
		waitForEnter()
		os.Exit(1)
	}
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

// readBody is a small helper for non-streaming body reads.
func readBody(r io.Reader) string {
	data, _ := io.ReadAll(r)
	return string(data)
}
