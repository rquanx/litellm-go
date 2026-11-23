package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func WithSkillMarketplace(next http.Handler, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS "LiteLLM_ClaudeCodePluginTable" (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, version TEXT, description TEXT, manifest_json TEXT,
			files_json TEXT DEFAULT '{}', enabled BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMP(3), updated_at TIMESTAMP(3), created_by TEXT)`)
		if err != nil {
			return nil, errors.New("could not initialize the skill marketplace")
		}
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		catalog := r.URL.Path == "/claude-code/marketplace.json"
		plugins := make([]map[string]any, 0)
		var exists bool
		if db != nil {
			if err := db.QueryRowContext(ctx, `SELECT to_regclass('"LiteLLM_ClaudeCodePluginTable"') IS NOT NULL`).Scan(&exists); err != nil {
				modelError(w, http.StatusInternalServerError, "Could not read the skill catalog")
				return
			}
		}
		if exists {
			var err error
			plugins, err = readPlugins(ctx, db, catalog, true, "")
			if err != nil {
				modelError(w, http.StatusInternalServerError, "Could not read the skill catalog")
				return
			}
		}
		if catalog {
			modelJSON(w, http.StatusOK, map[string]any{"name": "litellm", "owner": map[string]string{"name": "LiteLLM", "email": "support@litellm.ai"}, "plugins": plugins})
			return
		}
		modelJSON(w, http.StatusOK, map[string]any{"plugins": plugins, "count": len(plugins)})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public/skill_hub", list)
	mux.HandleFunc("GET /claude-code/marketplace.json", list)
	if db != nil && keys != nil {
		registerPluginManagement(mux, db, keys)
	}
	mux.Handle("/", next)
	return mux, nil
}

func readPlugins(ctx context.Context, db *sql.DB, catalog, enabledOnly bool, filter string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,name,version,description,COALESCE(manifest_json,'{}'),created_at,updated_at,enabled
		FROM "LiteLLM_ClaudeCodePluginTable" WHERE (NOT $1 OR enabled) AND ($2 = '' OR name=$2)
		ORDER BY created_at DESC, name`, enabledOnly, filter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plugins := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, raw string
		var version, description *string
		var created, updated *time.Time
		var enabled bool
		if err := rows.Scan(&id, &name, &version, &description, &raw, &created, &updated, &enabled); err != nil {
			return nil, err
		}
		var manifest map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &manifest) != nil {
			if catalog {
				continue
			}
			return nil, errors.New("invalid published plugin manifest")
		}
		if catalog && len(manifest["source"]) == 0 {
			continue
		}
		source := manifest["source"]
		if len(source) == 0 {
			source = json.RawMessage("{}")
		}
		item := map[string]any{"name": name, "source": source}
		for _, field := range []string{"author", "homepage", "keywords", "category"} {
			if value := manifest[field]; catalog && value != nil || !catalog {
				item[field] = value
			}
		}
		if catalog {
			if version != nil && *version != "" {
				item["version"] = version
			}
			if description != nil && *description != "" {
				item["description"] = description
			}
		} else {
			item["id"], item["enabled"], item["created_at"], item["updated_at"] = id, enabled, created, updated
			for _, name := range []string{"description", "version", "domain", "namespace"} {
				item[name] = manifest[name]
			}
		}
		plugins = append(plugins, item)
	}
	return plugins, rows.Err()
}
