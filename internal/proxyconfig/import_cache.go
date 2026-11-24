package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ImportLiteLLMCache copies supported cache settings into encrypted proxy state.
func ImportLiteLLMCache(db *sql.DB, master string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := postgresModels(db, master)
	store.name = "inference-cache"
	var settings cacheConnectionSettings
	if err := store.load(&settings); err != nil {
		return false, err
	}
	if store.revision != 0 {
		return false, nil
	}
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT cache_settings FROM "LiteLLM_CacheConfig" WHERE id='cache_config'`).
		Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("could not read LiteLLM cache settings")
	}
	var encoded string
	if json.Unmarshal(payload, &encoded) == nil && encoded != "" {
		payload = []byte(encoded)
	}
	plain, err := decryptLiteLLMParams(payload, ssoSettingsSalt(master))
	if err != nil {
		return false, err
	}
	if json.Unmarshal(plain, &settings) != nil {
		return false, errors.New("invalid LiteLLM cache settings")
	}
	if settings.Type != "local" {
		client, err := settings.newRedisClient()
		if err != nil {
			return false, err
		}
		client.Close()
	}
	if err := store.save(settings); err != nil {
		return false, err
	}
	return true, nil
}

