package proxyconfig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestAdminConnectionProbeAcceptsTemporaryProviderSettings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer temporary-key" {
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true})
	handler, err := WithModelManagement(server, Config{}, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key    string
		status int
	}{
		{"temporary-key", http.StatusOK},
		{"os.environ/SHOULD_NOT_BE_READ", http.StatusBadRequest},
	} {
		body := `{"mode":"embedding","litellm_params":{"model":"openai/text-embedding-fixture","api_base":"` + upstream.URL + `","api_key":"` + test.key + `"}}`
		r := httptest.NewRequest(http.MethodPost, "/health/test_connection", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status || (test.status == http.StatusOK && !strings.Contains(w.Body.String(), `"status":"success"`)) ||
			strings.Contains(w.Body.String(), test.key) {
			t.Fatalf("unexpected connection reply %d %s", w.Code, w.Body)
		}
	}
}
