package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
)

func usesMistralOCR(provider string) bool {
	return provider == "mistral" || provider == "azure_ai" || provider == "vertex_ai"
}

func isVertexDeepSeekOCR(candidate Candidate) bool {
	return candidate.Vertex != nil && strings.Contains(candidate.ModelID, "deepseek")
}

func vertexOCRTarget(base string, candidate Candidate) string {
	target := base + "/v1/projects/" + url.PathEscape(candidate.Vertex.Project) + "/locations/" + url.PathEscape(candidate.Vertex.Location)
	if isVertexDeepSeekOCR(candidate) {
		return target + "/endpoints/openapi/chat/completions"
	}
	return target + "/publishers/mistralai/models/" + url.PathEscape(candidate.ModelID) + ":rawPredict"
}

func deepSeekOCRRequest(payload map[string]any) map[string]any {
	document, _ := payload["document"].(map[string]any)
	kind, _ := document["type"].(string)
	model, _ := payload["model"].(string)
	request := map[string]any{
		"model": "deepseek-ai/" + strings.TrimPrefix(model, "deepseek-ai/"),
		"messages": []map[string]any{{
			"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": document[kind]}},
		}},
	}
	for _, name := range []string{"temperature", "max_tokens", "top_p", "n", "stop"} {
		if field, ok := payload[name]; ok {
			request[name] = field
		}
	}
	return request
}

func deepSeekOCRResponse(body []byte, alias string) []byte {
	var response struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Choices) == 0 || len(response.Choices[0].Message.Content) == 0 {
		return body
	}
	content := response.Choices[0].Message.Content
	var markdown string
	if json.Unmarshal(content, &markdown) != nil {
		markdown = string(content)
		content = []byte(markdown)
	} else {
		content = []byte(markdown)
	}
	var ocr map[string]any
	_ = json.Unmarshal(content, &ocr)
	if _, ok := ocr["pages"].([]any); !ok {
		if ocr == nil {
			ocr = make(map[string]any)
		}
		ocr["pages"] = []map[string]any{{"index": 0, "markdown": markdown}}
		if ocr["usage_info"] == nil {
			if response.Usage == nil {
				response.Usage = map[string]any{}
			}
			ocr["usage_info"] = response.Usage
		}
	}
	if pages, ok := ocr["pages"].([]any); ok {
		for _, raw := range pages {
			page, _ := raw.(map[string]any)
			if page == nil {
				continue
			}
			if _, exists := page["index"]; !exists {
				page["index"] = 0
			}
			if _, exists := page["markdown"]; !exists {
				page["markdown"] = ""
			}
		}
	}
	encoded, _ := json.Marshal(ocr)
	return mistralOCRResponse(encoded, alias)
}

func decodeOCRPayload(contentType string, body []byte) (requestPayload, error) {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if mediaType != "multipart/form-data" {
		payload, err := decodeJSONPayload(body)
		if err != nil {
			return requestPayload{}, err
		}
		document, ok := payload.fields["document"].(map[string]any)
		if !ok || document["type"] == "file" {
			return requestPayload{}, errors.New("OCR requires a document object or a multipart file upload")
		}
		return payload, nil
	}
	form, err := readMultipart(body, params["boundary"])
	if err != nil {
		return requestPayload{}, err
	}
	payload := requestPayload{fields: map[string]any{}}
	for _, part := range form.form {
		_, disposition, _ := mime.ParseMediaType(part.header.Get("Content-Disposition"))
		filename := disposition["filename"]
		switch {
		case part.name == "file" && filename != "":
			if len(part.body) == 0 || payload.fields["document"] != nil {
				return requestPayload{}, errors.New("OCR requires one nonempty uploaded file")
			}
			mimeType, _, _ := mime.ParseMediaType(part.header.Get("Content-Type"))
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType, _, _ = mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))))
			}
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			kind := "document_url"
			if strings.HasPrefix(mimeType, "image/") {
				kind = "image_url"
			}
			payload.fields["document"] = map[string]any{
				"type": kind, kind: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(part.body),
			}
		case filename == "" && part.name != "file" && part.name != "document":
			var value any
			if json.Unmarshal(part.body, &value) != nil {
				value = string(part.body)
			}
			payload.fields[part.name] = value
		}
	}
	if payload.fields["document"] == nil {
		return requestPayload{}, errors.New("OCR multipart request requires a file")
	}
	return payload, nil
}

func mistralOCRRequest(payload map[string]any) map[string]any {
	request := map[string]any{"model": payload["model"], "document": payload["document"]}
	for _, name := range []string{
		"pages", "include_image_base64", "image_limit", "image_min_size", "bbox_annotation_format",
		"document_annotation_format", "document_annotation_prompt", "extract_header", "extract_footer",
		"table_format", "confidence_scores_granularity", "id",
	} {
		if field, ok := payload[name]; ok {
			request[name] = field
		}
	}
	return request
}

func mistralOCRResponse(body []byte, model string) []byte {
	var upstream map[string]any
	if json.Unmarshal(body, &upstream) != nil || upstream == nil {
		return body
	}
	response := map[string]any{
		"object": "ocr", "model": model, "pages": []any{}, "document_annotation": nil, "usage_info": nil,
	}
	for _, field := range []string{"pages", "document_annotation", "usage_info"} {
		if raw, ok := upstream[field]; ok {
			response[field] = raw
		}
	}
	if pages, ok := response["pages"].([]any); ok {
		for _, item := range pages {
			page, _ := item.(map[string]any)
			addOCRNullFields(page, "images", "dimensions")
			if dimensions, ok := page["dimensions"].(map[string]any); ok {
				addOCRNullFields(dimensions, "dpi", "height", "width")
			}
			if images, ok := page["images"].([]any); ok {
				for _, item := range images {
					image, _ := item.(map[string]any)
					addOCRNullFields(image, "image_base64", "bbox")
				}
			}
		}
	}
	if usage, ok := response["usage_info"].(map[string]any); ok {
		addOCRNullFields(usage, "pages_processed", "doc_size_bytes")
	}
	encoded, _ := json.Marshal(response)
	return encoded
}

func addOCRNullFields(fields map[string]any, names ...string) {
	if fields == nil {
		return
	}
	for _, name := range names {
		if _, exists := fields[name]; !exists {
			fields[name] = nil
		}
	}
}
