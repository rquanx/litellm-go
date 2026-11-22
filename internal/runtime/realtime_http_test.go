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

func TestRealtimeTicketKeepsTheProviderSecretAndDeploymentPrivate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/realtime/client_secrets":
			var payload map[string]any
			if r.Header.Get("Authorization") != "Bearer provider-key" ||
				json.NewDecoder(r.Body).Decode(&payload) != nil || objectField(payload, "session")["model"] != "native" {
				t.Error("invalid provider client secret request")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"value":"provider-ephemeral","expires_at":%d,"session":{"model":"native","client_secret":{"value":"provider-ephemeral"}}}`,
				time.Now().Add(time.Minute).Unix())
		case "/v1/realtime/calls":
			if r.Header.Get("Authorization") != "Bearer provider-ephemeral" || r.FormValue("sdp") != "v=0" ||
				!strings.Contains(r.FormValue("session"), `"model":"native"`) {
				t.Error("SDP routing lost the ticket's provider key or model")
			}
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, "v=0\r\n")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "openai", DeploymentID: "primary", APIBase: upstream.URL, ModelID: "native", APIKey: "provider-key"}
	server := NewServer(Config{Token: "master", AuthorizeInference: true, Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}},
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"other"}, token == "restricted" }})
	send := func(path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != status {
			t.Fatalf("%s: got %d, want %d: %s", path, resp.Code, status, resp.Body)
		}
		return resp
	}
	send("/realtime/client_secrets", "restricted", `{"model":"demo"}`, 403)
	issued := send("/realtime/client_secrets", "master", `{"session":{"model":"demo"}}`, 200)
	var result struct{ Value string }
	if err := json.Unmarshal(issued.Body.Bytes(), &result); err != nil || !strings.HasPrefix(result.Value, "rtc_") ||
		strings.Contains(issued.Body.String(), "provider-ephemeral") {
		t.Fatalf("proxy did not encrypt the provider secret: %s", issued.Body)
	}
	send("/v1/realtime/calls", "master", "v=0", 401)
	send("/v1/realtime/calls", result.Value+"tampered", "v=0", 401)
	expired := server.sealRealtimeTicket(realtimeTicket{Secret: "expired", Deployment: "primary", Expires: time.Now().Add(-time.Minute).Unix()})
	send("/v1/realtime/calls", expired, "v=0", 401)
	answer := send("/openai/v1/realtime/calls?model=ignored", result.Value, "v=0", 201)
	if answer.Header().Get("Content-Type") != "application/sdp" || answer.Body.String() != "v=0\r\n" {
		t.Fatalf("invalid SDP answer: %q", answer.Body)
	}
}
