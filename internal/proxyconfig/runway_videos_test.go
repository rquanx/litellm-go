package proxyconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestRunwayVideoLifecycle(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video.mp4" {
			if r.Header.Get("Authorization") != "" {
				t.Error("Runway credential forwarded to video download")
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "fixture video")
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Runway-Version") != "2024-11-06" {
			t.Error("invalid Runway authentication")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/image_to_video":
			var fields map[string]any
			_ = json.NewDecoder(r.Body).Decode(&fields)
			if fields["promptText"] != "a fixture" || fields["model"] != "gen4_turbo" || fields["promptImage"] != "https://fixture.invalid/img" ||
				fields["ratio"] != "1280:720" || fields["duration"] != float64(5) {
				t.Errorf("invalid Runway payload %v", fields)
			}
			io.WriteString(w, `{"id":"task1","createdAt":"2026-01-01T00:00:00Z"}`)
		case "GET /v1/tasks/task1":
			fmt.Fprintf(w, `{"id":"task1","status":"SUCCEEDED","output":[%q],"progress":1}`, upstream.URL+"/video.mp4")
		case "POST /v1/tasks/task1/cancel":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Runway request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("RUNWAYML_API_SECRET", "fixture")
	t.Setenv("RUNWAYML_DEFAULT_API_VERSION", "")
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: video
    litellm_params: {model: runwayml/gen4_turbo, api_base: %q}
`, upstream.URL+"/v1")))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: config.Routes, Token: "master",
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"video"}, key == "first" || key == "second" }})
	call := func(method, path, body, key string, status int, fragment string) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != status || !strings.Contains(result.Body.String(), fragment) {
			t.Fatalf("%s %s returned %d %s", method, path, result.Code, result.Body.String())
		}
		return result.Body.String()
	}
	created := call("POST", "/videos", `{"model":"video","prompt":"a fixture","input_reference":"https://fixture.invalid/img","size":"1280x720","seconds":"5.0"}`,
		"first", http.StatusOK, `"seconds":"5"`)
	var video struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created), &video); err != nil || video.ID == "" {
		t.Fatalf("invalid video response: %s", created)
	}
	path := "/v1/videos/" + video.ID
	call("GET", path, "", "first", http.StatusOK, `"status":"completed"`)
	call("GET", path, "", "second", http.StatusNotFound, `"video_not_found"`)
	call("GET", path+"/content", "", "first", http.StatusOK, "fixture video")
	call("DELETE", path, "", "first", http.StatusOK, `"status":"cancelled"`)
}
