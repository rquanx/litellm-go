package proxyconfig

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

func (registry *modelRegistry) registerAllowedIPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /add/allowed_ip", registry.updateAllowedIP)
	mux.HandleFunc("POST /delete/allowed_ip", registry.updateAllowedIP)
}

func (registry *modelRegistry) updateAllowedIP(w http.ResponseWriter, r *http.Request) {
	if registry.keys == nil || !registry.keys.requireMaster(w, r) {
		return
	}
	var input struct {
		IPAddress string `json:"ip_address"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		net.ParseIP(strings.TrimSpace(input.IPAddress)) == nil {
		modelError(w, http.StatusBadRequest, "Supply a valid IP address")
		return
	}
	ip := strings.TrimSpace(input.IPAddress)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	contains := slices.Contains(registry.allowedIPs, ip)
	adding := r.URL.Path == "/add/allowed_ip"
	if adding && contains {
		modelError(w, http.StatusBadRequest, "IP address already exists")
		return
	}
	if !adding && !contains {
		modelError(w, http.StatusNotFound, "IP address does not exist")
		return
	}
	next := slices.Clone(registry.allowedIPs)
	if adding {
		next = append(next, ip)
	} else {
		next = slices.DeleteFunc(next, func(value string) bool { return value == ip })
	}
	if registry.store != nil {
		state := modelState{AllowedIPs: next, Models: registry.models, Credentials: registry.credentials, Router: registry.router,
			SearchTools: registry.searchTools, PassThrough: registry.passThrough, MCPServers: registry.mcpServers,
			MCPToolsets: registry.mcpToolsets, ToolPolicies: registry.toolPolicies, MCPSemanticFilter: registry.mcpSemanticFilter, PublicHub: registry.publicHub}
		if err := registry.store.save(state); err != nil {
			stateWriteError(w, err)
			return
		}
	}
	registry.allowedIPs = next
	registry.server.ReplaceAllowedIPs(next)
	message := "IP " + ip + " address added successfully"
	if !adding {
		message = "IP " + ip + " address deleted successfully"
	}
	modelJSON(w, http.StatusOK, map[string]string{"message": message, "status": "success"})
}

