package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type streamGuardrailState struct {
	texts map[int][]byte
	bytes int64
}

func (s *Server) checkStreamGuardrails(r *http.Request, plan responseGuardrails, state *streamGuardrailState, model string, output []byte) *guardrailFailure {
	if len(output) == 0 {
		return nil
	}
	if state.texts == nil {
		state.texts = make(map[int][]byte)
	}
	reader := bufio.NewReader(bytes.NewReader(output))
	for {
		frame, err := readSSEFrame(reader, s.config.MaxBodyBytes)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &guardrailFailure{502, "guardrail_error", "Invalid streaming response for moderation"}
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		raw := streamErrorBody(frame)
		if len(raw) > 0 && !bytes.Equal(raw, []byte("[DONE]")) {
			if json.Unmarshal(raw, &chunk) != nil {
				return &guardrailFailure{502, "guardrail_error", "Invalid streaming response for moderation"}
			}
			for _, choice := range chunk.Choices {
				content := choice.Delta.Content
				if content == "" {
					continue
				}
				state.bytes += int64(len(content))
				if state.bytes > s.config.MaxBodyBytes {
					return &guardrailFailure{502, "guardrail_error", "Streaming response exceeded the moderation limit"}
				}
				text := append(state.texts[choice.Index], content...)
				state.texts[choice.Index] = text
				for _, guard := range plan.independent {
					started := time.Now()
					for _, part := range azureSafetyChunks(string(text), 10000) {
						if failure := s.checkAzureSafetyChunk(r, guard, part); failure != nil {
							s.recordGuardrail(guard.Name, model, started, failure)
							return failure
						}
					}
					s.recordGuardrail(guard.Name, model, started, nil)
				}
			}
		}
	}
}

func writeStreamGuardrailError(w http.ResponseWriter, flush http.Flusher, failure *guardrailFailure) {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"message": failure.message, "type": failure.kind}})
	frame := append(append([]byte("data: "), raw...), '\n', '\n')
	writeStreamFrame(w, flush, frame)
}
