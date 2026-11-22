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
	Provider   string          `json:"custom_llm_provider"`
	StartTime  float64         `json:"startTime"`
	EndTime    float64         `json:"endTime"`
	Total      int             `json:"total_tokens"`
	Prompt     int             `json:"prompt_tokens"`
	Completion int             `json:"completion_tokens"`
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
	type usage struct {
		Prompt int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Input int `json:"input_tokens"`
		Output int `json:"output_tokens"`
		Total int `json:"total_tokens"`
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
}

func boundedCallbackJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxCallbackMessageBytes {
		return nil
	}
	return data
}
