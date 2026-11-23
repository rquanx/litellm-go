package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLegacyVectorStoreUsesAuthorizedDatabaseMapping(t *testing.T) {
	id := base64.RawURLEncoding.EncodeToString([]byte("litellm_proxy:vector_store;unified_id,fixture;resource_id,vs-untrusted"))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/v1/vector_stores/vs-native":
			io.WriteString(w, `{"id":"vs-native","object":"vector_store"}`)
		case "/v1/vector_stores/vs-native/search":
			io.WriteString(w, `{"data":[]}`)
		case "/v1/responses":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), `"vector_store_ids":["vs-native"]`) {
				t.Errorf("unexpected Responses body %s", data)
			}
			io.WriteString(w, `{"id":"resp-fixture","object":"response"}`)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{
		AuthorizeInference: true,
		Token:              "master",
		AuthorizeKey: func(key string) ([]string, bool) {
			if key == "restricted" {
				return []string{"other"}, true
			}
			return nil, key == "owner" || key == "other"
		},
		ResourceOwnerScopes: func(key string) []string { return []string{"user:" + key} },
		LegacyVectorStores: func(_ context.Context, requested string, owners []string, master bool) (map[string]string, error) {
			owned := len(owners) == 1 && (owners[0] == "user:owner" || owners[0] == "user:restricted")
			if requested != id || (!master && !owned) {
				return nil, nil
			}
			return map[string]string{"vector": "vs-native"}, nil
		},
		Routes: []Route{{Alias: "vector", Candidates: []Candidate{
			{Provider: "openai", DeploymentID: "vector", APIBase: upstream.URL, Endpoint: "responses"},
		}}},
	})
	path := "/v1/vector_stores/" + id
	response := `{"model":"vector","tools":[{"type":"file_search","vector_store_ids":["` + id + `"]}]}`
	for _, fixture := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", path, "owner", "", 200},
		{"GET", path, "other", "", 404},
		{"GET", path, "restricted", "", 404},
		{"POST", path + "/search", "owner", `{"query":"example"}`, 200},
		{"DELETE", path, "owner", "", 400},
		{"POST", "/v1/responses", "other", response, 400},
		{"POST", "/v1/responses", "owner", response, 200},
	} {
		r := httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(fixture.body))
		r.Header.Set("Authorization", "Bearer "+fixture.token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != fixture.status {
			t.Fatalf("%s %s as %s: got %d, want %d: %s", fixture.method, fixture.path, fixture.token, w.Code, fixture.status, w.Body)
		}
		if fixture.method == "GET" && w.Code == 200 && !strings.Contains(w.Body.String(), id) {
			t.Fatal("retrieval changed the managed vector store ID")
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream requests = %d, want 3", got)
	}
}

func TestVectorStoreRequestsKeepFileAndStoreAffinity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Error("vector store request did not use the provider credential")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/responses":
			var payload struct {
				Tools []struct {
					Stores []string `json:"vector_store_ids"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Tools) != 1 ||
				len(payload.Tools[0].Stores) != 1 || payload.Tools[0].Stores[0] != "vs-original" {
				t.Errorf("unexpected file_search references: %+v, err=%v", payload.Tools, err)
			}
			io.WriteString(w, `{"id":"resp-original","object":"response"}`)
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
	primary := Candidate{Provider: "openai", DeploymentID: "primary", APIBase: upstream.URL, APIKey: "provider-key", Endpoint: "responses"}
	secondary := Candidate{Provider: "openai", DeploymentID: "secondary", APIBase: "http://example.invalid", Endpoint: "responses"}
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
	searchTool := `"tools":[{"type":"file_search","vector_store_ids":["` + store + `"]}]`
	send("POST", "/v1/responses", "master", `{"model":"primary",`+searchTool+`}`, 200)
	send("POST", "/v1/responses", "master", `{"model":"secondary",`+searchTool+`}`, 400)
	previous := wrapResponseID("resp-other", secondary)
	send("POST", "/v1/responses", "master", `{"model":"primary","previous_response_id":"`+previous+`",`+searchTool+`}`, 400)
}
