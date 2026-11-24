package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRealtimeDetectedSpeechChecksAllowanceBeforeGeneration(t *testing.T) {
	for _, protocol := range []string{"beta", "ga"} {
		t.Run(protocol, func(t *testing.T) {
			config := func(automatic bool) string {
				detection := fmt.Sprintf(`{"type":"server_vad","create_response":%t,"prefix_padding_ms":180}`, automatic)
				if protocol == "ga" {
					return `{"type":"realtime","audio":{"input":{"turn_detection":` + detection + `}}}`
				}
				return `{"turn_detection":` + detection + `}`
			}
			upstreamDone := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamDone)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
				defer cancel()
				send := func(data string) bool {
					if err := conn.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
						t.Error(err)
						return false
					}
					return true
				}
				read := func(want string) bool {
					_, data, err := conn.Read(ctx)
					if err != nil || !strings.Contains(string(data), want) {
						t.Errorf("expected %s, got %s: %v", want, data, err)
						return false
					}
					if want == "session.update" {
						var update map[string]any
						if json.Unmarshal(data, &update) != nil {
							t.Error("invalid session update")
							return false
						}
						options := getRealtimeVADConfig(update)
						if options.ga != (protocol == "ga") || options.detection["prefix_padding_ms"] != float64(180) ||
							options.detection["create_response"] != false {
							t.Errorf("unexpected detection update: %s", data)
							return false
						}
					}
					return true
				}
				if !send(`{"type":"session.created","session":`+config(true)+`}`) || !read("session.update") ||
					!send(`{"type":"session.updated","session":`+config(false)+`}`) {
					return
				}
				if !read("session.update") || !send(`{"type":"session.updated","session":`+config(false)+`}`) {
					return
				}
				// A manual commit only stores audio. It does not spend an automatic turn.
				if !read("input_audio_buffer.commit") || !send(`{"type":"input_audio_buffer.committed","item_id":"manual"}`) {
					return
				}
				for _, item := range []string{"first", "second"} {
					if !read("input_audio_buffer.append") {
						return
					}
					stopped := `{"type":"input_audio_buffer.speech_stopped","item_id":"` + item + `"}`
					committed := `{"type":"input_audio_buffer.committed","item_id":"` + item + `"}`
					if protocol == "ga" {
						stopped, committed = committed, stopped
					}
					if !send(stopped) || !send(committed) {
						return
					}
					if item == "first" {
						if !read("response.create") || !send(`{"type":"response.done","response":{"id":"response-first","usage":{"total_tokens":8}}}`) {
							return
						}
					}
				}
				// Use a subsequent client message to prove a denied turn sent nothing.
				read("input_audio_buffer.clear")
			}))
			defer upstream.Close()
			allowance := 8
			s := NewServer(Config{Token: "master", AuthorizeInference: true,
				AuthorizeKey: func(token string) ([]string, bool) { return []string{"voice"}, token == "limited" },
				CheckKeyBudget: func(context.Context, string) (KeyBudget, error) {
					return KeyBudget{SpendIdentity: "voice", TPMLimit: &allowance}, nil
				},
				Routes: []Route{{Alias: "voice", Candidates: []Candidate{{Provider: "openai", APIBase: upstream.URL, ModelID: "native"}}}},
			})
			proxy := httptest.NewServer(s)
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, proxy.URL+"/v1/realtime?model=voice", &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer limited"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			read := func(want string) {
				t.Helper()
				_, data, err := conn.Read(ctx)
				if err != nil || !strings.Contains(string(data), want) {
					t.Fatalf("expected %s, got %s: %v", want, data, err)
				}
			}
			write := func(data string) {
				t.Helper()
				if err := conn.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			read("session.created")
			write(`{"type":"session.update","session":` + config(true) + `}`)
			read(`"create_response":true`)
			write(`{"type":"input_audio_buffer.commit"}`)
			read("manual")
			for _, want := range []string{"response.done", "Key token limit exceeded"} {
				write(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)
				if protocol == "ga" {
					read("input_audio_buffer.committed")
					read("input_audio_buffer.speech_stopped")
				} else {
					read("input_audio_buffer.speech_stopped")
					read("input_audio_buffer.committed")
				}
				read(want)
			}
			write(`{"type":"input_audio_buffer.clear"}`)
			select {
			case <-upstreamDone:
			case <-ctx.Done():
				t.Fatal("provider did not finish the audio fixture")
			}
		})
	}
}
