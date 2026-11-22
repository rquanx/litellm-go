package runtime

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

type bedrockSSEBody struct {
	io.ReadCloser
	decoder *eventstream.Decoder
	pending []byte
	stopped bool
	done    bool
}

func newBedrockSSEBody(source io.ReadCloser) *bedrockSSEBody {
	return &bedrockSSEBody{ReadCloser: source, decoder: eventstream.NewDecoder()}
}

func (body *bedrockSSEBody) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 && !body.done {
		message, err := body.decoder.Decode(body.ReadCloser, nil)
		if err != nil {
			if err == io.EOF && body.stopped {
				body.push(map[string]any{"type": "message_stop"})
				body.done = true
				break
			}
			return 0, err
		}
		var event map[string]any
		if err := json.Unmarshal(message.Payload, &event); err != nil {
			return 0, err
		}
		if fmt.Sprint(message.Headers.Get(":message-type")) == "exception" {
			body.push(map[string]any{"type": "error", "error": event})
			body.done = true
			break
		}
		body.accept(fmt.Sprint(message.Headers.Get(":event-type")), event)
	}
	if len(body.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(destination, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *bedrockSSEBody) push(event map[string]any) {
	payload, _ := json.Marshal(event)
	body.pending = append(body.pending, "data: "...)
	body.pending = append(body.pending, payload...)
	body.pending = append(body.pending, '\n', '\n')
}

func (body *bedrockSSEBody) accept(kind string, event map[string]any) {
	switch kind {
	case "messageStart":
		body.push(map[string]any{"type": "message_start", "message": map[string]any{"role": event["role"]}})
	case "contentBlockStart":
		start, _ := event["start"].(map[string]any)
		if tool, ok := start["toolUse"].(map[string]any); ok {
			body.push(map[string]any{"type": "content_block_start", "index": event["contentBlockIndex"],
				"content_block": map[string]any{"type": "tool_use", "id": tool["toolUseId"], "name": tool["name"]}})
		}
	case "contentBlockDelta":
		delta, _ := event["delta"].(map[string]any)
		var converted map[string]any
		if text, ok := delta["text"].(string); ok {
			converted = map[string]any{"type": "text_delta", "text": text}
		} else if tool, ok := delta["toolUse"].(map[string]any); ok {
			converted = map[string]any{"type": "input_json_delta", "partial_json": tool["input"]}
		}
		if converted != nil {
			body.push(map[string]any{"type": "content_block_delta", "index": event["contentBlockIndex"], "delta": converted})
		}
	case "messageStop":
		body.stopped = true
		body.push(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": bedrockAnthropicFinish(event["stopReason"])}})
	case "metadata":
		if usage, ok := event["usage"].(map[string]any); ok {
			converted := map[string]any{"input_tokens": usage["inputTokens"], "output_tokens": usage["outputTokens"],
				"cache_read_input_tokens": usage["cacheReadInputTokens"], "cache_creation_input_tokens": usage["cacheWriteInputTokens"]}
			body.push(map[string]any{"type": "message_delta", "usage": converted})
		}
		if body.stopped {
			body.push(map[string]any{"type": "message_stop"})
			body.done = true
		}
	}
}

func bedrockAnthropicFinish(reason any) string {
	switch bedrockFinish(reason) {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}
