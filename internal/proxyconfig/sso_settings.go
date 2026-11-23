package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const ssoConfigTable = `"LiteLLM_SSOConfig"`

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
		if role.Provider == "" || role.Claim == "" || (role.Default != "" && !internalSSORole(role.Default)) {
			return errors.New("invalid SSO internal role mapping")
		}
		for name := range role.Groups {
			if !internalSSORole(name) {
				return errors.New("invalid SSO internal role mapping")
			}
		}
	}
	return nil
}

func loadSSOMappings(ctx context.Context, db *sql.DB) (storedSSOMappings, error) {
	var mappings storedSSOMappings
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT sso_settings FROM `+ssoConfigTable+` WHERE id='sso_config'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return mappings, nil
	}
	if err != nil {
		return mappings, err
	}
	err = decodePolicyCondition(raw, &mappings)
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
		settings, err := loadSSOMappings(ctx, store.db)
		if err != nil {
			modelError(w, 500, "Could not read SSO settings")
			return
		}
		modelJSON(w, 200, map[string]any{"values": settings, "field_schema": map[string]any{"description": "Identity provider claim mappings",
			"properties": map[string]any{"role_mappings": map[string]string{"type": "object"}, "team_mappings": map[string]string{"type": "object"}}}})
		return
	}
	var patch map[string]json.RawMessage
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &patch); err != nil || len(patch) == 0 {
		modelError(w, 400, "Expected SSO claim mappings")
		return
	}
	for name := range patch {
		if name != "role_mappings" && name != "team_mappings" {
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
