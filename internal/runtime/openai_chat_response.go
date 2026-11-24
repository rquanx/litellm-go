package runtime

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

var chatMessageFields = strings.Fields("content role function_call tool_calls audio provider_specific_fields reasoning_content thinking_blocks annotations images")
var chatChoiceFields = strings.Fields("finish_reason index message logprobs enhancements provider_specific_fields")

func normalizeOpenAIChat(body []byte) []byte {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil || response == nil {
		return body
	}
	choices, ok := response["choices"].([]any)
	if !ok {
		return body
	}
	for index, raw := range choices {
		choice, _ := raw.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		specific := gatherChatExtensions(message, chatMessageFields)
		if _, present := specific["refusal"]; !present {
			specific["refusal"] = nil
		}
		for _, field := range []string{"reasoning_content", "thinking_blocks"} {
			if value := message[field]; value != nil {
				specific[field] = value
			}
		}
		if message["role"] == nil || message["role"] == "" {
			message["role"] = "assistant"
		}
		finish := choice["finish_reason"]
		if finish == nil {
			finish = choice["finish_details"]
			if finish == nil {
				finish = "stop"
			}
		}
		if calls, _ := message["tool_calls"].([]any); finish == "stop" && len(calls) > 0 {
			finish = "tool_calls"
		}
		gatherChatExtensions(choice, chatChoiceFields)
		choice["finish_reason"] = finish
		choice["index"] = index
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return body
	}
	return encoded
}

func gatherChatExtensions(fields map[string]any, known []string) map[string]any {
	provided, _ := fields["provider_specific_fields"].(map[string]any)
	extensions := make(map[string]any, len(provided))
	maps.Copy(extensions, provided)
	for name, value := range fields {
		if !slices.Contains(known, name) {
			extensions[name] = value
			delete(fields, name)
		}
	}
	fields["provider_specific_fields"] = extensions
	return extensions
}
