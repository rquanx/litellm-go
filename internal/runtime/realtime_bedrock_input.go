package runtime

import "encoding/json"

type bedrockRealtimeInput struct {
	prompt string
	audio string
	inputRate int
}

func bedrockRealtimeEvent(name string, value map[string]any) map[string]any {
	return map[string]any{"event": map[string]any{name: value}}
}

func (input *bedrockRealtimeInput) setup(session map[string]any) []map[string]any {
	input.prompt = newUUID()
	inference := map[string]any{"maxTokens": 1024, "topP": 0.9, "temperature": 0.7}
	for from, to := range map[string]string{"max_response_output_tokens":"maxTokens", "temperature":"temperature"} {
		if value, ok := session[from].(float64); ok {
			inference[to] = value
		}
	}
	input.inputRate = 16000
	if rate, _ := session["input_sample_rate_hertz"].(float64); rate == 8000 || rate == 16000 || rate == 24000 {
		input.inputRate = int(rate)
	}
	voice, _ := session["voice"].(string)
	if voice == "" {
		voice = "matthew"
	}
	audio := bedrockRealtimePCM(24000)
	audio["voiceId"] = voice
	prompt := map[string]any{"promptName": input.prompt, "textOutputConfiguration": map[string]string{"mediaType":"text/plain"},
		"audioOutputConfiguration": audio}
	var tools []map[string]any
	rawTools, _ := session["tools"].([]any)
	for _, raw := range rawTools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != "function" {
			continue
		}
		if nested := objectField(tool, "function"); nested != nil {
			tool = nested
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		params := objectField(tool, "parameters")
		if params == nil {
			params = map[string]any{}
		}
		schema, _ := json.Marshal(params)
		description, _ := tool["description"].(string)
		tools = append(tools, map[string]any{"toolSpec":map[string]any{"name":name,"description":description,"inputSchema":map[string]string{"json":string(schema)}}})
	}
	if len(tools) != 0 {
		prompt["toolUseOutputConfiguration"] = map[string]string{"mediaType":"application/json"}
		prompt["toolConfiguration"] = map[string]any{"tools":tools}
	}
	result := []map[string]any{bedrockRealtimeEvent("sessionStart", map[string]any{"inferenceConfiguration":inference}),
		bedrockRealtimeEvent("promptStart", prompt)}
	if text, _ := session["instructions"].(string); text != "" {
		result = append(result, input.text("SYSTEM", text)...)
	}
	return result
}

func bedrockRealtimePCM(rate int) map[string]any {
	return map[string]any{"mediaType":"audio/lpcm", "sampleRateHertz":rate, "sampleSizeBits":16,
		"channelCount":1, "audioType":"SPEECH", "encoding":"base64"}
}

func (input *bedrockRealtimeInput) base(content string) map[string]any {
	return map[string]any{"promptName":input.prompt, "contentName":content}
}

func (input *bedrockRealtimeInput) text(role, text string) []map[string]any {
	content := newUUID()
	start := input.base(content)
	start["type"], start["role"], start["interactive"] = "TEXT", role, role == "USER"
	start["textInputConfiguration"] = map[string]string{"mediaType":"text/plain"}
	part := input.base(content)
	part["content"] = text
	return []map[string]any{bedrockRealtimeEvent("contentStart", start), bedrockRealtimeEvent("textInput", part),
		bedrockRealtimeEvent("contentEnd", input.base(content))}
}

func (input *bedrockRealtimeInput) audioChunk(encoded string) []map[string]any {
	var result []map[string]any
	if input.audio == "" {
		input.audio = newUUID()
		start := input.base(input.audio)
		start["type"], start["role"], start["interactive"] = "AUDIO", "USER", true
		start["audioInputConfiguration"] = bedrockRealtimePCM(input.inputRate)
		result = append(result, bedrockRealtimeEvent("contentStart", start))
	}
	part := input.base(input.audio)
	part["content"] = encoded
	return append(result, bedrockRealtimeEvent("audioInput", part))
}

func (input *bedrockRealtimeInput) commit() []map[string]any {
	if input.audio == "" {
		return nil
	}
	end := bedrockRealtimeEvent("contentEnd", input.base(input.audio))
	input.audio = ""
	return []map[string]any{end}
}

func (input *bedrockRealtimeInput) toolResult(id, text string) []map[string]any {
	content := newUUID()
	start := input.base(content)
	start["type"], start["role"], start["interactive"] = "TOOL", "TOOL", false
	start["toolResultInputConfiguration"] = map[string]any{"toolUseId":id, "type":"TEXT",
		"textInputConfiguration":map[string]string{"mediaType":"text/plain"}}
	result := input.base(content)
	result["content"] = text
	return []map[string]any{bedrockRealtimeEvent("contentStart", start), bedrockRealtimeEvent("toolResult", result),
		bedrockRealtimeEvent("contentEnd", input.base(content))}
}
