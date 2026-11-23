package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const localMCPToolURL = "litellm_proxy/mcp"

var mcpFunctionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) expandMCPInferenceTools(r *http.Request, endpoint string, fields map[string]any, automatic map[string]mcpInferenceTool) error {
	tools, _ := fields["tools"].([]any)
	var selected []map[string]any
	expanded := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		address, _ := tool["server_url"].(string)
		if tool["type"] == "mcp" && (address == localMCPToolURL || strings.HasPrefix(address, localMCPToolURL+"/")) {
			selected = append(selected, tool)
		} else {
			expanded = append(expanded, raw)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	access, ok := s.mcpAccess(r)
	if !ok {
		return errors.New("MCP tool discovery requires access to the selected servers")
	}
	used := make(map[string]bool)
	for _, raw := range expanded {
		tool, _ := raw.(map[string]any)
		if function, ok := tool["function"].(map[string]any); ok {
			tool = function
		}
		if name, _ := tool["name"].(string); name != "" {
			used[name] = true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	discovered := make(map[string][]map[string]any)
	for _, declaration := range selected {
		address, _ := declaration["server_url"].(string)
		selector := strings.TrimPrefix(strings.TrimPrefix(address, localMCPToolURL), "/")
		names := strings.Split(selector, ",")
		for _, server := range s.currentMCPServers() {
			if !access(server, "") || !mcpInferenceServerMatch(server, names) {
				continue
			}
			entries, cached := discovered[server.ID]
			if !cached {
				authenticated := s.mcpRequestServer(r.WithContext(ctx), server)
				select {
				case s.sem <- struct{}{}:
				case <-ctx.Done():
					return errors.New("MCP tool discovery timed out")
				}
				var err error
				entries, err = s.mcpTools(ctx, authenticated)
				<-s.sem
				if err != nil {
					return errors.New("Could not list tools from the selected MCP server")
				}
				discovered[server.ID] = entries
			}
			for _, entry := range entries {
				name, _ := entry["name"].(string)
				fullName := server.Name + "-" + name
				if !access(server, name) || !mcpFunctionName.MatchString(fullName) ||
					!mcpInferenceToolAllowed(declaration["allowed_tools"], name, fullName) || used[fullName] {
					continue
				}
				used[fullName] = true
				if declaration["require_approval"] == "never" {
					automatic[fullName] = mcpInferenceTool{server: server, name: name}
				}
				description, _ := entry["description"].(string)
				schema, _ := entry["inputSchema"].(map[string]any)
				parameters := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
				for key, value := range schema {
					parameters[key] = value
				}
				function := map[string]any{"name": fullName, "description": description, "parameters": parameters, "strict": false}
				if endpoint == "chat-completions" {
					expanded = append(expanded, map[string]any{"type": "function", "function": function})
				} else {
					function["type"] = "function"
					expanded = append(expanded, function)
				}
			}
		}
	}
	if len(expanded) == len(tools)-len(selected) {
		return errors.New("No permitted MCP tools match the request")
	}
	fields["tools"] = expanded
	return nil
}

func mcpInferenceServerMatch(server MCPServer, selectors []string) bool {
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" || server.matches(selector) {
			return true
		}
	}
	return false
}

func mcpInferenceToolAllowed(allowed any, name, fullName string) bool {
	if allowed == nil {
		return true
	}
	if object, ok := allowed.(map[string]any); ok {
		allowed = object["tool_names"]
	}
	selectors, _ := allowed.([]any)
	for _, value := range selectors {
		if value == name || value == fullName {
			return true
		}
	}
	return false
}

type mcpInferenceTool struct {
	server MCPServer
	name   string
}

type mcpInferenceCall struct {
	name, id, arguments string
}

func (s *Server) inferWithMCPTools(w http.ResponseWriter, r *http.Request, endpoint, alias string, scope []string, fields map[string]any, allowed map[string]mcpInferenceTool, stream bool) {
	if stream || inputBool(fields, "background") {
		writeError(w, http.StatusBadRequest, "mcp_tool_error", "Automatic MCP tools currently require a foreground non-streaming request")
		return
	}
	fields = clone(fields)
	fields["cache"] = map[string]any{"no-cache": true, "no-store": true}
	target := endpoints[endpoint].path
	invoke := func() (*ragResult, bool) {
		data, _ := json.Marshal(fields)
		return s.ragStage(w, func(output http.ResponseWriter) {
			s.infer(output, ragSubrequest(r, http.MethodPost, target, data, false), endpoint, alias, scope)
		})
	}
	initial, ok := invoke()
	if !ok {
		return
	}
	var response map[string]any
	if json.Unmarshal(initial.body.Bytes(), &response) != nil || response == nil {
		writeRawResponse(w, initial.status, initial.header, initial.body.Bytes())
		return
	}
	calls, transcript := mcpInferenceCalls(endpoint, response)
	if len(calls) == 0 || len(calls) > 32 {
		writeRawResponse(w, initial.status, initial.header, initial.body.Bytes())
		return
	}
	arguments := make([]map[string]json.RawMessage, len(calls))
	seen := make(map[string]bool)
	for index, call := range calls {
		tool, found := allowed[call.name]
		if !found || call.id == "" || seen[call.id] || json.Unmarshal([]byte(call.arguments), &arguments[index]) != nil ||
			!tool.server.allowsTool(tool.name) || !tool.server.allowsArguments(tool.name, arguments[index]) {
			writeRawResponse(w, initial.status, initial.header, initial.body.Bytes())
			return
		}
		seen[call.id] = true
	}
	var outputs []any
	var execution []map[string]any
	for index, call := range calls {
		tool := allowed[call.name]
		output, err := s.runMCPInferenceTool(r, tool, arguments[index])
		if err != nil {
			output = `{"isError":true,"content":[{"type":"text","text":"MCP tool call failed"}]}`
		}
		execution = append(execution, map[string]any{"name": call.name, "call_id": call.id, "output": output})
		if endpoint == "chat-completions" {
			outputs = append(outputs, map[string]any{"role": "tool", "tool_call_id": call.id, "content": output})
		} else {
			outputs = append(outputs, map[string]any{"type": "function_call_output", "call_id": call.id, "output": output})
		}
	}
	if endpoint == "chat-completions" {
		messages, _ := fields["messages"].([]any)
		fields["messages"] = append(append(messages, transcript...), outputs...)
	} else {
		input, _ := fields["input"].([]any)
		if text, ok := fields["input"].(string); ok {
			input = []any{map[string]string{"role": "user", "content": text}}
		}
		fields["input"] = append(append(input, transcript...), outputs...)
	}
	fields["tool_choice"] = "none"
	if final, ok := invoke(); ok {
		body := final.body.Bytes()
		if endpoint == "responses" {
			body = appendMCPInferenceOutput(body, fields["tools"], execution)
		}
		writeRawResponse(w, final.status, final.header, body)
	}
}

func mcpInferenceCalls(endpoint string, response map[string]any) ([]mcpInferenceCall, []any) {
	if endpoint == "responses" {
		output, _ := response["output"].([]any)
		var calls []mcpInferenceCall
		for _, item := range output {
			fields, _ := item.(map[string]any)
			if fields["type"] == "function_call" {
				name, _ := fields["name"].(string)
				id, _ := fields["call_id"].(string)
				args, _ := fields["arguments"].(string)
				calls = append(calls, mcpInferenceCall{name, id, args})
			}
		}
		return calls, output
	}
	choices, _ := response["choices"].([]any)
	if len(choices) != 1 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	items, _ := message["tool_calls"].([]any)
	calls := make([]mcpInferenceCall, 0, len(items))
	for _, item := range items {
		fields, _ := item.(map[string]any)
		fn, _ := fields["function"].(map[string]any)
		name, _ := fn["name"].(string)
		id, _ := fields["id"].(string)
		args, _ := fn["arguments"].(string)
		calls = append(calls, mcpInferenceCall{name, id, args})
	}
	return calls, []any{message}
}

func (s *Server) runMCPInferenceTool(r *http.Request, tool mcpInferenceTool, args map[string]json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	server := s.mcpRequestServer(r.WithContext(ctx), tool.server)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.sem }()
	session, release, err := s.protocolMCPSession(ctx, server)
	if err != nil {
		return "", err
	}
	defer release()
	if args == nil {
		args = make(map[string]json.RawMessage)
	}
	result, err := session.rpcContext(ctx, "tools/call", map[string]any{"name": tool.name, "arguments": args})
	return string(result), err
}

func appendMCPInferenceOutput(body []byte, tools any, execution []map[string]any) []byte {
	var result map[string]any
	if json.Unmarshal(body, &result) != nil || result == nil {
		return body
	}
	output, ok := result["output"].([]any)
	if !ok {
		return body
	}
	for _, detail := range []struct {
		kind, prefix string
		value        any
	}{
		{"mcp_tools_fetched", "mcp_tools_", tools}, {"tool_execution_results", "tool_results_", execution},
	} {
		text, _ := json.MarshalIndent(detail.value, "", "  ")
		output = append(output, map[string]any{"type": detail.kind, "id": detail.prefix + newUUID()[:8], "status": "completed", "role": "system",
			"content": []map[string]any{{"type": "output_text", "text": string(text), "annotations": []any{}}}})
	}
	result["output"] = output
	encoded, err := json.Marshal(result)
	if err != nil {
		return body
	}
	return encoded
}
