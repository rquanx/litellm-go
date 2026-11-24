package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresBudgetManagementLifecycle(t *testing.T) {
	dsn:=os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn=="" {t.Skip("MINATO_TEST_POSTGRES_URL is unset")}
	db,err:=OpenPostgresState(dsn)
	if err!=nil {t.Fatal(err)}
	defer db.Close()
	keys,err:=LoadKeys("master","")
	if err!=nil {t.Fatal(err)}
	handler,err:=WithBudgetManagement(http.NotFoundHandler(),db,keys, "")
	if err!=nil {t.Fatal(err)}
	call:=func(method,path,body string,status int) string {
		r:=httptest.NewRequest(method,path,strings.NewReader(body))
		r.Header.Set("Authorization","Bearer master")
		w:=httptest.NewRecorder()
		handler.ServeHTTP(w,r)
		if w.Code!=status {t.Fatalf("%s returned %d %s",path,w.Code,w.Body)}
		return w.Body.String()
	}
	created:=call(http.MethodPost,"/budget/new",`{"budget_id":"fixture-budget","max_budget":12.5,"budget_duration":"1d","model_max_budget":{"demo":5}}`,200)
	var row budgetRecord
	if json.Unmarshal([]byte(created),&row)!=nil || row.ID!="fixture-budget" {t.Fatalf("unexpected budget: %s",created)}
	if row.Reset == nil {
		t.Fatal("expected an initial reset for a recurring budget")
	}
	call(http.MethodPost, "/budget/new", `{"budget_id":"fixture-budget","max_budget":1}`, 400)
	customers, err := OpenCustomers(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	const customerID = "fixture-budget-reset-customer"
	if _, err := db.Exec(`INSERT INTO `+customerTable+` (user_id,budget_id,spend) VALUES ($1,$2,20)`, customerID, row.ID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DELETE FROM `+customerTable+` WHERE user_id=$1`, customerID)
	policy, err := customers.Policy(context.Background(), customerID)
	if err != nil || policy.BudgetError == "" {
		t.Fatalf("expected an exceeded budget before reset: %+v %v", policy, err)
	}
	zone := budgetZone("Asia/Shanghai")
	now := row.Reset.In(zone).Add(time.Hour)
	if err := customers.resetExpiredBudgets(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	policy, err = customers.Policy(context.Background(), customerID)
	if err != nil || policy.BudgetError != "" {
		t.Fatalf("expected inference to be allowed after reset: %+v %v", policy, err)
	}
	var reset time.Time
	if err := db.QueryRow(`SELECT budget_reset_at FROM `+budgetTable+` WHERE budget_id=$1`, row.ID).Scan(&reset); err != nil {
		t.Fatal(err)
	}
	local := now.In(zone)
	expected := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, zone)
	if !reset.Equal(expected) {
		t.Fatalf("reset at %s, want %s", reset, expected)
	}
	call(http.MethodPost, "/budget/update", `{"budget_id":"fixture-budget","max_budget":-1}`, 400)
	call(http.MethodPost, "/budget/update", `{"soft_budget":8}`, 400)
	if listed := call(http.MethodGet, "/budget/list", "", 200); !strings.HasPrefix(listed, "[") {
		t.Fatalf("expected a list of budgets, got %s", listed)
	}
	settings := call(http.MethodGet, "/budget/settings?budget_id=fixture-budget", "", 200)
	if !strings.HasPrefix(settings, "[") || !strings.Contains(settings, `"field_name":"max_budget"`) ||
		!strings.Contains(settings, `"field_value":12.5`) {
		t.Fatalf("unexpected budget settings: %s", settings)
	}
	call(http.MethodPost,"/budget/update",`{"budget_id":"fixture-budget","soft_budget":8}`,200)
	call(http.MethodPost, "/budget/update", `{"budget_id":"fixture-budget","budget_duration":null}`, 200)
	results := call(http.MethodPost, "/budget/info", `{"budgets":["fixture-budget","missing-budget"]}`, 200)
	var records []budgetRecord
	if json.Unmarshal([]byte(results), &records) != nil || len(records) != 1 ||
		records[0].Max == nil || *records[0].Max != 12.5 || records[0].Duration != nil {
		t.Fatalf("unexpected updated budgets: %s", results)
	}
	deleted := call(http.MethodPost, "/budget/delete", `{"id":"fixture-budget"}`, 200)
	if json.Unmarshal([]byte(deleted), &row) != nil || row.ID != "fixture-budget" || row.CreatedAt.IsZero() || row.CreatedBy == "" {
		t.Fatalf("unexpected deleted budget: %s", deleted)
	}
}
