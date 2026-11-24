package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

const geminiLivePath = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
const vertexLivePath = "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"

func geminiLiveProvider(candidate Candidate) bool {
	return candidate.Provider == "gemini" || (candidate.Provider == "vertex_ai" && candidate.Vertex != nil)
}

func (s *Server) dialGeminiLive(ctx context.Context, candidate Candidate) (*websocket.Conn, error) {
	base := strings.TrimRight(candidate.APIBase, "/")
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/v1beta"), "/v1")
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return nil, err
	}
	address := base + geminiLivePath
	if candidate.Vertex != nil {
		if err := applyGeminiCredential(request, candidate, key); err != nil {
			return nil, err
		}
		request.Header.Set("x-goog-user-project", candidate.Vertex.Project)
		address = base + vertexLivePath
	} else {
		if key == "" {
			return nil, errors.New("Gemini Live requires an API key")
		}
		address += "?" + url.Values{"key": {key}}.Encode()
	}
	conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: s.config.HTTPClient, HTTPHeader: request.Header})
	return conn, err
}

func geminiLiveSetup(candidate Candidate, session map[string]any) map[string]any {
	model := strings.TrimPrefix(candidate.ModelID, "models/")
	setup := map[string]any{
		"model": "models/" + model, "inputAudioTranscription": map[string]any{},
	}
	generation := map[string]any{"responseModalities": []string{"AUDIO"}}
	if candidate.Vertex != nil {
		vertex := candidate.Vertex
		setup["model"] = "projects/" + vertex.Project + "/locations/" + vertex.Location + "/publishers/google/models/" + model
		setup["realtimeInputConfig"] = map[string]any{"automaticActivityDetection": map[string]any{"disabled": false, "silenceDurationMs": 800}}
		setup["outputAudioTranscription"] = map[string]any{}
	}
	if value, ok := session["instructions"].(string); ok {
		setup["systemInstruction"] = map[string]any{"role": "user", "parts": []map[string]string{{"text": value}}}
	}
	for name, target := range map[string]string{"temperature": "temperature", "max_response_output_tokens": "maxOutputTokens"} {
		if value, ok := session[name].(float64); ok {
			generation[target] = value
		}
	}
	if modalities, ok := session["modalities"].([]any); ok && len(modalities) != 0 {
		var values []string
		for _, item := range modalities {
			if value, ok := item.(string); ok {
				values = append(values, strings.ToUpper(value))
			}
		}
		if len(values) != 0 {
			generation["responseModalities"] = values
		}
	}
	if config := objectField(session, "turn_detection"); config != nil {
		detection := map[string]any{"disabled": !inputBool(config, "create_response")}
		for name, target := range map[string]string{"prefix_padding_ms": "prefixPaddingMs", "silence_duration_ms": "silenceDurationMs"} {
			if value, ok := config[name].(float64); ok {
				detection[target] = value
			}
		}
		setup["realtimeInputConfig"] = map[string]any{"automaticActivityDetection": detection}
	}
	tools, _ := session["tools"].([]any)
	var declarations []map[string]any
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if name, _ := tool["name"].(string); tool["type"] == "function" && name != "" {
			declaration := map[string]any{"name": name}
			if description, ok := tool["description"].(string); ok {
				declaration["description"] = description
			}
			if params, ok := tool["parameters"].(map[string]any); ok {
				declaration["parametersJsonSchema"] = params
			}
			declarations = append(declarations, declaration)
		}
	}
	if len(declarations) != 0 {
		setup["tools"] = []map[string]any{{"functionDeclarations": declarations}}
	}
	setup["generationConfig"] = generation
	return map[string]any{"setup": setup}
}

