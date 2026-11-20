package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Token        string
	HTTPClient   *http.Client
	MaxBodyBytes int64
}

type Server struct {
	config Config
	mu     sync.RWMutex
	routes map[string]Route
}

var credentialRefPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func NewServer(config Config) *Server {
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 8 << 20
	}
	return &Server{config: config, routes: map[string]Route{}}
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/healthz":
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		s.models(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/responses":
		s.infer(writer, request, "responses")
	case request.Method == http.MethodPost && request.URL.Path == "/v1/chat/completions":
		s.infer(writer, request, "chat-completions")
	case strings.HasPrefix(request.URL.Path, "/internal/v1/routes/"):
		s.route(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "not_found", "Route not found")
	}
}

func (s *Server) models(writer http.ResponseWriter) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data := make([]map[string]any, 0, len(s.routes))
	for alias := range s.routes {
		data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "minato-runtime"})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) route(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return
	}
	alias := strings.TrimPrefix(request.URL.Path, "/internal/v1/routes/")
	if alias == "" || strings.Contains(alias, "/") {
		writeError(writer, http.StatusBadRequest, "invalid_alias", "Invalid route alias")
		return
	}
	if request.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.routes, alias)
		s.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodPut {
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	var route Route
	if err := decodeBody(request, &route, s.config.MaxBodyBytes); err != nil || route.Alias != alias || route.Version == "" || len(route.Candidates) == 0 {
		writeError(writer, http.StatusBadRequest, "invalid_route", "A route alias, version and candidates are required")
		return
	}
	for _, candidate := range route.Candidates {
		if candidate.APIBase == "" || candidate.ModelID == "" || candidate.Endpoint == "" {
			writeError(writer, http.StatusBadRequest, "invalid_candidate", "Every candidate needs an API base, model ID and endpoint")
			return
		}
		if candidate.CredentialRef != "" && !credentialRefPattern.MatchString(candidate.CredentialRef) {
			writeError(writer, http.StatusBadRequest, "invalid_credential_ref", "Credential reference must be an environment variable name")
			return
		}
	}
	s.mu.Lock()
	s.routes[alias] = route
	s.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"alias": alias, "version": route.Version})
}

func (s *Server) infer(writer http.ResponseWriter, request *http.Request, endpoint string) {
	body, err := readBody(request, s.config.MaxBodyBytes)
	if err != nil {
		writeError(writer, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		return
	}
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", "Request body must be JSON")
		return
	}
	alias, _ := input["model"].(string)
	s.mu.RLock()
	route, ok := s.routes[alias]
	s.mu.RUnlock()
	if !ok {
		writer.Header().Set("X-Minato-Runtime-Route-Missing", "1")
		writeError(writer, http.StatusNotFound, "model_not_found", "Unknown runtime route")
		return
	}
	if request.Header.Get("Accept") == "text/event-stream" || inputBool(input, "stream") {
		s.inferStream(writer, request, endpoint, route, input)
		return
	}
	attempts := make([]map[string]any, 0, len(route.Candidates))
	for index, candidate := range route.Candidates {
		started := time.Now()
		status, responseBody, responseHeaders, retryable, callErr := s.callJSON(request.Context(), request, endpoint, candidate, input, route.Alias)
		attempt := map[string]any{"id": newUUID(), "sourceId": candidate.SourceID, "modelId": candidate.ModelID,
			"status": status, "durationMs": time.Since(started).Milliseconds(), "retryable": retryable}
		if callErr != nil {
			attempt["status"] = http.StatusBadGateway
		}
		if status >= 200 && status < 300 {
			var payload map[string]any
			if json.Unmarshal(responseBody, &payload) == nil {
				attempt["usage"] = payload["usage"]
			}
		}
		attempts = append(attempts, attempt)
		if callErr == nil && status >= 200 && status < 300 {
			writeRuntimeMeta(writer, candidate, index+1, attempts)
			writeResponse(writer, status, responseHeaders, normalizeResponse(endpoint, candidate.Endpoint, responseBody, route.Alias))
			return
		}
		if !retryable || index == len(route.Candidates)-1 {
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			if callErr != nil {
				writeError(writer, http.StatusBadGateway, "upstream_error", callErr.Error())
			} else {
				writeRawResponse(writer, status, responseHeaders, responseBody)
			}
			return
		}
	}
}

