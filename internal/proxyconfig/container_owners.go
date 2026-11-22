package proxyconfig

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

type containerOwners struct{ db *sql.DB }

func OpenContainerOwners(ctx context.Context, db *sql.DB) (runtime.ContainerOwners, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_proxy_container_owners (
		deployment_id TEXT NOT NULL, container_id TEXT NOT NULL, owner_scope TEXT NOT NULL,
		PRIMARY KEY (deployment_id, container_id)
	)`)
	if err != nil {
		return nil, errors.New("could not initialize container ownership state")
	}
	return containerOwners{db}, nil
}

func (store containerOwners) Read(ctx context.Context, deployment string, ids []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := store.db.QueryContext(ctx, `SELECT container_id,owner_scope FROM minato_proxy_container_owners
		WHERE deployment_id=$1 AND container_id=ANY($2)`, deployment, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := make(map[string]string)
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		owners[id] = owner
	}
	return owners, rows.Err()
}

func (store containerOwners) Record(ctx context.Context, deployment, id, owner string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := store.db.ExecContext(ctx, `INSERT INTO minato_proxy_container_owners (deployment_id,container_id,owner_scope)
		VALUES ($1,$2,$3) ON CONFLICT (deployment_id,container_id) DO UPDATE
		SET owner_scope=excluded.owner_scope WHERE minato_proxy_container_owners.owner_scope=excluded.owner_scope`, deployment, id, owner)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("container already has an owner")
	}
	return nil
}

func (store containerOwners) Delete(ctx context.Context, deployment, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(ctx, `DELETE FROM minato_proxy_container_owners WHERE deployment_id=$1 AND container_id=$2`, deployment, id)
	return err
}

func (registry *KeyRegistry) ContainerOwnerScopes(token string) []string {
	digest := sha256.Sum256([]byte(token))
	keyID := hex.EncodeToString(digest[:])
	registry.mu.RLock()
	key, ok := registry.state.Keys[keyID]
	registry.mu.RUnlock()
	if !ok {
		return nil
	}
	var scopes []string
	if key.ProjectID != "" {
		scopes = append(scopes, "project:"+key.ProjectID)
	} else {
		if key.UserID != "" {
			scopes = append(scopes, "user:"+key.UserID)
		}
		if key.TeamID != "" {
			scopes = append(scopes, "team:"+key.TeamID)
		}
		if key.OrgID != "" {
			scopes = append(scopes, "org:"+key.OrgID)
		}
	}
	return append(scopes, "key:"+keyID)
}
