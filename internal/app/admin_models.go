// admin_models.go — /admin/models: catalog view of live Z.AI models
// (IDs, descriptions, capabilities + per-capability stats). Built directly
// on fetchModelsFromZAI — the 5-minute cache in models.go remains the single
// source of truth, kept warm in the background by startModelRefresher.
package app

import (
	"encoding/json"
	"net/http"
)

// handleAdminModels GET /admin/models — model catalog with capability stats.
//
//	Response: {
//	  "count": 6, "cutoff": "glm-4.7",
//	  "models": [ {id, name, description, capabilities} ],
//	  "capabilities": { "agent_mode": {count, models}, ... }
//	}
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	models := s.fetchModelsFromZAI()

	capStats := make(map[string]int)
	modelsWithCap := make(map[string][]string)
	for _, m := range models {
		for capName, value := range m.Capabilities {
			if b, ok := value.(bool); ok && b {
				capStats[capName]++
				modelsWithCap[capName] = append(modelsWithCap[capName], m.ID)
			}
		}
	}

	// By-name view of the per-capability model lists
	capsView := make(map[string]interface{}, len(capStats))
	for capName := range capStats {
		capsView[capName] = map[string]interface{}{
			"count":  capStats[capName],
			"models": modelsWithCap[capName],
		}
	}

	// Keep the raw model list serializable — ModelInfo fields are exported,
	// but Capabilities values are interface{} straight from Z.AI JSON, so a
	// re-marshal round-trip keeps the response shape stable.
	type modelView struct {
		ID           string                 `json:"id"`
		Name         string                 `json:"name"`
		Description  string                 `json:"description,omitempty"`
		Capabilities map[string]interface{} `json:"capabilities,omitempty"`
	}
	views := make([]modelView, 0, len(models))
	for _, m := range models {
		views = append(views, modelView{
			ID:           m.ID,
			Name:         m.Name,
			Description:  m.Description,
			Capabilities: m.Capabilities,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":        len(models),
		"cutoff":       "glm-4.7",
		"models":       views,
		"capabilities": capsView,
	})
}
