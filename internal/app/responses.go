// responses.go — OpenAI Responses API (POST /v1/responses) for Codex CLI.
//
// Codex 0.147 talks ONLY to /v1/responses; this handler maps the Responses
// surface onto the same Z.AI pipeline used by /v1/chat/completions:
//
//	input items (message / function_call / function_call_output)
//	  → OpenAI-style messages → convertToolMessages (EXECUTION LAW framing)
//	  → sendToZAI → parseToolCalls → output items (message / function_call)
//
// Stateless by design: Codex resends the full history every request and
// uses store:false. Streaming emits the Responses SSE event sequence
// (response.created → output_item.added → output_text.delta → … →
// response.completed). Reasoning items are accepted and skipped.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── Request types ──

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Stream             bool              `json:"stream"`
	Tools              []json.RawMessage `json:"tools,omitempty"` // flat Responses format
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
}

// responsesInputItem is a union of all input item shapes we accept.
type responsesInputItem struct {
	Type    string          `json:"type"` // message | function_call | function_call_output | reasoning
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    string          `json:"arguments,omitempty"`
	Output  string          `json:"output,omitempty"`
	ID      string          `json:"id,omitempty"`
	Status  string          `json:"status,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"` // reasoning items — skipped
}

// responsesTool is the flat Responses tool definition.
type responsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	Strict      bool                   `json:"strict,omitempty"`
}

// ── Response output types ──

type responsesOutputText struct {
	Type string `json:"type"` // output_text
	Text string `json:"text"`
}

type responsesOutputMessage struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"` // message
	Status  string                `json:"status"`
	Role    string                `json:"role"`
	Content []responsesOutputText `json:"content"`
}

type responsesOutputFunctionCall struct {
	ID        string `json:"id"` // item id
	CallID    string `json:"call_id"`
	Type      string `json:"type"` // function_call
	Status    string `json:"status"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// mustJSON marshals v or panics on an impossible error (map[string]… only).
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // unreachable for map/slice values used here
	}
	return b
}

// ── Input conversion ──

// responsesContentText extracts concatenated text from a Responses content
// value (string or array of typed parts).
func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "refusal":
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// convertResponsesInput maps the Responses `input` value (string or item
// array) into OpenAI-style messages consumable by the chat pipeline.
func convertResponsesInput(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	var strInput string
	if json.Unmarshal(raw, &strInput) == nil {
		if strings.TrimSpace(strInput) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []json.RawMessage{mustJSON(map[string]string{"role": "user", "content": strInput})}, nil
	}

	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid input items: %w", err)
	}

	var msgs []json.RawMessage
	for _, it := range items {
		switch it.Type {
		case "", "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			text := responsesContentText(it.Content)
			imgParts := responsesImageParts(it.Content)
			if len(imgParts) > 0 {
				// message with images → typed content array (text + images);
				// processVisionMessages later rewrites urls to Z.AI file ids
				content := make([]map[string]interface{}, 0, len(imgParts)+1)
				if strings.TrimSpace(text) != "" {
					content = append(content, map[string]interface{}{"type": "text", "text": text})
				}
				content = append(content, imgParts...)
				msgs = append(msgs, mustJSON(map[string]interface{}{"role": role, "content": content}))
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgs = append(msgs, mustJSON(map[string]string{"role": role, "content": text}))

		case "function_call":
			if it.Name == "" {
				continue
			}
			callID := it.CallID
			if callID == "" {
				callID = "call_" + generateID()
			}
			args := it.Args
			if args == "" {
				args = "{}"
			}
			msgs = append(msgs, mustJSON(map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   callID,
					"type": "function",
					"function": map[string]string{
						"name":      it.Name,
						"arguments": args,
					},
				}},
			}))

		case "function_call_output":
			if it.CallID == "" {
				continue
			}
			msgs = append(msgs, mustJSON(map[string]string{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      it.Output,
			}))

		case "reasoning":
			// Codex reasoning items — skipped: Z.AI has its own thinking.

		default:
			// unknown item types are ignored for forward compatibility
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input produced no usable messages")
	}
	return msgs, nil
}

// responsesImageParts extracts input_image content parts (Responses format:
// {"type":"input_image","image_url":"<url-or-data-url>"}) as OpenAI image_url
// parts. Empty for text-only content.
func responsesImageParts(raw json.RawMessage) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		FileID   string `json:"file_id"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []map[string]interface{}
	for _, p := range parts {
		if p.Type != "input_image" {
			continue
		}
		u := strings.TrimSpace(p.ImageURL)
		if u == "" && p.FileID != "" {
			u = p.FileID
		}
		if u == "" {
			continue
		}
		out = append(out, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": u},
		})
	}
	return out
}

