package proxyconfig

import (
	"net/http"
	"os"
	"strings"
)

func publicUIConfig(w http.ResponseWriter, _ *http.Request) {
	writePublicUIConfig(w, false)
}

func writePublicUIConfig(w http.ResponseWriter, ready bool) {
	var base any
	if value, configured := os.LookupEnv("PROXY_BASE_URL"); configured {
		base = value
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"server_root_path":     os.Getenv("SERVER_ROOT_PATH"),
		"proxy_base_url":       base,
		"auto_redirect_to_sso": false,
		"admin_ui_disabled":    strings.EqualFold(os.Getenv("DISABLE_ADMIN_UI"), "true"),
		"sso_configured":       ready,
		"is_control_plane":     false,
		"workers":              []any{},
	})
}
