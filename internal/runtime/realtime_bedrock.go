package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/coder/websocket"
)

func (s *Server) relayBedrockRealtime(ctx context.Context, request *http.Request, client *websocket.Conn,
	stream *bedrockRealtimeStream, candidate Candidate, alias string, budget KeyBudget, checkTurns bool,
	admit func(context.Context) (bool, bool)) {
	write := func(ctx context.Context, event map[string]any) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return client.Write(ctx, websocket.MessageText, payload)
	}
	state := geminiLiveResponse{alias:alias, session:"sess_"+newUUID(), conversation:"conv_"+newUUID()}
	if write(ctx, map[string]any{"type":"session.created", "event_id":"event_"+newUUID(),
		"session":map[string]any{"id":state.session, "model":alias, "modalities":[]string{"text","audio"}}}) != nil {
		return
	}
	var generating atomic.Bool
	var toolCalls sync.Map
	providerDone := make(chan struct{})
	runSocketRelay(ctx, func(ctx context.Context) {
		var input bedrockRealtimeInput
		send := func(events []map[string]any) bool {
			for _, event := range events {
				payload, err := json.Marshal(event)
				if err != nil || stream.send(payload) != nil {
					return false
				}
			}
			return true
		}
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
				if status := websocket.CloseStatus(err); (status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway) && input.prompt != "" {
					timeout := time.AfterFunc(time.Second, stream.close)
					defer timeout.Stop()
					_ = send(input.commit())
					_ = send([]map[string]any{bedrockRealtimeEvent("promptEnd", map[string]any{"promptName":input.prompt}),
						bedrockRealtimeEvent("sessionEnd", map[string]any{})})
					if stream.finish() == nil {
						select {
						case <-providerDone:
						case <-stream.ctx.Done():
						}
					}
				}
				return
			}
			if !s.socketStillAllowed(request, alias, candidate) {
				_ = writeSocketError(ctx, client, http.StatusForbidden, "The key cannot use this Realtime session")
				return
			}
			data, err = realtimeEvent(kind, data, alias, candidate.ModelID)
			if err != nil {
				if writeSocketError(ctx, client, http.StatusBadRequest, err.Error()) != nil {
					return
				}
				continue
			}
			var message map[string]any
			_ = json.Unmarshal(data, &message)
			kindName, _ := message["type"].(string)
			if kindName == "session.update" {
				if input.prompt != "" {
					if writeSocketError(ctx, client, http.StatusBadRequest, "Bedrock Realtime session settings are fixed after initialization") != nil {
						return
					}
					continue
				}
				if !send(input.setup(objectField(message, "session"))) {
					return
				}
				continue
			}
			item := objectField(message, "item")
			audio, _ := message["audio"].(string)
			text := bedrockRealtimeUserText(item)
			callID, _ := item["call_id"].(string)
			toolOutput, _ := item["output"].(string)
			audioInput := kindName == "input_audio_buffer.append" && audio != ""
			toolInput := kindName == "conversation.item.create" && item["type"] == "function_call_output" && callID != ""
			textInput := kindName == "conversation.item.create" && text != ""
			if toolInput {
				if _, known := toolCalls.Load(callID); !known {
					if writeSocketError(ctx, client, http.StatusBadRequest, "Unknown Bedrock Realtime tool call") != nil {
						return
					}
					continue
				}
			}
			if checkTurns && ((audioInput && !generating.Load()) || toolInput || textInput) {
				allowed, keepOpen := admit(ctx)
				if !keepOpen {
					return
				}
				if !allowed {
					continue
				}
				generating.Store(true)
			}
			if input.prompt == "" && (audioInput || toolInput || textInput) && !send(input.setup(nil)) {
				return
			}
			var events []map[string]any
			switch {
			case audioInput:
				events = input.audioChunk(audio)
			case kindName == "input_audio_buffer.commit":
				events = input.commit()
			case toolInput:
				events = input.toolResult(callID, toolOutput)
			case textInput:
				events = input.text("USER", text)
			}
			if !send(events) {
				return
			}
			if toolInput {
				toolCalls.Delete(callID)
			}
		}
	}, func(ctx context.Context) {
		defer close(providerDone)
		stop := context.AfterFunc(ctx, stream.close)
		defer stop()
		response, err := stream.response(ctx)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusBadGateway, "Could not open the Bedrock Realtime stream")
			return
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 || response.ProtoMajor != 2 {
			status := response.StatusCode
			if status/100 == 2 {
				status = http.StatusBadGateway
			}
			_ = writeSocketError(ctx, client, status, "Bedrock rejected the Realtime stream")
			return
		}
		decoder := eventstream.NewDecoder()
		var accounting realtimeTokenUsage
		var output bedrockRealtimeOutput
		for {
			frame, err := decoder.Decode(response.Body, nil)
			if err != nil {
				return
			}
			if frame.Headers.Get(":message-type") != eventstream.StringValue("event") {
				_ = writeSocketError(ctx, client, http.StatusBadGateway, "Bedrock returned a Realtime stream error")
				return
			}
			var fields map[string]any
			if json.Unmarshal(frame.Payload, &fields) != nil {
				continue
			}
			for _, event := range state.convert(output.translate(objectField(fields, "event"))) {
				if event["type"] == "response.output_item.done" {
					item := objectField(event, "item")
					if callID, _ := item["call_id"].(string); item["type"] == "function_call" && callID != "" {
						toolCalls.Store(callID, struct{}{})
					}
				}
				if event["type"] == "response.done" {
					payload, _ := json.Marshal(event)
					first, err := accounting.charge(ctx, s, budget, payload)
					if err != nil {
						_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not record Realtime token usage")
						return
					}
					if first {
						s.recordSocketUsage(request, candidate, alias, "realtime", payload, budget)
					}
					generating.Store(false)
				}
				if write(ctx, event) != nil {
					return
				}
			}
		}
	})
}

func bedrockRealtimeUserText(item map[string]any) string {
	parts, _ := item["content"].([]any)
	var text []string
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if value, _ := part["text"].(string); part["type"] == "input_text" && value != "" {
			text = append(text, value)
		}
	}
	return strings.Join(text, " ")
}
