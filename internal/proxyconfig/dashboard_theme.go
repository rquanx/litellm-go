package proxyconfig

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type dashboardTheme struct {
	LogoURL *string `json:"logo_url" yaml:"logo_url"`
	FaviconURL *string `json:"favicon_url" yaml:"favicon_url"`
}

func validDashboardImageURL(raw *string) bool {
	if raw == nil || *raw == "" {
		return true
	}
	target, err := url.Parse(*raw)
	return err == nil && (target.Scheme == "http" || target.Scheme == "https") &&
		target.Hostname() != "" && target.User == nil && !strings.ContainsAny(*raw, "\r\n\\")
}

func (config Config) WithDashboardTheme(next http.Handler, keys *KeyRegistry, db *sql.DB) http.Handler {
	initial := config.DashboardTheme
	for target, name := range map[**string]string{&initial.LogoURL: "UI_LOGO_PATH", &initial.FaviconURL: "LITELLM_FAVICON_URL"} {
		if value := os.Getenv(name); *target == nil && value != "" && validDashboardImageURL(&value) {
			*target = &value
		}
	}
	read := func() (dashboardTheme, error) {
		if db == nil {
			return initial, nil
		}
		store := postgresModels(db, keys.master)
		store.name = "dashboard-theme"
		var current dashboardTheme
		if err := store.load(&current); err != nil {
			return dashboardTheme{}, err
		}
		if store.revision == 0 {
			return initial, nil
		}
		return current, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /get/ui_theme_settings", func(w http.ResponseWriter, r *http.Request) {
		current, err := read()
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read dashboard theme")
			return
		}
		modelJSON(w, http.StatusOK, map[string]any{
			"values": current,
			"field_schema": map[string]any{
				"description": "Configuration for UI theme customization",
				"properties": map[string]any{
					"logo_url": map[string]string{"type": "string", "description": "URL or path to custom logo image. Can be a local file path or HTTP/HTTPS URL"},
					"favicon_url": map[string]string{"type": "string", "description": "URL to custom favicon image. Must be an HTTP/HTTPS URL to a .ico, .png, or .svg file"},
				},
			},
		})
	})
	mux.HandleFunc("PATCH /update/ui_theme_settings", func(w http.ResponseWriter, r *http.Request) {
		if !keys.requireMaster(w, r) {
			return
		}
		var input *dashboardTheme
		if decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &input) != nil || input == nil ||
			!validDashboardImageURL(input.LogoURL) || !validDashboardImageURL(input.FaviconURL) {
			modelError(w, http.StatusBadRequest, "Dashboard image URLs must use HTTP or HTTPS")
			return
		}
		if db == nil {
			modelError(w, http.StatusInternalServerError, "Database not connected")
			return
		}
		store := postgresModels(db, keys.master)
		store.name = "dashboard-theme"
		var previous dashboardTheme
		if err := store.load(&previous); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read dashboard theme")
			return
		}
		var audit *managementAuditEvent
		if config.StoreAuditLogs {
			audit = newSettingsAudit(r, "LiteLLM_Config", "dashboard-theme", nil, map[string]bool{
				"logo_url_configured": input.LogoURL != nil && *input.LogoURL != "",
				"favicon_url_configured": input.FaviconURL != nil && *input.FaviconURL != "",
			})
		}
		if err := store.saveWithAudit(input, audit); err != nil {
			if errors.Is(err, errConcurrentState) {
				modelError(w, http.StatusConflict, "Dashboard theme was changed by another process")
			} else {
				modelError(w, http.StatusInternalServerError, "Could not save dashboard theme")
			}
			return
		}
		modelJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "UI theme settings updated successfully."})
	})
	asset := func(w http.ResponseWriter, r *http.Request) {
		current, err := read()
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read dashboard theme")
			return
		}
		target := current.LogoURL
		if r.URL.Path == "/get_favicon" {
			target = current.FaviconURL
		}
		if target != nil && *target != "" && validDashboardImageURL(target) {
			http.Redirect(w, r, *target, http.StatusTemporaryRedirect)
			return
		}
		next.ServeHTTP(w, r)
	}
	mux.HandleFunc("GET /get_image", asset)
	mux.HandleFunc("GET /get_favicon", asset)
	mux.Handle("/", next)
	return mux
}

