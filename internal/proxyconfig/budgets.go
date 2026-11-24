package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

const budgetTable = `"LiteLLM_BudgetTable"`

type budgetRecord struct {
	ID string `json:"budget_id"`
	Max *float64 `json:"max_budget"`
	Soft *float64 `json:"soft_budget"`
	Parallel *int `json:"max_parallel_requests"`
	TPM *int `json:"tpm_limit"`
	RPM *int `json:"rpm_limit"`
	Duration *string `json:"budget_duration"`
	Reset *time.Time `json:"budget_reset_at"`
	Model map[string]any `json:"model_max_budget"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string `json:"created_by"`
	UpdatedBy string `json:"updated_by"`
}

func openBudgetTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+budgetTable+` (
		budget_id TEXT PRIMARY KEY,max_budget DOUBLE PRECISION,soft_budget DOUBLE PRECISION,
		max_parallel_requests INTEGER,tpm_limit INTEGER,rpm_limit INTEGER,budget_duration TEXT,budget_reset_at TIMESTAMPTZ,
		model_max_budget JSONB,created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	ALTER TABLE `+budgetTable+` ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT 'default_user_id';
	ALTER TABLE `+budgetTable+` ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT 'default_user_id';
	ALTER TABLE `+budgetTable+` ADD COLUMN IF NOT EXISTS budget_period_started_at TIMESTAMPTZ;`)
	return err
}

func WithBudgetManagement(next http.Handler,db *sql.DB,keys *KeyRegistry, timezone string) (http.Handler,error) {
	if db==nil {return next,nil}
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
	defer cancel()
	if err:=openBudgetTable(ctx,db);err!=nil {return nil,err}
	api:=&budgetAPI{db:db,keys:keys, zone: budgetZone(timezone)}
	mux:=http.NewServeMux()
	mux.HandleFunc("POST /budget/new",api.write)
	mux.HandleFunc("POST /budget/update",api.write)
	mux.HandleFunc("POST /budget/info",api.info)
	mux.HandleFunc("GET /budget/settings",api.infoQuery)
	mux.HandleFunc("GET /budget/list",api.list)
	mux.HandleFunc("POST /budget/delete",api.delete)
	mux.Handle("/",next)
	return mux,nil
}

type budgetAPI struct {db *sql.DB; keys *KeyRegistry; zone *time.Location}

func (api *budgetAPI) admin(w http.ResponseWriter,r *http.Request) bool {
	return api.keys!=nil && api.keys.requireMaster(w,r)
}

const budgetFields = `budget_id,max_budget,soft_budget,max_parallel_requests,tpm_limit,rpm_limit,budget_duration,budget_reset_at,model_max_budget`
const budgetColumns = budgetFields + `,created_at,updated_at,created_by,updated_by`

func scanBudget(scanner interface{ Scan(...any) error }) (budgetRecord, error) {
	var row budgetRecord
	var model []byte
	err := scanner.Scan(&row.ID, &row.Max, &row.Soft, &row.Parallel, &row.TPM, &row.RPM, &row.Duration, &row.Reset, &model,
		&row.CreatedAt, &row.UpdatedAt, &row.CreatedBy, &row.UpdatedBy)
	if err == nil && len(model) > 0 {
		err = json.Unmarshal(model, &row.Model)
	}
	return row, err
}

func (api *budgetAPI) write(w http.ResponseWriter, r *http.Request) {
	if !api.admin(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var input budgetRecord
	if err != nil || decodeModelJSON(strings.NewReader(string(body)), &input) != nil || strings.TrimSpace(string(body)) == "null" {
		modelError(w, http.StatusBadRequest, "Invalid budget configuration")
		return
	}
	for _, value := range []*float64{input.Max, input.Soft} {
		if value != nil && *value < 0 {
			modelError(w, http.StatusBadRequest, "Budget amounts must be non-negative")
			return
		}
	}
	for _, limit := range []*int{input.Parallel, input.TPM, input.RPM} {
		if limit != nil && (*limit < 0 || *limit > 2147483647) {
			modelError(w, http.StatusBadRequest, "Budget limits must be non-negative 32-bit integers")
			return
		}
	}
	raw, _ := json.Marshal(input.Model)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var statement string
	var args []any
	if r.URL.Path == "/budget/update" {
		if strings.TrimSpace(input.ID) == "" {
			modelError(w, http.StatusBadRequest, "budget_id is required")
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid budget configuration")
			return
		}
		assignments := []string{"updated_at=CURRENT_TIMESTAMP", "updated_by='default_user_id'"}
		args = []any{input.ID}
		for _, field := range []struct {
			name string
			value any
		}{
			{"max_budget", input.Max}, {"soft_budget", input.Soft}, {"max_parallel_requests", input.Parallel},
			{"tpm_limit", input.TPM}, {"rpm_limit", input.RPM}, {"budget_duration", input.Duration},
			{"budget_reset_at", input.Reset}, {"model_max_budget", string(raw)},
		} {
			if _, present := fields[field.name]; present {
				args = append(args, field.value)
				assignments = append(assignments, field.name+"=$"+strconv.Itoa(len(args)))
			}
		}
		statement = "UPDATE "+budgetTable+" SET "+strings.Join(assignments, ",")+" WHERE budget_id=$1"
	} else {
		if input.ID == "" {
			input.ID = newEntityID()
		}
		if input.Duration != nil && input.Reset == nil {
			reset, err := nextBudgetReset(time.Now().In(api.zone), *input.Duration)
			if err != nil {
				modelError(w, http.StatusBadRequest, "Invalid budget duration")
				return
			}
			input.Reset = &reset
		}
		statement = "INSERT INTO "+budgetTable+" ("+budgetFields+",created_by,updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'default_user_id','default_user_id')"
		args = []any{input.ID, input.Max, input.Soft, input.Parallel, input.TPM, input.RPM, input.Duration, input.Reset, string(raw)}
	}
	row, err := scanBudget(api.db.QueryRowContext(ctx, statement+" RETURNING "+budgetColumns, args...))
	var postgres *pq.Error
	switch {
	case errors.Is(err, sql.ErrNoRows):
		modelError(w, http.StatusNotFound, "Budget not found")
	case errors.As(err, &postgres) && postgres.Code == "23505":
		modelError(w, http.StatusBadRequest, fmt.Sprintf("Budget with id '%s' already exists.", input.ID))
	case err != nil:
		modelError(w, http.StatusInternalServerError, "Could not store budget")
	default:
		modelJSON(w, http.StatusOK, row)
	}
}

func (api *budgetAPI) info(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	var input struct {Budgets []string `json:"budgets"`}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || len(input.Budgets) == 0 {
		modelError(w, http.StatusBadRequest, "Supply at least one budget ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := api.db.QueryContext(ctx, "SELECT "+budgetColumns+" FROM "+budgetTable+
		" WHERE budget_id=ANY($1) ORDER BY budget_id", pq.Array(input.Budgets))
	result, err := budgetRows(rows, err)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read budgets")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func budgetRows(rows *sql.Rows, err error) ([]budgetRecord, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []budgetRecord{}
	for rows.Next() {
		row, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (api *budgetAPI) infoQuery(w http.ResponseWriter, r *http.Request) {
	if !api.admin(w, r) {
		return
	}
	id := r.URL.Query().Get("budget_id")
	if strings.TrimSpace(id) == "" {
		modelError(w, http.StatusBadRequest, "Budget ID is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := scanBudget(api.db.QueryRowContext(ctx, "SELECT "+budgetColumns+" FROM "+budgetTable+" WHERE budget_id=$1", id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusInternalServerError, "Could not read budget")
		return
	}
	fields := []struct {
		name, kind, description string
		value any
	}{
		{"max_budget", "Float", "The maximum spend for the budget.", row.Max},
		{"soft_budget", "Float", "The spend threshold for budget alerts.", row.Soft},
		{"max_parallel_requests", "Integer", "The maximum number of parallel requests.", row.Parallel},
		{"tpm_limit", "Integer", "The tokens per minute limit.", row.TPM},
		{"rpm_limit", "Integer", "The requests per minute limit.", row.RPM},
		{"budget_duration", "String", "The budget reset period.", row.Duration},
		{"model_max_budget", "Object", "The per-model budget limits.", row.Model},
	}
	settings := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		settings = append(settings, map[string]any{
			"field_name": field.name, "field_type": field.kind, "field_description": field.description,
			"field_value": field.value, "stored_in_db": true, "field_default_value": nil,
		})
	}
	modelJSON(w, http.StatusOK, settings)
}

func (api *budgetAPI) list(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := api.db.QueryContext(ctx, "SELECT "+budgetColumns+" FROM "+budgetTable+" ORDER BY created_at DESC,budget_id")
	result, err := budgetRows(rows, err)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not list budgets")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func (api *budgetAPI) delete(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	var input struct {ID string `json:"id"`}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.ID == "" {
		modelError(w, http.StatusBadRequest, "Budget ID is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := scanBudget(api.db.QueryRowContext(ctx, "DELETE FROM "+budgetTable+" WHERE budget_id=$1 RETURNING "+budgetColumns, input.ID))
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusNotFound, "Budget not found")
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not delete budget")
		return
	}
	modelJSON(w, http.StatusOK, row)
}
