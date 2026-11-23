package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLiteLLMManagedFileLookupAndBatchInput(t *testing.T) {
	legacy := "file-" + base64.RawURLEncoding.EncodeToString([]byte("litellm_proxy:openai;unified_id,fixture;target_model_names,demo"))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/files/file-underlying/content":
			fmt.Fprint(w, "fixture body")
		case "/v1/batches/batch-fixture":
			fmt.Fprint(w, `{"id":"batch-fixture","status":"completed"}`)
		case "/v1/batches":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), `"input_file_id":"file-underlying"`) || strings.Contains(string(data), legacy) {
				t.Error("wrong batch input file")
			}
			fmt.Fprint(w, `{"id":"batch-fixture"}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{DeploymentID: "deployment", ModelID: "fixture", Provider: "openai", APIBase: provider.URL}}}},
		LegacyFiles: func(_ context.Context, id string, _ []string, master bool) (map[string]string, error) {
			if !master || id != legacy {
				t.Fatalf("unexpected managed lookup %q", id)
			}
			return map[string]string{"deployment": "file-underlying"}, nil
		},
	})
	call := func(method, path, body string) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	if result := call("GET", "/v1/files/"+url.PathEscape(legacy)+"/content", ""); result != "fixture body" {
		t.Fatal(result)
	}
	legacyBatch := "batch_" + base64.RawStdEncoding.EncodeToString([]byte("litellm_proxy;model_id:deployment;llm_batch_id:batch-fixture"))
	if result := call("GET", "/v1/batches/"+url.PathEscape(legacyBatch), ""); !strings.Contains(result, `"status":"completed"`) {
		t.Fatal(result)
	}
	call("POST", "/v1/batches", `{"input_file_id":"`+legacy+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`)
}
