package runtime

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (s *Server) activeCallbacks(w http.ResponseWriter) {
	callbacks := append([]string{}, s.config.CallbackNames...)
	writeJSON(w, http.StatusOK, map[string]any{
		"alerting": "None", "litellm.callbacks": callbacks,
		"litellm.input_callback": []string{}, "litellm.success_callback": []string{}, "litellm.failure_callback": []string{},
		"litellm._async_input_callback": []string{}, "litellm._async_success_callback": []string{}, "litellm._async_failure_callback": []string{},
		"all_litellm_callbacks": callbacks, "num_callbacks": len(callbacks), "num_alerting": 0,
		"litellm.request_timeout": s.config.HTTPClient.Timeout.Seconds(),
	})
}

func (s *Server) testModelConnection(w http.ResponseWriter, r *http.Request, scope []string) {
	var input struct {
		Mode   string         `json:"mode"`
		Params map[string]any `json:"litellm_params"`
		Info   struct {
			ID string `json:"id"`
		} `json:"model_info"`
	}
	if err := decodeBody(r, &input, s.config.MaxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid connection test request")
		return
	}
	model, _ := input.Params["model"].(string)
	for name := range input.Params {
		if name != "model" {
			writeError(w, http.StatusBadRequest, "invalid_request", "Connection tests use the deployment configuration")
			return
		}
	}
	if (model == "" && input.Info.ID == "") || strings.HasPrefix(model, "os.environ/") {
		writeError(w, http.StatusBadRequest, "invalid_request", "A configured model or deployment ID is required")
		return
	}

	var selected healthDeployment
	for _, deployment := range s.healthDeployments("", input.Info.ID, scope, false) {
		candidate := deployment.candidate
		if !candidateAllowed(scope, candidate) || (model != "" && deployment.alias != model &&
			candidate.Provider+"/"+candidate.ModelID != model && candidate.ModelID != model) {
			continue
		}
		if selected.candidate.DeploymentID == "" || deployment.alias == model {
			selected = deployment
		}
		if deployment.alias == model || input.Info.ID != "" {
			break
		}
	}
	candidate := selected.candidate
	if candidate.DeploymentID == "" {
		writeError(w, http.StatusNotFound, "model_not_found", "Unknown deployment")
		return
	}
	if input.Mode != "" && input.Mode != probeMode(candidate) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The test mode must match the configured deployment")
		return
	}

	s.TestModelDeployment(w, r, candidate)
}

// TestModelDeployment runs a probe against a deployment resolved by the management API.
func (s *Server) TestModelDeployment(w http.ResponseWriter, r *http.Request, candidate Candidate) {
	timeout := candidate.HealthTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	status := s.probeDeployment(ctx, r, candidate)
	outcome := "success"
	result := map[string]any{"model": candidate.Provider + "/" + candidate.ModelID}
	if status/100 != 2 {
		outcome = "error"
		result["error"] = "Provider connection test failed"
		result["status_code"] = status
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": outcome, "result": result})
}

func probeMode(candidate Candidate) string {
	if candidate.HealthMode != "" {
		return candidate.HealthMode
	}
	switch candidate.Endpoint {
	case "responses", "ocr", "rerank", "moderations":
		return candidate.Endpoint
	case "completions":
		return "completion"
	case "speech":
		return "audio_speech"
	case "transcription":
		return "audio_transcription"
	case "image-generation":
		return "image_generation"
	case "videos":
		return "video_generation"
	}
	if CompatibleEndpoints(candidate.Endpoint, "embeddings") {
		return "embedding"
	}
	return "chat"
}
