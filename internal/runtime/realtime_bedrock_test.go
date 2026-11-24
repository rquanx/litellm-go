package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/coder/websocket"
)

func TestBedrockRealtimeDuplexSignedEventStream(t *testing.T) {
	credentials := aws.Credentials{AccessKeyID:"fixture", SecretAccessKey:"fixture-secret", SessionToken:"fixture-session"}
	const region = "us-east-1"
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.URL.Path != "/model/amazon.nova-sonic-v1:0/invoke-with-bidirectional-stream" ||
			r.Header.Get("X-Amz-Content-Sha256") != bedrockStreamingHash || r.Header.Get("X-Amz-Security-Token") != credentials.SessionToken ||
			!strings.Contains(r.Header.Get("Authorization"), "/"+region+"/bedrock/aws4_request") {
			t.Error("incorrect Bedrock stream headers")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seed, err := v4.GetSignedRequestSignature(r)
		if err != nil {
			t.Error(err)
			return
		}
		signer := v4.NewStreamSigner(credentials, "bedrock", region, seed)
		decoder := eventstream.NewDecoder()
		encoder := eventstream.NewEncoder()
		w.Header().Set("Content-Type", bedrockEventStreamType)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		read := func(expected string) bool {
			frame, err := decoder.Decode(r.Body, nil)
			if err != nil {
				t.Error(err)
				return false
			}
			timestamp, _ := frame.Headers.Get(":date").(eventstream.TimestampValue)
			var dateFrame bytes.Buffer
			_ = encoder.Encode(&dateFrame, eventstream.Message{Headers:eventstream.Headers{{Name:":date", Value:timestamp}}})
			signature, _ := signer.GetSignature(r.Context(), dateFrame.Bytes()[12:dateFrame.Len()-4], frame.Payload, time.Time(timestamp))
			if !bytes.Equal(signature, []byte(frame.Headers.Get(":chunk-signature").(eventstream.BytesValue))) {
				t.Error("invalid Bedrock chunk signature")
				return false
			}
			if expected == "" {
				return len(frame.Payload) == 0
			}
			chunk, err := decoder.Decode(bytes.NewReader(frame.Payload), nil)
			if err != nil || chunk.Headers.Get(":event-type") != eventstream.StringValue("chunk") {
				t.Errorf("incorrect Bedrock event envelope: %v", err)
				return false
			}
			var data map[string]any
			if json.Unmarshal(chunk.Payload, &data) != nil || objectField(data, "event")[expected] == nil {
				t.Errorf("expected %s, got %s", expected, chunk.Payload)
				return false
			}
			return true
		}
		for _, expected := range []string{"sessionStart","promptStart","contentStart","audioInput","contentEnd"} {
			if !read(expected) {
				return
			}
		}
		for _, payload := range []string{
			`{"event":{"contentStart":{"role":"USER","type":"TEXT"}}}`,
			`{"event":{"textOutput":{"content":"hello"}}}`,
			`{"event":{"contentStart":{"role":"ASSISTANT","type":"AUDIO"}}}`,
			`{"event":{"audioOutput":{"content":"AAAA"}}}`,
			`{"event":{"completionEnd":{"stopReason":"END_TURN"}}}`,
			`{"event":{"usageEvent":{"totalInputTokens":4,"totalOutputTokens":3,"totalTokens":7}}}`,
		} {
			if err := encoder.Encode(w, eventstream.Message{Headers:eventstream.Headers{
				{Name:":message-type",Value:eventstream.StringValue("event")},
				{Name:":event-type",Value:eventstream.StringValue("chunk")},
			}, Payload:[]byte(payload)}); err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
		}
		for _, expected := range []string{"promptEnd","sessionEnd",""} {
			if !read(expected) {
				return
			}
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	callbacks := make(chan InferenceEvent, 1)
	candidate := Candidate{Provider:"bedrock", APIBase:upstream.URL, ModelID:"amazon.nova-sonic-v1:0", DeploymentID:"fixture",
		Bedrock:&BedrockConfig{Region:region,AccessKey:credentials.AccessKeyID,SecretKey:credentials.SecretAccessKey,SessionToken:credentials.SessionToken}}
	s := NewServer(Config{Token:"master", AuthorizeInference:true, HTTPClient:upstream.Client(),
		Routes:[]Route{{Alias:"voice",Candidates:[]Candidate{candidate}}},
		Callback:func(event InferenceEvent){ callbacks <- event }})
	proxy := httptest.NewServer(s)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, proxy.URL+"/v1/realtime?model=voice", &websocket.DialOptions{
		HTTPHeader:http.Header{"Authorization":{"Bearer master"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	_, first, err := client.Read(ctx)
	if err != nil || !bytes.Contains(first, []byte(`"model":"voice"`)) {
		t.Fatalf("did not receive the session alias: %s %v", first, err)
	}
	for _, data := range []string{
		`{"type":"session.update","session":{"voice":"matthew"}}`,
		`{"type":"input_audio_buffer.append","audio":"AAAA"}`,
		`{"type":"input_audio_buffer.commit"}`,
	} {
		if err := client.Write(ctx, websocket.MessageText, []byte(data)); err != nil { t.Fatal(err) }
	}
	seen := map[string]bool{}
	for !seen["response.done"] {
		_, data, err := client.Read(ctx)
		if err != nil { t.Fatal(err) }
		var event struct{ Type string }
		if err := json.Unmarshal(data, &event); err != nil { t.Fatal(err) }
		if event.Type == "error" {
			t.Fatalf("provider stream failed: %s", data)
		}
		seen[event.Type] = true
	}
	if !seen["conversation.item.input_audio_transcription.completed"] || !seen["response.audio.delta"] {
		t.Fatal("missing Nova Sonic transcription or audio")
	}
	select {
	case event := <-callbacks:
		if event.Total != 7 || event.Provider != "bedrock" || event.ModelGroup != "voice" {
			t.Fatalf("wrong Nova Sonic usage: %+v", event)
		}
	default:
		t.Fatal("missing Nova Sonic usage")
	}
	if err := client.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
}
