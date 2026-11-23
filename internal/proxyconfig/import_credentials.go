package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

func ImportLiteLLMCredentials(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	var state modelState
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go model state")
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
	rows, err := db.QueryContext(ctx, `SELECT credential_name,credential_values,COALESCE(credential_info,'{}'::jsonb)
		FROM "LiteLLM_CredentialsTable" ORDER BY credential_name`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM credentials")
	}
	defer rows.Close()
	known := combinedCredentials(config.credentials, state.Credentials)
	if state.Credentials == nil {
		state.Credentials = make(map[string]providerCredential)
	}
	count := 0
	for rows.Next() {
		var name string
		var values, info []byte
		if err := rows.Scan(&name, &values, &info); err != nil {
			return 0, errors.New("could not read a LiteLLM credential")
		}
		if _, exists := known[name]; exists {
			continue
		}
		values, err = decryptLiteLLMParams(values, salt)
		credential := providerCredential{Name: name}
		if err != nil || json.Unmarshal(values, &credential.Values) != nil ||
			json.Unmarshal(info, &credential.Info) != nil || credential.validate() != nil {
			return 0, fmt.Errorf("credential %q is incompatible with Go proxy state", name)
		}
		state.Credentials[name] = credential
		known[name] = credential
		count++
	}
	if rows.Err() != nil {
		return 0, errors.New("could not finish reading LiteLLM credentials")
	}
	if count == 0 {
		return 0, nil
	}
	if err := store.save(state); err != nil {
		return 0, errors.New("could not commit imported credential state")
	}
	return count, nil
}
