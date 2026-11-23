package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVertexBatchCreateListRetrieveAndCancel(t *testing.T) {
	const path = "/v1/projects/fixture/locations/us-central1/batchPredictionJobs"
	const body = `{"name":"projects/fixture/locations/us-central1/batchPredictionJobs/123","state":"JOB_STATE_SUCCEEDED","createTime":"2026-01-02T03:04:05Z","inputConfig":{"gcsSource":{"uris":["gs://fixture/jobs/input.jsonl"]}},"outputInfo":{"gcsOutputDirectory":"gs://fixture/jobs/output"}}`
	var cancelled atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing Vertex credentials")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST " + path:
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"model":"publishers/google/models/gemini-2.5-flash"`) ||
				!strings.Contains(string(raw), `"outputUriPrefix":"gs://fixture/jobs"`) {
				t.Errorf("unexpected Vertex batch submission: %s", raw)
			}
			io.WriteString(w, body)
		case "GET " + path:
			if r.URL.Query().Get("pageToken") != "fixture-token" {
				t.Error("Vertex list omitted the continuation token")
			}
			fmt.Fprintf(w, `{"batchPredictionJobs":[%s],"nextPageToken":"next"}`, body)
		case "POST " + path + "/123:cancel":
			cancelled.Store(true)
			io.WriteString(w, "{}")
		case "GET " + path + "/123":
			io.WriteString(w, body)
		default:
			t.Errorf("unexpected Vertex batch path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	candidate := Candidate{Provider: "vertex_ai", APIBase: provider.URL, APIKey: "fixture", DeploymentID: "fixture",
		ModelID: "gemini-2.5-flash", Endpoint: "gemini-generate", Vertex: &VertexConfig{Project: "fixture", Location: "us-central1"}}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"fixture"}, token == "owner" || token == "other" },
		Routes:       []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}},
	})
	call := func(method, path, key, body string, code int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, w.Code, code, w.Body)
		}
		return w.Body.String()
	}
	created := call("POST", "/v1/batches", "owner", `{"model":"fixture","input_file_id":"gs://fixture/jobs/input.jsonl"}`, 200)
	var job struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(created), &job) != nil || !strings.HasPrefix(job.ID, "minato_") ||
		!strings.Contains(created, `"output_file_id":"gs://fixture/jobs/output/predictions.jsonl"`) {
		t.Fatalf("invalid Vertex batch response: %s", created)
	}
	list := "/v1/batches?custom_llm_provider=vertex_ai&model=fixture&page_token=fixture-token"
	if result := call("GET", list, "other", "", 200); !strings.Contains(result, `"data":[]`) {
		t.Fatal("Vertex batch list exposed a different credential's job")
	}
	if result := call("GET", list, "owner", "", 200); !strings.Contains(result, job.ID) || !strings.Contains(result, `"next_page_token":"next"`) {
		t.Fatalf("unexpected Vertex batch list: %s", result)
	}
	get := "/v1/batches/" + job.ID
	call("GET", get, "other", "", 404)
	call("GET", get, "owner", "", 200)
	call("POST", get+"/cancel", "owner", "", 200)
	if !cancelled.Load() {
		t.Fatal("Vertex cancellation was not forwarded")
	}
}
