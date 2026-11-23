package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
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
