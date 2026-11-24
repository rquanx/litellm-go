package proxyconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestVirtualKeyModelSpendWindow(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spend, err := OpenSpendLog(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	customers, err := OpenCustomers(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer origin.Close()
	inputPrice, outputPrice := 0.25, 0.0
	candidate := runtime.Candidate{APIBase: origin.URL, Endpoint: "chat-completions", ModelID: "provider-model",
		UsagePrice: [2]*float64{&inputPrice, &outputPrice}}
	gateway, err := WithBudgetManagement(customers.Handler(keys.Handler(runtime.NewServer(runtime.Config{
		Token: "master", AuthorizeInference: true, AuthorizeKey: keys.Authorize,
		CheckKeyBudget: spend.CheckKeyBudget, CustomerPolicy: customers.Policy, Callback: spend.Start(ctx),
		Routes: []runtime.Route{{Alias: "limited", Candidates: []runtime.Candidate{candidate}},
			{Alias: "unlimited", Candidates: []runtime.Candidate{candidate}},
			{Alias: "rpm", Candidates: []runtime.Candidate{candidate}},
			{Alias: "tpm", Candidates: []runtime.Candidate{candidate}}},
	}))), db, keys, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, token, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		gateway.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s returned %d, expected %d: %s", path, res.Code, status, res.Body)
		}
		return res.Body.String()
	}
	var issued struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"model_max_budget":{"limited":{"budget_limit":0.25,"time_period":"1d"},"rpm":{"rpm_limit":1},"tpm":{"tpm_limit":0}}}`, 200)), &issued); err != nil || issued.Key == "" {
		t.Fatalf("could not issue key: %v", err)
	}
	infer := func(token, model string, status int) string {
		return call("/v1/chat/completions", token, `{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`, status)
	}
	infer(issued.Key, "limited", 200)
	infer(issued.Key, "limited", 200)
	if response := infer(issued.Key, "limited", 429); !strings.Contains(response, "budget_exceeded") {
		t.Fatal(response)
	}
	infer(issued.Key, "unlimited", 200)
	infer(issued.Key, "rpm", 200)
	infer(issued.Key, "rpm", 429)
	infer(issued.Key, "tpm", 429)
	replacement := newKeyToken()
	call("/key/regenerate", "master", `{"key":"`+issued.Key+`","new_key":"`+replacement+`"}`, 200)
	infer(replacement, "limited", 429)
	infer(replacement, "rpm", 429)
	if _, err := db.ExecContext(ctx, `UPDATE minato_proxy_key_model_spend SET reset_at=now()-interval '1 second'
		WHERE key_identity=$1`, keyDigest(issued.Key)); err != nil {
		t.Fatal(err)
	}
	infer(replacement, "limited", 200)
	var posted float64
	if err := db.QueryRowContext(ctx, `SELECT spend FROM minato_proxy_key_model_spend WHERE key_identity=$1 AND model_group='limited'`,
		keyDigest(issued.Key)).Scan(&posted); err != nil || posted != 0.25 || upstreamCalls.Load() != 5 {
		t.Fatalf("model window posted %g across %d provider calls: %v", posted, upstreamCalls.Load(), err)
	}
	call("/key/update", "master", `{"key":"`+replacement+`","model_max_budget":null}`, 200)
	infer(replacement, "rpm", 200)

	budgetID, customerID := newEntityID(), newEntityID()
	call("/budget/new", "master", `{"budget_id":"`+budgetID+`","model_max_budget":{"limited":{"max_budget":0.1,"budget_duration":"1d"}}}`, 200)
	call("/customer/new", "master", `{"user_id":"`+customerID+`","budget_id":"`+budgetID+`"}`, 200)
	call("/key/update", "master", `{"key":"`+replacement+`","model_max_budget":{"limited":{"max_budget":5,"budget_duration":"1d"}}}`, 200)
	body := `{"model":"limited","user":"`+customerID+`","messages":[]}`
	call("/v1/chat/completions", replacement, body, 200)
	if response := call("/v1/chat/completions", replacement, body, 429); !strings.Contains(response, "End User") {
		t.Fatal(response)
	}
	var count int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM minato_proxy_key_model_spend WHERE
		(key_identity=$1 AND spend=0.5 OR key_identity=$2 AND spend=0.25) AND model_group='limited'`,
		keyDigest(issued.Key), customerModelBudgetIdentity(customerID)).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("expected both model budget windows to be charged: %d, %v", count, err)
	}
}
