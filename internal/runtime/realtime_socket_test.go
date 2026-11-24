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
	t.Setenv("LITELLM_AZURE_REALTIME_PROTOCOL", "")
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

func TestAzureRealtimeDeploymentProtocol(t *testing.T) {
	t.Setenv("LITELLM_AZURE_REALTIME_PROTOCOL", "GA")
	candidate := Candidate{Provider: "azure", APIBase: "https://fixture.example/openai", ModelID: "voice deployment"}
	if got := realtimeAddress(candidate, ""); got != "https://fixture.example/openai/v1/realtime?model=voice+deployment" {
		t.Fatalf("wrong GA address: %s", got)
	}
	candidate.RealtimeProtocol = "beta"
	if got := realtimeAddress(candidate, ""); got != "https://fixture.example/openai/realtime?api-version=2024-10-01-preview&deployment=voice+deployment" {
		t.Fatalf("wrong beta address: %s", got)
	}
	candidate.RealtimeProtocol = "v1"
	candidate.APIBase += "/v1/"
	if got := realtimeAddress(candidate, ""); got != "https://fixture.example/openai/v1/realtime?model=voice+deployment" {
		t.Fatalf("GA base was duplicated: %s", got)
	}
}

func TestRealtimeWebSocketAccountsAudioAndTokenAllowance(t *testing.T) {
	t.Run("OpenAI", func(t *testing.T) {
		testRealtimeWebSocketAccountsAudioAndTokenAllowance(t, "openai", false)
	})
	t.Run("xAI browser authentication", func(t *testing.T) {
		testRealtimeWebSocketAccountsAudioAndTokenAllowance(t, "xai", true)
	})
}

func testRealtimeWebSocketAccountsAudioAndTokenAllowance(t *testing.T, provider string, browser bool) {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "native" ||
			r.URL.Query().Get("intent") != "conversation" || r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Errorf("unexpected Realtime handshake: %s", r.URL)
		}
		wantBeta := "realtime=v1"
		if provider == "xai" {
			wantBeta = ""
		}
		if got := r.Header.Get("OpenAI-Beta"); got != wantBeta {
			t.Errorf("OpenAI-Beta = %q, want %q", got, wantBeta)
		}
		if strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "openai-insecure-api-key.") {
			t.Error("a client credential was forwarded to the provider")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		event := `{"type":"response.done","response":{"id":"response-audio","usage":{"input_tokens":10,"output_tokens":6,"total_tokens":16,"input_token_details":{"audio_tokens":6,"cached_tokens":4,"cached_tokens_details":{"audio_tokens":2}},"output_token_details":{"audio_tokens":4}}}}`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
			t.Error(err)
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer origin.Close()
	limit := 16
	input, output, cache := 1.0, 2.0, 0.25
	inputAudio, outputAudio, cachedAudio := 3.0, 4.0, 0.5
	recorded := make(chan InferenceEvent, 2)
	server := NewServer(Config{Token: "master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"demo"}, token == "limited" },
		CheckKeyBudget: func(context.Context, string) (KeyBudget, error) {
			return KeyBudget{SpendIdentity: "realtime-limited",
				ModelPolicy: func(context.Context, string) (KeyModelPolicy, error) {
					return KeyModelPolicy{SpendDuration: "1d"}, nil
				},
				ModelRates: func(model string, _ OwnerRateLimits) []OwnerRateLimits {
					return []OwnerRateLimits{{Identity: "realtime-limited:" + model, TPM: &limit}}
				}}, nil
		},
		Callback: func(event InferenceEvent) { recorded <- event },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: origin.URL + "/v1", Provider: provider, APIKey: "fixture-provider",
			ModelID: "native", UsagePrice: [2]*float64{&input, &output}, CacheReadPrice: &cache,
			RealtimeAudio: RealtimeAudioPrices{Input: &inputAudio, Output: &outputAudio, CachedInput: &cachedAudio},
		}}}},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer limited"}, "OpenAI-Beta": {"realtime=v1"}},
	}
	address := proxy.URL + "/v1/realtime?model=demo&intent=conversation"
	if browser {
		options.Subprotocols = []string{"realtime", "openai-insecure-api-key.limited"}
		options.HTTPHeader.Set("Authorization", "Bearer wrong-key")
		invalid, response, err := websocket.Dial(ctx, address, options)
		if invalid != nil {
			invalid.CloseNow()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid Authorization header should take precedence over subprotocol: %v", err)
		}
		options.HTTPHeader.Del("Authorization")
	}
	conn, _, err := websocket.Dial(ctx, address, options)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if browser && conn.Subprotocol() != "realtime" {
		t.Fatalf("unexpected browser subprotocol %q", conn.Subprotocol())
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, data, err := conn.Read(ctx)
		if err != nil || !strings.Contains(string(data), "response.done") {
			t.Fatalf("expected response.done, received %s: %v", data, err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || !strings.Contains(string(data), "Key token limit exceeded") {
		t.Fatalf("expected token limit, received %s: %v", data, err)
	}
	event := <-recorded
	if event.Total != 16 || event.Spend == nil || *event.Spend != 35.5 ||
		event.KeyModelBudgetDuration != "1d" || !event.KeyBudgetTracked {
		t.Fatalf("unexpected Realtime usage: %+v", event)
	}
	if len(recorded) != 0 {
		t.Fatal("a repeated terminal event produced a second usage callback")
	}
}
