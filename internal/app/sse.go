// sse.go — stateful Z.AI SSE parser.
// Port of orig GLM-Free-API streamSSEResponse: edit_content+edit_index
// replacement, <details> reasoning split, inline errors, delta diffs.
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// utf16IndexToByteIndex converts a UTF-16 code-unit offset — the indexing
// semantics of JavaScript strings used by the Z.AI web frontend
// (content.substring(0, edit_index)) — into a byte offset within s.
// Clamps at the end of s and never returns an offset inside a multi-byte
// rune: an index landing between the units of a surrogate pair clamps to
// the start of that rune. Ported from the upstream garble fix (#23).
func utf16IndexToByteIndex(s string, utf16Idx int) int {
	if utf16Idx <= 0 {
		return 0
	}
	byteIdx, units := 0, 0
	for byteIdx < len(s) {
		if units == utf16Idx {
			return byteIdx
		}
		r, size := utf8.DecodeRuneInString(s[byteIdx:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1 // invalid byte: JS would also see one unit here
		}
		if units+ru > utf16Idx {
			return byteIdx // inside a surrogate pair — clamp to rune start
		}
		units += ru
		byteIdx += size
	}
	return len(s)
}

// sseParser accumulates Z.AI SSE chunks into a full text and emits
// content/reasoning deltas relative to what has already been sent.
type sseParser struct {
	fullText strings.Builder
	// prefix-aware delta emitters (issue #26): clientView re-syncs to the
	// authoritative target on every emit, so rewrites/shrinks never cause
	// growing-prefix re-emissions.
	contentEmitter   sseEmitter
	reasoningEmitter sseEmitter
	logLevel         string
}

func newSSEParser(logLevel string) *sseParser {
	return &sseParser{logLevel: logLevel}
}

// split divides accumulated raw text into (content, reasoning) by the
// <details ...>...</details> marker (handles an unclosed tag mid-stream).
func (p *sseParser) split() (string, string) {
	raw := p.fullText.String()
	idx := strings.Index(raw, "<details")
	if idx < 0 {
		return raw, ""
	}
	tagEnd := strings.Index(raw[idx:], ">")
	if tagEnd < 0 {
		return raw, "" // tag not complete yet
	}
	afterTag := raw[idx+tagEnd+1:]
	if closeIdx := strings.Index(afterTag, "</details>"); closeIdx >= 0 {
		reasoning := stripDetailsTags(afterTag[:closeIdx])
		content := raw[:idx] + afterTag[closeIdx+len("</details>"):]
		return content, reasoning
	}
	// <details> opened but not yet closed
	return raw[:idx], stripDetailsTags(afterTag)
}

// content returns the accumulated content text (reasoning excluded).
// TrimSpace here is safe: this is the FINAL authoritative value, not a
// streaming snapshot.
func (p *sseParser) content() string {
	c, _ := p.split()
	return strings.TrimSpace(c)
}

// contentLen returns the byte length of the accumulated content.
func (p *sseParser) contentLen() int {
	return len(p.content())
}

// reasoning returns the accumulated reasoning text (<details> blocks).
func (p *sseParser) reasoning() string {
	_, r := p.split()
	return strings.TrimSpace(r)
}

// stripDetailsTags removes <details ...>/</details> and <summary>/</summary>
// wrappers plus leading "> " markdown-quote prefixes from each line.
// NOTE: no TrimSpace here — the streaming emitter requires the stripped
// snapshot sequence to grow monotonically; trailing-whitespace trimming
// oscillates the target by ±1 char (holdback + re-sync then duplicate
// chars). Final consumers (content()/reasoning()) trim instead.
func stripDetailsTags(s string) string {
	if idx := strings.Index(s, "<details"); idx >= 0 {
		if end := strings.Index(s[idx:], ">"); end >= 0 {
			s = s[:idx] + s[idx+end+1:]
		}
	}
	if idx := strings.Index(s, "<summary"); idx >= 0 {
		if end := strings.Index(s[idx:], ">"); end >= 0 {
			s = s[:idx] + s[idx+end+1:]
		}
	}
	s = strings.ReplaceAll(s, "</details>", "")
	s = strings.ReplaceAll(s, "</summary>", "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "> ")
	}
	return strings.Join(lines, "\n")
}

// applyEdit applies an edit_content chunk. Semantics mirror the official
// Z.AI frontend: content = content.substring(0, edit_index) + edit_content,
// where edit_index counts UTF-16 code units. editIndex < 0 (field absent in
// the event) keeps the bridge's legacy append behavior — deliberately
// different from the frontend's default-0 full replacement to avoid a
// simultaneous behavioral flip; revisit if Z.AI is observed omitting the
// field. Indexes beyond the current length truncate at len(s) — matching
// substring(), i.e. plain concatenation, not space padding.
func (p *sseParser) applyEdit(ec string, editIndex int) {
	current := p.fullText.String()
	if editIndex < 0 {
		current += ec
	} else {
		byteIdx := utf16IndexToByteIndex(current, editIndex)
		current = current[:byteIdx] + ec
	}
	p.fullText.Reset()
	p.fullText.WriteString(current)
}

// holdbackRunes — how many trailing runes stay unemitted while a stream is
// live, so trailing edit_content backtracks (Z.AI rewrites the tail) are
// absorbed invisibly instead of leaking then vanishing on the client.
// Mirrors upstream STREAM_HOLDBACK default.
var holdbackRunes = 24

// holdBackTail trims up to n runes from the end of s (rune-safe).
func holdBackTail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i, count := len(s), 0
	for i > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		count++
	}
	return s[:i]
}

