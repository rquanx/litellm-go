package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type BedrockConfig struct {
	Region       string `json:"region"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken,omitempty"`
	Credentials aws.CredentialsProvider `json:"-"`
	ControlBase string `json:"controlBase,omitempty"`
	AgentBase string `json:"agentBase,omitempty"`
	BatchRole string `json:"batchRole,omitempty"`
	BatchOutputBucket string `json:"batchOutputBucket,omitempty"`
	BatchEncryptionKey string `json:"batchEncryptionKey,omitempty"`
	S3Bucket string `json:"s3Bucket,omitempty"`
	S3Endpoint string `json:"s3Endpoint,omitempty"`
	S3Region string `json:"s3Region,omitempty"`
}

func bedrockURL(base, model string, stream bool) string {
	action := "converse"
	if stream {
		action = "converse-stream"
	}
	return strings.TrimRight(base, "/") + "/model/" + url.PathEscape(model) + "/" + action
}

func authorizeBedrock(ctx context.Context, request *http.Request, candidate Candidate, body []byte, bearer string) error {
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
		return nil
	}
	return signAWSRequest(ctx, request, candidate.Bedrock, body, "bedrock")
}

func signAWSRequest(ctx context.Context, request *http.Request, config *BedrockConfig, body []byte, service string) error {
	if config == nil || config.Region == "" {
		return errors.New("AWS signing requires a region and credential")
	}
	credentials := aws.Credentials{
		AccessKeyID: config.AccessKey, SecretAccessKey: config.SecretKey, SessionToken: config.SessionToken,
	}
	if !credentials.HasKeys() && config.Credentials != nil {
		var err error
		credentials, err = config.Credentials.Retrieve(ctx)
		if err != nil {
			return errors.New("could not retrieve AWS credentials")
		}
	}
	if !credentials.HasKeys() {
		return errors.New("AWS signing requires credentials")
	}
	hash := sha256.Sum256(body)
	encoded := hex.EncodeToString(hash[:])
	region := config.Region
	if service == "s3" {
		request.Header.Set("X-Amz-Content-Sha256", encoded)
		if config.S3Region != "" {
			region = config.S3Region
		}
	}
	return v4.NewSigner().SignHTTP(ctx, credentials, request, encoded, service, region, time.Now(), func(options *v4.SignerOptions) {
		options.DisableURIPathEscaping = service == "s3"
	})
}

func chatToBedrock(chat map[string]any) map[string]any {
	anthropic := chatToAnthropicRequest(chat)
	request := map[string]any{}
	config := map[string]any{}
	for from, to := range map[string]string{"max_tokens": "maxTokens", "temperature": "temperature", "top_p": "topP", "stop_sequences": "stopSequences"} {
		if value, present := anthropic[from]; present {
			config[to] = value
		}
	}
	if len(config) != 0 {
		request["inferenceConfig"] = config
	}
	if system, ok := anthropic["system"].([]any); ok && len(system) != 0 {
		request["system"] = bedrockBlocks(system)
	}
	messages, _ := anthropic["messages"].([]any)
	converted := make([]any, 0, len(messages))
	for _, raw := range messages {
		message := raw.(map[string]any)
		converted = append(converted, map[string]any{
			"role": message["role"], "content": bedrockBlocks(message["content"].([]any)),
		})
	}
	request["messages"] = converted
	if tools, ok := anthropic["tools"].([]any); ok && len(tools) != 0 {
		mapped := make([]any, 0, len(tools))
		for _, value := range tools {
			tool := value.(map[string]any)
			spec := map[string]any{"name": tool["name"], "inputSchema": map[string]any{"json": tool["input_schema"]}}
			if description, ok := tool["description"]; ok {
				spec["description"] = description
			}
			mapped = append(mapped, map[string]any{"toolSpec": spec})
		}
		toolConfig := map[string]any{"tools": mapped}
		if choice, ok := anthropic["tool_choice"].(map[string]any); ok {
			switch choice["type"] {
			case "auto", "any":
				toolConfig["toolChoice"] = map[string]any{choice["type"].(string): map[string]any{}}
			case "tool":
				toolConfig["toolChoice"] = map[string]any{"tool": map[string]any{"name": choice["name"]}}
			case "none":
				toolConfig = nil
			}
		}
		if toolConfig != nil {
			request["toolConfig"] = toolConfig
		}
	}
	return request
}

func bedrockBlocks(parts []any) []any {
	result := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		switch part["type"] {
		case "text":
			result = append(result, map[string]any{"text": part["text"]})
		case "image":
			source, _ := part["source"].(map[string]any)
			media, _ := source["media_type"].(string)
			if source["type"] == "base64" && media != "" {
				format := strings.TrimPrefix(media, "image/")
				result = append(result, map[string]any{"image": map[string]any{"format": format,
					"source": map[string]any{"bytes": source["data"]}}})
			}
		case "tool_use":
			result = append(result, map[string]any{"toolUse": map[string]any{
				"toolUseId": part["id"], "name": part["name"], "input": part["input"],
			}})
		case "tool_result":
			content, _ := part["content"].([]any)
			result = append(result, map[string]any{"toolResult": map[string]any{
				"toolUseId": part["tool_use_id"], "content": bedrockBlocks(content),
			}})
		}
	}
	return result
}

func bedrockToChat(native map[string]any, alias string) map[string]any {
	output, _ := native["output"].(map[string]any)
	message, _ := output["message"].(map[string]any)
	var text strings.Builder
	calls := []any{}
	parts, _ := message["content"].([]any)
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
		if call, ok := part["toolUse"].(map[string]any); ok {
			data, _ := json.Marshal(call["input"])
			calls = append(calls, map[string]any{"id": call["toolUseId"], "type": "function",
				"function": map[string]any{"name": call["name"], "arguments": string(data)}})
		}
	}
	converted := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) != 0 {
		converted["tool_calls"] = calls
		if text.Len() == 0 {
			converted["content"] = nil
		}
	}
	return map[string]any{"id": "chatcmpl_" + newUUID(), "object": "chat.completion", "created": time.Now().Unix(),
		"model": alias, "choices": []any{map[string]any{"index": 0, "message": converted,
			"finish_reason": bedrockFinish(native["stopReason"])}}, "usage": bedrockUsage(native["usage"])}
}

func bedrockFinish(reason any) string {
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "content_filtered", "guardrail_intervened":
		return "content_filter"
	default:
		return "stop"
	}
}

func bedrockUsage(value any) any {
	usage, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := map[string]any{"prompt_tokens": usage["inputTokens"], "completion_tokens": usage["outputTokens"],
		"total_tokens": usage["totalTokens"]}
	if cached := usage["cacheReadInputTokens"]; cached != nil {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return result
}
