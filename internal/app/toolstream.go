// toolstream.go — streaming interceptor for <<<TOOL_CALL>>> blocks.
// Port of orig GLM-Free-API agentStreamInterceptor: rewrites assistant
// output containing <<<TOOL_CALL>>>{...}<<<END_TOOL_CALL>>> into OpenAI-style
// tool_calls deltas, streaming name/arguments incrementally.
package app

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
)

const agentToolCallStart = "<<<TOOL_CALL>>>"
const agentToolCallEnd = "<<<END_TOOL_CALL>>>"

// GLM corrupts the end marker now and then ("<<<END_TOOL_CALL   >>>"),
// so tolerate whitespace inside it.
var agentToolCallEndRe = regexp.MustCompile(`<<<END_TOOL_CALL\s*>>>`)

// repairJSONQuotes escapes stray double quotes and raw control characters
// inside JSON string values. Heuristic: while inside a string, a quote is a
// structural terminator only when the next non-space char is one of , } ] :;
// otherwise it's literal model content (typically unescaped Python
// f-strings). Raw newlines/tabs get escaped too. This is salvage — strict
// parsing always runs first.
func repairJSONQuotes(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 64)
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			sb.WriteByte(c)
			if c == '"' {
				inStr = true
			}
			continue
		}
		// Inside a string value
		switch c {
		case '\\':
			sb.WriteByte(c)
			if i+1 < len(s) {
				sb.WriteByte(s[i+1])
				i++
			}
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '"':
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j >= len(s) || s[j] == ',' || s[j] == '}' || s[j] == ']' || s[j] == ':' {
				sb.WriteByte(c)
				inStr = false
			} else {
				sb.WriteString(`\"`)
			}
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// parseToolCallRegion parses one block body, falling back to the quote
// repair for GLM's unescaped-quote output.
func parseToolCallRegion(region string) map[string]interface{} {
	var parsed map[string]interface{}
	if json.Unmarshal([]byte(region), &parsed) == nil {
		return parsed
	}
	repaired := repairJSONQuotes(region)
	if repaired != region && json.Unmarshal([]byte(repaired), &parsed) == nil {
		log.Printf("[Tools] repaired malformed tool-call JSON (%d chars)", len(region))
		return parsed
	}
	return nil
}

// toolStreamInterceptor rewrites assistant output containing
// <<<TOOL_CALL>>>{...}<<<END_TOOL_CALL>>> blocks into OpenAI-style
// tool_calls deltas. Non-tool-call text is passed through verbatim.
type toolStreamInterceptor struct {
	buf       strings.Builder
	flushed   int  // offset into buf that has been processed
	emitting  bool // currently inside a tool-call block
	callIndex int

	// Streaming tool-call state (incremental args streaming)
	tcNameFound    bool
	tcName         string
	tcId           string
	tcArgsFound    bool // found "arguments": and value start
	tcArgsPos      int  // absolute byte offset in buf where args value starts
	tcArgsStreamed int  // bytes of args value already streamed
	tcBraceDepth   int  // brace depth for tracking args object end
	tcInString     bool // inside a string in args
	tcEscapeNext   bool // next char is escaped in args
	tcArgsDone     bool // args object fully streamed
	tcFallback     bool // fallback to buffered mode
}

func newToolStreamInterceptor() *toolStreamInterceptor {
	return &toolStreamInterceptor{callIndex: -1}
}

// resetToolCallState clears streaming tool-call state for the next call.
func (a *toolStreamInterceptor) resetToolCallState() {
	a.tcNameFound = false
	a.tcName = ""
	a.tcId = ""
	a.tcArgsFound = false
	a.tcArgsPos = 0
	a.tcArgsStreamed = 0
	a.tcBraceDepth = 0
	a.tcInString = false
	a.tcEscapeNext = false
	a.tcArgsDone = false
	a.tcFallback = false
}

// tryExtractName extracts the "name" field value from partial JSON.
// Returns the name and byte offset after closing quote, or "" and -1.
// Only searches before "arguments" key to avoid matching nested keys.
func tryExtractName(text string) (string, int) {
	searchEnd := len(text)
	if argsIdx := strings.Index(text, `"arguments"`); argsIdx >= 0 {
		searchEnd = argsIdx
	}
	keyIdx := strings.Index(text[:searchEnd], `"name"`)
	if keyIdx < 0 {
		return "", -1
	}
	pos := keyIdx + len(`"name"`)
	for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t' || text[pos] == '\n' || text[pos] == '\r') {
		pos++
	}
	if pos >= len(text) || text[pos] != ':' {
		return "", -1
	}
	pos++
	for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t' || text[pos] == '\n' || text[pos] == '\r') {
		pos++
	}
	if pos >= len(text) || text[pos] != '"' {
		return "", -1
	}
	pos++
	nameStart := pos
	for pos < len(text) {
		if text[pos] == '"' && (pos == 0 || text[pos-1] != '\\') {
			return text[nameStart:pos], pos + 1
		}
		pos++
	}
	return "", -1
}

