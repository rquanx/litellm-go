package runtime

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type ModelHealthRecord struct {
	ID             string    `json:"health_check_id"`
	Model          string    `json:"model_name"`
	ModelID        string    `json:"model_id"`
	Status         string    `json:"status"`
	HealthyCount   int       `json:"healthy_count"`
	UnhealthyCount int       `json:"unhealthy_count"`
	ErrorMessage   *string   `json:"error_message"`
	ResponseMS     float64   `json:"response_time_ms"`
	Details        any       `json:"details"`
	CheckedBy      *string   `json:"checked_by"`
	CheckedAt      time.Time `json:"checked_at"`
	CreatedAt      time.Time `json:"created_at"`
	responseStatus int
}

type HealthHistoryQuery struct {
	Scope  []string
	Model  string
	Status string
	Limit  int
	Offset int
	Latest bool
}

type HealthHistory interface {
	Record(context.Context, []ModelHealthRecord)
	Read(context.Context, HealthHistoryQuery) ([]ModelHealthRecord, error)
}

func (s *Server) healthHistory(w http.ResponseWriter, r *http.Request, scope []string) {
	if s.config.HealthHistory == nil {
		writeError(w, http.StatusInternalServerError, "missing_database", "Database not initialized")
		return
	}
	query := HealthHistoryQuery{Scope: scope, Latest: r.URL.Path == "/health/latest", Limit: 100}
	if !query.Latest {
		fields := r.URL.Query()
		query.Model, query.Status = fields.Get("model"), fields.Get("status_filter")
		var limitErr, offsetErr error
		if fields.Has("limit") {
			query.Limit, limitErr = strconv.Atoi(fields.Get("limit"))
		}
		if fields.Has("offset") {
			query.Offset, offsetErr = strconv.Atoi(fields.Get("offset"))
		}
		if limitErr != nil || offsetErr != nil || query.Limit < 1 || query.Limit > 1000 || query.Offset < 0 {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "Invalid health history pagination")
			return
		}
	}
	if modelAllowed(scope, "*") {
		query.Scope = nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	records, err := s.config.HealthHistory.Read(ctx, query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "health_history_error", "Could not read health history")
		return
	}
	if query.Latest {
		latest := make(map[string]ModelHealthRecord, len(records))
		for _, entry := range records {
			latest[entry.ModelID] = entry
		}
		writeJSON(w, http.StatusOK, map[string]any{"latest_health_checks": latest, "total_models": len(latest)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health_checks": records, "total_records": len(records), "limit": query.Limit, "offset": query.Offset})
}
