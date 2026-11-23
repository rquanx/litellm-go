package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVertexBatchFileUploadAndOutputCorrelation(t *testing.T) {
	customID := "Example-完整-" + strings.Repeat("Long", 12)
	var object string
	var labels map[string]string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("GCS did not receive the Vertex credential")
		}
		if r.Method == "POST" && r.URL.Path == "/upload/storage/v1/b/fixture/o" {
			object = r.URL.Query().Get("name")
			if !strings.HasPrefix(object, "jobs/litellm-vertex-files/publishers/google/models/gemini-2.5-flash/") {
				t.Errorf("unexpected object %s", object)
			}
			var payload struct {
				Request struct {
					Labels   map[string]string `json:"labels"`
					Contents []any             `json:"contents"`
				} `json:"request"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Request.Contents) != 1 {
				t.Errorf("unexpected GCS file payload %+v: %v", payload, err)
			}
			labels = payload.Request.Labels
			fmt.Fprintf(w, `{"name":%q,"size":"10","timeCreated":"2026-01-02T03:04:05Z"}`, object)
			return
		}
		if r.Method != "GET" || !strings.HasSuffix(r.URL.EscapedPath(), "%2Fpredictions.jsonl") || r.URL.Query().Get("alt") != "media" {
			t.Errorf("unexpected GCS output request: %s %s", r.Method, r.URL.EscapedPath())
		}
		raw, _ := json.Marshal(labels)
		fmt.Fprintf(w, `{"request":{"labels":%s},"status":"","response":{"modelVersion":"gemini-2.5-flash@1","candidates":[{"content":{"parts":[{"text":"fixture result"}]}}]}}`, raw)
	}))
	defer provider.Close()
	candidate := Candidate{Provider: "vertex_ai", APIBase: provider.URL, APIKey: "fixture", DeploymentID: "fixture",
		ModelID: "gemini-2.5-flash", Endpoint: "gemini-generate", Vertex: &VertexConfig{Project: "fixture", Location: "us-central1",
			Bucket: "fixture/jobs", StorageBase: provider.URL}}
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}}})
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	if err := form.WriteField("purpose", "batch"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "file.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"custom_id":%q,"body":{"model":"fixture","messages":[{"role":"user","content":"example"}]}}`, customID)
	if _, err := io.WriteString(part, record); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/files?custom_llm_provider=vertex_ai&model=fixture", &data)
	request.Header.Set("Content-Type", form.FormDataContentType())
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != 200 || !strings.Contains(result.Body.String(), "gs://fixture/jobs/litellm-vertex-files/") || labels["litellm_custom_id_raw_1"] == "" {
		t.Fatalf("unexpected GCS upload: %d %s", result.Code, result.Body)
	}
	output := "gs://fixture/" + object + "/predictions.jsonl"
	result = httptest.NewRecorder()
	server.ServeHTTP(result, httptest.NewRequest("GET", "/v1/files/"+url.PathEscape(output)+"/content?model=fixture", nil))
	if result.Code != 200 || !strings.Contains(result.Body.String(), customID) || !strings.Contains(result.Body.String(), "fixture result") {
		t.Fatalf("unexpected GCS batch output: %d %s", result.Code, result.Body)
	}
}

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
