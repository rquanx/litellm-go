package proxyconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

// ImportLiteLLMModels copies supported LiteLLM deployments into empty Go proxy state.
func ImportLiteLLMModels(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	var existing modelState
	if err := store.load(&existing); err != nil {
		return 0, errors.New("could not read Go model state")
	}
	if len(existing.Models) != 0 {
		return 0, errors.New("Go model state already contains deployments")
	}
	salt := os.Getenv("LITELLM_SALT_KEY")
	if salt == "" {
		salt = config.MasterKey
	}
	if salt == "" {
		return 0, errors.New("the source LiteLLM salt or master key is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, errors.New("could not open a snapshot of LiteLLM model state")
	}
	defer tx.Rollback()
	models, err := readLiteLLMModels(ctx, tx, salt)
	if err != nil {
		return 0, err
	}
	if len(models) == 0 {
		return 0, nil
	}
	credentials, err := readReferencedCredentials(ctx, tx, salt, models, combinedCredentials(config.credentials, existing.Credentials))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, errors.New("could not finish reading LiteLLM model state")
	}
	existing.Credentials = maps.Clone(existing.Credentials)
	if existing.Credentials == nil {
		existing.Credentials = make(map[string]providerCredential)
	}
	maps.Copy(existing.Credentials, credentials)
	registry := modelRegistry{base: config, credentials: existing.Credentials, router: existing.Router, searchTools: existing.SearchTools}
	if _, err := registry.compile(models); err != nil {
		return 0, err
	}
	existing.Models = models
	if err := store.save(existing); err != nil {
		return 0, errors.New("could not commit imported model state")
	}
	return len(models), nil
}

func ImportLiteLLMKeys(db *sql.DB) (int, error) {
	store := &postgresState{db: db, name: "keys"}
	existing := accessState{Keys: make(map[string]virtualKey), Teams: make(map[string]team)}
	if err := store.load(&existing); err != nil {
		return 0, errors.New("could not read Go key state")
	}
	if len(existing.Keys) != 0 {
		return 0, errors.New("Go key state already contains credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT to_jsonb(key_row) FROM "LiteLLM_VerificationToken" AS key_row`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM_VerificationToken")
	}
	defer rows.Close()
	keys := make(map[string]virtualKey)
	for rows.Next() {
		var payload []byte
		if rows.Scan(&payload) != nil {
			return 0, errors.New("could not read a LiteLLM virtual key")
		}
		token, key, blocked, err := decodeLiteLLMKey(payload)
		if err != nil {
			return 0, err
		}
		if !blocked {
			keys[token] = key
		}
	}
	if rows.Err() != nil {
		return 0, errors.New("could not finish reading LiteLLM virtual keys")
	}
	if len(keys) == 0 {
		return 0, nil
	}
	existing.Keys = keys
	if err := store.save(existing); err != nil {
		return 0, errors.New("could not commit imported virtual keys")
	}
	return len(keys), nil
}

func decodeLiteLLMKey(payload []byte) (string, virtualKey, bool, error) {
	return decodeLiteLLMKeyForState(payload, nil)
}

func decodeLiteLLMKeyForState(payload []byte, state *accessState) (string, virtualKey, bool, error) {
	var key virtualKey
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return "", key, false, errors.New("invalid LiteLLM virtual key")
	}
	if string(fields["blocked"]) == "true" {
		return "", key, true, nil
	}
	expires, created := fields["expires"], fields["created_at"]
	delete(fields, "expires")
	delete(fields, "created_at")
	keyFields, _ := json.Marshal(fields)
	if json.Unmarshal(keyFields, &key) != nil {
		return "", key, false, errors.New("invalid LiteLLM virtual key")
	}
	var err error
	if key.Expires, err = liteLLMTimestamp(expires); err != nil {
		return "", key, false, errors.New("invalid LiteLLM key expiration")
	}
	if key.CreatedAt, err = liteLLMTimestamp(created); err != nil {
		return "", key, false, errors.New("invalid LiteLLM key creation time")
	}
	for _, field := range []string{"agent_id", "project_id", "budget_id",
		"max_budget", "max_parallel_requests", "tpm_limit", "rpm_limit", "object_permission_id", "allowed_routes",
		"allowed_cache_controls", "access_group_ids", "permissions", "policies", "router_settings", "config", "aliases",
		"model_max_budget", "budget_limits", "auto_rotate", "budget_duration"} {
		switch string(fields[field]) {
		case "", "null", "[]", "{}", "\"\"", "false":
		default:
			return "", key, false, fmt.Errorf("virtual key %q uses unsupported field %s", key.Alias, field)
		}
	}
	if slices.Contains(key.Models, "all-proxy-models") || (key.TeamID == "" && slices.Contains(key.Models, "all-team-models")) {
		return "", key, false, fmt.Errorf("virtual key %q uses an unsupported dynamic model scope", key.Alias)
	}
	if key.UserID != "" || key.TeamID != "" || key.OrgID != "" {
		if state == nil || !state.containsKeyOwners(key) {
			return "", key, false, fmt.Errorf("virtual key %q refers to a missing or unsupported owner", key.Alias)
		}
	}
	var token string
	if json.Unmarshal(fields["token"], &token) != nil {
		return "", key, false, errors.New("LiteLLM virtual key is missing its digest")
	}
	digest, err := hex.DecodeString(token)
	if err != nil || len(digest) != sha256.Size {
		return "", key, false, errors.New("LiteLLM virtual key uses an unsupported digest")
	}
	return hex.EncodeToString(digest), key, false, nil
}

