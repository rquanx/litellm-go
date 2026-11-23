package runtime

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

const maxCallbackMessageBytes = 64 << 10

type InferenceEvent struct {
	ID         string          `json:"id"`
	TraceID    string          `json:"trace_id"`
	CallType   string          `json:"call_type"`
	Stream     bool            `json:"stream"`
	Status     string          `json:"status"`
	Model      string          `json:"model"`
	ModelGroup string          `json:"model_group"`
	ModelID    string          `json:"model_id"`
	Provider   string          `json:"custom_llm_provider"`
	StartTime  float64         `json:"startTime"`
	EndTime    float64         `json:"endTime"`
	Total      int             `json:"total_tokens"`
	Prompt     int             `json:"prompt_tokens"`
	Completion int             `json:"completion_tokens"`
	Spend      *float64        `json:"response_cost"`
	CacheHit   bool            `json:"cache_hit"`
	CacheReadInputTokens int   `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	KeyDigest  string          `json:"-"`
	EndUser    string          `json:"end_user,omitempty"`
	Messages   json.RawMessage `json:"messages"`
	Response   json.RawMessage `json:"response"`
}

func newInferenceEvent(endpoint, alias string, stream bool) InferenceEvent {
	id := newUUID()
	call := map[string]string{"chat-completions": "acompletion", "responses": "aresponses", "interactions": "acreate_interaction", "embeddings": "aembedding"}[endpoint]
	if call == "" {
		call = endpoint
	}
	return InferenceEvent{ID: id, TraceID: id, CallType: call, Stream: stream, ModelGroup: alias,
		StartTime: float64(time.Now().UnixNano()) / float64(time.Second)}
}

type callbackResponse struct {
	http.ResponseWriter
	event InferenceEvent
	status int
	capture bool
	price [2]*float64
	cachePrice *float64
	cacheCreationPrice *float64
	cachedTokens int
	cacheCreationTokens int
}

func (recorder *callbackResponse) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *callbackResponse) Flush() {
	if writer, ok := recorder.ResponseWriter.(http.Flusher); ok {
		writer.Flush()
	}
}

func (recorder *callbackResponse) Write(data []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	if recorder.event.Stream && recorder.status < 400 {
		for _, frame := range bytes.Split(data, []byte("\n\n")) {
			if value := streamErrorBody(frame); len(value) != 0 {
				recorder.recordJSON(value)
			}
		}
	} else {
		recorder.recordJSON(data)
	}
	return recorder.ResponseWriter.Write(data)
}

func (recorder *callbackResponse) recordJSON(data []byte) {
	type tokenDetails struct {
		Cached int `json:"cached_tokens"`
		Creation int `json:"cache_creation_tokens"`
	}
	type interactionUsage struct {
		Input int `json:"total_input_tokens"`
		Output int `json:"total_output_tokens"`
		Reasoning int `json:"total_reasoning_tokens"`
		Cached int `json:"total_cached_tokens"`
		Total int `json:"total_tokens"`
	}
	type usage struct {
		Prompt int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Input int `json:"input_tokens"`
		Output int `json:"output_tokens"`
		Total int `json:"total_tokens"`
		PromptDetails tokenDetails `json:"prompt_tokens_details"`
		InputDetails tokenDetails `json:"input_tokens_details"`
		NativeCacheRead int `json:"cache_read_input_tokens"`
		NativeCacheCreation int `json:"cache_creation_input_tokens"`
	}
	var payload struct {
		Type string `json:"type"`
		Usage usage `json:"usage"`
		Response struct {
			Usage usage `json:"usage"`
		} `json:"response"`
		Message struct {
			Usage usage `json:"usage"`
		} `json:"message"`
		Gemini struct {
			Prompt int `json:"promptTokenCount"`
			Completion int `json:"candidatesTokenCount"`
			Thoughts int `json:"thoughtsTokenCount"`
			Total int `json:"totalTokenCount"`
			Cached int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	if recorder.event.CallType == "acreate_interaction" {
		var result struct {
			Usage interactionUsage `json:"usage"`
			Interaction struct { Usage interactionUsage `json:"usage"` } `json:"interaction"`
		}
		if json.Unmarshal(data, &result) == nil {
			data := result.Usage
			if result.Interaction.Usage != (interactionUsage{}) {
				data = result.Interaction.Usage
			}
			payload.Usage = usage{Input: data.Input, Output: data.Output + data.Reasoning, Total: data.Total,
				InputDetails: tokenDetails{Cached: data.Cached}}
		}
	}
	if payload.Response.Usage != (usage{}) {
		payload.Usage = payload.Response.Usage
	}
	if payload.Type == "message_start" && payload.Message.Usage != (usage{}) {
		payload.Usage = payload.Message.Usage
	}
	if payload.Gemini.Prompt != 0 || payload.Gemini.Completion != 0 || payload.Gemini.Thoughts != 0 {
		payload.Usage = usage{Prompt: payload.Gemini.Prompt, Completion: payload.Gemini.Completion + payload.Gemini.Thoughts,
			Total: payload.Gemini.Total, PromptDetails: tokenDetails{Cached: payload.Gemini.Cached}}
	}
	if payload.Usage != (usage{}) {
		if payload.Type != "message_delta" || payload.Usage.Input > 0 || payload.Usage.NativeCacheRead > 0 ||
			payload.Usage.NativeCacheCreation > 0 {
			recorder.event.Prompt = max(0, payload.Usage.Prompt, payload.Usage.Input +
				max(0, payload.Usage.NativeCacheRead) + max(0, payload.Usage.NativeCacheCreation))
			recorder.cachedTokens = min(recorder.event.Prompt, max(0, payload.Usage.PromptDetails.Cached, payload.Usage.InputDetails.Cached, payload.Usage.NativeCacheRead))
			recorder.cacheCreationTokens = min(recorder.event.Prompt-recorder.cachedTokens, max(0,
				payload.Usage.PromptDetails.Creation, payload.Usage.InputDetails.Creation, payload.Usage.NativeCacheCreation))
		}
		recorder.event.Completion = max(payload.Usage.Completion, payload.Usage.Output)
		recorder.event.Total = max(payload.Usage.Total, recorder.event.Prompt+recorder.event.Completion)
	}
	if recorder.capture && len(data) <= maxCallbackMessageBytes &&
		(!recorder.event.Stream || recorder.status >= 400 || bytes.Contains(data, []byte("response.completed"))) {
		recorder.event.Response = bytes.Clone(data)
	}
}

func (recorder *callbackResponse) finish() {
	recorder.event.CacheReadInputTokens = recorder.cachedTokens
	recorder.event.CacheCreationInputTokens = recorder.cacheCreationTokens
	recorder.event.EndTime = float64(time.Now().UnixNano()) / float64(time.Second)
	recorder.event.Model = recorder.Header().Get("X-Minato-Runtime-Model")
	recorder.event.Provider = recorder.Header().Get("X-Minato-Runtime-Source")
	recorder.event.Status = "success"
	if recorder.status >= 400 {
		recorder.event.Status = "failure"
	}
	if recorder.event.CacheHit {
		cost := 0.0
		recorder.event.Spend = &cost
		return
	}
	uncached := recorder.event.Prompt
	if recorder.cachePrice != nil {
		uncached -= recorder.cachedTokens
	}
	if recorder.cacheCreationPrice != nil {
		uncached -= recorder.cacheCreationTokens
	}
	if (recorder.event.Prompt != 0 || recorder.event.Completion != 0) &&
		(uncached == 0 || recorder.price[0] != nil) &&
		(recorder.event.Completion == 0 || recorder.price[1] != nil) {
		cost := 0.0
		if recorder.price[0] != nil {
			cost += float64(uncached) * *recorder.price[0]
		}
		if recorder.cachePrice != nil {
			cost += float64(recorder.cachedTokens) * *recorder.cachePrice
		}
		if recorder.cacheCreationPrice != nil {
			cost += float64(recorder.cacheCreationTokens) * *recorder.cacheCreationPrice
		}
		if recorder.price[1] != nil {
			cost += float64(recorder.event.Completion) * *recorder.price[1]
		}
		recorder.event.Spend = &cost
	}
}

func boundedCallbackJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxCallbackMessageBytes {
		return nil
	}
	return data
}
