package runtime

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	isAdmin := s.authorized(r)
	var scope []string
	method := http.MethodGet
	if path == "/health/test_connection" {
		method = http.MethodPost
	}
	switch path {
	case "/health/readiness", "/health/liveliness", "/health/liveness":
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.WriteHeader(http.StatusOK)
			return true
		}
	case "/health/readiness/details", "/health/backlog", "/health", "/health/history", "/health/latest", "/health/shared-status",
		"/test", "/active/callbacks", "/settings", "/health/license", "/health/test_connection", "/health/services":
		if r.Method == method && !isAdmin {
			valid := false
			if s.config.AuthorizeKey != nil {
				scope, valid = s.config.AuthorizeKey(ProxyCredential(r))
			}
			if !valid {
				writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
				return true
			}
		}
	default:
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return true
	}
	switch path {
	case "/health/liveliness", "/health/liveness":
		writeJSON(w, http.StatusOK, "I'm alive!")
	case "/test":
		writeJSON(w, http.StatusOK, map[string]string{"route": path})
	case "/active/callbacks", "/settings":
		s.activeCallbacks(w)
	case "/health/license":
		writeJSON(w, http.StatusOK, map[string]any{"has_license": false, "license_type": "community",
			"expiration_date": nil, "allowed_features": []string{}, "limits": map[string]any{"max_users": nil, "max_teams": nil}})
	case "/health/test_connection":
		s.testModelConnection(w, r, scope)
	case "/health/services":
		s.probeCallbackHealth(w, r, isAdmin)
	case "/health/backlog":
		writeJSON(w, http.StatusOK, map[string]int64{"in_flight_requests": s.inFlight.Load()})
	case "/health":
		s.modelHealth(w, r, scope, isAdmin)
	case "/health/history", "/health/latest":
		s.healthHistory(w, r, scope)
	case "/health/shared-status":
		s.sharedHealthStatus(w, r)
	default:
		s.readiness(w, r, path == "/health/readiness/details" || s.config.PublicReadinessDetails)
	}
	return true
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request, details bool) {
	status, db := http.StatusOK, "Not connected"
	if s.config.DatabasePing != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		db = "connected"
		if s.config.DatabasePing(ctx) != nil {
			status, db = http.StatusServiceUnavailable, "disconnected"
		}
	}
	payload := map[string]any{"status": "healthy", "db": db}
	if details {
		payload["cache"] = nil
		payload["litellm_version"] = "1.85.0"
		payload["success_callbacks"] = append([]string{}, s.config.CallbackNames...)
		payload["use_aiohttp_transport"] = false
		payload["log_level"] = "INFO"
		payload["is_detailed_debug"] = false
	}
	writeJSON(w, status, payload)
}

func (s *Server) probeCallbackHealth(w http.ResponseWriter, r *http.Request, admin bool) {
	if !admin {
		writeError(w, http.StatusForbidden, "forbidden", "Callback probes require the master key")
		return
	}
	service := r.URL.Query().Get("service")
	event := newInferenceEvent("chat-completions", "litellm-mock-response-model", false)
	event.Model, event.Provider, event.Status = event.ModelGroup, "openai", "success"
	event.EndTime, event.HTTPStatus = event.StartTime, http.StatusOK
	event.EndUser, event.APISource = "litellm:/health/services", "health_probe"
	event.Messages = []byte(`[{"role":"user","content":"Hey, how's it going?"}]`)
	event.Response = []byte(`{"choices":[{"message":{"role":"assistant","content":"This is a mock response"}}]}`)
	if service == "" || s.config.CallbackHealth == nil || !s.config.CallbackHealth(r.Context(), service, event) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The callback service is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "Mock LLM request made - check "+service+"."})
}