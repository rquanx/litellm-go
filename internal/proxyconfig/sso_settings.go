package proxyconfig

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

const ssoConfigTable = `"LiteLLM_SSOConfig"`

var ssoProviderFields = map[string]string{
	"google_client_id": "GOOGLE_CLIENT_ID", "google_client_secret": "GOOGLE_CLIENT_SECRET",
	"microsoft_client_id": "MICROSOFT_CLIENT_ID", "microsoft_client_secret": "MICROSOFT_CLIENT_SECRET", "microsoft_tenant": "MICROSOFT_TENANT",
	"generic_client_id": "GENERIC_CLIENT_ID", "generic_client_secret": "GENERIC_CLIENT_SECRET",
	"generic_authorization_endpoint": "GENERIC_AUTHORIZATION_ENDPOINT", "generic_token_endpoint": "GENERIC_TOKEN_ENDPOINT",
	"generic_userinfo_endpoint": "GENERIC_USERINFO_ENDPOINT", "proxy_base_url": "PROXY_BASE_URL",
}

func readSSOSettings(ctx context.Context, db *sql.DB, target any) error {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT sso_settings FROM `+ssoConfigTable+` WHERE id='sso_config'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return decodePolicyCondition(raw, target)
}

func (s *uiSSOServer) currentSSOConfig(parent context.Context) (uiSSOConfig, error) {
	store, ok := s.keys.store.(*postgresState)
	if !ok {
		return s.config, nil
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var settings map[string]json.RawMessage
	if err := readSSOSettings(ctx, store.db, &settings); err != nil {
		return uiSSOConfig{}, err
	}
	values, err := decryptSSOProviderSettings(settings, s.keys.master)
	if err != nil {
		return uiSSOConfig{}, err
	}
	return loadUISSOConfigFrom(func(env string) string {
		if stored, exists := values[env]; exists {
			return stored
		}
		return os.Getenv(env)
	}), nil
}

func ssoSettingsSalt(master string) string {
	if salt := os.Getenv("LITELLM_SALT_KEY"); salt != "" {
		return salt
	}
	return master
}

func decryptSSOProviderSettings(settings map[string]json.RawMessage, master string) (map[string]string, error) {
	values := make(map[string]string)
	var legacy map[string]string
	key := sha256.Sum256([]byte("minato-sso-setting\x00" + ssoSettingsSalt(master)))
	for name, env := range ssoProviderFields {
		raw, exists := settings[name]
		if !exists {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if encoded, ok := strings.CutPrefix(value, "minato-encrypted:"); ok {
			decrypted, err := openState(encoded, &key)
			if err != nil {
				return nil, err
			}
			values[env] = string(decrypted)
		} else {
			if legacy == nil {
				legacy = make(map[string]string)
			}
			legacy[env] = value
		}
	}
	if len(legacy) > 0 {
		raw, _ := json.Marshal(legacy)
		plain, err := decryptLiteLLMParams(raw, ssoSettingsSalt(master))
		if err != nil {
			return nil, err
		}
		legacy = nil
		if err := json.Unmarshal(plain, &legacy); err != nil {
			return nil, err
		}
		for env, value := range legacy {
			values[env] = value
		}
	}
	return values, nil
}

type storedSSOMappings struct {
	Roles *struct {
		Provider string              `json:"provider"`
		Claim    string              `json:"group_claim"`
		Default  string              `json:"default_role"`
		Groups   map[string][]string `json:"roles"`
	} `json:"role_mappings"`
	Teams *struct {
		Claim string `json:"team_ids_jwt_field"`
	} `json:"team_mappings"`
}

func (settings storedSSOMappings) validate() error {
	role := settings.Roles
	if role != nil {
		if role.Provider == "" || role.Claim == "" || (role.Default != "" && !dashboardUserRole(role.Default)) {
			return errors.New("invalid SSO internal role mapping")
		}
		for name := range role.Groups {
			if !dashboardUserRole(name) {
				return errors.New("invalid SSO internal role mapping")
			}
		}
	}
	return nil
}

func loadSSOMappings(ctx context.Context, db *sql.DB) (storedSSOMappings, error) {
	var mappings storedSSOMappings
	err := readSSOSettings(ctx, db, &mappings)
	return mappings, err
}

func (s *uiSSOServer) effectiveClaimMappings(ctx context.Context, cfg uiSSOConfig) (uiSSOConfig, string, error) {
	teamClaim := s.keys.ssoTeamClaim
	db, ok := s.keys.store.(*postgresState)
	if !ok {
		return cfg, teamClaim, nil
	}
	mappings, err := loadSSOMappings(ctx, db.db)
	if err != nil {
		return cfg, teamClaim, err
	}
	if roles := mappings.Roles; roles != nil && cfg.roleClaim == "" && strings.EqualFold(roles.Provider, cfg.provider) {
		if err := mappings.validate(); err != nil {
			return cfg, teamClaim, err
		}
		cfg.roleClaim, cfg.defaultRole, cfg.roleGroups = roles.Claim, roles.Default, roles.Groups
	}
	if teams := mappings.Teams; teams != nil && teams.Claim != "" {
		teamClaim = teams.Claim
	}
	return cfg, teamClaim, nil
}

func (s *uiSSOServer) ssoSettingsEndpoint(w http.ResponseWriter, r *http.Request) {
	if !s.keys.requireMaster(w, r) {
		return
	}
	store, ok := s.keys.store.(*postgresState)
	if !ok {
		modelError(w, http.StatusServiceUnavailable, "Database not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if r.Method == http.MethodGet {
		var raw map[string]json.RawMessage
		err := readSSOSettings(ctx, store.db, &raw)
		if err != nil {
			modelError(w, 500, "Could not read SSO settings")
			return
		}
		settings, err := decryptSSOProviderSettings(raw, s.keys.master)
		if err != nil {
			modelError(w, 500, "Could not read SSO provider settings")
			return
		}
		values := make(map[string]any)
		properties := make(map[string]any)
		for name, env := range ssoProviderFields {
			value, exists := settings[env]
			if !exists {
				value = os.Getenv(env)
			}
			if strings.HasSuffix(name, "_secret") && value != "" {
				value = "********"
			}
			values[name] = value
			properties[name] = map[string]string{"type": "string"}
		}
		for _, name := range []string{"role_mappings", "team_mappings"} {
			values[name] = raw[name]
			properties[name] = map[string]string{"type": "object"}
		}
		modelJSON(w, 200, map[string]any{"values": values, "field_schema": map[string]any{"description": "Identity provider settings", "properties": properties}})
		return
	}
	var patch map[string]json.RawMessage
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &patch); err != nil || len(patch) == 0 {
		modelError(w, 400, "Expected SSO claim mappings")
		return
	}
	key := sha256.Sum256([]byte("minato-sso-setting\x00" + ssoSettingsSalt(s.keys.master)))
	for name, raw := range patch {
		if _, ok := ssoProviderFields[name]; ok {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				modelError(w, 400, "Invalid SSO provider field")
				return
			}
			if strings.HasSuffix(name, "_secret") && value == "********" {
				delete(patch, name)
				continue
			}
			patch[name], _ = json.Marshal("minato-encrypted:" + sealState([]byte(value), &key))
		} else if name != "role_mappings" && name != "team_mappings" {
			modelError(w, 400, "Unsupported SSO settings field")
			return
		}
	}
	updated, err := updateSSOMappings(ctx, store.db, patch)
	if err != nil {
		modelError(w, 400, "Could not validate or store SSO mappings")
		return
	}
	modelJSON(w, 200, map[string]any{"message": "SSO settings updated successfully", "status": "success", "settings": updated})
}

func updateSSOMappings(ctx context.Context, db *sql.DB, patch map[string]json.RawMessage) (storedSSOMappings, error) {
	var mappings storedSSOMappings
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mappings, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('minato:sso-config',0))`); err != nil {
		return mappings, err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT sso_settings FROM `+ssoConfigTable+` WHERE id='sso_config' FOR UPDATE`).Scan(&raw)
	values := make(map[string]json.RawMessage)
	if err == nil {
		if err := decodePolicyCondition(raw, &values); err != nil {
			return mappings, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return mappings, err
	}
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	for key, value := range patch {
		values[key] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		return mappings, err
	}
	if err := json.Unmarshal(data, &mappings); err != nil {
		return mappings, err
	}
	if err := mappings.validate(); err != nil {
		return mappings, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+ssoConfigTable+` (id,sso_settings) VALUES('sso_config',$1)
		ON CONFLICT (id) DO UPDATE SET sso_settings=EXCLUDED.sso_settings,updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'`, string(data)); err != nil {
		return mappings, err
	}
	return mappings, tx.Commit()
}