// holdBackPartialDetailsTag trims a trailing fragment that could still be
// the beginning of a <details> tag whose completion has not arrived yet,
// so a tag streamed character by character never leaks to the client.
// A COMPLETE "</details>" literal is kept (legitimate text); a complete
// "<details" is held (waiting for its ">" to decide whether it is a tag).
func holdBackPartialDetailsTag(s string) string {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s
	}
	suffix := s[i:]
	// A partial OPEN tag is held regardless (its ">" decides nothing yet);
	// a partial CLOSE tag is held too; complete literals pass through.
	for _, open := range []string{"<details", "<summary"} {
		if len(suffix) <= len(open) && strings.HasPrefix(open, suffix) {
			return s[:i]
		}
	}
	for _, close := range []string{"</details>", "</summary>"} {
		if len(suffix) < len(close) && strings.HasPrefix(close, suffix) {
			return s[:i]
		}
	}
	return s
}

// holdBackPartialQuoteMarker trims a trailing ">" that forms the entire
// last line of s. Lines inside a <details> reasoning body are markdown-
// quoted ("> ..."), and while the body streams in one character at a
// time a new line's quote marker is transiently present as a bare ">"
// — which stripDetailsTags cannot strip yet (TrimPrefix needs the
// space) but strips one character later when the "> " completes.
// Forwarding that transient ">" makes the stripped-reasoning snapshot
// sequence non-monotonic: the client keeps the stray ">" forever.
// Holding the marker back keeps the snapshot sequence monotonic; the
// final flush releases it if the text really ends there.
// (Port of the upstream issue #26 fix.)
func holdBackPartialQuoteMarker(s string) string {
	if !strings.HasSuffix(s, ">") {
		return s
	}
	body := s[:len(s)-1]
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	return s
}

// sseEmitter is a prefix-aware delta emitter (upstream issue #26 fix).
// delta(target) returns what the client has not seen yet and re-syncs the
// tracked view to target — after a rewrite shrinks the snapshot, the old
// offset model would rewind and later RE-EMIT everything after the rewind
// point (growing-prefix duplication on the client). With re-sync semantics
// the emitter only ever forwards the common-prefix remainder; a shrunken
// snapshot simply yields an empty delta (the small holdback window absorbs
// the realistic rewind cases before they ever reach the client).
type sseEmitter struct {
	clientView string
}

// commonPrefixLen returns the byte length of the longest common prefix of
// a and b. Always on a rune boundary.
func commonPrefixLen(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[i:])
		rb, _ := utf8.DecodeRuneInString(b[i:])
		if ra != rb {
			break
		}
		i += sa
	}
	return i
}

