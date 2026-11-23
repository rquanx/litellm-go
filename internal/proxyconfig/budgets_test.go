package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPostgresBudgetManagementLifecycle(t *testing.T) {
	dsn:=os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn=="" {t.Skip("MINATO_TEST_POSTGRES_URL is unset")}
	db,err:=OpenPostgresState(dsn)
	if err!=nil {t.Fatal(err)}
	defer db.Close()
	keys,err:=LoadKeys("master","")
	if err!=nil {t.Fatal(err)}
	handler,err:=WithBudgetManagement(http.NotFoundHandler(),db,keys)
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
	call(http.MethodGet,"/budget/list","",200)
	call(http.MethodGet,"/budget/settings?budget_id=fixture-budget","",200)
	call(http.MethodPost,"/budget/update",`{"budget_id":"fixture-budget","soft_budget":8}`,200)
	call(http.MethodPost,"/budget/info",`{"budgets":["fixture-budget"]}`,200)
	call(http.MethodPost,"/budget/delete",`{"id":"fixture-budget"}`,200)
}
