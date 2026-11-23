package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

func LegacyVectorStoreLookup(db *sql.DB) runtime.LegacyFileLookup {
	return legacyResourceLookup(db, `"LiteLLM_ManagedVectorStoreTable"`, "unified_resource_id")
}

func LegacyFileLookup(db *sql.DB) runtime.LegacyFileLookup {
	return legacyResourceLookup(db, `"LiteLLM_ManagedFileTable"`, "unified_file_id")
}

func legacyResourceLookup(db *sql.DB, table, idColumn string) runtime.LegacyFileLookup {
	return func(ctx context.Context, id string, owners []string, master bool) (map[string]string, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var users, teams []string
		for _, owner := range owners {
			kind, value, _ := strings.Cut(owner, ":")
			switch kind {
			case "user":
				users = append(users, value)
			case "team":
				teams = append(teams, value)
			}
		}
		if !master && len(users)+len(teams) == 0 {
			return nil, nil
		}
		var data []byte
		err := db.QueryRowContext(ctx, `SELECT model_mappings FROM `+table+`
			WHERE `+idColumn+`=$1 AND (storage_backend IS NULL OR storage_backend IN ('','default'))
			AND ($2 OR created_by = ANY($3) OR (created_by IS NULL AND team_id = ANY($4)))`,
			id, master, pq.Array(users), pq.Array(teams)).Scan(&data)
		if err != nil {
			return nil, err
		}
		var encoded string
		if len(data) > 0 && data[0] == '"' && json.Unmarshal(data, &encoded) == nil {
			data = []byte(encoded)
		}
		var mappings map[string]string
		err = json.Unmarshal(data, &mappings)
		return mappings, err
	}
}
