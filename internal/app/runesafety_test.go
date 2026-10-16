package app

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Regression tests for the UTF-8 rune-split corruption (2026-09-16).
//
// Both the tool-stream interceptor's held-back tail and the text-response
// chunker slice by BYTE offsets. When such a cut lands inside a multi-byte
// rune, json.Marshal renders every dangling byte as U+FFFD, so the client
// receives "��" where the model emitted "в" (or "����" for an emoji).
// These tests fail on the pre-fix code and pass after the rune-boundary fix.

// ---------------------------------------------------------------- interceptor

func runInterceptor(t *testing.T, text string, chunkSize int) (string, []string) {
	t.Helper()
	a := newToolStreamInterceptor()
	var deltas []string
	for i := 0; i < len(text); i += chunkSize {
		j := i + chunkSize
		if j > len(text) {
			j = len(text)
		}
		cd, _, _ := a.feed(text[i:j])
		if cd != "" {
			deltas = append(deltas, cd)
		}
	}
	if rem := a.flushFinal(); rem != "" {
		deltas = append(deltas, rem)
	}
	return strings.Join(deltas, ""), deltas
}

func TestInterceptorNeverSplitsRunes(t *testing.T) {
	prose := strings.Repeat("Проверяю все репозитории 🔬 и baseline watchdog'а — тест. ", 30)
	call := agentToolCallStart +
		`{"name":"terminal","arguments":{"command":"echo hi"}}` +
		"<<<END_TOOL_CALL>>>"
	text := prose + call

	for _, cs := range []int{1, 2, 3, 5, 7, 11, 64, 1024} {
		joined, deltas := runInterceptor(t, text, cs)
		for n, d := range deltas {
			if !utf8.ValidString(d) {
				t.Errorf("chunkSize=%d delta#%d is INVALID UTF-8: %q", cs, n, d)
				break
			}
		}
		if joined != prose {
			t.Errorf("chunkSize=%d: content mismatch\n got=%q\nwant=%q",
				cs, preview(joined), preview(prose))
		}
	}
}

// ------------------------------------------------------------------- chunker

func chunkLoop(content string, chunkSize int) []string {
	var out []string
	for i := 0; i < len(content); {
		end := runeChunkEnd(content, i, i+chunkSize)
		if end <= i { // never hand the caller an empty chunk
			end = len(content)
		}
		out = append(out, content[i:end])
		i = end
	}
	return out
}

func TestChunkingIsRuneSafe(t *testing.T) {
	base := strings.Repeat("Проверяю все репозитории 🔬 и baseline watchdog'а — тест. ", 12)
	sample := []rune(base)
	for _, cs := range []int{1, 2, 3, 4, 5, 7, 13, 64, 199, 2000} {
		for trim := 0; trim < 12; trim++ {
			content := string(sample[:len(sample)-trim])
			chunks := chunkLoop(content, cs)
			if got := strings.Join(chunks, ""); got != content {
				t.Fatalf("cs=%d trim=%d: reassembly mismatch", cs, trim)
			}
			for n, ch := range chunks {
				if !utf8.ValidString(ch) {
					t.Errorf("cs=%d trim=%d chunk#%d is INVALID UTF-8: %q", cs, trim, n, ch)
				}
			}
		}
	}
}

func TestRuneChunkEndNeverLandsMidRune(t *testing.T) {
	s := "абв🔬где" // 2-byte Cyrillic + 4-byte emoji
	for i := 0; i < len(s); i++ {
		for max := i; max <= len(s)+1; max++ {
			end := runeChunkEnd(s, i, max)
			if end < i || end > len(s) {
				t.Fatalf("i=%d max=%d end=%d out of range", i, max, end)
			}
			if end < len(s) && !utf8.RuneStart(s[end]) {
				t.Errorf("i=%d max=%d end=%d is not a rune start (byte %q)", i, max, end, s[end])
			}
		}
	}
}

func preview(s string) string {
	if len(s) > 90 {
		return s[:90] + "..."
	}
	return s
}
