package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLangfusePassThroughMapsSDKCredentials(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		public, secret, ok := r.BasicAuth()
		if !ok || public != "pk-upstream" || secret != "sk-upstream" ||
			r.Method != http.MethodPost || r.URL.Path != "/api/public/ingestion" || r.URL.Query().Get("batch") != "test" {
			t.Errorf("unexpected Langfuse upstream request: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	t.Setenv("LANGFUSE_HOST", upstream.URL)
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-upstream")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-upstream")
	server := NewServer(Config{AuthorizeInference: true, Token: "master", HTTPClient: upstream.Client()})
	for _, item := range []struct {
		path string
		password string
		status int
	}{
		{"/langfuse/api/public/ingestion?batch=test", "invalid", http.StatusUnauthorized},
		{"/langfuse/api/public/%252e%252e/ingestion", "master", http.StatusBadRequest},
		{"/langfuse/api/public/ingestion?batch=test", "master", http.StatusAccepted},
	} {
		request := httptest.NewRequest(http.MethodPost, item.path, strings.NewReader("{}"))
		request.SetBasicAuth("pk-client", item.password)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != item.status {
			t.Fatalf("%s returned %d, want %d: %s", item.path, response.Code, item.status, response.Body)
		}
	}
	if calls != 1 {
		t.Fatalf("unexpected upstream call count: %d", calls)
	}
}