func (s *Server) inferStream(writer http.ResponseWriter, request *http.Request, endpoint string, route Route, input map[string]any) {
	eligible := make([]Candidate, 0, len(route.Candidates))
	for _, candidate := range route.Candidates {
		if endpoint == candidate.Endpoint {
			eligible = append(eligible, candidate)
		}
	}
	attempts := make([]map[string]any, 0, len(route.Candidates))
	for index, candidate := range eligible {
		started := time.Now()
		status, response, retryable, err := s.callStream(request.Context(), request, endpoint, candidate, input)
		var reader *bufio.Reader
		var firstFrame []byte
		streamFailure := false
		if err == nil && response != nil && status >= 200 && status < 300 {
			reader = bufio.NewReader(response.Body)
			firstFrame, err = readSSEFrame(reader, s.config.MaxBodyBytes)
			if err != nil || sseFrameFailure(firstFrame) {
				streamFailure = true
				retryable = true
				status = http.StatusBadGateway
				response.Body.Close()
			}
		}
		if err == nil && response != nil && (status < 200 || status >= 300) {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(body))
			if readErr != nil {
				retryable = true
			} else {
				retryable = retryableResponse(status, body)
			}
		}
		attempt := map[string]any{"id": newUUID(), "sourceId": candidate.SourceID, "modelId": candidate.ModelID,
			"status": status, "durationMs": time.Since(started).Milliseconds(), "retryable": retryable}
		if err != nil {
			attempt["status"] = http.StatusBadGateway
		}
		attempts = append(attempts, attempt)
		if err == nil && !streamFailure && status >= 200 && status < 300 {
			defer response.Body.Close()
			writeRuntimeMeta(writer, candidate, index+1, attempts)
			for key, values := range response.Header {
				if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Cache-Control") {
					writer.Header()[key] = values
				}
			}
			writer.Header().Set("Cache-Control", "no-cache, no-transform")
			writer.Header().Set("X-Accel-Buffering", "no")
			writer.WriteHeader(status)
			flush, _ := writer.(http.Flusher)
			buffer := make([]byte, 32*1024)
			if len(firstFrame) > 0 {
				_, _ = writer.Write(firstFrame)
				if flush != nil {
					flush.Flush()
				}
			}
			for {
				count, readErr := reader.Read(buffer)
				if count > 0 {
					_, _ = writer.Write(buffer[:count])
					if flush != nil {
						flush.Flush()
					}
				}
				if readErr != nil {
					break
				}
			}
			return
		}
		if !retryable || index == len(eligible)-1 {
			if streamFailure {
				writeError(writer, http.StatusBadGateway, "upstream_stream_failure", "Upstream stream failed before data")
				return
			}
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			if err != nil {
				writeError(writer, http.StatusBadGateway, "upstream_error", err.Error())
			} else {
				body, _ := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes))
				response.Body.Close()
				writeRawResponse(writer, status, response.Header, body)
			}
			return
		}
	}
	if len(eligible) == 0 {
		writeError(writer, http.StatusNotImplemented, "stream_conversion_unsupported", "Streaming across Responses and Chat Completions is not supported")
	}
}

func (s *Server) callJSON(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, input map[string]any, alias string) (int, []byte, http.Header, bool, error) {
	response, err := s.send(ctx, request, endpoint, candidate, input, false)
	if err != nil {
		return 0, nil, nil, true, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes))
	if err != nil {
		return response.StatusCode, nil, response.Header, true, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, body, response.Header, false, nil
	}
	return response.StatusCode, body, response.Header, retryableResponse(response.StatusCode, body), nil
}

func (s *Server) callStream(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, input map[string]any) (int, *http.Response, bool, error) {
	response, err := s.send(ctx, request, endpoint, candidate, input, true)
	if err != nil {
		return 0, nil, true, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, response, false, nil
	}
	return response.StatusCode, response, retryableStatus(response.StatusCode), nil
}

