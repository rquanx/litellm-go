package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

type tagRegistry struct {
	db *sql.DB
	keys *KeyRegistry
	zone *time.Location
	spendEnabled bool
}

type storedTag struct {
	Name string `json:"name"`
	Description *string `json:"description"`
	Models []string `json:"models"`
	ModelInfo map[string]string `json:"model_info"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy *string `json:"created_by"`
	Budget json.RawMessage `json:"litellm_budget_table,omitempty"`
}

type tagWrite struct {
	Name string `json:"name"`
	Description *string `json:"description"`
	Models []string `json:"models"`
	ModelInfo map[string]string `json:"model_info"`
	BudgetID *string `json:"budget_id"`
	Max *float64 `json:"max_budget"`
	Soft *float64 `json:"soft_budget"`
	Parallel *int `json:"max_parallel_requests"`
	TPM *int `json:"tpm_limit"`
	RPM *int `json:"rpm_limit"`
	Duration *string `json:"budget_duration"`
	ModelBudget map[string]any `json:"model_max_budget"`
}

func WithTagManagement(next http.Handler, db *sql.DB, keys *KeyRegistry, timezone string, spendEnabled bool) http.Handler {
	registry := tagRegistry{db: db, keys: keys, zone: budgetZone(timezone), spendEnabled: spendEnabled}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tag/new", registry.serve)
	mux.HandleFunc("POST /tag/update", registry.serve)
	mux.HandleFunc("POST /tag/info", registry.serve)
	mux.HandleFunc("GET /tag/list", registry.serve)
	mux.HandleFunc("POST /tag/delete", registry.serve)
	mux.Handle("/", next)
	return mux
}

func (registry tagRegistry) serve(w http.ResponseWriter, r *http.Request) {
	user := ""
	if r.URL.Path == "/tag/list" {
		caller, valid := registry.keys.identifyResourceCaller(r)
		if !valid {
			modelError(w, http.StatusUnauthorized, "Proxy token required")
			return
		}
		if !caller.admin {
			if caller.user == "" {
				modelError(w, http.StatusForbidden, "A user-bound key is required")
				return
			}
			user = caller.user
		}
	} else if !registry.keys.requireMaster(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	var response any
	var err error
	switch r.URL.Path {
	case "/tag/list":
		if registry.spendEnabled {
			response, err = registry.list(ctx, r.URL.Query(), user)
		} else if user == "" {
			response, err = registry.read(ctx, nil)
		} else {
			response = []storedTag{}
		}
	case "/tag/info":
		var request struct{ Names []string `json:"names"` }
		if decodeModelJSON(body, &request) != nil || len(request.Names) == 0 {
			modelError(w, http.StatusBadRequest, "names is required")
			return
		}
		response, err = registry.read(ctx, request.Names)
	case "/tag/new", "/tag/update":
		var request tagWrite
		if decodeModelJSON(body, &request) != nil || !validCustomerID(request.Name) {
			modelError(w, http.StatusBadRequest, "Invalid tag configuration")
			return
		}
		create := r.URL.Path == "/tag/new"
		var tag storedTag
		tag, err = registry.write(ctx, request, create)
		action := "updated"
		if create {
			action = "created"
		}
		response = map[string]any{"message": fmt.Sprintf("Tag %s %s successfully", request.Name, action), "tag": tag}
	case "/tag/delete":
		var request struct{ Name string `json:"name"` }
		if decodeModelJSON(body, &request) != nil || !validCustomerID(request.Name) {
			modelError(w, http.StatusBadRequest, "name is required")
			return
		}
		err = registry.db.QueryRowContext(ctx, `DELETE FROM `+tagTable+` WHERE tag_name=$1 RETURNING tag_name`, request.Name).Scan(&request.Name)
		response = map[string]string{"message": fmt.Sprintf("Tag %s deleted successfully", request.Name)}
	}
	var invalid customerInputError
	var postgres *pq.Error
	switch {
	case err == nil:
		modelJSON(w, http.StatusOK, response)
	case errors.As(err, &invalid):
		modelError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		modelError(w, http.StatusNotFound, "Tag not found")
	case errors.As(err, &postgres) && postgres.Code == "23505":
		modelError(w, http.StatusBadRequest, "Tag already exists")
	default:
		modelError(w, http.StatusInternalServerError, "Could not manage tags")
	}
}

const tagColumns = `tag_name,description,models,model_info,created_at,updated_at,created_by`

func scanTag(row interface{ Scan(...any) error }, includeBudget bool) (storedTag, error) {
	var tag storedTag
	var modelInfo, budget []byte
	targets := []any{&tag.Name, &tag.Description, pq.Array(&tag.Models), &modelInfo, &tag.CreatedAt, &tag.UpdatedAt, &tag.CreatedBy}
	if includeBudget {
		targets = append(targets, &budget)
	}
	if err := row.Scan(targets...); err != nil {
		return tag, err
	}
	tag.Budget = budget
	tag.ModelInfo = map[string]string{}
	if len(modelInfo) > 0 && string(modelInfo) != "null" {
		if modelInfo[0] == '"' {
			var nested string
			if err := json.Unmarshal(modelInfo, &nested); err != nil {
				return tag, err
			}
			modelInfo = []byte(nested)
		}
		if err := json.Unmarshal(modelInfo, &tag.ModelInfo); err != nil {
			return tag, err
		}
	}
	return tag, nil
}

func (registry tagRegistry) read(ctx context.Context, names []string) (any, error) {
	rows, err := registry.db.QueryContext(ctx, `SELECT `+tagColumns+`,
		(SELECT to_jsonb(b)-'budget_period_started_at' FROM `+budgetTable+` b WHERE b.budget_id=t.budget_id)
		FROM `+tagTable+` t WHERE $1::text[] IS NULL OR tag_name=ANY($1) ORDER BY tag_name`, pq.Array(names))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []storedTag{}
	for rows.Next() {
		tag, err := scanTag(rows, true)
		if err != nil {
			return nil, err
		}
		list = append(list, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if names == nil {
		return list, nil
	}
	byName := make(map[string]storedTag, len(list))
	for _, tag := range list {
		byName[tag.Name] = tag
	}
	for _, name := range names {
		if _, found := byName[name]; !found {
			return nil, sql.ErrNoRows
		}
	}
	return byName, nil
}

func (registry tagRegistry) write(ctx context.Context, request tagWrite, create bool) (storedTag, error) {
	for _, amount := range []*float64{request.Max, request.Soft} {
		if amount != nil && *amount < 0 {
			return storedTag{}, customerInputError{errors.New("Budget amounts must be non-negative")}
		}
	}
	for _, rate := range []*int{request.RPM, request.TPM, request.Parallel} {
		if rate != nil && (*rate < 0 || *rate > 2147483647) {
			return storedTag{}, customerInputError{errors.New("Budget limits must be non-negative 32-bit integers")}
		}
	}
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return storedTag{}, err
	}
	defer tx.Rollback()
	var budget *string
	if !create {
		if err := tx.QueryRowContext(ctx, `SELECT budget_id FROM `+tagTable+` WHERE tag_name=$1 FOR UPDATE`,
			request.Name).Scan(&budget); err != nil {
			return storedTag{}, err
		}
	}
	budget, err = registry.applyBudget(ctx, tx, request, budget)
	if err != nil {
		return storedTag{}, err
	}
	if request.ModelInfo == nil {
		request.ModelInfo = map[string]string{}
	}
	modelInfo, _ := json.Marshal(request.ModelInfo)
	models := pq.Array(append([]string{}, request.Models...))
	query := `INSERT INTO `+tagTable+` (tag_name,description,models,model_info,budget_id,created_by)
		VALUES ($1,$2,$3,$4,$5,'default_user_id') RETURNING `+tagColumns
	if !create {
		query = `UPDATE `+tagTable+` SET description=$2,models=$3,model_info=$4,budget_id=$5,updated_at=CURRENT_TIMESTAMP
			WHERE tag_name=$1 RETURNING `+tagColumns
	}
	tag, err := scanTag(tx.QueryRowContext(ctx, query, request.Name, request.Description, models, string(modelInfo), budget), false)
	if err != nil {
		return tag, err
	}
	return tag, tx.Commit()
}

func (registry tagRegistry) applyBudget(ctx context.Context, tx *sql.Tx, request tagWrite, existing *string) (*string, error) {
	hasChanges := request.Max != nil || request.Soft != nil || request.Parallel != nil || request.TPM != nil ||
		request.RPM != nil || request.Duration != nil || request.ModelBudget != nil
	if hasChanges && (existing != nil || request.BudgetID == nil) {
		var reset *time.Time
		if request.Duration != nil {
			value, err := nextBudgetReset(time.Now().In(registry.zone), *request.Duration)
			if err != nil {
				return nil, customerInputError{err}
			}
			reset = &value
		}
		var modelBudget any
		if request.ModelBudget != nil {
			raw, _ := json.Marshal(request.ModelBudget)
			modelBudget = string(raw)
		}
		query := `UPDATE `+budgetTable+` SET max_budget=COALESCE($2,max_budget),soft_budget=COALESCE($3,soft_budget),
			max_parallel_requests=COALESCE($4,max_parallel_requests),tpm_limit=COALESCE($5,tpm_limit),rpm_limit=COALESCE($6,rpm_limit),
			budget_duration=COALESCE($7,budget_duration),budget_reset_at=COALESCE($8,budget_reset_at),model_max_budget=COALESCE($9,model_max_budget),
			updated_at=CURRENT_TIMESTAMP,updated_by='default_user_id' WHERE budget_id=$1`
		if existing == nil {
			id := newEntityID()
			existing = &id
			query = `INSERT INTO `+budgetTable+` (`+budgetFields+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
		}
		if _, err := tx.ExecContext(ctx, query, *existing, request.Max, request.Soft, request.Parallel,
			request.TPM, request.RPM, request.Duration, reset, modelBudget); err != nil {
			return nil, err
		}
	}
	if request.BudgetID != nil {
		if strings.TrimSpace(*request.BudgetID) == "" {
			return nil, customerInputError{errors.New("Invalid budget_id")}
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+budgetTable+` WHERE budget_id=$1)`, *request.BudgetID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, customerInputError{errors.New("Budget not found")}
		}
		return request.BudgetID, nil
	}
	return existing, nil
}
