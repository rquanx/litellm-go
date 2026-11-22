package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVectorStoreRequestsKeepFileAndStoreAffinity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Error("vector store request did not use the provider credential")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/vector_stores":
			var payload struct {
				Files []string `json:"file_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Files) != 1 || payload.Files[0] != "file-original" {
				t.Errorf("create file references = %v, err = %v", payload.Files, err)
			}
			io.WriteString(w, `{"id":"vs-original","object":"vector_store"}`)
		case "GET /v1/vector_stores/vs-original":
			io.WriteString(w, `{"id":"vs-original","object":"vector_store"}`)
		case "GET /v1/vector_stores/vs-original/files":
			if r.URL.Query().Get("before") != "file-original" || r.URL.Query().Get("filter") != "completed" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"data":[{"id":"file-original","vector_store_id":"vs-original"}]}`)
		case "GET /v1/vector_stores/vs-original/files/file-original/content":
			io.WriteString(w, `{"data":[{"type":"text","text":"fixture"}]}`)
		case "POST /v1/vector_stores/vs-original/search":
			io.WriteString(w, `{"data":[{"file_id":"file-original","score":0.5}]}`)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	primary := Candidate{Provider: "openai", DeploymentID: "primary", APIBase: upstream.URL, APIKey: "provider-key"}
	secondary := Candidate{Provider: "openai", DeploymentID: "secondary", APIBase: "http://example.invalid"}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"secondary"}, key == "restricted" },
		Routes: []Route{
			{Alias: "primary", Candidates: []Candidate{primary}},
			{Alias: "secondary", Candidates: []Candidate{secondary}},
		}})
	send := func(method, path, token, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, response.Code, status, response.Body)
		}
		return response.Body.String()
	}
	file := encodeResourceID("files", "primary", "file-original")
	store := encodeResourceID("vector_stores", "primary", "vs-original")
	created := send("POST", "/vector_stores", "master", `{"file_ids":["`+file+`"]}`, 200)
	if !strings.Contains(created, store) {
		t.Fatal("create did not persist the provider deployment in the vector store ID")
	}
	path := "/v1/vector_stores/" + store
	send("GET", path, "restricted", "", 400)
	send("GET", path, "unknown", "", 401)
	send("GET", path, "master", "", 200)
	files := send("GET", path+"/files?before="+file+"&filter=completed", "master", "", 200)
	if !strings.Contains(files, file) || !strings.Contains(files, store) {
		t.Fatal("list did not preserve both file and store affinity")
	}
	send("GET", path+"/files/"+file+"/content", "master", "", 200)
	results := send("POST", path+"/search", "master", `{"query":"fixture"}`, 200)
	if !strings.Contains(results, file) {
		t.Fatal("search did not preserve file affinity")
	}
	foreign := encodeResourceID("files", "secondary", "file-other")
	send("GET", path+"/files/"+foreign, "master", "", 404)
	send("POST", path+"/files", "master", `{"file_id":"`+foreign+`"}`, 400)
}
