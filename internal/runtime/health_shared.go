package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type SharedHealthResult struct {
	Record     ModelHealthRecord
	HTTPStatus int
}

type HealthCoordinator interface {
	AcquireHealth(context.Context, string, string, time.Duration) (*SharedHealthResult, bool, error)
	PublishHealth(context.Context, string, string, SharedHealthResult, time.Duration) error
	PingHealth(context.Context) error
}

func (s *Server) sharedHealthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.config.SharedHealthChecks {
		writeJSON(w, http.StatusOK, map[string]any{"shared_health_check_enabled": false, "message": "Shared health check is not enabled"})
		return
	}
	if s.config.HealthCoordinator == nil {
		writeJSON(w, http.StatusOK, map[string]any{"shared_health_check_enabled": true, "redis_available": false, "message": "Redis is not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	available := s.config.HealthCoordinator.PingHealth(ctx) == nil
	writeJSON(w, http.StatusOK, map[string]any{"shared_health_check_enabled": true, "status": map[string]bool{"redis_connected": available}})
}

func (s *Server) sharedHealthProbe(ctx context.Context, deployment healthDeployment, timeout time.Duration, probe func() ModelHealthRecord) (ModelHealthRecord, bool) {
	candidate := deployment.candidate
	config, err := json.Marshal(candidate)
	if err != nil {
		return probe(), true
	}
	vertex := ""
	if candidate.Vertex != nil {
		vertex = candidate.Vertex.Project + "/" + candidate.Vertex.Location
	}
	identity := fmt.Sprintf("%s/%x/%s/%s/%s/%s/%s/%v/%v/%s/%s", deployment.alias, sha256.Sum256(config),
		candidate.HealthMode, candidate.HealthModel, candidate.HealthVoice, candidate.HealthReasoningEffort, candidate.HealthTimeout,
		healthFingerprintOption(candidate.HealthMaxTokens), healthFingerprintOption(candidate.HealthSupportsMaxTokens), vertex, candidate.AzureIdentityKey)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	token := newUUID()
	lease := timeout + 2*time.Second
	if lease < timeout {
		lease = timeout
	}

	wait, stop := context.WithTimeout(ctx, lease)
	defer stop()
	for wait.Err() == nil {
		step, cancel := context.WithTimeout(wait, time.Second)
		cached, acquired, err := s.config.HealthCoordinator.AcquireHealth(step, key, token, lease)
		cancel()
		if err != nil && wait.Err() == nil {
			return probe(), true
		}
		if cached != nil {
			cached.Record.responseStatus = cached.HTTPStatus
			return cached.Record, false
		}
		if acquired {
			record := probe()
			step, cancel := context.WithTimeout(ctx, time.Second)
			_ = s.config.HealthCoordinator.PublishHealth(step, key, token, SharedHealthResult{Record: record, HTTPStatus: record.responseStatus}, s.config.BackgroundHealthInterval)
			cancel()
			return record, true
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-wait.Done():
		}
	}
	return ModelHealthRecord{}, false
}

func healthFingerprintOption[T any](option *T) any {
	if option == nil {
		return nil
	}
	return *option
}
