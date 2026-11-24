package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestResponsesWebSocketPinsAndRewritesSessionModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.URL.Query().Get("model") != "native" ||
			r.Header.Get("Authorization") != "Bearer fixture-provider" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
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
				[]byte(`{"type":"response.completed","response":{"id":"resp_real","model":"native","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`)); err != nil {
				t.Error(err)
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer upstream.Close()
	inputPrice, outputPrice := 0.01, 0.02
	recorded := make(chan InferenceEvent, 2)
	candidate := Candidate{Provider: "openai", Endpoint: "responses", DeploymentID: "chosen", ModelID: "native",
		UsagePrice: [2]*float64{&inputPrice, &outputPrice},
		APIBase: upstream.URL + "/v1", APIKey: "fixture-provider"}
	proxy := httptest.NewServer(NewServer(Config{AuthorizeInference: true, Token: "master",
		Callback: func(event InferenceEvent) { recorded <- event },
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
	client, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"openai-insecure-api-key.master", "responses.v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if client.Subprotocol() != "responses.v1" {
		t.Fatalf("unexpected client protocol: %q", client.Subprotocol())
	}
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
	firstEvent, replayed := <-recorded, <-recorded
	if firstEvent.Total != 3 || firstEvent.Spend == nil || *firstEvent.Spend != 0.04 ||
		firstEvent.Model != "native" || firstEvent.ID != replayed.ID {
		t.Fatalf("unexpected socket usage accounting: %+v %+v", firstEvent, replayed)
	}
}

func TestManagedResponsesWebSocketReplaysConversation(t *testing.T) {
	var count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		turn := count.Add(1)
		if turn == 2 && (len(request.Messages) != 3 || request.Messages[1].Role != "assistant") {
			t.Errorf("the second turn did not include prior context: %+v", request.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"chat-%d\",\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n", turn)
		fmt.Fprintf(w, "data: {\"id\":\"chat-%d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", turn)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "openai", APIBase: upstream.URL, ModelID: "chat", Endpoint: "chat-completions"}
	proxy := httptest.NewServer(NewServer(Config{Token: "master", AuthorizeInference: true,
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, proxy.URL+"/v1/responses?model=demo",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer master"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	send := func(input string) string {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(input)); err != nil {
			t.Fatal(err)
		}
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Type     string
				Response struct{ ID string }
			}
			if err := json.Unmarshal(raw, &event); err != nil || event.Type == "error" {
				t.Fatalf("unexpected WebSocket event: %s, %v", raw, err)
			}
			if event.Type == "response.completed" {
				return event.Response.ID
			}
		}
	}
	first := send(`{"type":"response.create","input":"first"}`)
	if first == "" {
		t.Fatal("missing first response ID")
	}
	send(`{"type":"response.create","response":{"previous_response_id":"` + first + `","input":"follow-up"}}`)
}

func TestNativeResponsesSocketSharesKeyConcurrencyAndTokenLimits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer socket.CloseNow()
		for {
			_, _, err := socket.Read(r.Context())
			if err != nil {
				return
			}
			if err := socket.Write(r.Context(), websocket.MessageText,
				[]byte(`{"type":"response.completed","response":{"id":"resp_limit","usage":{"total_tokens":8}}}`)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	parallel, tpm := 1, 8
	recorded := make(chan InferenceEvent, 1)
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"demo"}, token == "scoped" },
		CheckKeyBudget: func(context.Context, string) (KeyBudget, error) {
			return KeyBudget{SpendIdentity: "scoped", ParallelLimit: &parallel,
				ModelPolicy: func(_ context.Context, model string) (KeyModelPolicy, error) {
					if model != "demo" {
						t.Errorf("unexpected budget model %q", model)
					}
					return KeyModelPolicy{SpendDuration: "1d"}, nil
				},
				ModelRates: func(model string, _ OwnerRateLimits) []OwnerRateLimits {
					return []OwnerRateLimits{{Identity: "scoped:" + model, TPM: &tpm}}
				}}, nil
		},
		Callback: func(event InferenceEvent) { recorded <- event },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "openai", Endpoint: "responses",
			ModelID: "native", APIBase: upstream.URL}}}},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, proxy.URL+"/v1/responses?model=demo",
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer scoped"}}})
	}
	conn, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	another, response, err := dial()
	if another != nil || err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected concurrency rejection, response %v, error %v", response, err)
	}
	for _, expected := range []string{"response.completed", "Key token limit exceeded"} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","input":"a"}`)); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil || !strings.Contains(string(data), expected) {
			t.Fatalf("expected %s, received %s: %v", expected, data, err)
		}
	}
	select {
	case event := <-recorded:
		if event.KeyModelBudgetDuration != "1d" || !event.KeyBudgetTracked || event.ModelGroup != "demo" || event.Total != 8 {
			t.Fatalf("unexpected scoped usage %+v", event)
		}
	default:
		t.Fatal("missing model usage callback")
	}
}
