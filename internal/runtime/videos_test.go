package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVideoResourcesRouteManagedIDsAndContent(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/videos":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "sora-2" {
				t.Error("video model selector was not mapped")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"video-1","status":"queued"}`)
		case "GET /v1/videos/video-1/content":
			if r.URL.Query().Get("variant") != "video" {
				t.Error("video variant was lost")
			}
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprint(w, "fixture-media")
		default:
			t.Errorf("unexpected video request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	candidate := Candidate{Provider: "openai", DeploymentID: "fixture", ModelID: "sora-2", APIBase: provider.URL}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	call := func(method, path, body, token string, code int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != code {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w
	}
	id := encodeResourceID("videos", "fixture", "video-1")
	result := call("POST", "/videos", `{"model":"demo","prompt":"fixture"}`, "master", 200)
	if !strings.Contains(result.Body.String(), id) {
		t.Fatal("video response lost its deployment")
	}
	content := "/v1/videos/" + id + "/content?variant=video"
	call("GET", content, "", "unknown", 401)
	result = call("GET", content, "", "master", 200)
	if result.Body.String() != "fixture-media" || result.Header().Get("Content-Type") != "video/mp4" {
		t.Fatal("video bytes were not preserved")
	}
}
