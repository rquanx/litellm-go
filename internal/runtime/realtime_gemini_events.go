package runtime

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

type geminiLiveResponse struct {
	alias, session, conversation string
	response, item string
	text strings.Builder
	parts []string
	output []map[string]any
	itemIndex int
	interrupted bool
	usage map[string]any
}

func (state *geminiLiveResponse) convert(message map[string]any) []map[string]any {
	var events []map[string]any
	emit := func(name string, fields map[string]any) {
		fields["type"], fields["event_id"] = name, "event_"+newUUID()
		events = append(events, fields)
	}
	response := func(status string) map[string]any {
		return map[string]any{"id": state.response, "object": "realtime.response", "status": status,
			"conversation_id": state.conversation, "output": []any{}, "modalities": []string{"audio"}}
	}
	start := func() {
		if state.response != "" {
			return
		}
		state.response = "resp_"+newUUID()
		emit("response.created", map[string]any{"response": response("in_progress")})
	}
	contentIndex := func(modality string) int {
		start()
		if state.item == "" {
			state.item = "item_"+newUUID()
			state.itemIndex = len(state.output)
			item := map[string]any{"id": state.item, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
			state.output = append(state.output, item)
			emit("response.output_item.added", map[string]any{"response_id": state.response, "output_index": state.itemIndex, "item": item})
		}
		index := slices.Index(state.parts, modality)
		if index < 0 {
			index = len(state.parts)
			state.parts = append(state.parts, modality)
			emit("response.content_part.added", map[string]any{"response_id": state.response, "item_id": state.item,
				"output_index": state.itemIndex, "content_index": index, "part": map[string]any{"type": modality}})
		}
		return index
	}
	delta := func(name, value, modality string) {
		index := contentIndex(modality)
		emit(name, map[string]any{"response_id": state.response, "item_id": state.item,
			"output_index": state.itemIndex, "content_index": index, "delta": value})
	}
	if _, exists := message["setupComplete"]; exists {
		emit("session.created", map[string]any{"session": map[string]any{"id": state.session, "model": state.alias, "modalities": []string{"audio"}}})
	}
	calls, _ := objectField(message, "toolCall")["functionCalls"].([]any)
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		id, _ := call["id"].(string)
		name, _ := call["name"].(string)
		if id == "" || name == "" {
			continue
		}
		start()
		args := objectField(call, "args")
		if args == nil {
			args = map[string]any{}
		}
		encoded, _ := json.Marshal(args)
		item := map[string]any{"id": "item_"+newUUID(), "type": "function_call", "call_id": id,
			"name": name, "arguments": string(encoded), "status": "completed"}
		index := len(state.output)
		state.output = append(state.output, item)
		emit("response.output_item.added", map[string]any{"response_id": state.response, "output_index": index,
			"item": map[string]any{"id": item["id"], "type": "function_call", "call_id": id, "name": name, "arguments": "", "status": "in_progress"}})
		emit("response.function_call_arguments.done", map[string]any{"response_id": state.response, "item_id": item["id"],
			"call_id": id, "output_index": index, "arguments": string(encoded)})
		emit("response.output_item.done", map[string]any{"response_id": state.response,
			"output_index": index, "item": item})
	}
	if usage := objectField(message, "usageMetadata"); usage != nil {
		input, _ := usage["promptTokenCount"].(float64)
		output, _ := usage["candidatesTokenCount"].(float64)
		reasoning, _ := usage["thoughtsTokenCount"].(float64)
		output += reasoning
		total, _ := usage["totalTokenCount"].(float64)
		state.usage = map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": max(total, input+output)}
		inputAudio := geminiLiveAudioTokens(usage["promptTokensDetails"])
		outputAudio := geminiLiveAudioTokens(usage["candidatesTokensDetails"])
		cachedAudio := geminiLiveAudioTokens(usage["cacheTokensDetails"])
		cached, _ := usage["cachedContentTokenCount"].(float64)
		if inputAudio != 0 || cached != 0 || cachedAudio != 0 {
			state.usage["input_token_details"] = map[string]any{"audio_tokens": inputAudio, "cached_tokens": cached,
				"cached_tokens_details": map[string]any{"audio_tokens": cachedAudio}}
		}
		if outputAudio != 0 || reasoning != 0 {
			state.usage["output_token_details"] = map[string]any{"audio_tokens": outputAudio, "reasoning_tokens": reasoning}
		}
	}
	content := objectField(message, "serverContent")
	if text, _ := objectField(content, "inputTranscription")["text"].(string); text != "" {
		emit("conversation.item.input_audio_transcription.completed", map[string]any{
			"item_id": "item_"+newUUID(), "content_index": 0, "transcript": text})
	}
	if text, _ := objectField(content, "outputTranscription")["text"].(string); text != "" {
		delta("response.audio_transcript.delta", text, "audio")
	}
	parts, _ := objectField(content, "modelTurn")["parts"].([]any)
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if text, _ := part["text"].(string); text != "" {
			if state.text.Len() < 64<<10 {
				size := min(len(text), (64<<10)-state.text.Len())
				for size < len(text) && !utf8.RuneStart(text[size]) {
					size--
				}
				state.text.WriteString(text[:size])
			}
			delta("response.text.delta", text, "text")
		}
		if audio, _ := objectField(part, "inlineData")["data"].(string); audio != "" {
			delta("response.audio.delta", audio, "audio")
		}
	}
	state.interrupted = state.interrupted || inputBool(content, "interrupted")
	if !inputBool(content, "turnComplete") {
		return events
	}
	start()
	status := "completed"
	if state.interrupted {
		status = "cancelled"
	}
	done := response(status)
	outputParts := make([]map[string]any, 0, len(state.parts))
	for index, modality := range state.parts {
		part := map[string]any{"type": modality}
		fields := map[string]any{"response_id": state.response, "item_id": state.item, "output_index": state.itemIndex, "content_index": index}
		if modality == "text" {
			part["text"], fields["text"] = state.text.String(), state.text.String()
		}
		emit("response."+modality+".done", fields)
		outputParts = append(outputParts, part)
		emit("response.content_part.done", map[string]any{"response_id": state.response, "item_id": state.item,
			"output_index": state.itemIndex, "content_index": index, "part": part})
	}
	if len(outputParts) != 0 {
		item := map[string]any{"id": state.item, "type": "message", "role": "assistant", "status": status, "content": outputParts}
		emit("response.output_item.done", map[string]any{"response_id": state.response, "output_index": state.itemIndex, "item": item})
		state.output[state.itemIndex] = item
	}
	if len(state.output) != 0 {
		done["output"] = state.output
	}
	if state.usage != nil {
		done["usage"] = state.usage
	}
	emit("response.done", map[string]any{"response": done})
	state.response, state.item, state.parts, state.usage = "", "", state.parts[:0], nil
	state.output = nil
	state.interrupted = false
	state.text.Reset()
	return events
}

func geminiLiveAudioTokens(details any) float64 {
	entries, _ := details.([]any)
	var total float64
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["modality"] == "AUDIO" {
			tokens, _ := entry["tokenCount"].(float64)
			total += tokens
		}
	}
	return total
}
