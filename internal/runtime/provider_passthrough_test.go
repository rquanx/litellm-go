package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderPassThroughKeepsGatewayCredentialsLocal(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fixture/v1beta/models":
			if r.URL.Query().Get("key") != "provider-fixture" || r.URL.Query().Get("pageSize") != "2" ||
				r.Header.Get("Authorization") != "" {
				t.Error("Gemini query did not use the configured upstream key")
			}
		case "/fixture/v2/models":
			if r.Header.Get("Authorization") != "Bearer provider-fixture" {
				t.Error("Cohere did not receive its provider credential")
			}
		case "/fixture/v1/threads":
			if r.Header.Get("Authorization") != "Bearer provider-fixture" || r.Header.Get("OpenAI-Beta") != "assistants=v2" {
				t.Error("OpenAI did not receive its provider credential and beta version")
			}
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Write([]byte(`{"fixture":true}`))
	}))
	defer origin.Close()
	t.Setenv("GEMINI_API_BASE", origin.URL+"/fixture")
	t.Setenv("GEMINI_API_KEY", "provider-fixture")
	t.Setenv("COHERE_API_BASE", origin.URL+"/fixture")
	t.Setenv("COHERE_API_KEY", "provider-fixture")
	t.Setenv("OPENAI_API_BASE", origin.URL+"/fixture")
	t.Setenv("OPENAI_API_KEY", "provider-fixture")
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"limited"}, token == "fixture-limited" }})
	for _, tc := range []struct {
		path, token string
		status int
	}{
		{"/gemini/v1beta/models?key=fixture-master&pageSize=2", "", 200},
		{"/gemini/v1beta/models?key=wrong", "", 401},
		{"/cohere/v2/models", "fixture-master", 200},
		{"/cohere/v2/models", "fixture-limited", 403},
		{"/openai/v1/threads", "fixture-master", 200},
		{"/openai_passthrough/v1/threads", "fixture-master", 200},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s returned %d, want %d", tc.path, w.Code, tc.status)
		}
	}
}
