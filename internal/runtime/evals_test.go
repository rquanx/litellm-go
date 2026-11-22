package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvalsPreserveDeploymentAndResourceReferences(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("eval request omitted upstream credentials")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/evals":
			var payload struct {
				Model  string            `json:"model"`
				Source map[string]string `json:"data_source_config"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "" || payload.Source["file_id"] != "file-fixture" {
				t.Errorf("unexpected eval payload %+v", payload)
			}
			io.WriteString(w, `{"id":"eval_fixture","object":"eval"}`)
		case "GET /v1/evals":
			if r.URL.Query().Get("after") != "eval_fixture" || r.URL.Query().Get("order_by") != "created_at" {
				t.Errorf("unexpected eval list query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"data":[]}`)
		case "POST /v1/evals/eval_fixture/runs", "POST /v1/evals/eval_fixture/runs/run_fixture/cancel":
			io.WriteString(w, `{"id":"run_fixture","eval_id":"eval_fixture","object":"eval.run"}`)
		default:
			t.Errorf("unexpected eval route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true, Routes: []Route{{
		Alias: "fixture", Candidates: []Candidate{{Provider: "openai", APIBase: upstream.URL, APIKey: "fixture", DeploymentID: "d1"}},
	}}, AuthorizeKey: func(key string) ([]string, bool) { return []string{"other"}, key == "restricted" }})
	send := func(method, path, key, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, request)
		if result.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, result.Code, status, result.Body)
		}
		return result.Body.String()
	}
	eval := encodeResourceID("evals", "d1", "eval_fixture")
	file := encodeResourceID("files", "d1", "file-fixture")
	run := encodeResourceID("eval_runs", "d1", "run_fixture")
	if body := send("POST", "/v1/evals", "master", `{"model":"fixture","data_source_config":{"file_id":"`+file+`"}}`, 200); !strings.Contains(body, eval) {
		t.Fatal(body)
	}
	send("GET", "/v1/evals?after="+eval+"&order_by=created_at", "master", "", 200)
	send("GET", "/v1/evals/"+eval, "restricted", "", 400)
	if body := send("POST", "/v1/evals/"+eval+"/runs", "master", `{"data_source":{"type":"jsonl"}}`, 200); !strings.Contains(body, eval) || !strings.Contains(body, run) {
		t.Fatal(body)
	}
	send("POST", "/v1/evals/"+eval+"/runs/"+run, "master", "", 200)
	send("GET", "/v1/evals/"+eval+"/runs/"+encodeResourceID("eval_runs", "d2", "other"), "master", "", 404)
}
