package runtime

import (
	"maps"
	"net/http"
	"slices"
)

func (s *Server) modelGroups(w http.ResponseWriter, request *http.Request, scope []string) {
	selected := request.URL.Query().Get("model_group")
	s.mu.RLock()
	routes := maps.Clone(s.configuredRoutes)
	s.mu.RUnlock()
	data := make([]map[string]any, 0, len(routes))
	for _, alias := range slices.Sorted(maps.Keys(routes)) {
		route := routes[alias]
		if (selected != "" && selected != alias) || !modelAllowed(scope, alias) || (route.Hidden && selected == "") {
			continue
		}
		providers := []string{}
		var primary Candidate
		found := false
		for _, deployment := range route.Candidates {
			if deployment.RoutingGroup != "" && deployment.RoutingGroup != alias {
				continue
			}
			if !found {
				primary, found = deployment, true
			}
			if deployment.Provider != "" && !slices.Contains(providers, deployment.Provider) {
				providers = append(providers, deployment.Provider)
			}
		}
		if !found {
			continue
		}
		mode := map[string]string{
			"embeddings":"embedding", "google-embeddings":"embedding", "bedrock-embeddings":"embedding",
			"completions":"completion", "image-generation":"image_generation", "image-edit":"image_generation",
			"transcription":"audio_transcription", "speech":"audio_speech", "moderations":"moderations", "rerank":"rerank",
		}[primary.Endpoint]
		if mode == "" {
			mode = "chat"
		}
		data = append(data, map[string]any{
			"model_group": alias, "providers": providers, "mode": mode, "rpm": primary.RPM, "tpm": primary.TPM,
			"input_cost_per_token": primary.UsagePrice[0], "output_cost_per_token": primary.UsagePrice[1],
		})
	}
	if len(data) == 0 && selected != "" {
		writeError(w, http.StatusNotFound, "model_not_found", "Model group not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
