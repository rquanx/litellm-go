package runtime

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
)

var geminiEmbeddingMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".mp4": "video/mp4",
	".mov": "video/quicktime", ".pdf": "application/pdf",
}

//go:embed vertex-embed-content-models.json
var vertexEmbedContentSnapshot []byte

var vertexEmbedContentModels = func() []string {
	var models []string
	if err := json.Unmarshal(vertexEmbedContentSnapshot, &models); err != nil {
		panic("invalid embedded Vertex embedContent model list")
	}
	return models
}()

func VertexUsesEmbedContent(model string) bool {
	return slices.Contains(vertexEmbedContentModels, strings.TrimPrefix(model, "models/"))
}

func (s *Server) geminiContentEmbeddings(ctx context.Context, request *http.Request, candidate Candidate, model string, fields map[string]any) (int, []byte, http.Header, bool, error) {
	vertex := candidate.Vertex != nil
	var items []any
	switch input := fields["input"].(type) {
	case string:
		items = []any{input}
	case []any:
		items = input
	}
	invalid := func(message string) (int, []byte, http.Header, bool, error) {
		data, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "invalid_request", "message": message}})
		return http.StatusBadRequest, data, http.Header{"Content-Type": {"application/json"}}, false, nil
	}
	if len(items) == 0 {
		return invalid("A Gemini embedding input is required")
	}
	taskType := fields["task_type"]
	if taskType == nil {
		taskType = fields["input_type"]
	}
	parameters := map[string]any{"outputDimensionality": fields["dimensions"], "taskType": taskType, "title": fields["title"]}
	newRequest := func(parts []map[string]any) map[string]any {
		body := map[string]any{"content": map[string]any{"parts": parts}}
		for key, value := range parameters {
			if value != nil {
				body[key] = value
			}
		}
		return body
	}
	requests := make([]map[string]any, 0, len(items))
	combined := make([]map[string]any, 0, len(items))
	fileParts := make(map[string]map[string]any)
	var textInputs []string
	var hasMedia bool
	request = request.WithContext(ctx)
	for _, item := range items {
		elements, nested := item.([]any)
		if !nested {
			elements = []any{item}
		} else if vertex || len(elements) == 0 {
			return invalid("Nested Gemini embedding input requires a nonempty batch input group")
		}
		parts := make([]map[string]any, 0, len(elements))
		for _, element := range elements {
			value, ok := element.(string)
			if !ok {
				return invalid("Gemini embedding inputs must contain strings")
			}
			part, err := geminiEmbeddingPart(value)
			if err != nil {
				return invalid(err.Error())
			}
			if strings.HasPrefix(value, "files/") {
				if vertex {
					return invalid("Gemini file references require a Gemini Developer API deployment")
				}
				part = fileParts[value]
				if part == nil {
					part, err = s.geminiEmbeddingFile(request, candidate, value)
					if err != nil {
						return invalid(err.Error())
					}
					fileParts[value] = part
				}
			}
			if _, text := part["text"]; text {
				textInputs = append(textInputs, value)
			} else {
				hasMedia = true
			}
			parts = append(parts, part)
		}
		if vertex {
			combined = append(combined, parts...)
		} else {
			body := newRequest(parts)
			body["model"] = "models/" + model
			requests = append(requests, body)
		}
	}
	payload := map[string]any{"requests": requests}
	count := len(requests)
	if vertex {
		payload = newRequest(combined)
		count = 1
	}
	status, response, headers, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: payload})
	if err != nil || status/100 != 2 {
		return status, response, headers, retryable, err
	}
	type embedding struct {
		Values json.RawMessage `json:"values"`
	}
	var result struct {
		Embedding  embedding   `json:"embedding"`
		Embeddings []embedding `json:"embeddings"`
		Usage      struct {
			Tokens *int `json:"promptTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(response, &result) != nil {
		return 0, nil, nil, true, errors.New("invalid Gemini embedding response")
	}
	var vectors []json.RawMessage
	if vertex {
		vectors = []json.RawMessage{result.Embedding.Values}
	} else {
		vectors = make([]json.RawMessage, 0, len(result.Embeddings))
		for _, item := range result.Embeddings {
			vectors = append(vectors, item.Values)
		}
	}
	var tokens int
	if result.Usage.Tokens != nil {
		tokens = *result.Usage.Tokens
	} else if !vertex || !hasMedia {
		if codec, err := tokenizerForModel(model); err == nil {
			tokens, _ = codec.Count(strings.Join(textInputs, " "))
		}
	}
	return s.googleEmbeddingResult(candidate.ModelID, vectors, count, tokens)
}

func geminiEmbeddingPart(input string) (map[string]any, error) {
	if strings.HasPrefix(input, "gs://") {
		kind := geminiEmbeddingMediaTypes[strings.ToLower(path.Ext(input))]
		if kind == "" {
			return nil, errors.New("Unsupported media type for a GCS embedding input")
		}
		return map[string]any{"file_data": map[string]string{"mime_type": kind, "file_uri": input}}, nil
	}
	if metadata, data, ok := strings.Cut(input, ";base64,"); ok && strings.HasPrefix(metadata, "data:") {
		kind := strings.TrimPrefix(metadata, "data:")
		supported := false
		for _, mediaType := range geminiEmbeddingMediaTypes {
			if kind == mediaType {
				supported = true
				break
			}
		}
		if !supported || data == "" {
			return nil, errors.New("Unsupported media type for an inline embedding input")
		}
		return map[string]any{"inline_data": map[string]string{"mime_type": kind, "data": data}}, nil
	}
	return map[string]any{"text": input}, nil
}

func (s *Server) geminiEmbeddingFile(request *http.Request, candidate Candidate, input string) (map[string]any, error) {
	id := strings.TrimPrefix(input, "files/")
	if !validResourceSegment(id) {
		return nil, errors.New("Invalid Gemini file reference")
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1beta")
	metadata, err := newGoogleResourceRequest(request, candidate, base+"/v1beta/files/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, errors.New("Could not create the Gemini file lookup")
	}
	metadata.Method = http.MethodGet
	select {
	case s.sem <- struct{}{}:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, data, _, err := s.sendGeminiFileRequest(&client, metadata)
	var file struct {
		URI      string `json:"uri"`
		MIMEType string `json:"mimeType"`
	}
	if err != nil || status/100 != 2 || json.Unmarshal(data, &file) != nil || file.URI == "" || file.MIMEType == "" {
		return nil, errors.New("Could not resolve the Gemini file reference")
	}
	return map[string]any{"file_data": map[string]string{"mime_type": file.MIMEType, "file_uri": file.URI}}, nil
}
