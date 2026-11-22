package runtime

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"
)

func watchPollingCancellation(ctx context.Context, store PollingStore, id string, cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot, err := store.Read(ctx, id)
			if err == nil && (snapshot == nil || snapshot.Body["status"] == "cancelled") {
				cancel()
				return
			}
		}
	}
}

func (s *Server) pollingSnapshotAllowed(r *http.Request, scope []string, snapshot *PollingSnapshot) bool {
	return snapshot != nil && modelAllowed(scope, snapshot.Model) &&
		(s.authorized(r) || snapshot.Owner == sha256.Sum256([]byte(ProxyCredential(r))))
}

func (s *Server) sharedPollingResource(w http.ResponseWriter, r *http.Request, id, action string, scope []string) {
	ctx := r.Context()
	store := s.config.PollingStore
	snapshot, err := store.Read(ctx, id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "polling_unavailable", "Could not read response polling state")
		return
	}
	if !s.pollingSnapshotAllowed(r, scope, snapshot) {
		writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling response")
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		writeJSON(w, http.StatusOK, snapshot.Body)
	case r.Method == http.MethodPost && action == "cancel":
		err = store.Cancel(ctx, id)
		if err == nil {
			snapshot, err = store.Read(ctx, id)
		}
		if err == nil && snapshot != nil {
			writeJSON(w, http.StatusOK, snapshot.Body)
			return
		}
		if err == nil {
			writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling response")
			return
		}
	case r.Method == http.MethodDelete && action == "":
		err = store.Delete(ctx, id)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response", "deleted": true})
			return
		}
	default:
		writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling operation")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "polling_unavailable", "Could not change response polling state")
	}
}
