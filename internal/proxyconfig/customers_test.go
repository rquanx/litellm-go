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
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestCustomerCRUDAndBlockedInference(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	customers, err := OpenCustomers(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	spend, err := OpenSpendLog(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[],"usage":{"total_tokens":5}}`)
	}))
	defer upstream.Close()
	var event runtime.InferenceEvent
	server := runtime.NewServer(runtime.Config{
		AuthorizeInference: true, Token: "fixture-master", CustomerPolicy: customers.Policy,
		Callback: func(got runtime.InferenceEvent) { event = got; spend.Record(got) },
		Routes: []runtime.Route{
			{Alias: "fixture", Candidates: []runtime.Candidate{
				{APIBase: upstream.URL, ModelID: "fixture", Endpoint: "chat-completions", Region: "us"},
			}},
			{Alias: "regional", Candidates: []runtime.Candidate{
				{APIBase: upstream.URL, ModelID: "fixture-eu", Endpoint: "chat-completions", Region: "eu"},
			}},
		},
	})
	handler := spend.Handler(customers.Handler(server))
	call := func(method, path, body, header, key string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		if header != "" {
			r.Header.Set("X-LiteLLM-Customer-ID", header)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, w.Code, status, w.Body)
		}
		return w.Body.String()
	}
	const id = "fixture-customer"
	const key = "fixture-master"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM `+customerTable+` WHERE user_id=$1`, id) })
	if _, err := db.Exec(`DELETE FROM `+customerTable+` WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	call("POST", "/customer/new", `{"user_id":"fixture-customer","alias":"fixture","spend":2}`, "", key, 200)
	call("POST", "/end_user/block", `{"user_ids":["fixture-customer"]}`, "", key, 200)
	call("POST", "/v1/chat/completions", `{"model":"fixture","user":"other","messages":[]}`, id, key, 403)
	if calls.Load() != 0 {
		t.Fatal("blocked requests reached the provider")
	}
	call("POST", "/end_user/unblock", `{"user_ids":["fixture-customer"]}`, "", key, 200)
	call("POST", "/v1/chat/completions", `{"model":"fixture","metadata":{"user_id":"fixture-customer"},"messages":[]}`, "", key, 200)
	if event.EndUser != id || calls.Load() != 1 {
		t.Fatalf("unexpected customer attribution %+v", event)
	}
	date := time.Now().UTC().Format(time.DateOnly)
	query := "?end_user_ids=" + id + "&start_date=" + date + "&end_date=" + date
	activity := call("GET", "/end_user/daily/activity"+query, "", "", key, 200)
	if !strings.Contains(activity, `"total_tokens":5`) || !strings.Contains(activity, `"total_successful_requests":1`) ||
		!strings.Contains(activity, `"alias":"fixture"`) {
		t.Fatalf("unexpected customer activity %s", activity)
	}
	var result customer
	if err := json.Unmarshal([]byte(call("GET", "/customer/info?end_user_id="+id, "", "", key, 200)), &result); err != nil ||
		result.ID != id || result.Alias == nil || *result.Alias != "fixture" || result.Spend != 2 {
		t.Fatalf("customer metadata %+v, %v", result, err)
	}
	call("POST", "/customer/update", `{"user_id":"fixture-customer","allowed_model_region":"eu"}`, "", key, 200)
	call("POST", "/v1/chat/completions", `{"model":"fixture","messages":[]}`, id, key, 400)
	call("POST", "/customer/update", `{"user_id":"fixture-customer","default_model":"regional"}`, "", key, 200)
	call("POST", "/v1/chat/completions", `{"model":"fixture","messages":[]}`, id, key, 200)
	if event.Model != "fixture-eu" || calls.Load() != 2 {
		t.Fatalf("regional fallback reached an unexpected deployment: %+v", event)
	}
	charge := 0.5
	usage := runtime.InferenceEvent{ID: newEntityID(), Model: "fixture", EndUser: id,
		StartTime: float64(time.Now().Unix()), Spend: &charge}
	spend.Record(usage)
	spend.Record(usage)
	body := call("GET", "/customer/info?end_user_id="+id, "", "", key, 200)
	if err := json.Unmarshal([]byte(body), &result); err != nil || result.Spend != 2.5 {
		t.Fatalf("expected one customer charge, got spend %g: %v", result.Spend, err)
	}
	var recorded float64
	if err := db.QueryRow(`SELECT spend FROM minato_proxy_spend WHERE request_id=$1`, usage.ID).Scan(&recorded); err != nil || recorded != charge {
		t.Fatalf("unexpected recorded spend %g: %v", recorded, err)
	}
	call("POST", "/customer/delete", `{"user_ids":["fixture-customer","missing-fixture"]}`, "", key, 404)
	call("GET", "/end_user/info?end_user_id="+id, "", "", key, 200)
	call("POST", "/customer/delete", `{"user_ids":["fixture-customer"]}`, "", key, 200)
	call("GET", "/customer/info?end_user_id="+id, "", "", key, 404)
}
