package runtime

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"
)

type geminiChatStream struct {
	alias        string
	id           string
	created      int64
	includeUsage bool
	usage        any
	choices      map[int]*geminiChoiceState
	done         bool
}

type geminiChoiceState struct {
	calls  int
	finish string
}

func newGeminiChatStream(alias string, includeUsage bool) *geminiChatStream {
	return &geminiChatStream{alias: alias, includeUsage: includeUsage, choices: make(map[int]*geminiChoiceState)}
}

func (stream *geminiChatStream) convert(frame []byte) []byte {
	if stream.done {
		return nil
	}
	var output bytes.Buffer
	if frame == nil {
		stream.complete(&output)
		return output.Bytes()
	}
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if bytes.Equal(data, []byte("[DONE]")) {
			stream.complete(&output)
			break
		}
		var chunk map[string]any
		if json.Unmarshal(data, &chunk) == nil {
			stream.accept(&output, chunk)
		}
		if stream.done {
			break
		}
	}
	return output.Bytes()
}

func (stream *geminiChatStream) accept(output *bytes.Buffer, chunk map[string]any) {
	if failure := chunk["error"]; failure != nil {
		writeChatData(output, map[string]any{"error": failure})
		stream.done = true
		return
	}
	if stream.id == "" {
		stream.id, _ = chunk["responseId"].(string)
		if stream.id == "" {
			stream.id = "chatcmpl_" + newUUID()
		}
		stream.created = time.Now().Unix()
	}
	if usage := geminiUsage(chunk); usage != nil {
		stream.usage = convertTokenUsage(usage, false)
	}
	candidates, _ := chunk["candidates"].([]any)
	for position, entry := range candidates {
		candidate, _ := entry.(map[string]any)
		index := position
		if value, ok := candidate["index"].(float64); ok && value >= 0 && value < 128 {
			index = int(value)
		}
		state := stream.choices[index]
		if state == nil {
			state = &geminiChoiceState{}
			stream.choices[index] = state
			stream.write(output, index, map[string]any{"role": "assistant"}, nil, nil)
		}
		message := geminiCandidateChatMessage(candidate)
		calls, _ := message["tool_calls"].([]any)
		for _, raw := range calls {
			call := raw.(map[string]any)
			call["index"] = state.calls
			state.calls++
		}
		if len(message) != 0 {
			stream.write(output, index, message, nil, nil)
		}
		if reason, ok := candidate["finishReason"].(string); ok && reason != "" && reason != "FINISH_REASON_UNSPECIFIED" {
			state.finish = geminiFinish(reason, state.calls != 0)
		}
	}
}

func (stream *geminiChatStream) complete(output *bytes.Buffer) {
	stream.done = true
	indices := make([]int, 0, len(stream.choices))
	for index, choice := range stream.choices {
		if choice.finish == "" {
			return
		}
		indices = append(indices, index)
	}
	if len(indices) == 0 {
		return
	}
	slices.Sort(indices)
	for _, index := range indices {
		stream.write(output, index, map[string]any{}, stream.choices[index].finish, nil)
	}
	if stream.includeUsage && stream.usage != nil {
		stream.write(output, 0, nil, nil, stream.usage)
	}
	output.WriteString("data: [DONE]\n\n")
}

func (stream *geminiChatStream) write(output *bytes.Buffer, index int, delta map[string]any, finish, usage any) {
	choices := []any{}
	if delta != nil {
		choices = append(choices, map[string]any{"index": index, "delta": delta, "finish_reason": finish})
	}
	chunk := map[string]any{"id": stream.id, "object": "chat.completion.chunk", "created": stream.created,
		"model": stream.alias, "choices": choices}
	if stream.includeUsage {
		chunk["usage"] = usage
	}
	writeChatData(output, chunk)
}
