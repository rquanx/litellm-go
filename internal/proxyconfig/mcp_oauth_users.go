package proxyconfig

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
	"golang.org/x/crypto/nacl/secretbox"
)

type MCPUserCredentialStore struct {
	db  *sql.DB
	key [32]byte
}

func OpenMCPUserCredentialStore(db *sql.DB, master string) (*MCPUserCredentialStore, error) {
	salt := os.Getenv("LITELLM_SALT_KEY")
	if salt == "" {
		salt = master
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS "LiteLLM_MCPUserCredentials" (
		id TEXT PRIMARY KEY,user_id TEXT NOT NULL,server_id TEXT NOT NULL,credential_b64 TEXT NOT NULL,
		created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		UNIQUE(user_id,server_id))`)
	if err != nil {
		return nil, errors.New("could not initialize MCP user OAuth storage")
	}
	return &MCPUserCredentialStore{db: db, key: sha256.Sum256([]byte(salt))}, nil
}

func (keys *KeyRegistry) MCPUserIdentity(r *http.Request) string {
	caller, ok := keys.identifyResourceCaller(r)
	if !ok || caller.admin {
		return ""
	}
	return caller.user
}

func (store *MCPUserCredentialStore) decode(value string) (runtime.MCPUserOAuthToken, error) {
	var token runtime.MCPUserOAuthToken
	for _, codec := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding} {
		data, err := codec.DecodeString(value)
		if err != nil || len(data) < 24+secretbox.Overhead {
			continue
		}
		var nonce [24]byte
		copy(nonce[:], data[:24])
		if raw, ok := secretbox.Open(nil, data[24:], &nonce, &store.key); ok {
			if json.Unmarshal(raw, &token) == nil && token.Type == "oauth2" && token.AccessToken != "" {
				return token, nil
			}
			break
		}
	}
	return runtime.MCPUserOAuthToken{}, runtime.ErrMCPUserOAuthNotFound
}

func (store *MCPUserCredentialStore) Read(ctx context.Context, user, server string) (runtime.MCPUserOAuthToken, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var value string
	err := store.db.QueryRowContext(ctx, `SELECT credential_b64 FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1 AND server_id=$2`, user, server).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.MCPUserOAuthToken{}, runtime.ErrMCPUserOAuthNotFound
	}
	if err != nil {
		return runtime.MCPUserOAuthToken{}, err
	}
	return store.decode(value)
}

func (store *MCPUserCredentialStore) Save(ctx context.Context, user, server string, token runtime.MCPUserOAuthToken) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	token.Type = "oauth2"
	raw, err := json.Marshal(token)
	if err != nil {
		return err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	value := base64.URLEncoding.EncodeToString(secretbox.Seal(nonce[:], raw, &nonce, &store.key))
	var previous string
	err = store.db.QueryRowContext(ctx, `SELECT credential_b64 FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1 AND server_id=$2`, user, server).Scan(&previous)
	if err == nil {
		if _, decodeErr := store.decode(previous); decodeErr != nil {
			return decodeErr
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	result, err := store.db.ExecContext(ctx, `INSERT INTO "LiteLLM_MCPUserCredentials" AS existing (id,user_id,server_id,credential_b64,updated_at)
		VALUES ($1,$2,$3,$4,CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
		ON CONFLICT(user_id,server_id) DO UPDATE SET credential_b64=EXCLUDED.credential_b64,updated_at=EXCLUDED.updated_at
		WHERE existing.credential_b64=$5`, newEntityID(), user, server, value, previous)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errors.New("MCP user credential changed during token exchange")
	}
	return nil
}

func (store *MCPUserCredentialStore) Delete(ctx context.Context, user, server string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var raw string
	err := store.db.QueryRowContext(ctx, `SELECT credential_b64 FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1 AND server_id=$2`, user, server).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := store.decode(raw); err != nil {
		return nil
	}
	_, err = store.db.ExecContext(ctx, `DELETE FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1 AND server_id=$2 AND credential_b64=$3`, user, server, raw)
	return err
}

func (store *MCPUserCredentialStore) List(ctx context.Context, user string) (map[string]runtime.MCPUserOAuthToken, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := store.db.QueryContext(ctx, `SELECT server_id,credential_b64 FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make(map[string]runtime.MCPUserOAuthToken)
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if token, err := store.decode(raw); err == nil {
			tokens[id] = token
		}
	}
	return tokens, rows.Err()
}
