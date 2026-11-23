package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

func (registry *modelRegistry) submitMCPServer(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if caller.admin {
		modelError(w, http.StatusForbidden, "Use the MCP server creation endpoint with a master key")
		return
	}
	if caller.team == "" {
		modelError(w, http.StatusBadRequest, "A team-scoped key is required")
		return
	}
	var input mcpServerInput
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || !validMCPSubmission(input.mcpServerConfig) {
		modelError(w, http.StatusBadRequest, "Invalid remote MCP server submission")
		return
	}
	now := time.Now().UTC()
	input.ServerID, input.ApprovalStatus, input.SubmittedAt, input.SubmittedBy, input.ReviewNotes = newEntityID(), "pending_review", &now, caller.user, ""
	if input.SubmittedBy == "" {
		input.SubmittedBy = caller.team
	}
	if input.Transport == "" {
		input.Transport = "sse"
	}
	if _, err := prepareMCPServers(map[string]mcpServerConfig{input.Name: input.mcpServerConfig}); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.mcpServers[input.Name]; exists {
		modelError(w, http.StatusConflict, "MCP server name already exists")
		return
	}
	next := maps.Clone(registry.mcpServers)
	if next == nil {
		next = make(map[string]mcpServerConfig)
	}
	next[input.Name] = input.mcpServerConfig
	if registry.replaceMCPServers(w, next) == nil {
		modelJSON(w, http.StatusCreated, mcpServerEntry(input.Name, input.mcpServerConfig))
	}
}

func validMCPSubmission(server mcpServerConfig) bool {
	if server.Transport == "stdio" || server.Command != "" || len(server.Args) > 0 || len(server.Env) > 0 {
		return false
	}
	for _, value := range []string{server.URL, server.AuthorizationURL, server.RegistrationURL, server.TokenURL, server.OAuthCallbackURL, server.ClientID, server.ClientSecret} {
		if strings.HasPrefix(value, "os.environ/") {
			return false
		}
	}
	for _, value := range server.Headers {
		if strings.HasPrefix(value, "os.environ/") {
			return false
		}
	}
	return true
}

func (registry *modelRegistry) listMCPSubmissions(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	items := []mcpServerInput{}
	counts := map[string]int{"pending_review": 0, "active": 0, "rejected": 0}
	for _, name := range slices.Sorted(maps.Keys(registry.mcpServers)) {
		entry := registry.mcpServers[name]
		if entry.SubmittedAt != nil || !entry.mcpApproved() {
			items = append(items, mcpServerEntry(name, entry))
			counts[entry.ApprovalStatus]++
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"total": len(items), "pending_review": counts["pending_review"], "active": counts["active"], "rejected": counts["rejected"], "items": items})
}

func (registry *modelRegistry) reviewMCPServer(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	status := "active"
	var body struct {
		Notes string `json:"review_notes"`
	}
	if strings.HasSuffix(r.Pattern, "/reject") {
		status = "rejected"
		if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &body) != nil {
			modelError(w, http.StatusBadRequest, "Invalid MCP review notes")
			return
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for name, entry := range registry.mcpServers {
		if entry.ServerID != r.PathValue("server_id") {
			continue
		}
		previous := entry.ApprovalStatus
		if previous == "" {
			previous = "active"
		}
		if previous == status {
			modelError(w, http.StatusBadRequest, "MCP server already has this status")
			return
		}
		entry.ApprovalStatus, entry.ReviewNotes = status, body.Notes
		next := maps.Clone(registry.mcpServers)
		next[name] = entry
		if registry.replaceMCPServers(w, next) == nil {
			modelJSON(w, http.StatusOK, mcpServerEntry(name, entry))
		}
		return
	}
	modelError(w, http.StatusNotFound, "MCP server not found")
}
