package runtime

import (
	"context"
	"net"
	"net/http"
	"slices"
	"time"
)

func (s *Server) serveMCPDiagnostics(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || (r.URL.Path != "/v1/mcp/server/health" && r.URL.Path != "/v1/mcp/network/client-ip") {
		return false
	}
	access, valid := s.mcpAccess(r)
	if !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return true
	}
	if r.URL.Path == "/v1/mcp/network/client-ip" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		writeJSON(w, http.StatusOK, map[string]string{"ip": host})
		return true
	}
	type serverStatus struct {
		ID string `json:"server_id"`
		Status string `json:"status"`
	}
	filter := r.URL.Query()["server_ids"]
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result := []serverStatus{}
	for _, server := range s.currentMCPServers() {
		if !access(server, "") || (len(filter) > 0 && !slices.Contains(filter, server.ID)) {
			continue
		}
		status := "unknown"
		select {
		case s.sem <- struct{}{}:
			_, err := s.mcpTools(ctx, s.mcpRequestServer(r.WithContext(ctx), server))
			<-s.sem
			if err == nil {
				status = "healthy"
			} else if ctx.Err() == nil {
				status = "unhealthy"
			}
		case <-ctx.Done():
		}
		result = append(result, serverStatus{ID: server.ID, Status: status})
	}
	writeJSON(w, http.StatusOK, result)
	return true
}

