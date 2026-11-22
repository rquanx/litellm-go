package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strconv"
)

type requestPayload struct {
	fields map[string]any
	form   []formPart
}

type formPart struct {
	header textproto.MIMEHeader
	name   string
	body   []byte
}

func decodeJSONPayload(body []byte) (requestPayload, error) {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return requestPayload{}, errors.New("expected a JSON object")
	}
	return requestPayload{fields: fields}, nil
}

func decodePayload(contentType string, body []byte, endpoint string) (requestPayload, error) {
	if endpoint != "image-edit" && endpoint != "transcription" && endpoint != "translation" {
		return decodeJSONPayload(body)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if endpoint == "image-edit" && (contentType == "" || mediaType == "application/json") {
		return decodeJSONPayload(body)
	}
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return requestPayload{}, errors.New("expected multipart/form-data")
	}
	return readMultipart(body, params["boundary"])
}

func readMultipart(body []byte, boundary string) (requestPayload, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	payload := requestPayload{fields: make(map[string]any), form: make([]formPart, 0, 4)}
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return requestPayload{}, errors.New("invalid multipart body")
		}
		name := part.FormName()
		if name == "" || len(payload.form) >= 128 {
			return requestPayload{}, errors.New("invalid or excessive multipart fields")
		}
		if name == "model" && part.FileName() != "" {
			return requestPayload{}, errors.New("model must be a text field")
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return requestPayload{}, errors.New("invalid multipart body")
		}
		if part.FileName() == "" {
			switch name {
			case "model":
				if _, present := payload.fields[name]; present {
					return requestPayload{}, errors.New("duplicate model field")
				}
				payload.fields[name] = string(value)
			case "stream":
				payload.fields[name] = string(value) == "true"
			}
		}
		payload.form = append(payload.form, formPart{header: part.Header, name: name, body: value})
	}
	return payload, nil
}

func (payload requestPayload) encodeMultipart(model string, stream bool) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	hasStream := false
	for _, part := range payload.form {
		destination, err := writer.CreatePart(part.header)
		if err != nil {
			return nil, "", err
		}
		value := part.body
		if part.name == "model" {
			value = []byte(model)
		}
		if part.name == "stream" {
			hasStream = true
			value = []byte(strconv.FormatBool(stream))
		}
		if _, err := destination.Write(value); err != nil {
			return nil, "", err
		}
	}
	if stream && !hasStream {
		if err := writer.WriteField("stream", "true"); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}