// findArgsStart finds the start position of the "arguments" value in partial JSON.
// Only returns a position if the value starts with '{' (object arguments).
// Returns -1 if not found or not enough data yet.
func findArgsStart(text string) int {
	keyIdx := strings.Index(text, `"arguments"`)
	if keyIdx < 0 {
		return -1
	}
	pos := keyIdx + len(`"arguments"`)
	for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t' || text[pos] == '\n' || text[pos] == '\r') {
		pos++
	}
	if pos >= len(text) || text[pos] != ':' {
		return -1
	}
	pos++
	for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t' || text[pos] == '\n' || text[pos] == '\r') {
		pos++
	}
	if pos >= len(text) {
		return -1
	}
	if text[pos] != '{' {
		return -1 // non-object args -> use fallback
	}
	return pos
}

// feed accepts a new chunk of assistant text and returns:
//   - contentDelta: text to emit as a content delta (may be "")
//   - toolCalls: parsed tool call deltas to emit (may be nil)
//   - finishToolCalls: true if a complete tool call was just emitted
func (a *toolStreamInterceptor) feed(chunk string) (contentDelta string, toolCalls []map[string]interface{}, finishToolCalls bool) {
	a.buf.WriteString(chunk)
	data := a.buf.String()

	for {
		if a.emitting {
			rawData := data[a.flushed:]
			endLoc := agentToolCallEndRe.FindStringIndex(rawData)

			var complete bool
			var jsonEnd, endLen int
			if endLoc != nil {
				complete = true
				jsonEnd = endLoc[0]
				endLen = endLoc[1] - endLoc[0]
			} else {
				jsonEnd = len(rawData)
			}

			jsonText := rawData[:jsonEnd]

			// ── Fallback mode: buffer everything, parse at end ──
			if a.tcFallback {
				if !complete {
					return
				}
				jsonRegion := strings.TrimSpace(jsonText)
				jsonRegion = strings.TrimPrefix(jsonRegion, "```json")
				jsonRegion = strings.TrimPrefix(jsonRegion, "```")
				jsonRegion = strings.TrimSuffix(jsonRegion, "```")
				jsonRegion = strings.TrimSpace(jsonRegion)
				if parsed := parseToolCallRegion(jsonRegion); parsed != nil {
					name, _ := parsed["name"].(string)
					args := parsed["arguments"]
					if args == nil {
						args = map[string]interface{}{}
					}
					argsJSON, _ := json.Marshal(args)
					a.callIndex++
					toolCalls = append(toolCalls, map[string]interface{}{
						"index": a.callIndex,
						"id":    fmt.Sprintf("call_%s_%d", generateID()[:8], a.callIndex),
						"type":  "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": string(argsJSON),
						},
					})
					finishToolCalls = true
				}
				a.resetToolCallState()
				a.emitting = false
				a.flushed += jsonEnd + endLen
				for a.flushed < len(data) && (data[a.flushed] == '\n' || data[a.flushed] == '\r') {
					a.flushed++
				}
				continue
			}

			// ── Streaming mode ──

			// Phase 1: Extract and emit name header
			if !a.tcNameFound {
				name, _ := tryExtractName(jsonText)
				if name != "" {
					a.tcName = name
					a.tcNameFound = true
					a.callIndex++
					a.tcId = fmt.Sprintf("call_%s_%d", generateID()[:8], a.callIndex)
					toolCalls = append(toolCalls, map[string]interface{}{
						"index": a.callIndex,
						"id":    a.tcId,
						"type":  "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": "",
						},
					})
				} else if !complete {
					return
				}
			}

			// Phase 2: Find arguments value start
			if a.tcNameFound && !a.tcArgsFound && !a.tcArgsDone {
				argsPos := findArgsStart(jsonText)
				if argsPos >= 0 {
					a.tcArgsFound = true
					a.tcArgsPos = a.flushed + argsPos
					a.tcArgsStreamed = 0
					a.tcBraceDepth = 0
					a.tcInString = false
					a.tcEscapeNext = false
				} else if !complete {
					return
				} else {
					// Complete but no object args found — use fallback
					a.tcFallback = true
					continue
				}
			}

			// Phase 3: Stream arguments bytes incrementally
			if a.tcArgsFound && !a.tcArgsDone {
				var streamEnd int
				if complete {
					streamEnd = a.flushed + jsonEnd
				} else {
					streamEnd = len(data)
				}

				argsText := data[a.tcArgsPos:streamEnd]
				var argsDelta strings.Builder
				i := a.tcArgsStreamed
				for i < len(argsText) {
					c := argsText[i]
					if a.tcEscapeNext {
						a.tcEscapeNext = false
						argsDelta.WriteByte(c)
						i++
						continue
					}
					if c == '\\' {
						a.tcEscapeNext = true
						argsDelta.WriteByte(c)
						i++
						continue
					}
					if c == '"' {
						a.tcInString = !a.tcInString
						argsDelta.WriteByte(c)
						i++
						continue
					}
					if a.tcInString {
						argsDelta.WriteByte(c)
						i++
						continue
					}
					if c == '{' {
						a.tcBraceDepth++
					} else if c == '}' {
						a.tcBraceDepth--
						if a.tcBraceDepth == 0 {
							argsDelta.WriteByte(c)
							i++
							a.tcArgsDone = true
							break
						}
					}
					argsDelta.WriteByte(c)
					i++
				}
				a.tcArgsStreamed = i

				if argsDelta.Len() > 0 {
					toolCalls = append(toolCalls, map[string]interface{}{
						"index": a.callIndex,
						"function": map[string]interface{}{
							"arguments": argsDelta.String(),
						},
					})
				}
			}

			// Phase 4: Finalize on completion
			if complete {
				if !a.tcNameFound {
					// Name never extracted — try fallback parse
					a.tcFallback = true
					continue
				}
				a.resetToolCallState()
				a.emitting = false
				a.flushed += jsonEnd + endLen
				for a.flushed < len(data) && (data[a.flushed] == '\n' || data[a.flushed] == '\r') {
					a.flushed++
				}
				finishToolCalls = true
				continue
			}

			return
		}

		// Not emitting — look for start marker
		relIdx := strings.Index(data[a.flushed:], agentToolCallStart)
		if relIdx < 0 {
			// No start marker. Emit everything except a tail that could
			// be a partial marker (len-1 chars held back).
			safe := len(data) - a.flushed
			tail := len(agentToolCallStart) - 1
			if safe > tail {
				emit := safe - tail
				contentDelta += data[a.flushed : a.flushed+emit]
				a.flushed += emit
			}
			return
		}
		// Emit text before the start marker as content
		if relIdx > 0 {
			contentDelta += data[a.flushed : a.flushed+relIdx]
			a.flushed += relIdx
		}
		// Advance past the start marker
		a.flushed += len(agentToolCallStart)
		a.emitting = true
		a.resetToolCallState()
		// Skip trailing newline after start marker
		for a.flushed < len(data) && (data[a.flushed] == '\n' || data[a.flushed] == '\r') {
			a.flushed++
		}
	}
}

