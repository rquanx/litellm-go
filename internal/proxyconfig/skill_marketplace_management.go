package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

var pluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
var pluginRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
var pluginSubdir = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

func registerPluginManagement(mux *http.ServeMux, db *sql.DB, keys *KeyRegistry) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !keys.requireMaster(w, r) {
			return
		}
		name := r.PathValue("plugin")
		if name != "" && !pluginName.MatchString(name) {
			modelError(w, http.StatusBadRequest, "Invalid plugin name")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		switch r.Method {
		case http.MethodGet:
			plugins, err := readPlugins(ctx, db, false, r.URL.Query().Get("enabled_only") == "true", name)
			if err != nil {
				modelError(w, http.StatusInternalServerError, "Could not read plugins")
			} else if name == "" {
				modelJSON(w, http.StatusOK, map[string]any{"plugins": plugins, "count": len(plugins)})
			} else if len(plugins) == 0 {
				modelError(w, http.StatusNotFound, "Plugin not found")
			} else {
				modelJSON(w, http.StatusOK, plugins[0])
			}
		case http.MethodDelete:
			updatePlugin(w, ctx, db, name, nil)
		case http.MethodPost:
			if name == "" {
				upsertPlugin(w, r.WithContext(ctx), db)
			} else {
				enabled := r.PathValue("action") == "enable"
				updatePlugin(w, ctx, db, name, &enabled)
			}
		}
	}
	mux.HandleFunc("GET /claude-code/plugins", handler)
	mux.HandleFunc("GET /claude-code/plugins/{plugin}", handler)
	mux.HandleFunc("POST /claude-code/plugins", handler)
	mux.HandleFunc("POST /claude-code/plugins/{plugin}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if action := r.PathValue("action"); action == "enable" || action == "disable" {
			handler(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("DELETE /claude-code/plugins/{plugin}", handler)
}

func validPluginSource(value map[string]any) bool {
	kind, _ := value["source"].(string)
	if kind == "github" {
		repo, _ := value["repo"].(string)
		return pluginRepo.MatchString(repo)
	}
	if kind != "url" && kind != "git-subdir" {
		return false
	}
	if kind == "git-subdir" {
		path, _ := value["path"].(string)
		if !pluginSubdir.MatchString(path) {
			return false
		}
	}
	raw, _ := value["url"].(string)
	address, err := url.Parse(raw)
	return err == nil && address.Scheme == "https" && address.Hostname() != "" && address.User == nil &&
		address.RawQuery == "" && address.Fragment == ""
}

func upsertPlugin(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var fields map[string]any
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &fields) != nil {
		modelError(w, http.StatusBadRequest, "Invalid plugin manifest")
		return
	}
	name, _ := fields["name"].(string)
	source, _ := fields["source"].(map[string]any)
	if !pluginName.MatchString(name) || !validPluginSource(source) {
		modelError(w, http.StatusBadRequest, "Invalid plugin name or source")
		return
	}
	version, _ := fields["version"].(string)
	description, _ := fields["description"].(string)
	manifest := map[string]any{"name": name, "source": source}
	for _, key := range []string{"version", "description", "author", "homepage", "keywords", "category", "domain", "namespace"} {
		if fields[key] != nil {
			manifest[key] = fields[key]
		}
	}
	data, _ := json.Marshal(manifest)
	generated := newEntityID()
	var id string
	var enabled bool
	err := db.QueryRowContext(r.Context(), `INSERT INTO "LiteLLM_ClaudeCodePluginTable"
		(id,name,version,description,manifest_json,files_json,enabled,created_at,updated_at)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,'{}',true,CURRENT_TIMESTAMP AT TIME ZONE 'UTC',CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
		ON CONFLICT(name) DO UPDATE SET version=EXCLUDED.version,description=EXCLUDED.description,
			manifest_json=EXCLUDED.manifest_json,updated_at=EXCLUDED.updated_at RETURNING id,enabled`,
		generated, name, version, description, string(data)).Scan(&id, &enabled)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not save plugin")
		return
	}
	action := "updated"
	if id == generated {
		action = "created"
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "action": action, "plugin": map[string]any{
		"id": id, "name": name, "version": fields["version"], "description": fields["description"], "source": source, "enabled": enabled,
	}})
}

func updatePlugin(w http.ResponseWriter, ctx context.Context, db *sql.DB, name string, enabled *bool) {
	var id string
	var err error
	if enabled == nil {
		err = db.QueryRowContext(ctx, `DELETE FROM "LiteLLM_ClaudeCodePluginTable" WHERE name=$1 RETURNING id`, name).Scan(&id)
	} else {
		err = db.QueryRowContext(ctx, `UPDATE "LiteLLM_ClaudeCodePluginTable" SET enabled=$2,updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
			WHERE name=$1 RETURNING id`, name, *enabled).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusNotFound, "Plugin not found")
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not update plugin")
		return
	}
	result := map[string]any{"status": "success", "name": name}
	if enabled != nil {
		result["enabled"] = *enabled
	}
	modelJSON(w, http.StatusOK, result)
}