// convertResponsesTools maps flat Responses tools to the nested OpenAI
// format used by buildSchemaMap/convertToolMessages.
func convertResponsesTools(tools []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, raw := range tools {
		var t responsesTool
		if json.Unmarshal(raw, &t) != nil || t.Type != "function" || t.Name == "" {
			continue
		}
		nested := map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
			},
		}
		if t.Parameters != nil {
			nested["function"].(map[string]interface{})["parameters"] = t.Parameters
		}
		out = append(out, mustJSON(nested))
	}
	return out
}

// ── Handler ──

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(405)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed — POST /v1/responses"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.PreviousResponseID != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "previous_response_id is not supported — this bridge is stateless, resend the full input",
		})
		return
	}

	msgs, err := convertResponsesInput(req.Input)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if req.Instructions != "" {
		msgs = append([]json.RawMessage{mustJSON(map[string]string{"role": "system", "content": req.Instructions})}, msgs...)
	}

	model := resolveGLMModel(req.Model)
	tools := convertResponsesTools(req.Tools)

	// Codex resends the full history — isolate from chat sessions and keep
	// one Z.AI chat per responses-session for continuity.
	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "responses-default"
	}
	conv := s.conversations.getOrCreate(sessionID, false)
	requestID := generateID()

	// ── Vision: upload input_image parts, rewrite urls to Z.AI file ids.
	var visionFiles []map[string]interface{}
	if rawMsgs, merr := json.Marshal(msgs); merr == nil {
		cleaned, files, vErr := s.processVisionMessages(r.Context(), rawMsgs)
		if vErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{
				"message": vErr.Error(), "type": "invalid_request_error",
			}})
			return
		}
		if files != nil {
			if err := json.Unmarshal(cleaned, &msgs); err == nil {
				// fallthrough: msgs now carry file-id references
				_ = err
			}
			visionFiles = files
		}
	}

	if len(tools) > 0 {
		converted := convertToolMessages(msgs, tools)
		msgs = converted
	}
	prompt := messagesToPrompt(msgs)

	opts := sendOpts{
		model:          model,
		chatID:         conv.chatID,
		messages:       conv.messages,
		clientMessages: msgs,
		files:          visionFiles,
	}

	if !req.Stream {
		s.responsesNonStream(w, r, req, model, prompt, opts, tools, requestID)
		return
	}
	s.responsesStream(w, r, req, model, prompt, opts, tools, requestID)
}

// ── Response assembly ──

type responsesResult struct {
	Message     string // assistant text (tool blocks stripped)
	TextEmitted bool   // any text was produced
	Calls       []parsedToolCall
}

func (s *Server) responsesRun(ctx context.Context, prompt string, opts sendOpts, tools []json.RawMessage, onDelta func(string)) (responsesResult, string, string, error) {
	// Tool blocks are stripped from text deltas via the interceptor; the
	// authoritative calls ALWAYS come from parseToolCalls on the final
	// content (the interceptor emits arguments incrementally — unusable for
	// a complete function_call item).
	var interceptor *toolStreamInterceptor
	onContent := onDelta
	if len(tools) > 0 {
		interceptor = newToolStreamInterceptor()
		onContent = func(delta string) {
			contentDelta, _, _ := interceptor.feed(delta)
			if contentDelta != "" && onDelta != nil {
				onDelta(contentDelta)
			}
		}
	}
	content, reasoning, err := s.sendToZAI(ctx, prompt, opts, onContent, nil)
	if err != nil {
		return responsesResult{}, content, reasoning, err
	}
	if interceptor != nil {
		tail := interceptor.flushFinal()
		if tail != "" && onDelta != nil {
			onDelta(tail)
		}
	}

	clean := stripAgentToolCallBlocks(content)
	var calls []parsedToolCall
	if len(tools) > 0 {
		calls = parseToolCalls(content, tools)
	}
	res := responsesResult{Message: clean, TextEmitted: strings.TrimSpace(clean) != "", Calls: calls}
	return res, content, reasoning, nil
}

