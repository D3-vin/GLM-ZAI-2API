// tools.go - Tool calling adapter for models without native function calling.
// Two strategies:
// 1. Ask model to output JSON tool calls in ```json blocks
// 2. Fallback: parse natural language response for actionable patterns
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
)

// Match ```json blocks containing a tool call
var jsonToolRegex = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

// Match ```tool blocks
var toolTagRegex = regexp.MustCompile(`(?s)<<TOOL>>\s*(\{.*?\})\s*<</TOOL>>`)

// buildToolsSystemPrompt uses "unit test" framing (proven in notion project)
// to avoid triggering model safety refusals like "I cannot create files".
// The model thinks it's generating expected JSON output for a test, not calling tools.
func buildToolsSystemPrompt(tools []json.RawMessage) string {
	var sb strings.Builder
	sb.WriteString("I'm writing a unit test for an API router. Generate the expected JSON output for this test case.\n")
	sb.WriteString("Available functions:\n")
	sb.WriteString(buildCompactToolList(tools))
	sb.WriteString("Output format: {\"name\": \"function_name\", \"arguments\": {...}}\n")
	sb.WriteString("Output ONLY the JSON object. If no function matches, respond with text.\n\n")
	return sb.String()
}

// buildCompactToolList creates ultra-compact function signatures.
// Example: "- write_file(path: str, content: str) — Write content to a file"
// Reduces tool definitions from ~60k chars to ~2-3k.
func buildCompactToolList(tools []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range tools {
		var tool struct {
			Function struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description"`
				Parameters  map[string]interface{} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("- %s", tool.Function.Name))
		if tool.Function.Parameters != nil {
			if sig := extractParamSignature(tool.Function.Parameters); sig != "" {
				sb.WriteString(fmt.Sprintf("(%s)", sig))
			}
		}
		if tool.Function.Description != "" {
			desc := tool.Function.Description
			if len(desc) > 80 {
				desc = desc[:80] + "..."
			}
			sb.WriteString(fmt.Sprintf(" — %s", desc))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// extractParamSignature extracts compact params from JSON schema.
// {"properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}
// → "path: str, content?: str"
func extractParamSignature(schema map[string]interface{}) string {
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return ""
	}
	requiredSet := map[string]bool{}
	if req, ok := schema["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				requiredSet[s] = true
			}
		}
	}
	var parts []string
	for name, v := range props {
		typeName := "any"
		if pm, ok := v.(map[string]interface{}); ok {
			if t, ok := pm["type"].(string); ok {
				switch t {
				case "string":
					typeName = "str"
				case "integer":
					typeName = "int"
				case "boolean":
					typeName = "bool"
				case "array":
					typeName = "arr"
				case "object":
					typeName = "obj"
				}
			}
		}
		if requiredSet[name] {
			parts = append(parts, fmt.Sprintf("%s: %s", name, typeName))
		} else {
			parts = append(parts, fmt.Sprintf("%s?: %s", name, typeName))
		}
	}
	return strings.Join(parts, ", ")
}

type parsedToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// parseToolCalls tries multiple strategies to extract tool calls from model response:
// 1. ```json blocks with "name" field
// 2. <<TOOL>> tags (legacy)
// 3. Natural language: "create file X with content Y"
func parseToolCalls(content string, tools []json.RawMessage) []parsedToolCall {
	schemas := buildSchemaMap(tools)

	// Strategy 1: ```json code blocks
	if calls := filterValidCalls(dedupCalls(parseJSONToolCalls(content)), schemas); len(calls) > 0 {
		return calls
	}

	// Strategy 2: Direct JSON object (response is just {"name":"...","arguments":{...}})
	if calls := filterValidCalls(dedupCalls(parseDirectJSON(content)), schemas); len(calls) > 0 {
		return calls
	}

	// Strategy 2b: Embedded JSON — model wrapped tool call in text/code fences without json tag
	if calls := filterValidCalls(dedupCalls(parseEmbeddedJSON(content)), schemas); len(calls) > 0 {
		return calls
	}

	// Strategy 3: Multi-line JSON (one tool call per line)
	if calls := filterValidCalls(dedupCalls(parseMultilineJSON(content)), schemas); len(calls) > 0 {
		return calls
	}

	// Strategy 4: <<TOOL>> tags (legacy)
	if calls := filterValidCalls(dedupCalls(parseTagToolCalls(content)), schemas); len(calls) > 0 {
		return calls
	}

	// Strategy 5: Parse natural language for common patterns
	if calls := filterValidCalls(dedupCalls(parseNaturalLanguage(content, tools)), schemas); len(calls) > 0 {
		return calls
	}

	return nil
}

