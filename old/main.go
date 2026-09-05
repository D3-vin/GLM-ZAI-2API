// main.go — HTTP server, config, OpenAI-compatible routes, dashboard.
// Replaces main.js (Express) entirely. Captcha logic is in-process (no pipe IPC).
package main

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
	"time"
)

// ── Config ──

type Config struct {
	Host           string
	Port           string
	AuthEnabled    bool
	AuthToken      string
	ZAIToken       string
	LogLevel       string
	LogFormat      string
	Timeout        time.Duration
	DBPath         string
	CaptchaTimeout time.Duration
}

func loadConfig() *Config {
	cfg := &Config{
		Host:           envOr("HOST", "0.0.0.0"),
		Port:           envOr("PORT", "5082"),
		AuthEnabled:    true,
		AuthToken:      envOr("AUTH_TOKEN", "d3vin"),
		ZAIToken:       envOr("ZAI_TOKEN", ""),
		LogLevel:       envOr("LOG_LEVEL", "debug"),
		LogFormat:      envOr("LOG_FORMAT", "text"),
		Timeout:        time.Duration(envIntOr("TIMEOUT", 300000)) * time.Millisecond,
		DBPath:         envOr("DB_PATH", "tokens.sqlite"),
		CaptchaTimeout: 90 * time.Second,
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

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
	// No overall timeout — streaming requests need long-lived connections.
}

// ── Server ──

type Server struct {
	cfg           *Config
	zaiSession    *ZaiSession
	conversations *ConversationStore
	tokens        *TokenStore
}

// ── OpenAI response types ──

type openAIDelta struct {
	Content string `json:"content"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

var knownModels = []string{"glm-4.7", "GLM-5-Turbo", "GLM-5v-Turbo", "GLM-5.1", "glm-5.2"}

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

func newCompletion(content, prompt, model, requestID string) openAIResponse {
	pt := estimateTokens(prompt)
	ct := estimateTokens(content)
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      &openAIMessage{Role: "assistant", Content: content},
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

// ── Chat request ──

type chatRequest struct {
	Model     string            `json:"model"`
	Messages  []json.RawMessage `json:"messages"`
	Stream    *bool             `json:"stream"`
	DeepThink *bool             `json:"deepThink"`
	Search    *bool             `json:"search"`
	WebSearch *bool             `json:"webSearch"`
	Tools     []json.RawMessage `json:"tools,omitempty"`
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

	mux.HandleFunc("/prompt", s.withAuth(s.handlePrompt))
	mux.HandleFunc("/features", s.withAuth(s.handleFeatures))

	mux.HandleFunc("/admin/stats", s.handleAdminStats)
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
	_, userName, userID, feVersion, features, init := s.zaiSession.snapshot()
	if !init {
		userID = ""
	}
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
	})
}

func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	data := make([]map[string]interface{}, len(knownModels))
	for i, m := range knownModels {
		data[i] = map[string]interface{}{
			"id":           m,
			"object":       "model",
			"created":      now,
			"owned_by":     "z-ai",
			"display_name": m,
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models":       knownModels,
		"currentModel": "glm-4.7",
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
	prompt := messagesToPrompt(req.Messages)
	features := s.zaiSession.getFeatures()

	// Tool calling adapter: if request has tools, convert them to system prompt
	// and use non-streaming internally (need full response to parse tool calls)
	if len(req.Tools) > 0 {
		s.handleChatWithTools(w, r, req, prompt, model, requestID, conv, features)
		return
	}

	opts := sendOpts{
		model:          model,
		webSearch:      boolOr(req.WebSearch, boolOr(req.Search, features.WebSearch)),
		thinking:       boolOr(req.DeepThink, features.Thinking),
		imageGen:       features.ImageGen,
		previewMode:    features.PreviewMode,
		chatID:         conv.chatID,
		messages:       conv.messages,
		clientMessages: req.Messages,
	}

	if req.streamEnabled() {
		s.handleChatStream(w, r, prompt, model, requestID, opts, conv, features.PersistHistory)
	} else {
		s.handleChatNonStream(w, r, prompt, model, requestID, opts, conv, features.PersistHistory)
	}
}

// handleChatWithTools processes requests with tools (function calling).
// Internally uses non-streaming: buffers full response, parses for <<TOOL>> tags,
// returns either tool_calls or normal text in OpenAI format.
func (s *Server) handleChatWithTools(w http.ResponseWriter, r *http.Request, req chatRequest, prompt, model, requestID string, conv *Conversation, features Features) {
	// Convert messages: prepend tools system prompt, convert tool/assistant roles
	convertedMsgs := convertToolMessages(req.Messages, req.Tools)
	// Recompute prompt from converted messages for signature
	convertedPrompt := messagesToPrompt(convertedMsgs)

	opts := sendOpts{
		model:          model,
		webSearch:      boolOr(req.WebSearch, boolOr(req.Search, features.WebSearch)),
		thinking:       boolOr(req.DeepThink, features.Thinking),
		imageGen:       features.ImageGen,
		previewMode:    features.PreviewMode,
		chatID:         conv.chatID,
		messages:       conv.messages,
		clientMessages: convertedMsgs,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var fullContent strings.Builder
	err := s.sendToZAI(ctx, convertedPrompt, opts, func(chunk string) {
		fullContent.WriteString(chunk)
	})

	if err != nil {
		log.Printf("[Tools] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		statusCode := 500
		if strings.Contains(err.Error(), "401") {
			statusCode = 401
		}
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	content := fullContent.String()

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
	if req.streamEnabled() {
		// Client expects SSE — send text as stream chunks
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(newCompletion(content, prompt, model, requestID))
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

		// Split content into chunks to avoid oversized SSE events
		const chunkSize = 2000
		for i := 0; i < len(content); i += chunkSize {
			end := i + chunkSize
			if end > len(content) {
				end = len(content)
			}
			writeSSE(newChunk(content[i:end], model, requestID, nil))
		}

		// Final chunk with finish_reason
		writeSSE(newChunk("", model, requestID, strPtr("stop")))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// Non-streaming JSON
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newCompletion(content, prompt, model, requestID))
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

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation, persist bool) {
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

	writeSSE := func(v interface{}) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Initial chunk
	writeSSE(newChunk("", model, requestID, nil))

	// Keepalive ticker
	keepAlive := time.NewTicker(5 * time.Second)
	defer keepAlive.Stop()
	go func() {
		for range keepAlive.C {
			writeSSE(newChunk("", model, requestID, nil))
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var fullContent strings.Builder
	var sentContent strings.Builder

	err := s.sendToZAI(ctx, prompt, opts, func(chunk string) {
		fullContent.WriteString(chunk)
		delta := fullContent.String()[sentContent.Len():]
		if delta != "" {
			sentContent.WriteString(delta)
			writeSSE(newChunk(delta, model, requestID, nil))
		}
	})

	if err != nil {
		log.Printf("[Stream] Error: %v", err)
		errJSON, _ := json.Marshal(map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
	}

	// Final chunk
	writeSSE(newChunk("", model, requestID, strPtr("stop")))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// History persistence
	if persist && err == nil {
		p := prompt
		if fullContent.Len() > 0 {
			conv.messages = append(conv.messages,
				map[string]interface{}{"role": "user", "content": p},
				map[string]interface{}{"role": "assistant", "content": fullContent.String()},
			)
		}
	}
}

func (s *Server) handleChatNonStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation, persist bool) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var fullContent strings.Builder
	err := s.sendToZAI(ctx, prompt, opts, func(chunk string) {
		fullContent.WriteString(chunk)
	})

	if err != nil {
		log.Printf("[API] Error: %v", err)
		statusCode := 500
		if strings.Contains(err.Error(), "401") {
			statusCode = 401
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	if persist {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent.String()},
		)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newCompletion(fullContent.String(), prompt, model, requestID))
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

	features := s.zaiSession.getFeatures()
	opts := sendOpts{
		webSearch: boolOr(body.WebSearch, boolOr(body.Search, features.WebSearch)),
		thinking:  boolOr(body.DeepThink, features.Thinking),
		chatID:    conv.chatID,
		messages:  conv.messages,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var fullContent strings.Builder
	err := s.sendToZAI(ctx, body.Prompt, opts, func(chunk string) {
		fullContent.WriteString(chunk)
	})

	if err != nil {
		log.Printf("[Prompt] Error: %v", err)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	if features.PersistHistory {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": body.Prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent.String()},
		)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"response": fullContent.String(),
	})
}

func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WebSearch      *bool `json:"webSearch"`
		Thinking       *bool `json:"thinking"`
		ImageGen       *bool `json:"imageGen"`
		PreviewMode    *bool `json:"previewMode"`
		PersistHistory *bool `json:"persistHistory"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	f := s.zaiSession.getFeatures()
	if body.WebSearch != nil {
		f.WebSearch = *body.WebSearch
		f.AutoWebSearch = *body.WebSearch
	}
	if body.Thinking != nil {
		f.Thinking = *body.Thinking
	}
	if body.ImageGen != nil {
		f.ImageGen = *body.ImageGen
	}
	if body.PreviewMode != nil {
		f.PreviewMode = *body.PreviewMode
	}
	if body.PersistHistory != nil {
		f.PersistHistory = *body.PersistHistory
	}
	s.zaiSession.updateFeatures(f)
	log.Printf("[Features] Updated: %+v", f)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "features": f})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	totalMsgs := s.conversations.totalMessages()
	_, _, _, _, _, init := s.zaiSession.snapshot()
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

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

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
	fmt.Println(cyan + "  📦 Version:" + reset + " 1.0.3")
	fmt.Println()
}

// ── Main ──

func main() {
	var (
		dbPath  string
		verbose bool
	)
	flag.StringVar(&dbPath, "db-path", "", "Path to tokens.sqlite (overrides DB_PATH env)")
	flag.BoolVar(&verbose, "verbose", false, "Enable captcha verbose logging")
	flag.Parse()

	loadEnvFile(".env")

	cfg := loadConfig()
	if dbPath != "" {
		cfg.DBPath = dbPath
	}
	captchaVerbose = verbose

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

	// Initialize session (non-blocking on failure — will retry on first request)
	go func() {
		if err := srv.zaiSession.initialize(cfg.ZAIToken); err != nil {
			log.Printf("[Startup] Session init deferred — will retry on first request: %v", err)
		}
	}()

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
