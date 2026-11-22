package proxyconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPostgresSpendLogsAndCostTrend(t *testing.T) {
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
	if _, err := db.Exec("CREATE TEMP TABLE minato_proxy_spend (request_id TEXT PRIMARY KEY, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ NOT NULL," +
		" api_key TEXT, key_alias TEXT, user_id TEXT, team_id TEXT, model TEXT, model_group TEXT, provider TEXT, call_type TEXT," +
		" status TEXT, prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, spend DOUBLE PRECISION)"); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := keyDigest("sk-test")
	keys.state.Keys[digest] = virtualKey{Alias: "test-key"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spend, err := OpenSpendLog(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.01
	now := float64(time.Now().Add(-time.Second).Unix())
	spend.Record(runtime.InferenceEvent{ID: "req-one", KeyDigest: digest, Model: "fixture", ModelGroup: "demo",
		StartTime: now, EndTime: now, Status: "success", Total: 3, Spend: &cost})
	spend.Record(runtime.InferenceEvent{ID: "req-two", Model: "unpriced", ModelGroup: "demo",
		StartTime: now, EndTime: now, Status: "success", Total: 2})
	handler := spend.Handler(http.NotFoundHandler())
	call := func(token, path string, status int, content string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status || !strings.Contains(response.Body.String(), content) {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	call("sk-test", "/spend/logs", 401, "master key")
	found := call("master", "/spend/logs/v2?api_key=sk-test", 200, `"total_count":1`)
	if !strings.Contains(found, "test-key") || !strings.Contains(found, digest) || strings.Contains(found, "sk-test") {
		t.Fatalf("unexpected spend log attribution: %s", found)
	}
	call("master", "/spend/logs?request_id=req-two", 200, `"spend":null`)
	call("master", "/global/spend/logs", 200, `"total_tokens":5`)
}