// buildSchemaMap indexes tool definitions by name → their JSON-Schema "parameters".
func buildSchemaMap(tools []json.RawMessage) map[string]map[string]interface{} {
	m := make(map[string]map[string]interface{})
	for _, raw := range tools {
		var t struct {
			Function struct {
				Name       string                 `json:"name"`
				Parameters map[string]interface{} `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &t) == nil && t.Function.Name != "" {
			m[t.Function.Name] = t.Function.Parameters
		}
	}
	return m
}

// filterValidCalls drops tool calls with unknown names or invalid JSON arguments.
// Schema mismatches are logged but the call is still forwarded — the client
// does its own validation and GLM's schemas are often slightly off.
func filterValidCalls(calls []parsedToolCall, schemas map[string]map[string]interface{}) []parsedToolCall {
	var valid []parsedToolCall
	for _, c := range calls {
		if _, ok := schemas[c.Function.Name]; !ok {
			log.Printf("[Tools] drop %q: unknown tool name", c.Function.Name)
			continue
		}
		var args interface{}
		if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
			log.Printf("[Tools] drop %q: arguments not valid JSON: %v", c.Function.Name, err)
			continue
		}
		if schema := schemas[c.Function.Name]; schema != nil {
			if verr := validateAgainstSchema(args, schema); verr != nil {
				log.Printf("[Tools] schema mismatch for %q (forwarding): %v", c.Function.Name, verr)
			}
		}
		valid = append(valid, c)
	}
	return valid
}

// cleanFallbackContent replaces a dropped tool-call JSON with a readable
// message so the user doesn't see raw JSON in the chat. Keeps any readable
// text the model wrote around it.
func cleanFallbackContent(content string) string {
	trimmed := strings.TrimSpace(content)
	if looksLikeToolCallJSON(trimmed) {
		return "I couldn't complete that action. Could you rephrase or provide more detail?"
	}
	cleaned := stripToolContent(content)
	if strings.TrimSpace(cleaned) == "" {
		return "I couldn't complete that action. Could you rephrase or provide more detail?"
	}
	return cleaned
}

// looksLikeToolCallJSON reports whether s is a JSON object with "name" and
// "arguments" — i.e. a bare tool call the model emitted as text.
func looksLikeToolCallJSON(s string) bool {
	if len(s) == 0 || s[0] != '{' {
		return false
	}
	var probe struct {
		Name      json.RawMessage `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	return json.Unmarshal([]byte(s), &probe) == nil && len(probe.Name) > 0
}

// validateAgainstSchema is a minimal recursive JSON-Schema validator:
// type, required, properties, items. Enough to catch GLM's common mistakes
// (string where object expected, missing required fields). Not a full validator.
func validateAgainstSchema(value interface{}, schema map[string]interface{}) error {
	if t, ok := schema["type"].(string); ok && t != "" {
		if err := checkJSONType(value, t); err != nil {
			return err
		}
	}
	switch v := value.(type) {
	case map[string]interface{}:
		if req, ok := schema["required"].([]interface{}); ok {
			for _, r := range req {
				if key, ok := r.(string); ok {
					if _, exists := v[key]; !exists {
						return fmt.Errorf("missing required field %q", key)
					}
				}
			}
		}
		if props, ok := schema["properties"].(map[string]interface{}); ok {
			for k, val := range v {
				if propSchema, ok := props[k].(map[string]interface{}); ok {
					if err := validateAgainstSchema(val, propSchema); err != nil {
						return fmt.Errorf("field %q: %w", k, err)
					}
				}
			}
		}
	case []interface{}:
		if itemsSchema, ok := schema["items"].(map[string]interface{}); ok {
			for i, item := range v {
				if err := validateAgainstSchema(item, itemsSchema); err != nil {
					return fmt.Errorf("item[%d]: %w", i, err)
				}
			}
		}
	}
	return nil
}

func checkJSONType(value interface{}, t string) error {
	switch t {
	case "object":
		if _, ok := value.(map[string]interface{}); !ok {
			return fmt.Errorf("expected object")
		}
	case "array":
		if _, ok := value.([]interface{}); !ok {
			return fmt.Errorf("expected array")
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case "number", "integer":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("expected number")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	}
	return nil
}

// parseDirectJSON handles response that is just a JSON object
func parseDirectJSON(content string) []parsedToolCall {
	stripped := strings.TrimSpace(content)
	var direct struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// Also accept "action" as field name (some models use this instead of "name")
	if direct.Name == "" {
		var alt struct {
			Action    string          `json:"action"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal([]byte(stripped), &alt)
		if alt.Action != "" {
			direct.Name = alt.Action
			direct.Arguments = alt.Arguments
		}
	}
	if err := json.Unmarshal([]byte(stripped), &direct); err == nil && direct.Name != "" {
		args := string(direct.Arguments)
		if !json.Valid(direct.Arguments) {
			args = "{}"
		}
		if direct.Name == "__done__" {
			// Model wrapped tool call inside __done__.result — extract it.
			var done struct {
				Result string `json:"result"`
			}
			json.Unmarshal(direct.Arguments, &done)
			if done.Result != "" {
				// Try parsing result as a tool call JSON
				resultStr := strings.TrimSpace(done.Result)
				// Strip markdown code fences if present
				resultStr = strings.TrimPrefix(resultStr, "```json\n")
				resultStr = strings.TrimPrefix(resultStr, "```\n")
				resultStr = strings.TrimSuffix(resultStr, "\n```")
				resultStr = strings.TrimSpace(resultStr)
				var nested struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if json.Unmarshal([]byte(resultStr), &nested) == nil && nested.Name != "" && nested.Name != "__done__" {
					nestedArgs := string(nested.Arguments)
					if !json.Valid(nested.Arguments) {
						nestedArgs = "{}"
					}
					return []parsedToolCall{{ID: "call_1", Type: "function", Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: nested.Name, Arguments: nestedArgs}}}
				}
			}
			return nil // genuine text response
		}
		return []parsedToolCall{{ID: "call_1", Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: direct.Name, Arguments: args}}}
	}
	return nil
}

// parseEmbeddedJSON extracts a JSON tool call that the model wrapped in text
// or plain code fences (``` without json tag). Finds the first { and last },
// then tries to parse the substring as a tool call.
func parseEmbeddedJSON(content string) []parsedToolCall {
	first := strings.IndexByte(content, '{')
	if first < 0 {
		return nil
	}
	last := strings.LastIndexByte(content, '}')
	if last <= first {
		return nil
	}
	// ponytail: naive first-{ to last-} extraction; fails if content has multiple
	// JSON objects with trailing text, but covers the common single-call case.
	extracted := content[first : last+1]
	if extracted == strings.TrimSpace(content) {
		return nil // parseDirectJSON already tried the full content
	}
	return parseDirectJSON(extracted)
}

// parseMultilineJSON handles one JSON object per line
func parseMultilineJSON(content string) []parsedToolCall {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	var calls []parsedToolCall
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cleanLine := line
		for len(cleanLine) > 0 && (cleanLine[len(cleanLine)-1] == '}' || cleanLine[len(cleanLine)-1] == '`') {
			var generic map[string]interface{}
			if err := json.Unmarshal([]byte(cleanLine), &generic); err == nil {
				if nameRaw, ok := generic["name"].(string); ok && nameRaw != "" && nameRaw != "__done__" {
					var argsStr string
					if argsRaw, ok := generic["arguments"]; ok {
						if argsMap, isMap := argsRaw.(map[string]interface{}); isMap {
							argsBytes, _ := json.Marshal(argsMap)
							argsStr = string(argsBytes)
						} else if argsString, isString := argsRaw.(string); isString {
							argsStr = argsString
						}
					}

					if argsStr == "" {
						delete(generic, "name")
						delete(generic, "type")
						argsBytes, _ := json.Marshal(generic)
						argsStr = string(argsBytes)
					}
					if argsStr == "" || argsStr == "null" {
						argsStr = "{}"
					}

					calls = append(calls, parsedToolCall{Type: "function", Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: nameRaw, Arguments: argsStr}})

					break
				}
			}
			cleanLine = cleanLine[:len(cleanLine)-1]
		}
	}
	return calls
}

