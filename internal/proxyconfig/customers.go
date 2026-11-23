package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

const customerTable = `"LiteLLM_EndUserTable"`
const customerColumns = `user_id,alias,spend,blocked,allowed_model_region,default_model,budget_id,object_permission_id`

type CustomerRegistry struct {
	db   *sql.DB
	keys *KeyRegistry
}

type customer struct {
	ID    string  `json:"user_id"`
	Alias *string `json:"alias"`
	Spend float64 `json:"spend"`
	Blocked bool  `json:"blocked"`
	Region *string `json:"allowed_model_region"`
	DefaultModel *string `json:"default_model"`
	Budget *string `json:"budget_id"`
	Permission *string `json:"object_permission_id"`
}

func OpenCustomers(ctx context.Context, db *sql.DB, keys *KeyRegistry) (*CustomerRegistry, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+customerTable+` (
		user_id TEXT PRIMARY KEY, alias TEXT, spend DOUBLE PRECISION NOT NULL DEFAULT 0,
		blocked BOOLEAN NOT NULL DEFAULT FALSE, allowed_model_region TEXT, default_model TEXT, budget_id TEXT,
		object_permission_id TEXT
	)`)
	if err != nil {
		return nil, errors.New("could not initialize end-user storage")
	}
	if err := openBudgetTable(ctx, db); err != nil {
		return nil, err
	}
	return &CustomerRegistry{db: db, keys: keys}, nil
}

func (registry *CustomerRegistry) Policy(ctx context.Context, id string) (runtime.CustomerPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var policy runtime.CustomerPolicy
	var spent float64
	var limit *float64
	err := registry.db.QueryRowContext(ctx, `SELECT c.blocked,COALESCE(c.allowed_model_region,''),COALESCE(c.default_model,''),c.spend,b.max_budget
		FROM `+customerTable+` c LEFT JOIN `+budgetTable+` b ON b.budget_id=c.budget_id WHERE c.user_id=$1`, id).
		Scan(&policy.Blocked, &policy.Region, &policy.DefaultModel, &spent, &limit)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.CustomerPolicy{}, nil
	}
	if err != nil || limit == nil || policy.Blocked { return policy, err }
	if spent > *limit {
		policy.BudgetError = fmt.Sprintf("ExceededBudget: End User=%s over budget. Spend=%g, Budget=%g", id, spent, *limit)
	}
	return policy, nil
}

func (registry *CustomerRegistry) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	for _, prefix := range []string{"/customer", "/end_user"} {
		for _, name := range []string{"new", "update", "block", "unblock", "delete"} {
			mux.HandleFunc("POST "+prefix+"/"+name, registry.serve)
		}
		for _, name := range []string{"info", "list"} {
			mux.HandleFunc("GET "+prefix+"/"+name, registry.serve)
		}
	}
	mux.Handle("/", next)
	return mux
}

func scanCustomer(row interface{ Scan(...any) error }) (customer, error) {
	var result customer
	err := row.Scan(&result.ID, &result.Alias, &result.Spend, &result.Blocked, &result.Region,
		&result.DefaultModel, &result.Budget, &result.Permission)
	return result, err
}

func validCustomerID(id string) bool {
	return len(id) > 0 && strings.TrimSpace(id) != "" && !strings.ContainsRune(id, 0)
}

func (registry *CustomerRegistry) serve(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.requireMaster(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	var payload any
	var err error
	switch action {
	case "info":
		id := r.URL.Query().Get("end_user_id")
		if !validCustomerID(id) {
			modelError(w, http.StatusBadRequest, "end_user_id is required")
			return
		}
		payload, err = scanCustomer(registry.db.QueryRowContext(ctx, `SELECT `+customerColumns+` FROM `+customerTable+` WHERE user_id=$1`, id))
	case "list":
		payload, err = registry.list(ctx)
	case "new", "update":
		payload, err = registry.write(ctx, http.MaxBytesReader(w, r.Body, 1<<20), action == "new")
	case "block", "unblock", "delete":
		var input struct{ IDs []string `json:"user_ids"` }
		if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.IDs) == 0 {
			modelError(w, http.StatusBadRequest, "user_ids is required")
			return
		}
		for _, id := range input.IDs {
			if !validCustomerID(id) {
				modelError(w, http.StatusBadRequest, "Invalid customer ID")
				return
			}
		}
		payload, err = registry.bulk(ctx, action, input.IDs)
	}
	if err != nil {
		status, message := http.StatusInternalServerError, "Could not manage end users"
		var invalid customerInputError
		var postgres *pq.Error
		switch {
		case errors.As(err, &invalid):
			status, message = http.StatusBadRequest, err.Error()
		case errors.Is(err, sql.ErrNoRows):
			status, message = http.StatusNotFound, "Customer not found"
		case errors.As(err, &postgres) && postgres.Code == "23505":
			status, message = http.StatusBadRequest, "Customer already exists"
		}
		modelError(w, status, message)
		return
	}
	modelJSON(w, http.StatusOK, payload)
}

