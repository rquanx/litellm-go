package runtime

import (
	"bytes"
	"encoding/json"
)

type anthropicChatStream struct {
	chat   responsesChatStream
	tools  map[float64]int
	usage  map[string]any
	finish string
}

func newAnthropicChatStream(alias string, includeUsage bool) *anthropicChatStream {
	return &anthropicChatStream{
		chat: responsesChatStream{alias: alias, includeUsage: includeUsage},
		tools: make(map[float64]int),
		usage: make(map[string]any),
	}
}

func (stream *anthropicChatStream) convert(frame []byte) []byte {
	if stream.chat.done {
		return nil
	}
	var output bytes.Buffer
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		var event map[string]any
		if json.Unmarshal(bytes.TrimSpace(data), &event) == nil {
			stream.accept(&output, event)
		}
		if stream.chat.done {
			break
		}
	}
	return output.Bytes()
}

func (stream *anthropicChatStream) accept(output *bytes.Buffer, event map[string]any) {
	switch event["type"] {
	case "message_start":
		message, _ := event["message"].(map[string]any)
		stream.chat.start(output, message)
		stream.collectUsage(message["usage"])
	case "content_block_start":
		stream.chat.start(output, nil)
		index, _ := event["index"].(float64)
		block, _ := event["content_block"].(map[string]any)
		switch block["type"] {
		case "tool_use":
			i := len(stream.tools)
			stream.tools[index] = i
			arguments := ""
			if input, ok := block["input"].(map[string]any); ok && len(input) != 0 {
				encoded, _ := json.Marshal(input)
				arguments = string(encoded)
			}
			stream.chat.tool(output, map[string]any{"index": i, "id": block["id"], "type": "function",
				"function": map[string]any{"name": block["name"], "arguments": arguments}})
		case "text":
			if text, _ := block["text"].(string); text != "" {
				stream.chat.chunk(output, map[string]any{"content": text}, nil, nil)
			}
		}
	case "content_block_delta":
		stream.chat.start(output, nil)
		delta, _ := event["delta"].(map[string]any)
		switch delta["type"] {
		case "text_delta":
			stream.chat.chunk(output, map[string]any{"content": delta["text"]}, nil, nil)
		case "thinking_delta":
			stream.chat.chunk(output, map[string]any{"reasoning_content": delta["thinking"]}, nil, nil)
		case "input_json_delta":
			index, _ := event["index"].(float64)
			if i, present := stream.tools[index]; present {
				stream.chat.tool(output, map[string]any{"index": i,
					"function": map[string]any{"arguments": delta["partial_json"]}})
			}
		}
	case "message_delta":
		delta, _ := event["delta"].(map[string]any)
		if reason, _ := delta["stop_reason"].(string); reason != "" {
			stream.finish = anthropicFinish(reason)
		}
		stream.collectUsage(event["usage"])
	case "message_stop":
		stream.chat.start(output, nil)
		if stream.finish == "" {
			stream.finish = "stop"
		}
		stream.chat.chunk(output, map[string]any{}, stream.finish, nil)
		if stream.chat.includeUsage && len(stream.usage) != 0 {
			stream.chat.chunk(output, nil, nil, anthropicUsage(stream.usage))
		}
		output.WriteString("data: [DONE]\n\n")
		stream.chat.done = true
	case "error":
		writeChatData(output, map[string]any{"error": event["error"]})
		stream.chat.done = true
	}
}

func (stream *anthropicChatStream) collectUsage(value any) {
	usage, _ := value.(map[string]any)
	for key, value := range usage {
		if value != nil {
			stream.usage[key] = value
		}
	}
}
