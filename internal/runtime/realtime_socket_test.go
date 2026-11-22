package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRealtimeWebSocketRoutesScopedEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/realtime" || r.URL.Query().Get("deployment") != "native" ||
			r.URL.Query().Get("api-version") != "2025-04-01-preview" || r.URL.Query().Get("unexpected") != "" ||
			r.Header.Get("api-key") != "provider-key" {
			t.Errorf("unexpected realtime handshake: %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, body, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		var event map[string]any
		if err := json.Unmarshal(body, &event); err != nil || objectField(event, "session")["model"] != "native" {
			t.Errorf("session model was not mapped upstream: %s", body)
		}
		conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.updated"}`))
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "azure", DeploymentID: "azure", APIBase: upstream.URL, ModelID: "native",
		APIKey: "provider-key", APIVersion: "2025-04-01-preview"}
	proxy := httptest.NewServer(NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, proxy.URL+"/openai/v1/realtime?model=demo&unexpected=discarded", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer master"}}, Subprotocols: []string{"realtime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "realtime" {
		t.Fatal("client Realtime subprotocol was not selected")
	}
	for _, step := range []struct{ payload, expected string }{
		{`{"type":"session.update","session":{"model":"restricted"}}`, "invalid_request"},
		{`{"type":"session.update","session":{"model":"demo"}}`, "session.updated"},
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(step.payload)); err != nil {
			t.Fatal(err)
		}
		_, frame, err := conn.Read(ctx)
		if err != nil || !strings.Contains(string(frame), step.expected) {
			t.Fatalf("response=%q, err=%v, want %s", frame, err, step.expected)
		}
	}
}
