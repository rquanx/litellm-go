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

func TestResponsesWebSocketPinsAndRewritesSessionModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.URL.Query().Get("model") != "native" ||
			r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Errorf("unexpected provider handshake: %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for index := range 2 {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			var event map[string]any
			if json.Unmarshal(body, &event) != nil || event["model"] != "native" ||
				(index == 1 && event["previous_response_id"] != "resp_real") {
				t.Errorf("unexpected provider event: %s", body)
			}
			if err := conn.Write(r.Context(), websocket.MessageText,
				[]byte(`{"type":"response.completed","response":{"id":"resp_real","model":"native"}}`)); err != nil {
				t.Error(err)
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "openai", Endpoint: "responses", DeploymentID: "chosen", ModelID: "native",
		APIBase: upstream.URL + "/v1", APIKey: "fixture-provider"}
	proxy := httptest.NewServer(NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"other"}, token == "restricted" },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}}))
	defer proxy.Close()
	url := proxy.URL + "/responses?model=demo"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(token string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	}
	for token, status := range map[string]int{"restricted": 403, "unknown": 401} {
		if client, response, err := dial(token); client != nil || response == nil || response.StatusCode != status || err == nil {
			t.Fatalf("unauthorized handshake: response=%v, err=%v", response, err)
		}
	}
	client, _, err := dial("master")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	exchange := func(payload string) string {
		t.Helper()
		if err := client.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		_, data, err := client.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if observed := exchange(`{"type":"response.create","model":"other"}`); !strings.Contains(observed, `"code":"invalid_request"`) {
		t.Fatalf("model override was not rejected: %s", observed)
	}
	id := wrapResponseID("resp_real", candidate)
	first := exchange(`{"type":"response.create","input":"fixture"}`)
	if !strings.Contains(first, id) || !strings.Contains(first, `"model":"demo"`) {
		t.Fatalf("completion did not retain the route alias and deployment: %s", first)
	}
	exchange(`{"type":"response.create","previous_response_id":"` + id + `","input":"follow-up"}`)
}
