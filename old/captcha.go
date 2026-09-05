// captcha.go — Aliyun Captcha V3 verification logic.
// Ported from .assets/token-helper/main.go, now called in-process
// (no named-pipe IPC needed since the whole server is Go).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Aliyun Captcha public keys (from Z.AI frontend — not secrets).
const (
	captchaAccessKey  = "LTAI5tSEBwYMwVKAQGpxmvTd"
	captchaSecretKey  = "YSKfst7GaVkXwZYvVihJsKF9r89koz"
	captchaSceneID    = "didk33e0"
	captchaMaxRetries = 2
)

var captchaHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
	Timeout: 30 * time.Second,
}

// ── URL encoding with custom safe table (zero allocations for safe chars) ──

const hexUpper = "0123456789ABCDEF"
const hexLower = "0123456789abcdef"

var baseSafeTable [256]bool

func init() {
	for i := 0; i < 256; i++ {
		c := byte(i)
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			baseSafeTable[i] = true
		}
	}
}

func urlEncode(s string, safe string) string {
	safeTable := baseSafeTable
	for i := 0; i < len(safe); i++ {
		safeTable[safe[i]] = true
	}
	var b strings.Builder
	b.Grow(len(s)*3 + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if safeTable[c] {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
	return b.String()
}

func fromHex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return 0
	}
}

// ── UUID v4 (manual hex encoding) ──

func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80

	var dst [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexLower[b[i]>>4]
		dst[j+1] = hexLower[b[i]&0xF]
		j += 2
	}
	return string(dst[:])
}

// ── Helpers ──

func getTimestampUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func currentTimeMillis() int64 {
	return time.Now().UnixMilli()
}

// jsonMarshal disables HTML escaping to match the original.
func jsonMarshal(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	return raw[:len(raw)-1], nil // trim trailing newline
}

// ── Aliyun signature: sorted params, HMAC-SHA1, base64 ──

func generateAliyunSignature(params map[string]string, secKey string) string {
	keys := make([]string, 0, len(params)+1)
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	canonical.Grow(512)
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(urlEncode(k, ""))
		canonical.WriteByte('=')
		canonical.WriteString(urlEncode(params[k], ""))
	}

	stringToSign := "POST&" + urlEncode("/", "") + "&" + urlEncode(canonical.String(), "")
	signingKey := secKey + "&"
	h := hmac.New(sha1.New, []byte(signingKey))
	h.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func buildQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(512)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(k, ""))
		b.WriteByte('=')
		b.WriteString(urlEncode(params[k], ""))
	}
	return b.String()
}

func captchaHTTPPost(url, body string, extraHeaders map[string]string) (string, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.ContentLength = int64(len(body))
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := captchaHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ── JSON struct types (field order matches original) ──

type initCaptchaResponse struct {
	CertifyID string `json:"CertifyId"`
}

type cvp struct {
	CertifyID   string `json:"certifyId"`
	Data        string `json:"data"`
	DeviceToken string `json:"deviceToken"`
	SceneID     string `json:"sceneId"`
}

type verifyCaptchaResponse struct {
	Success bool `json:"Success"`
	Result  struct {
		VerifyResult  bool   `json:"VerifyResult"`
		SecurityToken string `json:"securityToken"`
		CertifyID     string `json:"certifyId"`
	} `json:"Result"`
}

type finalPayload struct {
	CertifyID     string `json:"certifyId"`
	IsSign        bool   `json:"isSign"`
	SceneID       string `json:"sceneId"`
	SecurityToken string `json:"securityToken"`
}

type trackList struct {
	FI        string `json:"fi"`
	KS        string `json:"ks"`
	MC        string `json:"mc"`
	MP        string `json:"mp"`
	MU        string `json:"mu"`
	StartTime int64  `json:"startTime"`
	TC        string `json:"tc"`
	TE        string `json:"te"`
	TMV       string `json:"tmv"`
}

type track struct {
	TrackList      trackList `json:"TrackList"`
	TrackStartTime int64     `json:"TrackStartTime"`
	VerifyTime     int64     `json:"VerifyTime"`
	Arg            string    `json:"arg"`
}

// ── PART 1: InitCaptchaV3 ──

func initCaptcha() (string, error) {
	params := map[string]string{
		"AccessKeyId":      captchaAccessKey,
		"Action":           "InitCaptchaV3",
		"Format":           "JSON",
		"Language":         "en",
		"Mode":             "popup",
		"SceneId":          captchaSceneID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   generateUUID(),
		"SignatureVersion": "1.0",
		"Timestamp":        getTimestampUTC(),
		"UpLang":           "true",
		"Version":          "2023-03-05",
	}
	params["Signature"] = generateAliyunSignature(params, captchaSecretKey)

	body := buildQueryString(params)
	resp, err := captchaHTTPPost(
		"https://no8xfe.captcha-open-southeast.aliyuncs.com/", body, nil)
	if err != nil {
		return "", err
	}

	var result initCaptchaResponse
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return "", fmt.Errorf("parse InitCaptchaV3 response: %w", err)
	}
	return result.CertifyID, nil
}

