package runtime

import (
	"bytes"
	"encoding/json"
	"time"
)

type responsesChatStream struct {
	alias        string
	id           string
	created      any
	includeUsage bool
	started      bool
	done         bool
	calls        map[string]int
}

func (stream *responsesChatStream) convert(frame []byte) []byte {
	if stream.done {
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
		if stream.done {
			break
		}
	}
	return output.Bytes()
}

func (stream *responsesChatStream) accept(output *bytes.Buffer, event map[string]any) {
	response, _ := event["response"].(map[string]any)
	switch event["type"] {
	case "response.created", "response.in_progress":
		stream.start(output, response)
	case "response.output_text.delta", "response.reasoning_summary_text.delta":
		stream.start(output, response)
		key := "content"
		if event["type"] == "response.reasoning_summary_text.delta" {
			key = "reasoning_content"
		}
		stream.chunk(output, map[string]any{key: event["delta"]}, nil, nil)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item["type"] != "function_call" {
			return
		}
		stream.start(output, response)
		id, _ := item["id"].(string)
		if _, exists := stream.calls[id]; exists {
			return
		}
		index := len(stream.calls)
		stream.calls[id] = index
		stream.tool(output, map[string]any{
			"index": index, "id": item["call_id"], "type": "function",
			"function": map[string]any{"name": item["name"], "arguments": item["arguments"]},
		})
	case "response.function_call_arguments.delta":
		id, _ := event["item_id"].(string)
		if index, ok := stream.calls[id]; ok {
			stream.tool(output, map[string]any{
				"index": index, "function": map[string]any{"arguments": event["delta"]},
			})
		}
	case "response.completed", "response.incomplete":
		stream.start(output, response)
		finish := "stop"
		if len(stream.calls) != 0 {
			finish = "tool_calls"
		}
		if event["type"] == "response.incomplete" {
			finish = "length"
			details, _ := response["incomplete_details"].(map[string]any)
			if details["reason"] == "content_filter" {
				finish = "content_filter"
			}
		}
		stream.chunk(output, map[string]any{}, finish, nil)
		if stream.includeUsage && response["usage"] != nil {
			stream.chunk(output, nil, nil, convertTokenUsage(response["usage"], false))
		}
		output.WriteString("data: [DONE]\n\n")
		stream.done = true
	case "response.failed", "error":
		failure := response["error"]
		if failure == nil {
			failure = event["error"]
		}
		if failure == nil {
			failure = map[string]any{"code": event["code"], "message": event["message"]}
		}
		writeChatData(output, map[string]any{"error": failure})
		stream.done = true
	}
}

func (stream *responsesChatStream) start(output *bytes.Buffer, response map[string]any) {
	if stream.started {
		return
	}
	stream.id, _ = response["id"].(string)
	if stream.id == "" {
		stream.id = "chatcmpl_" + newUUID()
	}
	stream.created = response["created_at"]
	if stream.created == nil {
		stream.created = time.Now().Unix()
	}
	stream.calls = make(map[string]int)
	stream.started = true
	stream.chunk(output, map[string]any{"role": "assistant"}, nil, nil)
}

func (stream *responsesChatStream) tool(output *bytes.Buffer, call map[string]any) {
	stream.chunk(output, map[string]any{"tool_calls": []any{call}}, nil, nil)
}

func (stream *responsesChatStream) chunk(output *bytes.Buffer, delta map[string]any, finish, usage any) {
	choices := []any{}
	if delta != nil {
		choices = append(choices, map[string]any{"index": 0, "delta": delta, "finish_reason": finish})
	}
	chunk := map[string]any{
		"id": stream.id, "object": "chat.completion.chunk", "created": stream.created, "model": stream.alias, "choices": choices,
	}
	if stream.includeUsage {
		chunk["usage"] = usage
	}
	writeChatData(output, chunk)
}

func writeChatData(output *bytes.Buffer, value map[string]any) {
	payload, _ := json.Marshal(value)
	output.WriteString("data: ")
	output.Write(payload)
	output.WriteString("\n\n")
}