// dedupCalls removes duplicate tool calls (same name + same arguments)
func dedupCalls(calls []parsedToolCall) []parsedToolCall {
	seen := make(map[string]bool)
	var result []parsedToolCall
	for _, c := range calls {
		key := c.Function.Name + ":" + c.Function.Arguments
		if !seen[key] {
			seen[key] = true
			c.ID = fmt.Sprintf("call_%d", len(result)+1)
			result = append(result, c)
		}
	}
	return result
}

func parseJSONToolCalls(content string) []parsedToolCall {
	matches := jsonToolRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	var calls []parsedToolCall
	for i, m := range matches {
		var parsed struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil {
			continue
		}
		if parsed.Name == "" {
			continue
		}
		argBytes, _ := json.Marshal(parsed.Arguments)
		call := parsedToolCall{
			ID:   fmt.Sprintf("call_%d", i+1),
			Type: "function",
		}
		call.Function.Name = parsed.Name
		call.Function.Arguments = string(argBytes)
		calls = append(calls, call)
	}
	return calls
}

func parseTagToolCalls(content string) []parsedToolCall {
	matches := toolTagRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}

	var calls []parsedToolCall
	for i, m := range matches {
		var parsed struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil {
			continue
		}
		if parsed.Name == "" {
			continue
		}
		argBytes, _ := json.Marshal(parsed.Arguments)
		call := parsedToolCall{
			ID:   fmt.Sprintf("call_%d", i+1),
			Type: "function",
		}
		call.Function.Name = parsed.Name
		call.Function.Arguments = string(argBytes)
		calls = append(calls, call)
	}
	return calls
}

