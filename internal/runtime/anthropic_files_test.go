package runtime

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicFilesReadContentAndResolveMessages(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture" || r.Header.Get("anthropic-beta") != "files-api-2025-04-14" {
			t.Error("Anthropic file headers missing")
		}
		switch r.URL.Path {
		case "/v1/files/file-demo":
			fmt.Fprint(w, `{"id":"file-demo","type":"file","size_bytes":7,"created_at":"2026-01-02T00:00:00Z"}`)
		case "/v1/files/file-demo/content":
			fmt.Fprint(w, "fixture")
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"file_id":"file-demo"`) {
				t.Errorf("Anthropic file reference was not resolved: %s", body)
			}
			fmt.Fprint(w, `{"id":"msg-fixture","type":"message"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master", Routes: []Route{{Alias: "demo", Candidates: []Candidate{{
		Provider: "anthropic", Endpoint: "anthropic-messages", ModelID: "native", APIBase: provider.URL, APIKey: "fixture", DeploymentID: "deployment",
	}}}}})
	file := encodeResourceID("files", "deployment", "file-demo")
	call := func(method, path, body string) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		r.Header.Set("anthropic-beta", "files-api-2025-04-14")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	info := call("GET", "/v1/files/"+file, "")
	if !strings.Contains(info, `"bytes":7`) || !strings.Contains(info, `"purpose":"messages"`) {
		t.Fatal(info)
	}
	if content := call("GET", "/v1/files/"+file+"/content", ""); content != "fixture" {
		t.Fatal(content)
	}
	call("POST", "/v1/messages", `{"model":"demo","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"`+file+`"}}]}]}`)
}
