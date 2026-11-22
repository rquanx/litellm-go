package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestResponseResourcesKeepDeploymentAffinity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-upstream" {
			t.Error("upstream did not receive the provider credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["input"] == "follow-up" && body["previous_response_id"] != "resp_remote" {
				t.Errorf("previous_response_id = %v", body["previous_response_id"])
			}
			_, _ = io.WriteString(w, `{"id":"resp_remote","object":"response","model":"fixture"}`)
		case "/v1/responses/resp_remote/input_items":
			if r.URL.Query().Get("limit") != "5" {
				t.Error("pagination parameters were lost")
			}
			_, _ = io.WriteString(w, `{"data":[]}`)
		case "/v1/responses/resp_remote", "/v1/responses/resp_remote/cancel":
			_, _ = io.WriteString(w, `{"id":"resp_remote","object":"response"}`)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "openai", DeploymentID: "deployment-a", ModelID: "fixture",
		APIBase: upstream.URL, APIKey: "fixture-upstream", Endpoint: "responses"}
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"other"}, key == "restricted" },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	assertRequest := func(method, path, body, token string, code int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != code {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, result.Code, code, result.Body.String())
		}
		return result.Body.String()
	}
	body := assertRequest("POST", "/v1/responses", `{"model":"demo","input":"start"}`, "fixture-master", 200)
	id := wrapResponseID("resp_remote", candidate)
	if !strings.Contains(body, id) {
		t.Fatalf("response ID did not retain deployment metadata: %s", body)
	}
	resource := "/v1/responses/" + url.PathEscape(id)
	assertRequest("GET", resource, "", "restricted", 404)
	assertRequest("GET", resource+"/input_items?limit=5", "", "fixture-master", 200)
	if response := assertRequest("GET", resource, "", "fixture-master", 200); !strings.Contains(response, id) {
		t.Fatal("retrieval changed the managed response ID")
	}
	assertRequest("POST", resource+"/cancel", "", "fixture-master", 200)
	assertRequest("DELETE", resource, "", "fixture-master", 200)
	followUp := `{"model":"demo","input":"follow-up","previous_response_id":"` + id + `"}`
	assertRequest("POST", "/v1/responses", followUp, "fixture-master", 200)
}
