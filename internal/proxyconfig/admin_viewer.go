package proxyconfig

import (
	"net/http"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

const adminViewerRole = "proxy_admin_viewer"

func (registry *KeyRegistry) adminViewer(r *http.Request) bool {
	if registry == nil {
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.adminViewerLocked(runtime.ProxyCredential(r))
}

func (registry *KeyRegistry) adminViewerLocked(token string) bool {
	if token == "" {
		return false
	}
	key, exists := registry.state.Keys[keyDigest(token)]
	now := time.Now()
	if !exists || key.Blocked || (key.Expires != nil && !now.Before(*key.Expires)) {
		return false
	}
	if key.RotationTarget != "" {
		key, exists = registry.state.Keys[key.RotationTarget]
	}
	return exists && !key.Blocked && key.RotationTarget == "" &&
		(key.Expires == nil || now.Before(*key.Expires)) &&
		key.TeamID == "" && key.ProjectID == "" && key.OrgID == "" &&
		registry.state.Users[key.UserID].Role == adminViewerRole
}

func (registry *KeyRegistry) requireAdminRead(w http.ResponseWriter, r *http.Request) bool {
	return registry.adminViewer(r) || registry.requireMaster(w, r)
}

func (registry *KeyRegistry) identifyReadCaller(r *http.Request) (memoryCaller, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.identifyReadCallerLocked(r)
}

func (registry *KeyRegistry) identifyReadCallerLocked(r *http.Request) (memoryCaller, bool) {
	if registry.adminViewerLocked(runtime.ProxyCredential(r)) {
		return memoryCaller{admin: true}, true
	}
	return registry.identifyResourceCallerLocked(r)
}

func dashboardUserRole(role string) bool {
	return role == adminViewerRole || internalSSORole(role)
}
