package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/coder/websocket"
)

func (s *Server) managedResponsesSocket(w http.ResponseWriter, request *http.Request, alias string) {
	conn, err := acceptInferenceSocket(w, request)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(s.config.MaxBodyBytes)
	ctx := request.Context()
	history := socketHistory{turns: make(map[string][]byte), limit: s.config.MaxBodyBytes}
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		event, err := parseSocketEvent(kind, data, alias)
		if err != nil {
			if writeSocketError(ctx, conn, http.StatusBadRequest, err.Error()) != nil {
				return
			}
			continue
		}
		if generate, ok := event["generate"].(bool); ok && !generate {
			if writeSocketError(ctx, conn, http.StatusBadRequest, "Warmup requires a native Responses WebSocket") != nil {
				return
			}
			continue
		}
		delete(event, "generate")
		input := socketInput(event["input"])
		previous, _ := event["previous_response_id"].(string)
		if messages := history.turns[previous]; len(messages) != 0 {
			var prior []any
			if json.Unmarshal(messages, &prior) == nil {
				input = append(prior, input...)
				event["input"] = input
				delete(event, "previous_response_id")
			}
		}
		event["model"] = alias
		event["stream"] = true
		body, err := json.Marshal(event)
		if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
			if writeSocketError(ctx, conn, http.StatusRequestEntityTooLarge, "Conversation exceeds the request size limit") != nil {
				return
			}
			continue
		}
		call := request.Clone(ctx)
		call.Method = http.MethodPost
		call.Header.Del("Upgrade")
		call.Header.Set("Content-Type", "application/json")
		call.Header.Set("Accept", "text/event-stream")
		call.Body = io.NopCloser(bytes.NewReader(body))
		call.ContentLength = int64(len(body))
		stream := &socketStreamWriter{ctx: ctx, conn: conn, headers: make(http.Header), history: &history, input: input}
		s.ServeHTTP(stream, call)
		if stream.err != nil {
			return
		}
	}
}

func socketInput(input any) []any {
	switch value := input.(type) {
	case string:
		return []any{map[string]any{"type": "message", "role": "user", "content": value}}
	case []any:
		return value
	default:
		return nil
	}
}

type socketHistory struct {
	order []string
	turns map[string][]byte
	bytes int64
	limit int64
}

func (history *socketHistory) save(id string, items []any) {
	payload, err := json.Marshal(items)
	weight := int64(len(payload))
	if err != nil || id == "" || weight > history.limit || history.turns[id] != nil {
		return
	}
	for len(history.order) >= 32 || history.bytes+weight > history.limit {
		oldest := history.order[0]
		history.bytes -= int64(len(history.turns[oldest]))
		delete(history.turns, oldest)
		history.order = history.order[1:]
	}
	history.order = append(history.order, id)
	history.turns[id] = payload
	history.bytes += weight
}

type socketStreamWriter struct {
	ctx     context.Context
	conn    *websocket.Conn
	headers http.Header
	status  int
	err     error
	history *socketHistory
	input   []any
}

func (writer *socketStreamWriter) Header() http.Header { return writer.headers }

func (writer *socketStreamWriter) WriteHeader(status int) { writer.status = status }

func (writer *socketStreamWriter) Write(body []byte) (int, error) {
	size := len(body)
	if writer.status >= 400 {
		var upstream struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		message := "Responses request failed"
		if json.Unmarshal(body, &upstream) == nil && upstream.Error.Message != "" {
			message = upstream.Error.Message
		}
		writer.err = writeSocketError(writer.ctx, writer.conn, writer.status, message)
		return size, writer.err
	}
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	for _, frame := range bytes.Split(body, []byte("\n\n")) {
		data := streamErrorBody(frame)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				ID     string `json:"id"`
				Output []any  `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &event) == nil && event.Type == "response.completed" {
			messages := append(append([]any{}, writer.input...), event.Response.Output...)
			writer.history.save(event.Response.ID, messages)
		}
		if writer.err = writer.conn.Write(writer.ctx, websocket.MessageText, data); writer.err != nil {
			return 0, writer.err
		}
	}
	return size, nil
}

func writeSocketError(ctx context.Context, conn *websocket.Conn, status int, message string) error {
	data, _ := json.Marshal(map[string]any{"type": "error", "status": status,
		"error": map[string]string{"code": "invalid_request", "message": message}})
	return conn.Write(ctx, websocket.MessageText, data)
}
