package runtime

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

type responseStreamItem struct {
	index int
	id    string
	name  string
	call  bool
	data  strings.Builder
}

type chatResponsesStream struct {
	alias     string
	limit     int64
	size      int64
	id        string
	created   any
	sequence  int
	text      *responseStreamItem
	calls     map[int]*responseStreamItem
	items     []*responseStreamItem
	usage     any
	finish    string
	done      bool
}

func (stream *chatResponsesStream) convert(frame []byte) []byte {
	if stream.done {
		return nil
	}
	var output bytes.Buffer
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if bytes.Equal(data, []byte("[DONE]")) {
			// The usage-only chunk can arrive after finish_reason.
			if stream.finish != "" {
				stream.complete(&output)
			}
			stream.done = true
			break
		}
		var chunk map[string]any
		if json.Unmarshal(data, &chunk) != nil || chunk == nil {
			continue
		}
		stream.accept(&output, chunk)
		if stream.done {
			break
		}
	}
	return output.Bytes()
}

func (stream *chatResponsesStream) accept(output *bytes.Buffer, chunk map[string]any) {
	if failure := chunk["error"]; failure != nil {
		stream.fail(output, failure)
		return
	}
	if stream.id == "" {
		stream.id, _ = chunk["id"].(string)
		if stream.id == "" {
			stream.id = "resp_" + newUUID()
		}
		stream.created = chunk["created"]
		stream.calls = make(map[int]*responseStreamItem)
		for _, event := range []string{"response.created", "response.in_progress"} {
			stream.emit(output, event, map[string]any{"response": stream.response("in_progress", []any{})})
		}
	}
	if chunk["usage"] != nil {
		stream.usage = convertTokenUsage(chunk["usage"], true)
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if text, _ := delta["content"].(string); text != "" {
		if stream.text == nil {
			stream.text = &responseStreamItem{id: stream.id + "_msg", index: len(stream.items)}
			stream.add(output, stream.text)
			stream.emit(output, "response.content_part.added", map[string]any{
				"item_id": stream.text.id, "output_index": stream.text.index, "content_index": 0, "part": textPart(""),
			})
		}
		if !stream.append(output, stream.text, text) {
			return
		}
		stream.emit(output, "response.output_text.delta", map[string]any{
			"item_id": stream.text.id, "output_index": stream.text.index, "content_index": 0, "delta": text,
		})
	}
	calls, _ := delta["tool_calls"].([]any)
	for _, raw := range calls {
		if !stream.toolDelta(output, raw) {
			return
		}
	}
	if finish, _ := choice["finish_reason"].(string); finish != "" {
		stream.finish = finish
	}
}

func (stream *chatResponsesStream) toolDelta(output *bytes.Buffer, raw any) bool {
	call, _ := raw.(map[string]any)
	index, _ := call["index"].(float64)
	function, _ := call["function"].(map[string]any)
	if index < 0 || index >= 128 || float64(int(index)) != index {
		stream.fail(output, map[string]any{"code": "invalid_tool_index", "message": "Upstream tool index is out of bounds"})
		return false
	}
	item := stream.calls[int(index)]
	if item == nil {
		id, _ := call["id"].(string)
		if id == "" {
			id = stream.id + "_call_" + strconv.Itoa(int(index))
		}
		name, _ := function["name"].(string)
		item = &responseStreamItem{id: id, name: name, call: true, index: len(stream.items)}
		stream.calls[int(index)] = item
		stream.add(output, item)
	}
	arguments, _ := function["arguments"].(string)
	if arguments == "" {
		return true
	}
	if !stream.append(output, item, arguments) {
		return false
	}
	stream.emit(output, "response.function_call_arguments.delta", map[string]any{
		"item_id": item.id, "output_index": item.index, "delta": arguments,
	})
	return true
}

func (stream *chatResponsesStream) add(output *bytes.Buffer, item *responseStreamItem) {
	stream.items = append(stream.items, item)
	stream.emit(output, "response.output_item.added", map[string]any{"output_index": item.index, "item": item.output("in_progress")})
}

func (stream *chatResponsesStream) completionStatus() (string, string) {
	switch stream.finish {
	case "length":
		return "incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

func (stream *chatResponsesStream) complete(output *bytes.Buffer) {
	status, reason := stream.completionStatus()
	items := make([]any, 0, len(stream.items))
	for _, item := range stream.items {
		if item.call {
			stream.emit(output, "response.function_call_arguments.done", map[string]any{
				"item_id": item.id, "output_index": item.index, "name": item.name, "arguments": item.data.String(),
			})
		} else {
			stream.emit(output, "response.output_text.done", map[string]any{
				"item_id": item.id, "output_index": item.index, "content_index": 0, "text": item.data.String(),
			})
			stream.emit(output, "response.content_part.done", map[string]any{
				"item_id": item.id, "output_index": item.index, "content_index": 0, "part": textPart(item.data.String()),
			})
		}
		final := item.output(status)
		items = append(items, final)
		stream.emit(output, "response.output_item.done", map[string]any{"output_index": item.index, "item": final})
	}
	response := stream.response(status, items)
	if reason != "" {
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	stream.emit(output, "response."+status, map[string]any{"response": response})
}

func (stream *chatResponsesStream) response(status string, items []any) map[string]any {
	return map[string]any{
		"id": stream.id, "object": "response", "created_at": stream.created,
		"model": stream.alias, "status": status, "output": items, "usage": stream.usage,
	}
}

func (stream *chatResponsesStream) append(output *bytes.Buffer, item *responseStreamItem, delta string) bool {
	stream.size += int64(len(delta))
	if stream.size > stream.limit {
		stream.fail(output, map[string]any{"code": "stream_output_limit", "message": "Converted stream exceeds the output limit"})
		return false
	}
	item.data.WriteString(delta)
	return true
}

func (stream *chatResponsesStream) fail(output *bytes.Buffer, failure any) {
	response := stream.response("failed", []any{})
	response["error"] = failure
	stream.emit(output, "response.failed", map[string]any{"response": response})
	stream.done = true
}

func (stream *chatResponsesStream) emit(output *bytes.Buffer, event string, fields map[string]any) {
	fields["type"] = event
	fields["sequence_number"] = stream.sequence
	stream.sequence++
	payload, _ := json.Marshal(fields)
	output.WriteString("event: " + event + "\ndata: ")
	output.Write(payload)
	output.WriteString("\n\n")
}

func (item *responseStreamItem) output(status string) map[string]any {
	if item.call {
		return map[string]any{
			"id": item.id, "type": "function_call", "call_id": item.id, "name": item.name,
			"arguments": item.data.String(), "status": status,
		}
	}
	content := []any{}
	if status != "in_progress" {
		content = append(content, textPart(item.data.String()))
	}
	return map[string]any{"id": item.id, "type": "message", "role": "assistant", "status": status, "content": content}
}

func textPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}
