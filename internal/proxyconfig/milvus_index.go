package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func MilvusIndexLookup(db *sql.DB, keys *KeyRegistry) runtime.MilvusIndexLookup {
	return func(r *http.Request, index, permission string) (alias, collection, database string, found, allowed bool, err error) {
		caller, ok := keys.identifyResourceCaller(r)
		if !ok {
			return "", "", "", false, false, nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		rows, err := db.QueryContext(ctx, `SELECT COALESCE(store.litellm_params->>'model',''),
			COALESCE(idx.litellm_params->>'vector_store_index',''), COALESCE(store.litellm_params->>'milvus_db_name','')
			FROM "LiteLLM_ManagedVectorStoreIndexTable" idx
			JOIN `+vectorRegistryTable+` store ON store.vector_store_name=idx.litellm_params->>'vector_store_name'
			WHERE idx.index_name=$4 AND store.custom_llm_provider='milvus' AND `+vectorRegistryScope+` LIMIT 2`,
			caller.admin, caller.user, caller.team, index)
		if err != nil {
			return "", "", "", false, false, err
		}
		defer rows.Close()
		if !rows.Next() {
			return "", "", "", false, false, rows.Err()
		}
		if err = rows.Scan(&alias, &collection, &database); err != nil {
			return "", "", "", false, false, err
		}
		if rows.Next() {
			return "", "", "", false, false, errors.New("ambiguous Milvus vector store")
		}
		if err = rows.Err(); err != nil {
			return "", "", "", false, false, err
		}
		if caller.admin {
			return alias, collection, database, true, true, nil
		}
		keys.mu.RLock()
		defer keys.mu.RUnlock()
		token := runtime.ProxyCredential(r)
		if _, _, ok := keys.authorizeLocked(token); !ok {
			return "", "", "", false, false, nil
		}
		key := keys.state.Keys[keyDigest(token)]
		if key.RotationTarget != "" {
			key = keys.state.Keys[key.RotationTarget]
		}
		allowed = allowsMilvusIndex(key.Metadata, index, permission) || allowsMilvusIndex(keys.state.Teams[caller.team].Metadata, index, permission)
		return alias, collection, database, true, allowed, nil
	}
}

func allowsMilvusIndex(metadata map[string]any, index, permission string) bool {
	entries, _ := metadata["allowed_vector_store_indexes"].([]any)
	for _, value := range entries {
		entry, _ := value.(map[string]any)
		if entry["index_name"] != index {
			continue
		}
		grants, _ := entry["index_permissions"].([]any)
		if slices.Contains(grants, any(permission)) {
			return true
		}
	}
	return false
}

func (api *vectorRegistry) createIndex(w http.ResponseWriter, r *http.Request) {
	caller, ok := api.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	var input struct {
		Name   string `json:"index_name"`
		Params struct {
			Store    string `json:"vector_store_name"`
			Resource string `json:"vector_store_index"`
		} `json:"litellm_params"`
		Info map[string]any `json:"index_info"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		!validVectorIndexName(input.Name) || !validVectorIndexName(input.Params.Resource) || strings.TrimSpace(input.Params.Store) == "" {
		modelError(w, http.StatusBadRequest, "An index name, vector store name and provider index are required")
		return
	}
	var info any
	if input.Info != nil {
		raw, _ := json.Marshal(input.Info)
		info = string(raw)
	}
	params, _ := json.Marshal(input.Params)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var result json.RawMessage
	err := api.db.QueryRowContext(ctx, `INSERT INTO "LiteLLM_ManagedVectorStoreIndexTable" AS idx
		(id,index_name,litellm_params,index_info,created_by,updated_by)
		SELECT $4,$5,$6,$7,NULLIF($2,''),NULLIF($2,'') FROM `+vectorRegistryTable+`
		WHERE vector_store_name=$8 AND `+vectorRegistryScope+`
		HAVING count(*)=1
		ON CONFLICT (index_name) DO NOTHING RETURNING to_jsonb(idx)`,
		caller.admin, caller.user, caller.team, newEntityID(), input.Name, string(params), info, input.Params.Store).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusBadRequest, "The index exists or the vector store is unavailable")
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not register vector store index")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func validVectorIndexName(value string) bool {
	return len(value) > 0 && len(value) <= 1024 && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00") &&
		strings.TrimSpace(value) == value
}
