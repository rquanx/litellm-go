package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
)

type agentCompletionMessage struct {
	ID    string           `json:"messageId"`
	Role  string           `json:"role"`
	Parts []map[string]any `json:"parts"`
}

func (s *Server) relayAgentCompletion(w http.ResponseWriter, r *http.Request, agent Agent, message mcpRPCRequest) {
	var params struct {
		Message *agentCompletionMessage `json:"message"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.Message == nil {
		writeMCPError(w, http.StatusOK, message.ID, -32602, "Expected an A2A message")
		return
	}
	input := params.Message
	role := input.Role
	switch role {
	case "", "user":
		role = "user"
	case "agent", "assistant":
		role = "assistant"
	case "system":
	default:
		writeMCPError(w, http.StatusOK, message.ID, -32602, "Invalid A2A message role")
		return
	}
	text := make([]string, 0, len(input.Parts))
	for _, part := range input.Parts {
		if value, ok := part["text"].(string); ok && part["kind"] == "text" {
			text = append(text, value)
		}
	}
	candidate := *agent.Completion
	fields := map[string]any{"model": candidate.ModelID, "messages": []map[string]string{{"role": role, "content": strings.Join(text, "\n")}}}
	for name, value := range agent.CompletionOptions {
		fields[name] = value
	}
	if len(agent.Headers) != 0 {
		candidate.Headers = maps.Clone(candidate.Headers)
		if candidate.Headers == nil {
			candidate.Headers = make(map[string]string)
		}
		for key := range agent.Headers {
			candidate.Headers[key] = agent.Headers.Get(key)
		}
	}
	payload := requestPayload{fields: fields}
	if recorder, ok := w.(*callbackResponse); ok {
		recorder.price = candidate.UsagePrice
		recorder.cachePrice = candidate.CacheReadPrice
		recorder.cacheCreationPrice = candidate.CacheCreationPrice
	}
	if message.Method == "message/stream" {
		s.streamAgentCompletion(w, r, candidate, payload, message.ID, *input)
		return
	}
	status, data, _, _, err := s.callJSON(r.Context(), r, "chat-completions", candidate, payload)
	if err != nil || status/100 != 2 {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Agent model request failed")
		return
	}
	data = normalizeResponse("chat-completions", candidate.Endpoint, data, agent.Name)
	if recorder, ok := w.(*callbackResponse); ok {
		recorder.recordJSON(data)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &response) != nil || len(response.Choices) == 0 {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Invalid agent model response")
		return
	}
	writeMCPResult(w, message.ID, map[string]any{"message": map[string]any{
		"role": "agent", "parts": agentTextParts(response.Choices[0].Message.Content), "messageId": strings.ReplaceAll(newUUID(), "-", ""),
	}})
}

func (s *Server) streamAgentCompletion(w http.ResponseWriter, r *http.Request, candidate Candidate, payload requestPayload, id json.RawMessage, input agentCompletionMessage) {
	payload.fields["stream_options"] = map[string]any{"include_usage": true}
	status, response, _, err := s.callStream(r.Context(), r, "chat-completions", candidate, payload)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil || status/100 != 2 {
		writeMCPError(w, http.StatusBadGateway, id, -32603, "Agent model stream failed")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	contextID, taskID := newUUID(), newUUID()
	emit := func(result map[string]any) bool {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		return writeStreamFrame(w, flush, []byte(fmt.Sprintf("data: %s\n\n", data)))
	}
	statusEvent := func(state string, final bool) bool {
		if recorder, ok := w.(*callbackResponse); ok && state == "failed" {
			recorder.rpcFailure = true
		}
		return emit(map[string]any{"kind": "status-update", "contextId": contextID, "taskId": taskID, "final": final, "status": map[string]string{"state": state}})
	}
	if input.ID == "" {
		input.ID = strings.ReplaceAll(newUUID(), "-", "")
	}
	if input.Role == "" {
		input.Role = "user"
	}
	if !emit(map[string]any{"id": taskID, "contextId": contextID, "kind": "task", "status": map[string]string{"state": "submitted"},
		"history": []map[string]any{{"contextId": contextID, "kind": "message", "messageId": input.ID, "parts": input.Parts, "role": input.Role, "taskId": taskID}}}) ||
		!statusEvent("working", false) {
		return
	}
	converter := streamConverter("chat-completions", candidate.Endpoint, candidate.ModelID, payload.fields, s.config.MaxBodyBytes)
	reader := bufio.NewReader(response.Body)
	var text strings.Builder
	done, finished := false, false
	for !done {
		frame, readErr := readSSEFrame(reader, s.config.MaxBodyBytes)
		frame = converter(frame)
		for _, part := range strings.Split(string(frame), "\n\n") {
			data := streamErrorBody([]byte(part))
			if string(data) == "[DONE]" {
				done = true
				break
			}
			if len(data) == 0 {
				continue
			}
			if recorder, ok := w.(*callbackResponse); ok {
				recorder.recordJSON(data)
			}
			var chunk struct {
				Error   json.RawMessage `json:"error"`
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					Finish *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(data, &chunk) != nil {
				continue
			}
			if len(chunk.Error) != 0 && string(chunk.Error) != "null" {
				statusEvent("failed", true)
				return
			}
			if len(chunk.Choices) > 0 {
				next := chunk.Choices[0].Delta.Content
				if int64(text.Len()+len(next)) > s.config.MaxBodyBytes {
					statusEvent("failed", true)
					return
				}
				text.WriteString(next)
				if chunk.Choices[0].Finish != nil {
					finished = true
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF || (!done && !finished) {
				statusEvent("failed", true)
				return
			}
			break
		}
	}
	if text.Len() > 0 && !emit(map[string]any{"kind": "artifact-update", "contextId": contextID, "taskId": taskID,
		"artifact": map[string]any{"artifactId": newUUID(), "name": "response", "parts": agentTextParts(text.String())}}) {
		return
	}
	statusEvent("completed", true)
}

func agentTextParts(text string) []map[string]string {
	return []map[string]string{{"kind": "text", "text": text}}
}
