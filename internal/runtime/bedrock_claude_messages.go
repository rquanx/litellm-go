package runtime

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

//go:embed bedrock-anthropic-beta.json
var bedrockAnthropicBetaSnapshot []byte

var bedrockAnthropicBetas = func() map[string]string {
	var values map[string]string
	if err := json.Unmarshal(bedrockAnthropicBetaSnapshot, &values); err != nil {
		panic("invalid embedded Bedrock Anthropic beta mapping")
	}
	return values
}()

func bedrockClaudeMessages(endpoint string, candidate Candidate) bool {
	return endpoint == "anthropic-messages" && candidate.Endpoint == "bedrock-converse" &&
		strings.Contains(candidate.ModelID, "anthropic.claude-") && !strings.Contains(candidate.ModelID, "anthropic.claude-v2")
}

func bedrockClaudePayload(input map[string]any, model string, header []string) ([]byte, error) {
	body := map[string]any{"anthropic_version": "bedrock-2023-05-31"}
	for _, name := range []string{"anthropic_version", "max_tokens", "messages", "system", "stop_sequences",
		"temperature", "top_p", "top_k", "tools", "tool_choice", "thinking", "metadata", "output_config"} {
		if value := input[name]; value != nil {
			body[name] = value
		}
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return nil, invalidMediaRequest{errors.New("Bedrock Claude requires messages")}
	}
	// Nested maps are copied before normalization so retry attempts see the original request.
	ttl := strings.Contains(model, "claude-sonnet-4-5") || strings.Contains(model, "claude-haiku-4-5") ||
		strings.Contains(model, "claude-opus-4-5") || strings.Contains(model, "claude-sonnet-4-6") ||
		strings.Contains(model, "claude-opus-4-6") || strings.Contains(model, "claude-opus-4-7")
	body = cleanBedrockClaudeCache(body, ttl).(map[string]any)
	tools, _ := body["tools"].([]any)
	for _, value := range tools {
		if tool, ok := value.(map[string]any); ok {
			delete(tool, "custom")
			if schema, ok := tool["input_schema"].(map[string]any); ok && schema["type"] == nil {
				schema["type"] = "object"
			}
		}
	}
	if format, _ := input["output_format"].(map[string]any); format["schema"] != nil {
		schema, err := json.Marshal(format["schema"])
		if err != nil {
			return nil, err
		}
		messages, _ := body["messages"].([]any)
		for i := len(messages) - 1; i >= 0; i-- {
			message, _ := messages[i].(map[string]any)
			if message["role"] != "user" {
				continue
			}
			parts, _ := message["content"].([]any)
			if text, ok := message["content"].(string); ok {
				parts = []any{map[string]string{"type": "text", "text": text}}
			}
			message["content"] = append(parts, map[string]string{"type": "text", "text": "\n\nRespond with JSON matching this schema:\n" + string(schema)})
			break
		}
	}
	var betas []string
	if contextManagement, _ := input["context_management"].(map[string]any); contextManagement != nil {
		edits, _ := contextManagement["edits"].([]any)
		compact := make([]any, 0, len(edits))
		for _, value := range edits {
			if edit, _ := value.(map[string]any); edit["type"] == "compact_20260112" {
				compact = append(compact, edit)
			}
		}
		if len(compact) > 0 {
			cfg := clone(contextManagement)
			cfg["edits"] = compact
			body["context_management"] = cfg
			betas = append(betas, "compact-2026-01-12")
		}
	}
	for _, value := range header {
		for _, beta := range strings.Split(value, ",") {
			if mapped := bedrockAnthropicBetas[strings.TrimSpace(beta)]; mapped != "" {
				betas = append(betas, mapped)
			}
		}
	}
	if len(betas) != 0 {
		slices.Sort(betas)
		body["anthropic_beta"] = slices.Compact(betas)
	}
	return json.Marshal(body)
}