func liteLLMTimestamp(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var date string
	if err := json.Unmarshal(raw, &date); err != nil {
		return nil, err
	}
	instant, err := time.Parse(time.RFC3339Nano, date)
	if err != nil {
		instant, err = time.Parse("2006-01-02T15:04:05.999999999", date)
	}
	return &instant, err
}

func readLiteLLMModels(ctx context.Context, tx *sql.Tx, salt string) ([]deployment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT model_id, model_name, litellm_params, COALESCE(model_info, '{}'::jsonb)
		FROM "LiteLLM_ProxyModelTable" ORDER BY created_at, model_id`)
	if err != nil {
		return nil, errors.New("could not read LiteLLM_ProxyModelTable")
	}
	defer rows.Close()
	var models []deployment
	for rows.Next() {
		var model deployment
		var id string
		var params, info []byte
		if err := rows.Scan(&id, &model.Alias, &params, &info); err != nil {
			return nil, errors.New("could not read a LiteLLM deployment")
		}
		params, err = decryptLiteLLMParams(params, salt)
		if err != nil || decodeModelJSON(bytes.NewReader(params), &model.Params) != nil ||
			json.Unmarshal(info, &model.Info) != nil {
			return nil, fmt.Errorf("model %q has incompatible configuration or credentials", model.Alias)
		}
		model.Info.ID = id
		models = append(models, model)
	}
	if rows.Err() != nil {
		return nil, errors.New("could not finish reading LiteLLM models")
	}
	return models, nil
}

func readReferencedCredentials(ctx context.Context, tx *sql.Tx, salt string, models []deployment,
	known map[string]providerCredential) (map[string]providerCredential, error) {
	imported := make(map[string]providerCredential)
	for _, model := range models {
		name := model.Params.CredentialName
		if name == "" || known[name].Name != "" || imported[name].Name != "" {
			continue
		}
		var values, info []byte
		if err := tx.QueryRowContext(ctx, `SELECT credential_values, COALESCE(credential_info, '{}'::jsonb)
			FROM "LiteLLM_CredentialsTable" WHERE credential_name = $1`, name).Scan(&values, &info); err != nil {
			return nil, fmt.Errorf("could not read the referenced credential for model %q", model.Alias)
		}
		decoded, err := decryptLiteLLMParams(values, salt)
		credential := providerCredential{Name: name}
		if err != nil || json.Unmarshal(decoded, &credential.Values) != nil ||
			json.Unmarshal(info, &credential.Info) != nil || credential.validate() != nil {
			return nil, fmt.Errorf("model %q refers to an incompatible credential", model.Alias)
		}
		imported[name] = credential
	}
	return imported, nil
}

func decryptLiteLLMParams(payload []byte, salt string) ([]byte, error) {
	var params map[string]any
	if err := json.Unmarshal(payload, &params); err != nil || params == nil {
		return nil, errors.New("invalid LiteLLM model parameters")
	}
	digest := sha256.Sum256([]byte(salt))
	for name, value := range params {
		encoded, ok := value.(string)
		if !ok || encoded == "" {
			continue
		}
		data, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			data, err = base64.StdEncoding.DecodeString(encoded)
		}
		if err != nil || len(data) < 24+secretbox.Overhead {
			continue
		}
		var nonce [24]byte
		copy(nonce[:], data[:24])
		plain, valid := secretbox.Open(nil, data[24:], &nonce, &digest)
		if !valid {
			return nil, errors.New("invalid LiteLLM database salt")
		}
		params[name] = string(plain)
	}
	return json.Marshal(params)
}
