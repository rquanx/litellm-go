package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed callback_configs.json
var callbackConfigs json.RawMessage

func (callbacks GenericCallbacks) ManagementHandler(next http.Handler, keys *KeyRegistry) http.Handler {
	active := map[string][]string{"success": {}, "failure": {}, "success_and_failure": {}}
	for _, callback := range callbacks {
		switch {
		case callback.success && callback.failure:
			active["success_and_failure"] = append(active["success_and_failure"], callback.name)
		case callback.success:
			active["success"] = append(active["success"], callback.name)
		case callback.failure:
			active["failure"] = append(active["failure"], callback.name)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /callbacks/list", func(w http.ResponseWriter, r *http.Request) {
		if keys.requireAdminRead(w, r) {
			modelJSON(w, http.StatusOK, active)
		}
	})
	mux.HandleFunc("GET /callbacks/configs", func(w http.ResponseWriter, r *http.Request) {
		if keys.requireAdminRead(w, r) {
			modelJSON(w, http.StatusOK, callbackConfigs)
		}
	})
	activeSettings := func(w http.ResponseWriter, r *http.Request) {
		if _, valid := keys.identifyReadCaller(r); !valid {
			modelError(w, http.StatusUnauthorized, "Proxy token required")
			return
		}
		all := append([]string{}, active["success"]...)
		all = append(all, active["failure"]...)
		all = append(all, active["success_and_failure"]...)
		modelJSON(w, http.StatusOK, map[string]any{
			"alerting": "[]", "litellm.callbacks": all,
			"litellm.success_callback": active["success"],
			"litellm.failure_callback": active["failure"],
			"all_litellm_callbacks": all, "num_callbacks": len(all), "num_alerting": 0,
		})
	}
	mux.HandleFunc("GET /settings", activeSettings)
	mux.HandleFunc("GET /active/callbacks", activeSettings)
	mux.HandleFunc("POST /key/health", func(w http.ResponseWriter, r *http.Request) {
		callbacks.keyHealth(w, r, keys)
	})
	mux.Handle("/", next)
	return mux
}

