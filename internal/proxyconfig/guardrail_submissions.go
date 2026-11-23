package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

const submissionColumns = "guardrail_id,guardrail_name,litellm_params,guardrail_info,team_id,status,submitted_at,reviewed_at,created_at,updated_at"

type guardrailSubmission struct {
	record              guardrailRecord
	status              string
	submitted, reviewed *time.Time
	created, updated    time.Time
}

func (keys *KeyRegistry) GuardrailTeam(r *http.Request) string {
	caller, _ := keys.identifyResourceCaller(r)
	return caller.team
}

func (keys *KeyRegistry) guardrailSubmissionTeams(caller memoryCaller) []string {
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	teams := []string{}
	for id, team := range keys.state.Teams {
		if !team.Blocked && (id == caller.team || (caller.user != "" && memberIndex(team, caller.user) >= 0)) {
			teams = append(teams, id)
		}
	}
	return teams
}

func scanSubmission(row interface{ Scan(...any) error }) (guardrailSubmission, error) {
	var result guardrailSubmission
	var params, info []byte
	err := row.Scan(&result.record.ID, &result.record.Spec.Name, &params, &info, &result.record.Spec.Team, &result.status,
		&result.submitted, &result.reviewed, &result.created, &result.updated)
	if err != nil {
		return result, err
	}
	if err := decodePolicyCondition(params, &result.record.Spec.Params); err != nil {
		return result, err
	}
	_ = decodePolicyCondition(info, &result.record.Info)
	return result, nil
}

func (entry guardrailSubmission) public() map[string]any {
	spec := entry.record.Spec
	return map[string]any{
		"guardrail_id": entry.record.ID, "guardrail_name": spec.Name, "team_id": spec.Team, "status": entry.status,
		"team_guardrail": true, "guardrail_info": entry.record.Info,
		"submitted_by_user_id": entry.record.Info["submitted_by_user_id"], "submitted_by_email": entry.record.Info["submitted_by_email"],
		"submitted_at": entry.submitted, "reviewed_at": entry.reviewed, "created_at": entry.created, "updated_at": entry.updated,
		"litellm_params": map[string]any{"guardrail": spec.Params.Provider, "mode": spec.Params.Mode, "default_on": spec.Params.Default, "api_base": spec.Params.APIBase},
	}
}

