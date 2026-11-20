package app

// preflight.go — resolve the captcha param BEFORE committing HTTP 200 on a
// streaming response, and hand the resolved param to the send.
//
// Why: handleChatStream used to write the 200 header first and call
// sendToZAI afterwards. Every captcha/route/Z.AI failure therefore arrived as
// "data: {error...}" glued to an already-successful SSE stream. An
// OpenAI-compatible client raises nothing on that shape: it sees zero content
// chunks plus finish_reason=stop and reports an EMPTY SUCCESSFUL answer. The
// caller (Hermes fallback chain among them) never retries and never alerts —
// the failure is invisible from the outside. A captcha outage surfaced in the
// client as "vars() argument must have __dict__ attribute", naming neither the
// proxy nor the captcha.
//
// The fix is ordering, not error handling: anything that can fail before the
// first token must fail before the headers go out. Resolving the param here
// also removes a double burn — the previous flow resolved captcha inside
// sendToZAI while a pre-flight probe would have burned a second single-use
// device token for the same request.

import "fmt"

// preflightCaptcha resolves a captcha param for this request and returns it.
// The caller passes it into sendOpts so sendToZAI does not resolve again.
//
// Failure modes map to 503 at the call site: an empty/absent tokens.sqlite,
// an exhausted pool, or a Z.AI session that cannot mint a param are all
// transient (tokens are minted in batches), and a 5xx makes the caller retry
// and the alerting fire. An empty 200 does neither.
func (s *Server) preflightCaptcha(model string, opts sendOpts) (string, error) {
	if s == nil || s.tokens == nil {
		return "", fmt.Errorf("captcha unavailable: token store not loaded (tokens.sqlite missing)")
	}
	if n := s.tokens.Count(); n == 0 {
		return "", fmt.Errorf("captcha unavailable: tokens.sqlite holds 0 device tokens — run token-collector")
	}
	param := computeCaptchaParam(s, s.tokens)
	if param == "" {
		return "", fmt.Errorf("captcha unavailable: pool exhausted or every device token was rejected — see [captcha] log lines")
	}
	return param, nil
}