func cleanBedrockClaudeCache(value any, ttl bool) any {
	switch node := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(node))
		for name, item := range node {
			if args, ok := item.(map[string]any); name == "cache_control" && ok {
				args = clone(args)
				delete(args, "scope")
				if !ttl {
					delete(args, "ttl")
				}
				result[name] = args
			} else {
				result[name] = cleanBedrockClaudeCache(item, ttl)
			}
		}
		return result
	case []any:
		result := make([]any, len(node))
		for i, item := range node {
			result[i] = cleanBedrockClaudeCache(item, ttl)
		}
		return result
	default:
		return value
	}
}

type bedrockClaudeSSE struct {
	io.ReadCloser
	decoder    *eventstream.Decoder
	alias      string
	pending    []byte
	startUsage map[string]any
	delta      map[string]any
	done       bool
}

func newBedrockClaudeSSE(body io.ReadCloser, alias string) *bedrockClaudeSSE {
	return &bedrockClaudeSSE{ReadCloser: body, decoder: eventstream.NewDecoder(), alias: alias}
}

func (body *bedrockClaudeSSE) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 && !body.done {
		frame, err := body.decoder.Decode(body.ReadCloser, nil)
		if err != nil {
			if err == io.EOF && body.delta != nil {
				body.done = true
				body.flushDelta(nil)
				break
			}
			return 0, err
		}
		var outer struct {
			Data []byte `json:"bytes"`
		}
		var payload map[string]any
		if messageType := fmt.Sprint(frame.Headers.Get(":message-type")); messageType == "exception" || messageType == "error" {
			var detail map[string]any
			_ = json.Unmarshal(frame.Payload, &detail)
			payload = map[string]any{"type": "error", "error": detail}
		} else if json.Unmarshal(frame.Payload, &outer) != nil || json.Unmarshal(outer.Data, &payload) != nil || payload == nil {
			return 0, errors.New("Invalid Bedrock Claude stream event")
		}
		kind, _ := payload["type"].(string)
		if kind == "" || strings.ContainsAny(kind, "\r\n") {
			return 0, errors.New("Invalid Bedrock Claude stream event type")
		}
		if message, ok := payload["message"].(map[string]any); kind == "message_start" && ok {
			body.startUsage, _ = message["usage"].(map[string]any)
			if body.alias != "" {
				message["model"] = body.alias
			}
		}
		if metrics, ok := payload["amazon-bedrock-invocationMetrics"].(map[string]any); ok {
			usage, _ := payload["usage"].(map[string]any)
			if usage == nil {
				usage = make(map[string]any)
			}
			for key, name := range map[string]string{"inputTokenCount": "input_tokens", "outputTokenCount": "output_tokens"} {
				if value := metrics[key]; value != nil {
					usage[name] = value
				}
			}
			payload["usage"] = usage
			delete(payload, "amazon-bedrock-invocationMetrics")
		}
		body.done = kind == "message_stop" || kind == "error"
		if kind == "message_delta" && body.delta == nil {
			body.delta = payload
			continue
		}
		if kind == "message_stop" {
			stopUsage, _ := payload["usage"].(map[string]any)
			body.flushDelta(stopUsage)
		} else {
			body.flushDelta(nil)
		}
		body.push(kind, payload)
	}
	if len(body.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(dst, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *bedrockClaudeSSE) flushDelta(stopUsage map[string]any) {
	if body.delta == nil {
		return
	}
	usage, _ := body.delta["usage"].(map[string]any)
	if usage == nil {
		usage = make(map[string]any)
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation"} {
		if value := stopUsage[name]; value != nil {
			usage[name] = value
		} else if value := body.startUsage[name]; usage[name] == nil && value != nil && strings.HasPrefix(name, "cache_") {
			usage[name] = value
		}
	}
	if len(usage) > 0 {
		body.delta["usage"] = usage
	}
	body.push("message_delta", body.delta)
	body.delta = nil
}

func (body *bedrockClaudeSSE) push(kind string, payload map[string]any) {
	data, _ := json.Marshal(payload)
	body.pending = append(body.pending, "event: "+kind+"\ndata: "...)
	body.pending = append(body.pending, data...)
	body.pending = append(body.pending, '\n', '\n')
}
