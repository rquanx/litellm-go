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
	switch path {
	case "/health/readiness", "/health/liveliness", "/health/liveness":
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.WriteHeader(http.StatusOK)
			return true
		}
	case "/health/readiness/details", "/health/backlog", "/health", "/health/history", "/health/latest", "/health/shared-status":
		if r.Method == http.MethodGet && !isAdmin {
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
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return true
	}
	switch path {
	case "/health/liveliness", "/health/liveness":
		writeJSON(w, http.StatusOK, "I'm alive!")
	case "/health/backlog":
		writeJSON(w, http.StatusOK, map[string]int64{"in_flight_requests": s.inFlight.Load()})
	case "/health":
		s.modelHealth(w, r, scope, isAdmin)
	case "/health/history", "/health/latest":
		s.healthHistory(w, r, scope)
	case "/health/shared-status":
		writeJSON(w, http.StatusOK, map[string]any{"shared_health_check_enabled": false, "message": "Shared health check is not enabled"})
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
