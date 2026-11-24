package runtime

import "encoding/json"

type bedrockRealtimeOutput struct {
	userText bool
	stopReason string
	completion bool
	usageSeen bool
}

// Nova Sonic frames are converted into the shared Realtime turn representation
// used to assemble OpenAI output items and usage events.
func (output *bedrockRealtimeOutput) translate(event map[string]any) map[string]any {
	switch {
	case event["contentStart"] != nil:
		output.userText = objectField(event, "contentStart")["role"] == "USER"
	case event["textOutput"] != nil:
		text, _ := objectField(event, "textOutput")["content"].(string)
		if output.userText {
			return map[string]any{"serverContent":map[string]any{"inputTranscription":map[string]any{"text":text}}}
		}
		return bedrockRealtimePart(map[string]any{"text":text})
	case event["audioOutput"] != nil:
		audio, _ := objectField(event, "audioOutput")["content"].(string)
		return bedrockRealtimePart(map[string]any{"inlineData":map[string]any{"data":audio}})
	case event["toolUse"] != nil:
		tool := objectField(event, "toolUse")
		args := objectField(tool, "input")
		if args == nil {
			raw, _ := tool["input"].(string)
			_ = json.Unmarshal([]byte(raw), &args)
		}
		return map[string]any{"toolCall":map[string]any{"functionCalls":[]any{map[string]any{
			"id":tool["toolUseId"], "name":tool["toolName"], "args":args,
		}}}}
	case event["completionEnd"] != nil:
		output.stopReason, _ = objectField(event, "completionEnd")["stopReason"].(string)
		output.completion = true
		if output.usageSeen {
			return map[string]any{"serverContent":output.finish()}
		}
	case event["usageEvent"] != nil:
		usage := objectField(event, "usageEvent")
		output.usageSeen = true
		result := map[string]any{"usageMetadata":map[string]any{"promptTokenCount":usage["totalInputTokens"],
			"candidatesTokenCount":usage["totalOutputTokens"], "totalTokenCount":usage["totalTokens"]}}
		if output.completion {
			result["serverContent"] = output.finish()
		}
		return result
	case event["promptEnd"] != nil:
		if output.completion {
			return map[string]any{"serverContent":output.finish()}
		}
	}
	return nil
}

func (output *bedrockRealtimeOutput) finish() map[string]any {
	interrupted := output.stopReason == "INTERRUPTED" || output.stopReason == "INTERRUPTION"
	output.stopReason, output.completion, output.usageSeen = "", false, false
	return map[string]any{"turnComplete":true, "interrupted":interrupted}
}

func bedrockRealtimePart(part map[string]any) map[string]any {
	return map[string]any{"serverContent":map[string]any{"modelTurn":map[string]any{"parts":[]any{part}}}}
}