func (registry *CustomerRegistry) list(ctx context.Context) ([]customer, error) {
	rows, err := registry.db.QueryContext(ctx, `SELECT `+customerColumns+` FROM `+customerTable+` ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []customer{}
	for rows.Next() {
		entry, err := scanCustomer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

type customerInputError struct{ error }

func (registry *CustomerRegistry) write(ctx context.Context, body io.Reader, create bool) (customer, error) {
	var fields map[string]json.RawMessage
	if err := decodeModelJSON(body, &fields); err != nil {
		return customer{}, customerInputError{errors.New("Invalid customer request")}
	}
	var id string
	if json.Unmarshal(fields["user_id"], &id) != nil || !validCustomerID(id) {
		return customer{}, customerInputError{errors.New("user_id is required")}
	}
	for _, name := range []string{"max_budget", "budget_duration", "object_permission", "tpm_limit", "rpm_limit", "object_permission_id"} {
		if value := fields[name]; len(value) != 0 && string(value) != "null" {
			return customer{}, customerInputError{errors.New("Unsupported customer setting: " + name)}
		}
	}
	var alias, region, defaultModel, budgetID *string
	var blocked *bool
	var spend *float64
	for name, target := range map[string]any{"alias": &alias, "blocked": &blocked, "spend": &spend,
		"allowed_model_region": &region, "default_model": &defaultModel, "budget_id": &budgetID} {
		if value := fields[name]; len(value) > 0 && json.Unmarshal(value, target) != nil {
			return customer{}, customerInputError{errors.New("Invalid customer setting: " + name)}
		}
	}
	if spend != nil && *spend < 0 {
		return customer{}, customerInputError{errors.New("spend must be non-negative")}
	}
	if region != nil && *region != "eu" && *region != "us" {
		return customer{}, customerInputError{errors.New("allowed_model_region must be eu or us")}
	}
	if budgetID != nil {
		var exists bool
		if err := registry.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+budgetTable+` WHERE budget_id=$1)`, *budgetID).Scan(&exists); err != nil { return customer{}, err }
		if !exists { return customer{}, customerInputError{errors.New("Budget not found")} }
	}
	if create {
		return scanCustomer(registry.db.QueryRowContext(ctx, `INSERT INTO `+customerTable+
			` (user_id,alias,spend,blocked,allowed_model_region,default_model,budget_id)
			VALUES ($1,$2,COALESCE($3,0),COALESCE($4,false),$5,$6,$7) RETURNING `+customerColumns,
			id, alias, spend, blocked, region, defaultModel, budgetID))
	}
	_, aliasSupplied := fields["alias"]
	_, regionSupplied := fields["allowed_model_region"]
	_, modelSupplied := fields["default_model"]
	_, budgetSupplied := fields["budget_id"]
	return scanCustomer(registry.db.QueryRowContext(ctx, `UPDATE `+customerTable+`
		SET alias=CASE WHEN $2 THEN $3 ELSE alias END, spend=COALESCE($4,spend), blocked=COALESCE($5,blocked),
		allowed_model_region=CASE WHEN $6 THEN $7 ELSE allowed_model_region END,
		default_model=CASE WHEN $8 THEN $9 ELSE default_model END,
		budget_id=CASE WHEN $10 THEN $11 ELSE budget_id END
		WHERE user_id=$1 RETURNING `+customerColumns, id, aliasSupplied, alias, spend, blocked, regionSupplied, region, modelSupplied, defaultModel, budgetSupplied, budgetID))
}

func (registry *CustomerRegistry) bulk(ctx context.Context, action string, ids []string) (any, error) {
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(ids))
	result := make([]customer, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var row *sql.Row
		switch action {
		case "block":
			row = tx.QueryRowContext(ctx, `INSERT INTO `+customerTable+` (user_id,blocked) VALUES ($1,true)
				ON CONFLICT (user_id) DO UPDATE SET blocked=true RETURNING `+customerColumns, id)
		case "unblock":
			row = tx.QueryRowContext(ctx, `UPDATE `+customerTable+` SET blocked=false WHERE user_id=$1 RETURNING `+customerColumns, id)
		case "delete":
			row = tx.QueryRowContext(ctx, `DELETE FROM `+customerTable+` WHERE user_id=$1 RETURNING `+customerColumns, id)
		}
		entry, err := scanCustomer(row)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if action == "delete" {
		return map[string]any{"deleted_customers": len(result), "message": "Deleted customers: " + strings.Join(ids, ", ")}, nil
	}
	return map[string]any{action + "ed_users": result}, nil
}
