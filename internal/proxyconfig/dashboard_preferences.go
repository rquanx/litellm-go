package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

	"github.com/rquanx/litellm-go/internal/runtime"
)

var dashboardUIPreferences = map[string]string{
	"disable_model_add_for_internal_users": "If true, internal users cannot add models from the UI",
	"disable_team_admin_delete_team_user": "Prevents Team Admins from deleting users from the teams they manage. Useful for SCIM provisioning where team membership is defined externally.",
	"enabled_ui_pages_internal_users": "List of page keys that internal users (non-admins) can see in the UI sidebar. If not set, all pages are visible based on role permissions.",
	"require_auth_for_public_ai_hub": "If true, requires authentication for accessing the public AI Hub.",
	"allow_public_health_readiness_details": "If true, returns the legacy detailed payload from the unauthenticated /health/readiness endpoint.",
	"scope_user_search_to_org": "If enabled, the user search endpoint restricts results by organization.",
	"disable_custom_api_keys": "If true, users cannot specify custom key values. All keys must be auto-generated.",
}

func (config Config) WithDashboardPreferences(next http.Handler, keys *KeyRegistry, db *sql.DB, server *runtime.Server, providerAPIs bool) http.Handler {
	read := func(withDefaults bool) (map[string]json.RawMessage, *postgresState, error) {
		values := map[string]json.RawMessage{
			"disable_model_add_for_internal_users": json.RawMessage("false"),
			"disable_team_admin_delete_team_user": json.RawMessage("false"),
			"enabled_ui_pages_internal_users": json.RawMessage("null"),
			"require_auth_for_public_ai_hub": json.RawMessage("false"),
			"allow_public_health_readiness_details": json.RawMessage("false"),
			"scope_user_search_to_org": json.RawMessage("false"),
			"disable_custom_api_keys": json.RawMessage("false"),
		}
		if config.PublicReadinessDetails {
			values["allow_public_health_readiness_details"] = json.RawMessage("true")
		}
		if !withDefaults {
			clear(values)
		}
		if db == nil {
			return values, nil, nil
		}
		store := postgresModels(db, keys.master)
		store.name = "dashboard-preferences"
		var persisted map[string]json.RawMessage
		if err := store.load(&persisted); err != nil {
			return nil, nil, err
		}
		maps.Copy(values, persisted)
		return values, store, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /get/ui_settings", func(w http.ResponseWriter, r *http.Request) {
		if _, valid := keys.identifyReadCaller(r); !valid {
			modelError(w, http.StatusUnauthorized, "Proxy token required")
			return
		}
		values, _, err := read(true)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read dashboard settings")
			return
		}
		properties := make(map[string]any, len(dashboardUIPreferences))
		for name, description := range dashboardUIPreferences {
			item := map[string]any{"description": description, "type": "boolean"}
			if name == "enabled_ui_pages_internal_users" {
				item["type"] = "array"
				item["items"] = map[string]string{"type": "string"}
			}
			properties[name] = item
		}
		modelJSON(w, http.StatusOK, map[string]any{"values": values,
			"field_schema": map[string]any{"description": "Configuration for UI-specific flags", "properties": properties}})
	})
	mux.HandleFunc("PATCH /update/ui_settings", func(w http.ResponseWriter, r *http.Request) {
		if !keys.requireMaster(w, r) {
			return
		}
		var changes map[string]json.RawMessage
		if decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &changes) != nil || len(changes) == 0 {
			modelError(w, http.StatusUnprocessableEntity, "Supply UI settings to update")
			return
		}
		for name, value := range changes {
			if _, supported := dashboardUIPreferences[name]; !supported {
				modelError(w, http.StatusUnprocessableEntity, "Unsupported dashboard setting: "+name)
				return
			}
			if name == "enabled_ui_pages_internal_users" {
				var pageKeys []string
				if json.Unmarshal(value, &pageKeys) != nil {
					modelError(w, http.StatusUnprocessableEntity, "UI page keys must be strings")
					return
				}
			} else if string(value) != "true" && string(value) != "false" {
				modelError(w, http.StatusUnprocessableEntity, "UI flags must be booleans")
				return
			}
		}
		values, store, err := read(false)
		if err != nil || store == nil {
			modelError(w, http.StatusInternalServerError, "Could not read dashboard settings storage")
			return
		}
		maps.Copy(values, changes)
		var audit *managementAuditEvent
		if config.StoreAuditLogs {
			audit = newSettingsAudit(r, "LiteLLM_UISettings", "ui_settings", nil, changes)
		}
		if err := store.saveWithAudit(values, audit); err != nil {
			if errors.Is(err, errConcurrentState) {
				modelError(w, http.StatusConflict, "Dashboard preferences were changed by another process")
			} else {
				modelError(w, http.StatusInternalServerError, "Could not save dashboard settings")
			}
			return
		}
		modelJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "UI settings updated successfully", "settings": values})
	})
	mux.HandleFunc("GET /user/filter/ui", func(w http.ResponseWriter, r *http.Request) {
		values, _, err := read(true)
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read dashboard settings")
			return
		}
		keys.searchDashboardUsers(w, r, string(values["scope_user_search_to_org"]) == "true")
	})
	for path, flag := range map[string]string{
		"GET /public/model_hub": "require_auth_for_public_ai_hub",
		"GET /public/model_hub/info": "require_auth_for_public_ai_hub",
		"GET /public/mcp_hub": "require_auth_for_public_ai_hub",
		"GET /public/agent_hub": "require_auth_for_public_ai_hub",
		"GET /public/skill_hub": "require_auth_for_public_ai_hub",
		"POST /team/member_delete": "disable_team_admin_delete_team_user",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			values, _, err := read(true)
			if err != nil {
				modelError(w, http.StatusServiceUnavailable, "Could not read dashboard settings")
				return
			}
			if string(values[flag]) == "true" {
				_, valid := keys.identifyReadCaller(r)
				if !valid {
					modelError(w, http.StatusUnauthorized, "Proxy token required")
					return
				}
				if flag == "disable_team_admin_delete_team_user" && runtime.ProxyCredential(r) != keys.master {
					modelError(w, http.StatusForbidden, "Only proxy admins can remove team members")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	keyRequest := func(w http.ResponseWriter, r *http.Request) {
		values, _, err := read(true)
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read dashboard settings")
			return
		}
		if string(values["disable_custom_api_keys"]) == "true" {
			r = r.WithContext(context.WithValue(r.Context(), customAPIKeyDisabled{}, true))
		}
		next.ServeHTTP(w, r)
	}
	mux.HandleFunc("POST /key/generate", keyRequest)
	mux.HandleFunc("POST /key/service-account/generate", keyRequest)
	mux.HandleFunc("POST /key/regenerate", keyRequest)
	mux.HandleFunc("POST /key/{key}/regenerate", keyRequest)
	if providerAPIs {
		mux.HandleFunc("GET /health/readiness", func(w http.ResponseWriter, r *http.Request) {
			values, _, err := read(true)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			server.ServeReadiness(w, r, string(values["allow_public_health_readiness_details"]) == "true")
		})
	}
	mux.Handle("/", next)
	return mux
}


type customAPIKeyDisabled struct{}

func customKeyAllowed(w http.ResponseWriter, r *http.Request, value string) bool {
	if value != "" && r.Context().Value(customAPIKeyDisabled{}) == true {
		modelError(w, http.StatusForbidden, "Custom API key values are disabled. Keys must be auto-generated")
		return false
	}
	return true
}