func geminiLiveInput(candidate Candidate, event map[string]any) (map[string]any, bool) {
	switch event["type"] {
	case "session.update":
		if candidate.Vertex == nil {
			return geminiLiveSetup(candidate, objectField(event, "session")), false
		}
	case "input_audio_buffer.append":
		audio, _ := event["audio"].(string)
		if audio != "" {
			mime := "audio/pcm"
			if candidate.Vertex != nil {
				mime = "audio/pcm;rate=16000"
			}
			return map[string]any{"realtime_input": map[string]any{"audio": map[string]string{"mimeType": mime, "data": audio}}}, true
		}
	case "conversation.item.create":
		parts, _ := objectField(event, "item")["content"].([]any)
		var text []string
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			if value, _ := part["text"].(string); part["type"] == "input_text" && value != "" {
				text = append(text, value)
			}
		}
		if len(text) != 0 {
			return map[string]any{"realtime_input": map[string]any{"text": strings.Join(text, " ")}}, true
		}
	}
	return nil, false
}

func (s *Server) relayGeminiLive(ctx context.Context, request *http.Request, client, upstream *websocket.Conn,
	candidate Candidate, alias string, budget KeyBudget, checkTurns bool, admit func(context.Context) (bool, bool)) {
	write := func(ctx context.Context, conn *websocket.Conn, fields map[string]any) error {
		body, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return conn.Write(ctx, websocket.MessageText, body)
	}
	if write(ctx, upstream, geminiLiveSetup(candidate, nil)) != nil {
		return
	}
	var generating atomic.Bool
	var tools sync.Map
	runSocketRelay(ctx, func(ctx context.Context) {
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
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
			payload, generates := geminiLiveInput(candidate, message)
			item := objectField(message, "item")
			var toolCall string
			if message["type"] == "conversation.item.create" && item["type"] == "function_call_output" {
				toolCall, _ = item["call_id"].(string)
				name, pending := tools.Load(toolCall)
				output, valid := item["output"].(string)
				if !pending || !valid {
					if writeSocketError(ctx, client, http.StatusBadRequest, "Unknown Gemini Live tool call or invalid output") != nil {
						return
					}
					continue
				}
				result := map[string]any{}
				if json.Unmarshal([]byte(output), &result) != nil || result == nil {
					result = map[string]any{"output": output}
				}
				payload = map[string]any{"toolResponse": map[string]any{"functionResponses": []map[string]any{
					{"id": toolCall, "name": name, "response": result},
				}}}
				generates = true
			}
			if checkTurns && generates && (message["type"] == "conversation.item.create" || !generating.Load()) {
				allowed, keepOpen := admit(ctx)
				if !keepOpen {
					return
				}
				if !allowed {
					continue
				}
				generating.Store(true)
			}
			if payload != nil && write(ctx, upstream, payload) != nil {
				return
			}
			if toolCall != "" {
				tools.Delete(toolCall)
			}
		}
	}, func(ctx context.Context) {
		state := geminiLiveResponse{alias: alias, session: "sess_" + newUUID(), conversation: "conv_" + newUUID()}
		var accounting realtimeTokenUsage
		for {
			_, data, err := upstream.Read(ctx)
			if err != nil {
				return
			}
			var message map[string]any
			_ = json.Unmarshal(data, &message)
			if cancelled, _ := objectField(message, "toolCallCancellation")["ids"].([]any); cancelled != nil {
				for _, id := range cancelled {
					if call, ok := id.(string); ok {
						tools.Delete(call)
					}
				}
			}
			for _, event := range state.convert(message) {
				item := objectField(event, "item")
				if event["type"] == "response.output_item.done" && item["type"] == "function_call" {
					callID, _ := item["call_id"].(string)
					name, _ := item["name"].(string)
					if callID != "" && name != "" {
						tools.Store(callID, name)
					}
				}
				if event["type"] == "response.done" {
					body, _ := json.Marshal(event)
					first, err := accounting.charge(ctx, s, budget, body)
					if err != nil {
						_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not record Realtime token usage")
						return
					}
					if first {
						s.recordSocketUsage(request, candidate, alias, "realtime", body, budget)
					}
					generating.Store(false)
				}
				if write(ctx, client, event) != nil {
					return
				}
			}
		}
	})
}
