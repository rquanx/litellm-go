package proxyconfig

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestTagManagementAndSpendBudget(t *testing.T) {
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
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"tag-fixture","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer origin.Close()
	input, output := 0.25, 0.0
	customers := &CustomerRegistry{db: db, keys: keys}
	router := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, EnableTagFiltering: true,
		CheckTagBudget: spend.CheckTagBudget, ResolveDeploymentTags: customers.ResolveDeploymentTags, Callback: spend.Start(ctx),
		Routes: []runtime.Route{{Alias: "tagged", Candidates: []runtime.Candidate{{
			APIBase: origin.URL, Endpoint: "chat-completions", ModelID: "fixture", RoutingGroup: "tagged", UsagePrice: [2]*float64{&input, &output},
		}}}},
	})
	gateway := spend.Handler(WithTagManagement(router, db, keys, "", true))
	call := func(method, path, body string, want int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, expected %d: %s", method, path, w.Code, want, w.Body)
		}
		return w.Body.String()
	}
	tag := newEntityID()
	name := `"name":"`+tag+`"`
	call("POST", "/tag/new", `{`+name+`, "max_budget":0.1,"budget_duration":"1d","models":["tagged"]}`, 200)
	call("POST", "/tag/new", `{`+name+`, "max_budget":100}`, 400)
	info := call("POST", "/tag/info", `{"names":["`+tag+`"]}`, 200)
	if !strings.Contains(info, `"max_budget":0.1`) || !strings.Contains(info, `"models":["tagged"]`) {
		t.Fatal(info)
	}
	call("GET", "/tag/list", "", 200)
	body := `{"model":"tagged","messages":[],"tags":["`+tag+`"],"metadata":{"tags":["`+tag+`"]}}`
	call("POST", "/v1/chat/completions", body, 200)
	if response := call("POST", "/v1/chat/completions", body, 429); !strings.Contains(response, "Tag="+tag) {
		t.Fatal(response)
	}
	var total float64
	if err := db.QueryRowContext(ctx, `SELECT spend FROM `+tagTable+` WHERE tag_name=$1`, tag).Scan(&total); err != nil || total != 0.25 {
		t.Fatalf("tag charged %g: %v", total, err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE `+budgetTable+` SET budget_reset_at=NOW()-interval '1 second'
		WHERE budget_id=(SELECT budget_id FROM `+tagTable+` WHERE tag_name=$1)`, tag); err != nil {
		t.Fatal(err)
	}
	if err := customers.resetExpiredBudgets(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	call("POST", "/v1/chat/completions", body, 200)
	activity := call("GET", "/tag/daily/activity?tags="+tag, "", 200)
	if !strings.Contains(activity, tag) || !strings.Contains(activity, `"total_spend":0.5`) {
		t.Fatal(activity)
	}
	call("POST", "/tag/update", `{`+name+`, "models":["tagged"], "max_budget":1}`, 200)
	call("POST", "/v1/chat/completions", body, 200)
	call("POST", "/tag/delete", `{`+name+`}`, 200)
	call("POST", "/tag/info", `{"names":["`+tag+`"]}`, 404)
	call("POST", "/v1/chat/completions", body, 400)
	if calls.Load() != 3 {
		t.Fatalf("provider received %d calls", calls.Load())
	}
}
