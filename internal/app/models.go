// models.go — live model list from Z.AI /api/models (cached 5 min).
// Port of orig GLM-Free-API fetchModelsFromZAI.
package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

type ModelInfo struct {
	ID           string
	Name         string
	Description  string
	Capabilities map[string]interface{}
}

var (
	modelsCache     []ModelInfo
	modelsCacheTime time.Time
	modelsCacheMu   sync.Mutex
)

const modelsCacheTTL = 5 * time.Minute

// After a failed live fetch (e.g. WAF 403) don't hammer Z.AI again for this
// long: capability lookups and dashboard polls would otherwise fire a fresh
// request on every call and spam the log. Var for test injection.
var (
	modelsFailureBackoff = time.Minute
	modelsLastFailure    time.Time
)

// fallbackModels is used when Z.AI is unreachable and the cache is empty.
// Snapshot of the live /api/models lineup (HAR 2026-08): GLM-5.1 was retired
// upstream and replaced by glm-5.3 — keep this list in sync with Z.AI.
//
// Capabilities here are limited to what feature dispatch actually consumes:
// reasoning_effort on the two flagships (HAR 2026-08 thinking_effort_level),
// so applyReasoningEffort keeps working even when the live list cannot be
// fetched — GET /api/models is WAF/cookie-gated against non-browser clients,
// so the live path fails closed far more often than not.
var fallbackModels = []ModelInfo{
	{ID: "x-preview-l", Name: "GLM-5.3-Flash", Description: "Lightweight flagship model, premium quality, instant response",
		Capabilities: map[string]interface{}{"reasoning_effort": true}},
	{ID: "glm-5.3", Name: "GLM-5.3", Description: "Flagship model, excels at coding and long-horizon tasks",
		Capabilities: map[string]interface{}{"reasoning_effort": true}},
	{ID: "glm-5.2", Name: "GLM-5.2", Description: "Previous flagship model"},
	{ID: "GLM-5-Turbo", Name: "GLM-5-Turbo", Description: "New model for chat, coding, and agentic tasks"},
	{ID: "GLM-5v-Turbo", Name: "GLM-5V-Turbo", Description: "Vision model with evolved intelligence"},
	{ID: "glm-4.7", Name: "GLM-4.7", Description: "Classic high-performance model"},
}

// fetchModelsFromZAI retrieves models from Z.AI /api/models,
// keeping only models down to glm-4.7 (inclusive; API returns newest-first).
// The cutoff is an intentional decision: glm-4.7 is the supported floor and
// legacy entries below it (glm-4.5, Z1, 4.1V, …) are dropped. To expose the
// full upstream list, remove the cutoff in the loop below.
func (s *Server) fetchModelsFromZAI() []ModelInfo {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	if len(modelsCache) > 0 && time.Since(modelsCacheTime) < modelsCacheTTL {
		return modelsCache
	}

	fail := func(msg string) []ModelInfo {
		modelsLastFailure = time.Now()
		if msg != "" {
			log.Printf("[Models] %s", msg)
		}
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	// Inside the failure backoff window: serve cache/fallback silently —
	// no request, no log line (the failure was already logged once).
	if !modelsLastFailure.IsZero() && time.Since(modelsLastFailure) < modelsFailureBackoff {
		return fail("")
	}

	s.zaiSession.mu.Lock()
	token := s.zaiSession.token
	s.zaiSession.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := zaiHTTP(nil)
	if err != nil {
		return fail("fetchModels client: " + err.Error())
	}
	req, err := fhttp.NewRequestWithContext(ctx, "GET", baseURL+"/api/models", nil)
	if err != nil {
		return fail("fetchModels request: " + err.Error())
	}
	// Mirror the browser's request shape (HAR 2026-08, entry #4): without the
	// Origin/Referer + Client Hints / Sec-Fetch headers the WAF answers
	// GET /api/models with 403 even when the session token itself is valid.
	// Version strings stay consistent with the chrome_146 TLS profile
	// (zaiUserAgent), matching initialize()'s header set in zai.go.
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("User-Agent", zaiUserAgent)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Google Chrome";v="146", "Not)A;Brand";v="24"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	resp, err := client.Do(req)
	if err != nil {
		return fail("fetchModels do: " + err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fail("fetchModels status: " + resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail("fetchModels read: " + err.Error())
	}

	var apiResp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Info struct {
				Name string `json:"name"`
				Meta struct {
					Description  string                 `json:"description"`
					Capabilities map[string]interface{} `json:"capabilities"`
				} `json:"meta"`
			} `json:"info"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &apiResp); err != nil {
		return fail("fetchModels parse: " + err.Error())
	}

	var filtered []ModelInfo
	for _, m := range apiResp.Data {
		filtered = append(filtered, ModelInfo{
			ID:           m.ID,
			Name:         m.Name,
			Description:  m.Info.Meta.Description,
			Capabilities: m.Info.Meta.Capabilities,
		})
		// glm-4.7 is the cutoff — stop here (inclusive)
		if m.ID == "glm-4.7" {
			break
		}
	}

	if len(filtered) > 0 {
		modelsCache = filtered
		modelsCacheTime = time.Now()
		log.Printf("[Models] fetched %d models from Z.AI", len(filtered))
	}

	if len(modelsCache) > 0 {
		return modelsCache
	}
	return fallbackModels
}

// startModelRefresher keeps the live model cache warm: one fetch shortly
// after startup, then a refresh every modelsCacheTTL. It feeds the same
// single cache used by fetchModelsFromZAI (no second source of truth) so the
// first client after an idle period never blocks on a cold fetch. Intended
// to run as a background goroutine launched from main().
func (s *Server) startModelRefresher() {
	time.Sleep(5 * time.Second)
	s.fetchModelsFromZAI()
	ticker := time.NewTicker(modelsCacheTTL)
	defer ticker.Stop()
	for range ticker.C {
		s.fetchModelsFromZAI()
	}
}

// findModelByID returns the model with a case-insensitive ID match
// (Z.AI mixes lower/upper case in IDs: glm-4.7 vs GLM-5-Turbo).
func findModelByID(models []ModelInfo, id string) *ModelInfo {
	for i := range models {
		if strings.EqualFold(models[i].ID, id) {
			return &models[i]
		}
	}
	return nil
}

// getModelCapabilities returns the raw capabilities map for a model.
func (s *Server) getModelCapabilities(modelID string) map[string]interface{} {
	if m := findModelByID(s.fetchModelsFromZAI(), modelID); m != nil {
		return m.Capabilities
	}
	return nil
}

// modelIDs lists known model IDs (live cache, or fallback when Z.AI is
// unreachable). Single source of truth for model-name checks — replaces
// the old hardcoded knownModels slice in main.go.
func modelIDs() []string {
	var s Server
	models := s.fetchModelsFromZAI()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// modelSupportsReasoningEffort returns true only when the model's capabilities
// explicitly contain "reasoning_effort": true.
func (s *Server) modelSupportsReasoningEffort(modelID string) bool {
	if modelID == "" {
		return false
	}
	caps := s.getModelCapabilities(modelID)
	if caps == nil {
		return false
	}
	v, ok := caps["reasoning_effort"].(bool)
	return ok && v
}

// isValidReasoningEffort validates the accepted reasoning_effort values.
// Z.AI exposes thinking_effort_level ["low","high","max"] (HAR 2026-08).
func isValidReasoningEffort(value string) bool {
	switch value {
	case "low", "high", "max":
		return true
	default:
		return false
	}
}