// ── PART 2: generateArg — RC4-like stream cipher with custom KSA ──

var argPermTable = [64]int{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

const argConstant = "4xrihv8zb8tf1mfj"

func generateArg(certifyID string) string {
	encoded := urlEncode(certifyID, "")

	// URL-decode (identity for already-decoded strings, kept for faithfulness)
	o := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] == '%' && i+2 < len(encoded) {
			o = append(o, fromHex(encoded[i+1])<<4|fromHex(encoded[i+2]))
			i += 3
		} else {
			o = append(o, encoded[i])
			i++
		}
	}

	// KSA
	r := argPermTable // stack-allocated copy
	n := argConstant
	rlen := 64

	i, j := 0, 0
	for i < rlen {
		j = (((i + j + r[i] + r[j]) >> 1) + int(n[i%len(n)])) & (rlen - 1)
		if i != j {
			r[i], r[j] = r[j], r[i]
		}
		i++
	}

	// PRGA
	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for idx := 0; idx < len(o); idx++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[idx])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64.StdEncoding.EncodeToString(t)
}

// ── PART 4: ali_hash — custom hash with 16-byte state ──

func aliHash(inputStr, saltStr string) string {
	o := inputStr
	r := saltStr
	aLen := len(o)
	m := len(r)

	var e [16]int
	for i := 0; i < 16; i++ {
		e[i] = (i << 4) + (i % 16)
	}
	f := 16

	i, j := 0, 0
	for i < f {
		j = (((i + j + e[i] + e[j]) >> 1) + int(r[i%m])) & (f - 1)
		e[i], e[j] = e[j], e[i]
		i++
	}

	idx, p, q := 0, 0, 0
	for idx < aLen {
		q = ((p ^ q) + (e[p] ^ e[q])) & (f - 1)
		e[p], e[q] = e[q], e[p]
		C := int(o[idx])
		C = (C + p + q) ^ e[p] ^ e[q]
		C = C & 255
		e[p] = C
		p = (p + 1) & (f - 1)
		idx++
	}

	for step := 0; step < 2*f; step++ {
		pos := step % f
		if pos != 0 {
			e[pos] ^= e[pos-1]
		} else {
			e[0] ^= e[f-1]
		}
	}

	var result [32]byte
	for i, b := range e {
		result[i*2] = hexLower[(b>>4)&0xF]
		result[i*2+1] = hexLower[b&0xF]
	}
	return string(result[:])
}

// ── PART 7: encrypt — same RC4-like cipher, different key ──

const encryptKey = "3e627e1b4c63f913"

func encrypt(plaintext []byte) string {
	o := plaintext
	n := encryptKey
	r := argPermTable // stack-allocated copy
	rlen := 64

	oKsa, tKsa := 0, 0
	for oKsa < rlen {
		tKsa = (((oKsa + tKsa + r[oKsa] + r[tKsa]) >> 1) + int(n[oKsa%len(n)])) & (rlen - 1)
		if oKsa != tKsa {
			r[oKsa], r[tKsa] = r[tKsa], r[oKsa]
		}
		oKsa++
	}

	t := make([]byte, 0, len(o))
	e, a := 0, 0
	for nPrga := 0; nPrga < len(o); nPrga++ {
		a = ((e ^ a) + (r[e] ^ r[a])) & (rlen - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(o[nPrga])
		m = m + e + r[e] - a - r[a]
		m = m ^ (r[e] + r[a])
		m = m ^ r[(r[e]+r[a])&(rlen-1)]
		m = m & 255
		t = append(t, byte(m))
		e = (e + 1) & (rlen - 1)
	}
	return base64.StdEncoding.EncodeToString(t)
}

// ── zlib compress ──

func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data) + len(data)/2 + 128)
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// ── PART 8: VerifyCaptchaV3 ──

