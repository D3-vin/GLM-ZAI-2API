// features.go — per-model feature resolution + /features handlers.
// Port of orig GLM-Free-API model feature state. persistHistory is a
// bridge-local toggle (not a Z.AI feature) kept server-wide for go-ver's
// conversation history.
package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// ModelFeatureState tracks per-model feature configuration.
// IncludeAll: when true, ALL server capabilities are sent to /completions.
// Overrides: user-supplied per-model feature overrides (snake_case keys).
type ModelFeatureState struct {
	IncludeAll bool
	Overrides  map[string]interface{}
}

var (
	modelFeatureStates   = make(map[string]*ModelFeatureState)
	modelFeatureStatesMu sync.Mutex
)

// persistHistory is bridge-local (not sent to Z.AI) — gates conversation
// history persistence in the ConversationStore.
var persistHistory atomic.Bool

func persistHistoryEnabled() bool { return persistHistory.Load() }

// normalizeFeatureKey converts a camelCase key to snake_case.
func normalizeFeatureKey(k string) string {
	var sb strings.Builder
	for i, r := range k {
		if i > 0 && r >= 'A' && r <= 'Z' {
			sb.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			sb.WriteRune(r + 32)
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// getModelFeatureState returns the per-model state, creating it if necessary.
// Caller must NOT hold modelFeatureStatesMu.
func getModelFeatureState(modelID string) *ModelFeatureState {
	modelFeatureStatesMu.Lock()
	defer modelFeatureStatesMu.Unlock()
	if st, ok := modelFeatureStates[modelID]; ok {
		return st
	}
	st := &ModelFeatureState{
		IncludeAll: false,
		Overrides:  make(map[string]interface{}),
	}
	modelFeatureStates[modelID] = st
	return st
}

// resolveFeaturesForModel computes the final feature map for /completions.
//
// Rules:
//   - By default, auto_web_search and web_search are NOT included.
//   - enable_thinking defaults to true for all models.
//   - 'think' is never included — only enable_thinking reaches the request.
//   - IncludeAll: include ALL server capabilities (except reasoning_effort).
//   - User overrides always take precedence.
//   - image_generation is ALWAYS forced to false.
func (s *Server) resolveFeaturesForModel(modelID string) map[string]interface{} {
	caps := s.getModelCapabilities(modelID)
	modelFeatureStatesMu.Lock()
	state, ok := modelFeatureStates[modelID]
	modelFeatureStatesMu.Unlock()
	if !ok {
		state = &ModelFeatureState{
			IncludeAll: false,
			Overrides:  make(map[string]interface{}),
		}
	}
	return resolveFeaturesWithState(caps, state)
}

func resolveFeaturesWithState(caps map[string]interface{}, state *ModelFeatureState) map[string]interface{} {
	result := make(map[string]interface{})

	if state.IncludeAll {
		// Include ALL server capabilities except reasoning_effort
		// (it's a boolean support flag, not an actual feature value)
		for k, v := range caps {
			if k == "reasoning_effort" {
				continue
			}
			result[k] = v
		}
	}

	// Apply user overrides (per-model stored overrides take precedence)
	for k, v := range state.Overrides {
		result[k] = v
	}

	// Defensive: reasoning_effort is never a stored feature — strip stale values.
	delete(result, "reasoning_effort")

	// enable_thinking defaults to true unless explicitly overridden
	if _, ok := result["enable_thinking"]; !ok {
		result["enable_thinking"] = true
	}

	// Remove 'think' entirely — only enable_thinking reaches the request
	delete(result, "think")

	// ALWAYS exclude image_generation
	result["image_generation"] = false

	return result
}

// handleFeatures dispatches GET/POST /features (per-model).
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleFeaturesGet(w, r)
	case http.MethodPost:
		s.handleFeaturesPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleFeaturesGet(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model != "" {
		resolved := s.resolveFeaturesForModel(model)
		state := getModelFeatureState(model)
		caps := s.getModelCapabilities(model)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":        model,
			"features":     resolved,
			"includeAll":   state.IncludeAll,
			"overrides":    state.Overrides,
			"capabilities": caps,
		})
		return
	}
	// No model specified — return all per-model states
	modelFeatureStatesMu.Lock()
	states := make(map[string]interface{})
	for k, v := range modelFeatureStates {
		states[k] = map[string]interface{}{
			"includeAll": v.IncludeAll,
			"overrides":  v.Overrides,
		}
	}
	modelFeatureStatesMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"states":         states,
		"persistHistory": persistHistoryEnabled(),
	})
}

func (s *Server) handleFeaturesPost(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Failed to read body"})
		return
	}

	// Parse as raw map to capture arbitrary capability keys
	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid JSON"})
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "model is required"})
		return
	}

	includeAllHeader := strings.EqualFold(r.Header.Get("Include-All-Features"), "true")

	modelFeatureStatesMu.Lock()
	state, ok := modelFeatureStates[model]
	if !ok {
		state = &ModelFeatureState{
			IncludeAll: false,
			Overrides:  make(map[string]interface{}),
		}
		modelFeatureStates[model] = state
	}

	if includeAllHeader {
		state.IncludeAll = true
	}

	// Process user overrides — any key except "model" is a feature override.
	for k, v := range body {
		if k == "model" {
			continue
		}

		// persistHistory — bridge-local, not a Z.AI feature
		if k == "persistHistory" || k == "persist_history" {
			if b, ok := v.(bool); ok {
				persistHistory.Store(b)
			}
			continue
		}

		// reasoning: true/false -> enable_thinking
		if k == "reasoning" {
			if b, ok := v.(bool); ok {
				state.Overrides["enable_thinking"] = b
			}
			continue
		}

		// "thinking": {"type":"enabled"|"disabled"} or bool -> enable_thinking
		if k == "thinking" {
			if b, ok := v.(bool); ok {
				state.Overrides["enable_thinking"] = b
				continue
			}
			if m, ok := v.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok {
					state.Overrides["enable_thinking"] = (t == "enabled")
				}
			}
			continue
		}

		snakeKey := normalizeFeatureKey(k)
		// image_generation overrides are ignored — always forced false
		if snakeKey == "image_generation" {
			continue
		}
		// 'think' is not accepted — use enable_thinking, reasoning, or thinking
		if snakeKey == "think" {
			continue
		}
		// reasoning_effort is per-request only, never stored
		if snakeKey == "reasoning_effort" {
			continue
		}
		state.Overrides[snakeKey] = v
		// Z.AI expects both search flags together
		if snakeKey == "web_search" {
			state.Overrides["auto_web_search"] = v
		}
	}

	caps := s.getModelCapabilities(model)
	resolved := resolveFeaturesWithState(caps, state)
	includeAll := state.IncludeAll
	overrides := make(map[string]interface{})
	for k, v := range state.Overrides {
		overrides[k] = v
	}
	modelFeatureStatesMu.Unlock()

	log.Printf("[Features] model=%s includeAll=%v overrides=%+v resolved=%+v",
		model, includeAll, overrides, resolved)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"model":          model,
		"includeAll":     includeAll,
		"overrides":      overrides,
		"features":       resolved,
		"persistHistory": persistHistoryEnabled(),
	})
}
