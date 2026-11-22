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
	KeyDigest  string          `json:"-"`
	Messages   json.RawMessage `json:"messages"`
	Response   json.RawMessage `json:"response"`
}

func newInferenceEvent(endpoint, alias string, stream bool) InferenceEvent {
	id := newUUID()
	call := map[string]string{"chat-completions": "acompletion", "responses": "aresponses", "embeddings": "aembedding"}[endpoint]
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
	cachedTokens int
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
	}
	type usage struct {
		Prompt int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Input int `json:"input_tokens"`
		Output int `json:"output_tokens"`
		Total int `json:"total_tokens"`
		PromptDetails tokenDetails `json:"prompt_tokens_details"`
		InputDetails tokenDetails `json:"input_tokens_details"`
	}
	var payload struct {
		Usage usage `json:"usage"`
		Response struct {
			Usage usage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	if payload.Response.Usage != (usage{}) {
		payload.Usage = payload.Response.Usage
	}
	if payload.Usage != (usage{}) {
		recorder.event.Prompt = max(payload.Usage.Prompt, payload.Usage.Input)
		recorder.event.Completion = max(payload.Usage.Completion, payload.Usage.Output)
		recorder.event.Total = max(payload.Usage.Total, recorder.event.Prompt+recorder.event.Completion)
		recorder.cachedTokens = min(recorder.event.Prompt, max(0, payload.Usage.PromptDetails.Cached, payload.Usage.InputDetails.Cached))
	}
	if recorder.capture && len(data) <= maxCallbackMessageBytes &&
		(!recorder.event.Stream || recorder.status >= 400 || bytes.Contains(data, []byte("response.completed"))) {
		recorder.event.Response = bytes.Clone(data)
	}
}

func (recorder *callbackResponse) finish() {
	recorder.event.EndTime = float64(time.Now().UnixNano()) / float64(time.Second)
	recorder.event.Model = recorder.Header().Get("X-Minato-Runtime-Model")
	recorder.event.Provider = recorder.Header().Get("X-Minato-Runtime-Source")
	recorder.event.Status = "success"
	if recorder.status >= 400 {
		recorder.event.Status = "failure"
	}
	uncached := recorder.event.Prompt
	if recorder.cachePrice != nil {
		uncached -= recorder.cachedTokens
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