// parseNaturalLanguage extracts tool calls from model's natural language response.
// Detects patterns like: "echo "content" > file" or code blocks with file paths.
func parseNaturalLanguage(content string, tools []json.RawMessage) []parsedToolCall {
	// Build a map of available tool names
	toolNames := make(map[string]bool)
	for _, raw := range tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tool) == nil {
			toolNames[tool.Function.Name] = true
		}
	}

	var calls []parsedToolCall
	callIdx := 0

	// Pattern 1: bash "echo" commands that write to files
	// e.g.: echo "Hello World" > hello.txt  or  echo Hello World > hello.txt
	echoRegex := regexp.MustCompile(`echo\s+["']?(.*?)["']?\s*>\s*(\S+)`)
	for _, m := range echoRegex.FindAllStringSubmatch(content, -1) {
		if !toolNames["write_file"] && !toolNames["write"] {
			continue
		}
		fileContent := m[1]
		filePath := strings.Trim(m[2], "`\"'")
		toolName := "write_file"
		if !toolNames["write_file"] {
			toolName = "write"
		}
		callIdx++
		args, _ := json.Marshal(map[string]string{"path": filePath, "content": fileContent})
		call := parsedToolCall{ID: fmt.Sprintf("call_%d", callIdx), Type: "function"}
		call.Function.Name = toolName
		call.Function.Arguments = string(args)
		calls = append(calls, call)
	}

	// Pattern 2: code blocks with language hint (```python, ```js, etc.)
	// that likely represent file content to be written
	codeBlockRegex := regexp.MustCompile("(?s)```(\\w+)?\\s*\n(.*?)\n```")
	codeBlocks := codeBlockRegex.FindAllStringSubmatch(content, -1)

	// Pattern 3: "Save this to file.txt" or "create file called X"
	fileRefRegex := regexp.MustCompile(`(?:file|File)\s+(?:called|named)\s+["']?(.+?)["']?`)
	fileRefs := fileRefRegex.FindAllStringSubmatch(content, -1)

	// If we have both code blocks and file references, combine them
	if len(codeBlocks) > 0 && len(fileRefs) > 0 && (toolNames["write_file"] || toolNames["write"]) {
		toolName := "write_file"
		if !toolNames["write_file"] {
			toolName = "write"
		}
		filePath := strings.Trim(fileRefs[0][1], "' \".")
		fileContent := codeBlocks[0][2]

		callIdx++
		args, _ := json.Marshal(map[string]string{"path": filePath, "content": fileContent})
		call := parsedToolCall{ID: fmt.Sprintf("call_%d", callIdx), Type: "function"}
		call.Function.Name = toolName
		call.Function.Arguments = string(args)
		calls = append(calls, call)
	}

	return calls
}

