package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestWorkflowOwnershipAndConcurrentEvents(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL required")
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
	keys.state.Keys[keyDigest("owner")] = virtualKey{}
	keys.state.Keys[keyDigest("peer")] = virtualKey{}
	handler, err := WithWorkflowManagement(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, body string, status int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Errorf("%s %s returned %d, want %d: %s", method, path, w.Code, status, w.Body)
		}
		return w
	}
	check := func(w *httptest.ResponseRecorder, target any) {
		t.Helper()
		if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
			t.Fatalf("invalid response %q: %v", w.Body.String(), err)
		}
	}
	const root = "/v1/workflows/runs"
	var run struct {
		ID string `json:"run_id"`
		Session string `json:"session_id"`
		Owner string `json:"created_by"`
		Status string
		Metadata map[string]string
	}
	check(call("POST", root, "owner", `{"workflow_type":"job","metadata":{"keep":"value"}}`, 200), &run)
	if run.ID == "" || run.Session == "" || run.Status != "pending" || run.Owner != keyDigest("owner") {
		t.Fatalf("invalid run: %+v", run)
	}
	path := root + "/" + run.ID
	call("GET", path, "peer", "", 404)
	call("PATCH", path, "peer", `{"status":"running"}`, 404)
	call("GET", path, "master", "", 200)
	call("POST", path+"/messages", "owner", `{"role":"user","content":"text","session_id":"fixture"}`, 200)
	call("GET", path+"/messages", "owner", "", 200)
	call("GET", path+"/messages", "peer", "", 404)
	const writers = 8
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call("POST", path+"/events", "owner", `{"event_type":"hook.waiting","step_name":"task","data":{"test":true}}`, 200)
		}()
	}
	wg.Wait()
	var events struct {
		Events []struct {
			Sequence int `json:"sequence_number"`
		}
		Count int
	}
	check(call("GET", path+"/events", "owner", "", 200), &events)
	if events.Count != writers || len(events.Events) != writers {
		t.Fatalf("expected %d events, got %+v", writers, events)
	}
	for i, entry := range events.Events {
		if entry.Sequence != i {
			t.Fatalf("unexpected sequence %+v at %d", entry, i)
		}
	}
	details := call("GET", path, "owner", "", 200)
	if !strings.Contains(details.Body.String(), `"status":"paused"`) {
		t.Fatalf("missing event status update: %s", details.Body)
	}
	check(call("PATCH", path, "owner", `{"output":{"done":true},"status":"completed"}`, 200), &run)
	if run.Metadata["keep"] != "value" || run.Status != "completed" {
		t.Fatalf("update did not preserve metadata: %+v", run)
	}
	var list struct{ Count int }
	check(call("GET", root+"?workflow_type=job&status=failed,completed", "owner", "", 200), &list)
	if list.Count != 1 {
		t.Fatalf("unexpected owner list: %+v", list)
	}
	check(call("GET", root, "peer", "", 200), &list)
	if list.Count != 0 {
		t.Fatalf("unexpected peer list: %+v", list)
	}
}