func (registry *guardrailRegistry) submitGuardrail(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, 401, "Proxy token required")
		return
	}
	var request struct {
		genericGuardrailSpec
		TeamID string         `json:"team_id"`
		Info   map[string]any `json:"guardrail_info"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &request); err != nil {
		modelError(w, 400, "Invalid guardrail submission")
		return
	}
	request.Team = request.TeamID
	if request.Team == "" {
		request.Team = caller.team
	}
	if request.Team == "" {
		modelError(w, 400, "A team ID is required")
		return
	}
	registry.keys.mu.RLock()
	team, exists := registry.keys.state.Teams[request.Team]
	email := registry.keys.state.Users[caller.user].Email
	allowed := exists && !team.Blocked && (caller.admin || caller.team == request.Team ||
		(caller.user != "" && memberIndex(team, caller.user) >= 0))
	registry.keys.mu.RUnlock()
	if !allowed {
		modelError(w, 403, "Team membership required")
		return
	}
	if _, err := prepareGenericGuardrails([]genericGuardrailSpec{request.genericGuardrailSpec}); err != nil {
		modelError(w, 400, "Invalid team guardrail configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, guard := range registry.static {
		if guard.Name == request.Name {
			modelError(w, 400, "Guardrail name already configured")
			return
		}
	}
	if request.Info == nil {
		request.Info = map[string]any{}
	}
	request.Info["submitted_by_user_id"] = caller.user
	request.Info["submitted_by_email"] = email
	request.Info["team_guardrail"] = true
	params, _ := json.Marshal(request.Params)
	info, _ := json.Marshal(request.Info)
	id := newEntityID()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var submitted time.Time
	err := registry.db.QueryRowContext(ctx, `INSERT INTO `+guardrailTable+`
		(guardrail_id,guardrail_name,litellm_params,guardrail_info,team_id,status,submitted_at)
		VALUES ($1,$2,$3,$4,$5,'pending_review',CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
		ON CONFLICT (guardrail_name) DO NOTHING RETURNING submitted_at`, id, request.Name, string(params), string(info), request.Team).Scan(&submitted)
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, 400, "Guardrail name already configured")
		return
	}
	if err != nil {
		modelError(w, 500, "Could not store guardrail submission")
		return
	}
	modelJSON(w, 200, map[string]any{"guardrail_id": id, "guardrail_name": request.Name, "status": "pending_review", "submitted_at": submitted})
}

func (registry *guardrailRegistry) readSubmissions(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, 401, "Proxy token required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	teams := registry.keys.guardrailSubmissionTeams(caller)
	const visible = ` FROM ` + guardrailTable + ` WHERE team_id IS NOT NULL AND ($1 OR team_id=ANY($2))`
	if id := r.PathValue("guardrail_id"); id != "" {
		entry, err := scanSubmission(registry.db.QueryRowContext(ctx, `SELECT `+submissionColumns+visible+` AND guardrail_id=$3`, caller.admin, pq.Array(teams), id))
		if errors.Is(err, sql.ErrNoRows) {
			modelError(w, 404, "Guardrail submission not found")
		} else if err != nil {
			modelError(w, 500, "Could not read guardrail submission")
		} else {
			modelJSON(w, 200, entry.public())
		}
		return
	}
	rows, err := registry.db.QueryContext(ctx, `SELECT `+submissionColumns+visible+` ORDER BY created_at DESC,guardrail_id`, caller.admin, pq.Array(teams))
	if err != nil {
		modelError(w, 500, "Could not list guardrail submissions")
		return
	}
	defer rows.Close()
	summary := map[string]int{"total": 0, "active": 0, "pending_review": 0, "rejected": 0}
	result := []map[string]any{}
	query := r.URL.Query()
	team, status, search := query.Get("team_id"), query.Get("status"), strings.ToLower(query.Get("search"))
	for rows.Next() {
		entry, err := scanSubmission(rows)
		if err != nil {
			modelError(w, 500, "Could not read guardrail submissions")
			return
		}
		summary["total"]++
		summary[entry.status]++
		description, _ := entry.record.Info["description"].(string)
		if (team == "" || team == entry.record.Spec.Team) && (status == "" || status == entry.status) &&
			(strings.Contains(strings.ToLower(entry.record.Spec.Name), search) || strings.Contains(strings.ToLower(description), search)) {
			result = append(result, entry.public())
		}
	}
	if rows.Err() != nil {
		modelError(w, 500, "Could not list guardrail submissions")
		return
	}
	modelJSON(w, 200, map[string]any{"submissions": result, "summary": summary})
}

func (registry *guardrailRegistry) reviewSubmission(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.requireMaster(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := r.PathValue("guardrail_id")
	approve := strings.HasSuffix(r.Pattern, "/approve")
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, err := scanSubmission(registry.db.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM `+guardrailTable+` WHERE guardrail_id=$1 AND team_id IS NOT NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, 404, "Guardrail submission not found")
		return
	}
	if err != nil {
		modelError(w, 500, "Could not read guardrail submission")
		return
	}
	if entry.status != "pending_review" {
		modelError(w, 400, "Guardrail is not pending review")
		return
	}
	status, message := "rejected", "Guardrail rejected"
	next := maps.Clone(registry.rows)
	if approve {
		status, message = "active", "Guardrail approved"
		next[id] = entry.record
	}
	guards, err := registry.compile(next)
	if err != nil {
		modelError(w, 400, "Invalid team guardrail")
		return
	}
	err = registry.db.QueryRowContext(ctx, `UPDATE `+guardrailTable+` SET status=$2,reviewed_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC',updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
		WHERE guardrail_id=$1 AND status='pending_review' RETURNING guardrail_id`, id, status).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, 409, "Guardrail was already reviewed")
		return
	}
	if err != nil {
		modelError(w, 500, "Could not review guardrail submission")
		return
	}
	registry.rows = next
	registry.server.ReplaceGuardrails(guards)
	modelJSON(w, 200, map[string]string{"guardrail_id": id, "status": status, "message": message})
}