func verifyCaptcha(certifyID, dataValue, deviceToken string) (string, error) {
	cvpJSON, err := jsonMarshal(cvp{
		CertifyID:   certifyID,
		Data:        dataValue,
		DeviceToken: deviceToken,
		SceneID:     captchaSceneID,
	})
	if err != nil {
		return "", err
	}

	params := map[string]string{
		"AccessKeyId":        captchaAccessKey,
		"Action":             "VerifyCaptchaV3",
		"Format":             "JSON",
		"SignatureMethod":    "HMAC-SHA1",
		"SignatureVersion":   "1.0",
		"Timestamp":          getTimestampUTC(),
		"Version":            "2023-03-05",
		"SceneId":            captchaSceneID,
		"CertifyId":          certifyID,
		"CaptchaVerifyParam": string(cvpJSON),
		"SignatureNonce":     generateUUID(),
	}
	params["Signature"] = generateAliyunSignature(params, captchaSecretKey)

	body := buildQueryString(params)
	resp, err := captchaHTTPPost(
		"https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/",
		body, map[string]string{"Referer": ""})
	if err != nil {
		return "", err
	}

	var respJSON verifyCaptchaResponse
	if err := json.Unmarshal([]byte(resp), &respJSON); err != nil {
		return "", fmt.Errorf("parse VerifyCaptchaV3 response: %w", err)
	}

	if respJSON.Success && respJSON.Result.VerifyResult {
		st := respJSON.Result.SecurityToken
		ci := respJSON.Result.CertifyID
		if st != "" && ci != "" {
			fpJSON, err := jsonMarshal(finalPayload{
				CertifyID:     ci,
				IsSign:        true,
				SceneID:       captchaSceneID,
				SecurityToken: st,
			})
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(fpJSON), nil
		}
		captchaLog("VerifyCaptchaV3 succeeded but securityToken/certifyId empty for deviceToken=" + truncateToken(deviceToken))
	} else if respJSON.Success {
		captchaLog("deviceToken failed verification (VerifyResult=false): " + truncateToken(deviceToken))
	} else {
		captchaLog("VerifyCaptchaV3 request unsuccessful for deviceToken=" + truncateToken(deviceToken) + " response=" + resp)
	}
	return "", nil
}

// ── Compute final payload — tries tokens until success or exhausted ──

// computeCaptchaParam replaces the entire named-pipe flow.
// It tries up to captchaMaxRetries device tokens and returns the
// base64-encoded captcha_verify_param, or "" on failure.
func computeCaptchaParam(tokenStore *TokenStore) string {
	if tokenStore == nil {
		captchaLog("No token store available — tokens.sqlite missing")
		return ""
	}
	for attempt := 0; attempt < captchaMaxRetries; attempt++ {
		deviceToken, ok := tokenStore.getNext()
		if !ok {
			captchaLog(fmt.Sprintf("No device tokens remaining (attempt %d/%d)", attempt+1, captchaMaxRetries))
			return ""
		}
		captchaLog(fmt.Sprintf("Attempt %d/%d using deviceToken=%s", attempt+1, captchaMaxRetries, truncateToken(deviceToken)))

		payload, err := tryComputeCaptcha(tokenStore, deviceToken)
		if err != nil {
			captchaLog(fmt.Sprintf("Attempt %d failed for deviceToken=%s: %v", attempt+1, truncateToken(deviceToken), err))
			continue
		}
		if payload != "" {
			return payload
		}
		captchaLog("deviceToken=" + truncateToken(deviceToken) + " produced empty payload, retrying")
	}
	captchaLog(fmt.Sprintf("All %d token retries exhausted", captchaMaxRetries))
	return ""
}

func tryComputeCaptcha(tokenStore *TokenStore, deviceToken string) (string, error) {
	certifyID, err := initCaptcha()
	if err != nil {
		tokenStore.remove(deviceToken)
		return "", fmt.Errorf("initCaptcha: %w", err)
	}

	argValue := generateArg(certifyID)
	ct := currentTimeMillis()

	tr := track{
		TrackList: trackList{
			StartTime: ct,
		},
		TrackStartTime: ct,
		VerifyTime:     ct + 300,
		Arg:            argValue,
	}
	jsonBytes, err := jsonMarshal(tr)
	if err != nil {
		tokenStore.remove(deviceToken)
		return "", err
	}

	h := aliHash(string(jsonBytes), "0000")
	combined := h + string(jsonBytes)
	compressed := zlibCompress([]byte(combined))
	fb64 := base64.StdEncoding.EncodeToString(compressed)
	finalVal := encrypt([]byte(fb64))

	// Always remove token after use — prevents conflicts
	tokenStore.remove(deviceToken)

	payload, err := verifyCaptcha(certifyID, finalVal, deviceToken)
	if err != nil {
		return "", fmt.Errorf("verifyCaptcha: %w", err)
	}
	return payload, nil
}

// truncateToken returns first 10 + ... + last 10 chars for log readability.
func truncateToken(s string) string {
	if len(s) <= 23 {
		return s
	}
	return s[:10] + "..." + s[len(s)-10:]
}

// captchaLog is a thin wrapper — verbose flag gates output.
func captchaLog(msg string) {
	if captchaVerbose {
		logPrintf("[Captcha] %s", msg)
	}
}

var captchaVerbose bool
