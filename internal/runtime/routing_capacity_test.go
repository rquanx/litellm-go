package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapacityRoutingFallsBackWithoutSendingAnOverLimitRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary/v1/chat/completions" {
			w.Write([]byte(`{"choices":[],"usage":{"total_tokens":17}}`))
			return
		}
		w.Write([]byte(`{"choices":[],"usage":{"total_tokens":4}}`))
	}))
	defer upstream.Close()
	tpm := 16
	primary := Candidate{SourceID: "primary", DeploymentID: "primary", RoutingGroup: "primary", Endpoint: "chat-completions",
		ModelID: "m", APIBase: upstream.URL + "/primary", TPM: &tpm}
	backup := Candidate{SourceID: "backup", DeploymentID: "backup", RoutingGroup: "backup", Endpoint: "chat-completions",
		ModelID: "m", APIBase: upstream.URL + "/backup"}
	server := NewServer(Config{RoutingStrategy: "usage-based-routing-v2",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{primary, backup}}}})
	for _, source := range []string{"primary", "backup"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("X-Minato-Runtime-Source") != source {
			t.Fatalf("expected %s, got %s (%d): %s", source, w.Header(), w.Code, w.Body)
		}
	}
}