func (s *Server) send(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, input map[string]any, stream bool) (*http.Response, error) {
	targetEndpoint := candidate.Endpoint
	payload := clone(input)
	payload["model"] = candidate.ModelID
	payload["stream"] = stream
	if endpoint == "responses" && targetEndpoint == "chat-completions" {
		payload = responsesToChat(payload)
	} else if endpoint == "chat-completions" && targetEndpoint == "responses" {
		payload = chatRequestToResponses(payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	path := "/v1/responses"
	if targetEndpoint == "chat-completions" {
		path = "/v1/chat/completions"
	}
	base := strings.TrimRight(candidate.APIBase, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	upstream.Header.Set("Content-Type", "application/json")
	apiKey := candidate.APIKey
	if apiKey == "" && candidate.CredentialRef != "" {
		apiKey = os.Getenv(candidate.CredentialRef)
	}
	if apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for key, value := range candidate.Headers {
		upstream.Header.Set(key, value)
	}
	if session := request.Header.Get("x-opencode-session"); session != "" {
		upstream.Header.Set("x-opencode-session", session)
	}
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func normalizeResponse(endpoint, candidateEndpoint string, body []byte, alias string) []byte {
	if endpoint == "chat-completions" && candidateEndpoint == "responses" {
		var response map[string]any
		if json.Unmarshal(body, &response) != nil {
			return body
		}
		encoded, _ := json.Marshal(responsesToChatResponse(response, alias))
		return encoded
	}
	if endpoint != "responses" || candidateEndpoint != "chat-completions" {
		return rewriteModel(body, alias)
	}
	var chat map[string]any
	if json.Unmarshal(body, &chat) != nil {
		return body
	}
	encoded, _ := json.Marshal(chatToResponses(chat, alias))
	return encoded
}

func (s *Server) authorized(request *http.Request) bool {
	return s.config.Token == "" || request.Header.Get("Authorization") == "Bearer "+s.config.Token
}

func retryableStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
func retryableResponse(status int, body []byte) bool {
	if retryableStatus(status) {
		return true
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, value := range []string{payload.Error.Code, payload.Error.Type} {
		if value == "model_not_found" || value == "model_unavailable" || value == "invalid_model" {
			return true
		}
	}
	return false
}
func inputBool(input map[string]any, key string) bool { value, _ := input[key].(bool); return value }
func decodeBody(request *http.Request, target any, max int64) error {
	body, err := readBody(request, max)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}
func readBody(request *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, max+1))
	if err != nil || int64(len(body)) > max {
		return nil, errors.New("body_too_large")
	}
	return body, nil
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
}
func writeRawResponse(writer http.ResponseWriter, status int, headers http.Header, body []byte) {
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Type") {
			writer.Header()[key] = values
		}
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
func writeResponse(writer http.ResponseWriter, status int, headers http.Header, body []byte) {
	writeRawResponse(writer, status, headers, body)
}
func writeRuntimeMeta(writer http.ResponseWriter, candidate Candidate, attempts int, attemptLog []map[string]any) {
	writer.Header().Set("X-Minato-Runtime-Source", candidate.SourceID)
	writer.Header().Set("X-Minato-Runtime-Model", candidate.ModelID)
	writer.Header().Set("X-Minato-Runtime-Attempts", strconv.Itoa(attempts))
	if encoded, err := json.Marshal(attemptLog); err == nil {
		writer.Header().Set("X-Minato-Runtime-Attempt-Log", base64.RawURLEncoding.EncodeToString(encoded))
	}
}
func newUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func readSSEFrame(reader *bufio.Reader, max int64) ([]byte, error) {
	frame := make([]byte, 0, 256)
	for int64(len(frame)) <= max {
		line, err := reader.ReadBytes('\n')
		frame = append(frame, line...)
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 && len(frame) > len(line) {
			return frame, nil
		}
		if err != nil {
			if len(frame) == 0 {
				return nil, err
			}
			return frame, nil
		}
	}
	return nil, errors.New("sse_frame_too_large")
}

func sseFrameFailure(frame []byte) bool {
	if len(frame) == 0 {
		return true
	}
	sawData := false
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		sawData = true
		data := strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("data:"))))
		if data == "" || data == "[DONE]" {
			return true
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			return false
		}
		if _, ok := event["error"]; ok {
			return true
		}
		if event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete" {
			return true
		}
		return false
	}
	return !sawData
}
func rewriteModel(body []byte, alias string) []byte {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	value["model"] = alias
	result, _ := json.Marshal(value)
	return result
}