// flushFinal emits any remaining buffered content (called at stream end).
// Returns "" if we were mid-tool-call (incomplete — discarded).
func (a *toolStreamInterceptor) flushFinal() string {
	if a.emitting {
		return ""
	}
	data := a.buf.String()
	if a.flushed >= len(data) {
		return ""
	}
	rem := data[a.flushed:]
	a.flushed = len(data)
	return rem
}

// extractAgentToolCalls parses all <<<TOOL_CALL>>>{...}<<<END_TOOL_CALL>>>
// blocks from text and returns OpenAI-style tool_calls entries.
func extractAgentToolCalls(text string) []map[string]interface{} {
	var out []map[string]interface{}
	idx := 0
	for {
		start := strings.Index(text[idx:], agentToolCallStart)
		if start < 0 {
			break
		}
		afterStart := idx + start + len(agentToolCallStart)
		loc := agentToolCallEndRe.FindStringIndex(text[afterStart:])
		if loc == nil {
			break
		}
		jsonRegion := strings.TrimSpace(text[afterStart : afterStart+loc[0]])
		jsonRegion = strings.TrimPrefix(jsonRegion, "```json")
		jsonRegion = strings.TrimPrefix(jsonRegion, "```")
		jsonRegion = strings.TrimSuffix(jsonRegion, "```")
		jsonRegion = strings.TrimSpace(jsonRegion)
		if parsed := parseToolCallRegion(jsonRegion); parsed != nil {
			name, _ := parsed["name"].(string)
			args := parsed["arguments"]
			if args == nil {
				args = map[string]interface{}{}
			}
			argsJSON, _ := json.Marshal(args)
			out = append(out, map[string]interface{}{
				"id":   "call_" + generateID()[:8],
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
		}
		idx = afterStart + loc[1]
	}
	return out
}

// stripAgentToolCallBlocks removes all tool-call blocks from text and
// returns the residual content (trimmed).
func stripAgentToolCallBlocks(text string) string {
	var sb strings.Builder
	idx := 0
	for {
		start := strings.Index(text[idx:], agentToolCallStart)
		if start < 0 {
			sb.WriteString(text[idx:])
			break
		}
		sb.WriteString(text[idx : idx+start])
		afterStart := idx + start + len(agentToolCallStart)
		loc := agentToolCallEndRe.FindStringIndex(text[afterStart:])
		if loc == nil {
			break
		}
		idx = afterStart + loc[1]
		if idx < len(text) && text[idx] == '\n' {
			idx++
		}
	}
	return strings.TrimSpace(sb.String())
}
