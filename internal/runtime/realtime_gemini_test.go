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

func TestGeminiAndVertexLiveWebSocket(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex_ai"} {
		t.Run(provider, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				model := "models/live"
				if provider == "vertex_ai" {
					model = "projects/fixture/locations/us-central1/publishers/google/models/live"
					if r.URL.Path != vertexLivePath || r.Header.Get("Authorization") != "Bearer provider-key" ||
						r.Header.Get("x-goog-user-project") != "fixture" || r.URL.RawQuery != "" {
						t.Error("incorrect Vertex Live handshake")
					}
				} else if r.URL.Path != geminiLivePath || r.URL.Query().Get("key") != "provider-key" || r.Header.Get("Authorization") != "" {
					t.Error("incorrect Gemini Live handshake")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
				defer cancel()
				_, data, err := conn.Read(ctx)
				var event map[string]any
				if err != nil || json.Unmarshal(data, &event) != nil || objectField(event, "setup")["model"] != model {
					t.Errorf("unexpected Live setup: %s", data)
					return
				}
				if err := conn.Write(ctx, websocket.MessageBinary, []byte(`{"setupComplete":{}}`)); err != nil {
					t.Error(err)
					return
				}
				_, data, err = conn.Read(ctx)
				mime := "audio/pcm"
				if provider == "vertex_ai" {
					mime = "audio/pcm;rate=16000"
				}
				if err != nil || !strings.Contains(string(data), `"data":"AAAA"`) ||
					!strings.Contains(string(data), `"mimeType":"`+mime+`"`) {
					t.Errorf("unexpected Live audio: %s", data)
					return
				}
				_, data, err = conn.Read(ctx)
				if err != nil || !strings.Contains(string(data), `"text":"hello world"`) {
					t.Errorf("unexpected Live input: %s", data)
					return
				}
				err = conn.Write(ctx, websocket.MessageBinary, []byte(`{"toolCall":{"functionCalls":[{"id":"fixture-call","name":"lookup","args":{"city":"Paris"}}]}}`))
				if err != nil {
					t.Error(err)
					return
				}
				_, data, err = conn.Read(ctx)
				if err != nil || !strings.Contains(string(data), `"functionResponses"`) ||
					!strings.Contains(string(data), `"id":"fixture-call"`) || !strings.Contains(string(data), `"name":"lookup"`) ||
					!strings.Contains(string(data), `"found":true`) {
					t.Errorf("unexpected tool response: %s", data)
					return
				}
				err = conn.Write(ctx, websocket.MessageBinary, []byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"fixture"}]},"inputTranscription":{"text":"hello"},"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`))
				if err != nil {
					t.Error(err)
				}
				_, _, _ = conn.Read(ctx)
			}))
			defer upstream.Close()
			candidate := Candidate{Provider: provider, APIBase: upstream.URL + "/v1", APIKey: "provider-key", ModelID: "live", DeploymentID: "fixture"}
			if provider == "vertex_ai" {
				candidate.Vertex = &VertexConfig{Project: "fixture", Location: "us-central1"}
			}
			usage := make(chan InferenceEvent, 1)
			proxy := httptest.NewServer(NewServer(Config{Token: "master", AuthorizeInference: true,
				Routes: []Route{{Alias: "voice", Candidates: []Candidate{candidate}}},
				Callback: func(event InferenceEvent) { usage <- event }}))
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, proxy.URL+"/v1/realtime?model=voice", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer master"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_, frame, err := conn.Read(ctx)
			if err != nil || !strings.Contains(string(frame), `"model":"voice"`) {
				t.Fatalf("session alias was lost: %s %v", frame, err)
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
				t.Fatal(err)
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"conversation.item.create","item":{"content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":"world"}]}}`)); err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]bool)
			for !seen["response.done"] {
				_, frame, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var event struct {
					Type string
					Item struct {
						Type string
						CallID string `json:"call_id"`
					}
				}
				if err := json.Unmarshal(frame, &event); err != nil {
					t.Fatal(err)
				}
				seen[event.Type] = true
				if event.Type == "response.output_item.done" && event.Item.Type == "function_call" {
					if event.Item.CallID != "fixture-call" {
						t.Fatalf("wrong tool call ID %q", event.Item.CallID)
					}
					if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"fixture-call","output":"{\"found\":true}"}}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, event := range []string{"conversation.item.input_audio_transcription.completed", "response.created", "response.text.delta", "response.output_item.done"} {
				if !seen[event] {
					t.Errorf("missing event %s", event)
				}
			}
			select {
			case event := <-usage:
				if event.Total != 3 || event.ModelGroup != "voice" || event.Provider != provider {
					t.Fatalf("incorrect usage event %+v", event)
				}
			default:
				t.Fatal("missing usage callback")
			}
		})
	}
}