func (e *sseEmitter) delta(target string) string {
	// Monotonic view: when the (holdback-trimmed) target is shorter than
	// what the client already received, emit nothing and KEEP the view —
	// rewinding it here made the final flush re-emit everything after the
	// rewind point (the very duplication issue #26 is about).
	if len(target) < len(e.clientView) {
		return ""
	}
	cp := commonPrefixLen(e.clientView, target)
	delta := target[cp:]
	e.clientView = target
	return delta
}

// flush emits content/reasoning deltas relative to already-sent offsets.
// While the stream is live (final=false) a small tail is held back so
// trailing edit_content backtracks are absorbed invisibly, partial
// <details>/</details> tag fragments never leak, and transient bare ">"
// quote markers (reasoning) don't reach the client. FlushFinal releases
// everything.
func (p *sseParser) flush(onContent, onReasoning func(string)) {
	p.emit(onContent, onReasoning, false)
}

// FlushFinal releases all held-back tail content at end of stream.
func (p *sseParser) FlushFinal(onContent, onReasoning func(string)) {
	p.emit(onContent, onReasoning, true)
}

func (p *sseParser) emit(onContent, onReasoning func(string), final bool) {
	content, reasoning := p.split()

	if onReasoning != nil {
		if !final {
			reasoning = holdBackTail(reasoning, holdbackRunes)
			reasoning = holdBackPartialDetailsTag(reasoning)
			reasoning = holdBackPartialQuoteMarker(reasoning)
		}
		if delta := p.reasoningEmitter.delta(reasoning); delta != "" {
			onReasoning(delta)
		}
	}
	if onContent != nil {
		if !final {
			content = holdBackTail(content, holdbackRunes)
			content = holdBackPartialDetailsTag(content)
		}
		if delta := p.contentEmitter.delta(content); delta != "" {
			onContent(delta)
		}
	}
}

// extractZAIError pulls an inline error detail from an SSE JSON object
// (HTTP 200 with an error in the body).
func extractZAIError(j map[string]interface{}) string {
	if data, ok := j["data"].(map[string]interface{}); ok {
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			if detail, _ := errObj["detail"].(string); detail != "" {
				return detail
			}
			if msg, _ := errObj["message"].(string); msg != "" {
				return msg
			}
		}
	}
	if errObj, ok := j["error"].(map[string]interface{}); ok {
		if detail, _ := errObj["detail"].(string); detail != "" {
			return detail
		}
		if msg, _ := errObj["message"].(string); msg != "" {
			return msg
		}
	}
	return ""
}

// feedLine parses one SSE line, accumulates content and emits deltas.
// Returns io.EOF on [DONE] / phase done, or an error when Z.AI reports
// an inline error.
func (p *sseParser) feedLine(line string, onContent, onReasoning func(string)) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "data: ") {
		return nil
	}
	dataStr := trimmed[6:]
	if dataStr == "[DONE]" {
		return io.EOF
	}

	var j map[string]interface{}
	if err := json.Unmarshal([]byte(dataStr), &j); err != nil {
		if p.logLevel == "debug" {
			log.Printf("[DEBUG] Z.AI failed to parse SSE: %s", dataStr)
		}
		return nil
	}

	// Inline errors (HTTP 200 with error in body)
	if detail := extractZAIError(j); detail != "" {
		if p.logLevel == "debug" {
			log.Printf("[DEBUG] Z.AI inline SSE error: %s", detail)
		}
		return fmt.Errorf("Z.AI: %s", detail)
	}

	if data, ok := j["data"].(map[string]interface{}); ok {
		if phase, ok := data["phase"].(string); ok && phase == "done" {
			return io.EOF
		}

		if ec, ok := data["edit_content"].(string); ok && ec != "" {
			// edit_content = FULL replacement starting at edit_index
			editIndex := -1
			if ei, ok := data["edit_index"].(float64); ok {
				editIndex = int(ei)
			}
			p.applyEdit(ec, editIndex)
		} else if dc, ok := data["delta_content"].(string); ok && dc != "" {
			p.fullText.WriteString(dc)
		} else if tc, ok := data["content"].(string); ok && tc != "" {
			p.fullText.WriteString(tc)
		} else if choices, ok := j["choices"].([]interface{}); ok && len(choices) > 0 {
			// OpenAI-style fallback
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok && content != "" {
						p.fullText.WriteString(content)
					}
				}
			}
		}
	}

	p.flush(onContent, onReasoning)
	return nil
}
