package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"path/filepath"
	"strings"
)

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
