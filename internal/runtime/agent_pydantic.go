package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type pydanticAgentResult struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
	Artifacts []agentCompletionMessage `json:"artifacts"`
	History   []agentCompletionMessage `json:"history"`
	Message   agentCompletionMessage   `json:"message"`
}

func (s *Server) relayPydanticAgent(w http.ResponseWriter, r *http.Request, agent Agent, message mcpRPCRequest) {
	var fields map[string]any
	if json.Unmarshal(message.Params, &fields) != nil || fields == nil {
		writeMCPError(w, http.StatusOK, message.ID, -32602, "Expected A2A message parameters")
		return
	}
	input, ok := fields["message"].(map[string]any)
	if !ok {
		writeMCPError(w, http.StatusOK, message.ID, -32602, "Expected an A2A message")
		return
	}
	input["kind"] = "message"
	params, _ := json.Marshal(removeNullAgentValues(fields))
	request := message
	request.Method, request.Params = "message/send", params
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, ok := s.pydanticAgentRequest(ctx, agent, request)
	if ok && result.Status.State != "completed" && result.ID != "" && !pydanticTaskFailed(result.Status.State) {
		taskID := result.ID
		for attempt := 0; attempt < 30; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					writeMCPError(w, http.StatusGatewayTimeout, message.ID, -32603, "Agent task polling timed out")
					return
				}
			}
			id, _ := json.Marshal(fmt.Sprintf("poll-%d-%s", attempt, newUUID()[:8]))
			params, _ := json.Marshal(map[string]string{"id": taskID})
			result, ok = s.pydanticAgentRequest(ctx, agent, mcpRPCRequest{Version: "2.0", ID: id, Method: "tasks/get", Params: params})
			if !ok || result.Status.State == "completed" || pydanticTaskFailed(result.Status.State) {
				break
			}
		}
	}
	if !ok || (result.Status.State != "" && result.Status.State != "completed") {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Pydantic AI agent task did not complete")
		return
	}
	output := pydanticAgentMessage(result)
	if message.Method != "message/stream" {
		writeMCPResult(w, message.ID, map[string]any{"message": output})
		return
	}
	var initial agentCompletionMessage
	encoded, _ := json.Marshal(input)
	_ = json.Unmarshal(encoded, &initial)
	for _, history := range result.History {
		if history.Role == "user" {
			initial = history
			break
		}
	}
	streamPydanticAgentResult(w, r, message.ID, initial, output)
}

func removeNullAgentValues(value any) any {
	switch object := value.(type) {
	case map[string]any:
		for key, entry := range object {
			if entry == nil {
				delete(object, key)
			} else {
				object[key] = removeNullAgentValues(entry)
			}
		}
	case []any:
		for index, entry := range object {
			object[index] = removeNullAgentValues(entry)
		}
	}
	return value
}

func pydanticTaskFailed(state string) bool {
	switch state {
	case "failed", "canceled", "cancelled", "rejected":
		return true
	default:
		return false
	}
}

func (s *Server) pydanticAgentRequest(ctx context.Context, agent Agent, message mcpRPCRequest) (pydanticAgentResult, bool) {
	var reply struct {
		Result *pydanticAgentResult `json:"result"`
	}
	body, _ := json.Marshal(message)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(agent.URL, "/"), bytes.NewReader(body))
	if err != nil {
		return pydanticAgentResult{}, false
	}
	request.Header = agent.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return pydanticAgentResult{}, false
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, data, _, err := s.sendGeminiFileRequest(&client, request)
	if err != nil || status/100 != 2 || json.Unmarshal(data, &reply) != nil || reply.Result == nil {
		return pydanticAgentResult{}, false
	}
	return *reply.Result, true
}

func agentMessageText(parts []map[string]any) string {
	var text strings.Builder
	for _, part := range parts {
		if content, ok := part["text"].(string); ok && part["kind"] == "text" {
			text.WriteString(content)
		}
	}
	return text.String()
}

func pydanticAgentMessage(result pydanticAgentResult) agentCompletionMessage {
	found := result.Message
	for index := len(result.History) - 1; index >= 0; index-- {
		item := result.History[index]
		if item.Role == "agent" && agentMessageText(item.Parts) != "" {
			found = item
			break
		}
	}
	for _, item := range result.Artifacts {
		if agentMessageText(item.Parts) != "" {
			found = item
			break
		}
	}
	if found.ID == "" {
		found.ID = newUUID()
	}
	if len(found.Parts) == 0 {
		found.Parts = []map[string]any{{"kind": "text", "text": ""}}
	}
	found.Role = "agent"
	return found
}

func streamPydanticAgentResult(w http.ResponseWriter, r *http.Request, id json.RawMessage, input, output agentCompletionMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	contextID, taskID, artifactID := newUUID(), newUUID(), newUUID()
	emit := func(result map[string]any) bool {
		data, _ := json.Marshal(map[string]any{"id": id, "jsonrpc": "2.0", "result": result})
		return writeStreamFrame(w, flush, []byte(fmt.Sprintf("data: %s\n\n", data)))
	}
	statusEvent := func(state string, final bool) bool {
		return emit(map[string]any{"contextId": contextID, "taskId": taskID, "kind": "status-update", "status": map[string]string{"state": state}, "final": final})
	}
	if input.ID == "" {
		input.ID = newUUID()
	}
	if input.Role == "" {
		input.Role = "user"
	}
	if !emit(map[string]any{"id": taskID, "contextId": contextID, "kind": "task", "status": map[string]string{"state": "submitted"},
		"history": []map[string]any{{"contextId": contextID, "kind": "message", "messageId": input.ID, "parts": input.Parts, "role": input.Role, "taskId": taskID}}}) ||
		!statusEvent("working", false) {
		return
	}
	text := []rune(agentMessageText(output.Parts))
	for start := 0; start < len(text); start += 50 {
		if r.Context().Err() != nil {
			return
		}
		end := min(start+50, len(text))
		if !emit(map[string]any{"contextId": contextID, "taskId": taskID, "kind": "artifact-update", "append": start > 0, "lastChunk": end == len(text),
			"artifact": map[string]any{"artifactId": artifactID, "name": "response", "parts": agentTextParts(string(text[start:end]))}}) {
			return
		}
	}
	statusEvent("completed", true)
}
