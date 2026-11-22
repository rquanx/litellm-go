package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponsePollingReturnsPartialOutputAndChecksOwner(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["background"] != nil || payload["stream"] != true {
			t.Errorf("unexpected background request %+v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_origin\",\"status\":\"completed\",\"output\":[],\"usage\":{\"total_tokens\":3}}}\n\n")
	}))
	defer upstream.Close()
	runtime := NewServer(Config{AuthorizeInference: true, Token: "master", ResponsePolling: ResponsePollingConfig{All: true, TTL: 10 * time.Second},
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"demo"}, key == "a" || key == "b" },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, ModelID: "upstream", Endpoint: "responses", Provider: "openai", DeploymentID: "deployment"}}}},
	})
	send := func(method, path, key, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		res := httptest.NewRecorder()
		runtime.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, res.Code, status, res.Body)
		}
		return res.Body.String()
	}
	var initial struct{ ID string `json:"id"` }
	if raw := send("POST", "/v1/responses", "a", `{"model":"demo","background":true,"input":"fixture"}`, 200); json.Unmarshal([]byte(raw), &initial) != nil ||
		!strings.HasPrefix(initial.ID, pollingIDPrefix) || !strings.Contains(raw, `"status":"queued"`) {
		t.Fatal(raw)
	}
	path := "/v1/responses/" + initial.ID
	send("GET", path, "b", "", 404)
	await := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if raw := send("GET", path, "a", "", 200); strings.Contains(raw, want) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("polling response never contained %s", want)
	}
	await(`"text":"first"`)
	close(release)
	await(`"total_tokens":3`)
	send("DELETE", path, "a", "", 200)
	send("GET", path, "a", "", 404)
}
