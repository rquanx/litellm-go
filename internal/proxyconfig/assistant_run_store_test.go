package proxyconfig

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestPendingAssistantRunsRecoverWithoutResubmission(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spend, err := OpenSpendLog(ctx, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE minato_proxy_spend (LIKE public.minato_proxy_spend INCLUDING ALL);
		CREATE TEMP TABLE minato_pending_assistant_runs (id TEXT PRIMARY KEY, payload TEXT NOT NULL,
			available_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	store, err := spend.OpenAssistantRunStore(ctx, "master")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/threads/thread_fixture/runs/run_fixture" ||
			r.Header.Get("Authorization") != "Bearer fixture-current" {
			t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`{"id":"run_fixture","object":"thread.run","status":"completed","usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer upstream.Close()
	price := 0.25
	path := "/v1/threads/thread_fixture/runs"
	job := runtime.PendingAssistantRun{
		ID: "assistant-run-fixture", Path: path, Deployment: "fixture", Provider: "openai", RunID: "run_fixture",
		TargetHash: fmt.Sprintf("%x", sha256.Sum256([]byte(upstream.URL+path))),
		UsagePrice: [2]*float64{&price, &price}, Attribution: &runtime.SpendAttribution{UserID: "fixture-owner"},
		AvailableAt: time.Now(), Event: runtime.InferenceEvent{Model: "model", ModelID: "fixture", ModelGroup: "alias",
			Provider: "openai", StartTime: float64(time.Now().Unix())},
	}
	if err := store.Schedule(ctx, job); err != nil {
		t.Fatal(err)
	}
	restarted, err := spend.OpenAssistantRunStore(ctx, "master")
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: []runtime.Route{{Alias: "alias", Candidates: []runtime.Candidate{
		{DeploymentID: "fixture", Provider: "openai", APIBase: upstream.URL, APIKey: "fixture-current"},
	}}}, HTTPClient: upstream.Client()})
	for range 2 {
		if err := restarted.recoverOne(ctx, server.RecoverAssistantRun); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	var cost float64
	var owner string
	if err := db.QueryRowContext(ctx, "SELECT total_tokens, spend, user_id FROM minato_proxy_spend WHERE request_id=$1", job.ID).
		Scan(&total, &cost, &owner); err != nil || calls != 1 || total != 5 || cost != 1.25 || owner != "fixture-owner" {
		t.Fatalf("unexpected recovered spend: calls=%d tokens=%d cost=%g owner=%s err=%v", calls, total, cost, owner, err)
	}
}