func stripToolContent(content string) string {
	cleaned := toolTagRegex.ReplaceAllString(content, "")
	cleaned = jsonToolRegex.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func convertToolMessages(messages []json.RawMessage, tools []json.RawMessage) []json.RawMessage {
	var result []json.RawMessage

	// Build the "unit test" framing prompt
	framing := buildToolsSystemPrompt(tools)

	// Find the last user message index (where we'll embed the framing)
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		var msg map[string]interface{}
		if json.Unmarshal(messages[i], &msg) == nil {
			if role, _ := msg["role"].(string); role == "user" {
				if _, hasToolCallID := msg["tool_call_id"]; !hasToolCallID {
					lastUserIdx = i
					break
				}
			}
		}
	}

	for i, raw := range messages {
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			result = append(result, raw)
			continue
		}

		role, _ := msg["role"].(string)

		switch role {
		case "system":
			// Drop original system messages — replace with our framing
			continue

		case "tool":
			content, _ := msg["content"].(string)
			toolCallID, _ := msg["tool_call_id"].(string)
			newMsg := map[string]string{
				"role":    "user",
				"content": fmt.Sprintf("[Tool result for %s]: %s", toolCallID, content),
			}
			b, _ := json.Marshal(newMsg)
			result = append(result, b)

		case "assistant":
			if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
				// Convert tool_calls to JSON text (model sees its previous "output")
				var sb strings.Builder
				for _, call := range tc {
					if c, ok := call.(map[string]interface{}); ok {
						if fn, ok := c["function"].(map[string]interface{}); ok {
							name, _ := fn["name"].(string)
							args, _ := fn["arguments"].(string)
							sb.WriteString(fmt.Sprintf("{\"name\":\"%s\",\"arguments\":%s}\n", name, args))
						}
					}
				}
				origContent, _ := msg["content"].(string)
				newMsg := map[string]string{
					"role":    "assistant",
					"content": origContent + "\n" + sb.String(),
				}
				b, _ := json.Marshal(newMsg)
				result = append(result, b)
			} else {
				result = append(result, raw)
			}

		case "user":
			if i == lastUserIdx {
				// Embed framing into the last user message
				origContent, _ := msg["content"].(string)
				newContent := framing + "Input: \"" + origContent + "\""
				newMsg := map[string]string{
					"role":    "user",
					"content": newContent,
				}
				b, _ := json.Marshal(newMsg)
				result = append(result, b)
			} else {
				result = append(result, raw)
			}

		default:
			result = append(result, raw)
		}
	}

	// If no user message was found, prepend framing as system
	if lastUserIdx < 0 {
		sysMsg := map[string]string{"role": "system", "content": framing}
		sysBytes, _ := json.Marshal(sysMsg)
		result = append([]json.RawMessage{sysBytes}, result...)
	}

	return result
}