func (s *Server) responsesNonStream(w http.ResponseWriter, r *http.Request, req responsesRequest, model, prompt string, opts sendOpts, tools []json.RawMessage, requestID string) {
	res, _, _, err := s.responsesRun(r.Context(), prompt, opts, tools, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{
			"message": err.Error(), "type": "bridge_error",
		}})
		return
	}

	output := make([]interface{}, 0, 2)
	if res.TextEmitted || len(res.Calls) == 0 {
		output = append(output, responsesOutputMessage{
			ID: "msg_" + requestID, Type: "message", Status: "completed",
			Role:    "assistant",
			Content: []responsesOutputText{{Type: "output_text", Text: res.Message}},
		})
	}
	for i, c := range res.Calls {
		output = append(output, responsesOutputFunctionCall{
			ID: fmt.Sprintf("fc_%s_%d", requestID, i), CallID: c.ID, Type: "function_call",
			Status: "completed", Name: c.Function.Name, Arguments: c.Function.Arguments,
		})
	}

	usage := responsesUsage{
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(res.Message) / 4,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                  "resp_" + requestID,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"usage":               usage,
	})
}

// ── Streaming ──

type responsesSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher
	seq     int
}

func newResponsesSSE(w http.ResponseWriter) *responsesSSE {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		flusher = nil
	}
	return &responsesSSE{w: w, flusher: flusher}
}

func (s *responsesSSE) send(event string, payload map[string]interface{}) {
	s.seq++
	payload["sequence_number"] = s.seq
	data, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *Server) responsesStream(w http.ResponseWriter, r *http.Request, req responsesRequest, model, prompt string, opts sendOpts, tools []json.RawMessage, requestID string) {
	respID := "resp_" + requestID
	sse := newResponsesSSE(w)

	created := map[string]interface{}{
		"id": respID, "object": "response", "status": "in_progress", "model": model,
	}
	sse.send("response.created", map[string]interface{}{"type": "response.created", "response": created})
	sse.send("response.in_progress", map[string]interface{}{"type": "response.in_progress", "response": created})

	outputIdx := 0
	var fullText strings.Builder
	textStarted := false

	startMessage := func() {
		if textStarted {
			return
		}
		textStarted = true
		sse.send("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": outputIdx,
			"item": map[string]interface{}{"id": "msg_" + requestID, "type": "message",
				"status": "in_progress", "role": "assistant", "content": []interface{}{}},
		})
		sse.send("response.content_part.added", map[string]interface{}{
			"type": "response.content_part.added", "item_id": "msg_" + requestID,
			"output_index": outputIdx, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
	}

	res, _, _, err := s.responsesRun(r.Context(), prompt, opts, tools, func(delta string) {
		startMessage()
		fullText.WriteString(delta)
		sse.send("response.output_text.delta", map[string]interface{}{
			"type": "response.output_text.delta", "item_id": "msg_" + requestID,
			"output_index": outputIdx, "content_index": 0, "delta": delta,
		})
	})
	if err != nil {
		sse.send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id": respID, "object": "response", "status": "failed", "model": model,
				"error": map[string]interface{}{"code": "bridge_error", "message": err.Error()},
			},
		})
		return
	}

	output := make([]interface{}, 0, 2)
	if textStarted || res.TextEmitted || len(res.Calls) == 0 {
		sse.send("response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "item_id": "msg_" + requestID,
			"output_index": outputIdx, "content_index": 0, "text": res.Message,
		})
		sse.send("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIdx,
			"item": responsesOutputMessage{
				ID: "msg_" + requestID, Type: "message", Status: "completed",
				Role: "assistant", Content: []responsesOutputText{{Type: "output_text", Text: res.Message}},
			},
		})
		output = append(output, responsesOutputMessage{
			ID: "msg_" + requestID, Type: "message", Status: "completed",
			Role: "assistant", Content: []responsesOutputText{{Type: "output_text", Text: res.Message}},
		})
		outputIdx++
	}

	for i, c := range res.Calls {
		item := responsesOutputFunctionCall{
			ID: fmt.Sprintf("fc_%s_%d", requestID, i), CallID: c.ID, Type: "function_call",
			Status: "completed", Name: c.Function.Name, Arguments: c.Function.Arguments,
		}
		sse.send("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": outputIdx,
			"item": map[string]interface{}{"id": item.ID, "type": "function_call",
				"status": "in_progress", "call_id": item.CallID, "name": item.Name,
				"arguments": ""},
		})
		sse.send("response.function_call_arguments.delta", map[string]interface{}{
			"type": "response.function_call_arguments.delta", "item_id": item.ID,
			"output_index": outputIdx, "delta": c.Function.Arguments,
		})
		sse.send("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": outputIdx, "item": item,
		})
		output = append(output, item)
		outputIdx++
	}

	usage := responsesUsage{InputTokens: len(prompt) / 4, OutputTokens: len(res.Message) / 4}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens

	sse.send("response.completed", map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id": respID, "object": "response", "status": "completed", "model": model,
			"output": output, "usage": usage,
		},
	})
}
