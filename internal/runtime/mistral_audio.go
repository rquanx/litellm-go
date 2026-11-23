package runtime

import (
	"encoding/json"
	"errors"
	"strings"
)

func mistralTranscriptionRequest(input requestPayload, candidate Candidate) (string, []byte, string, error) {
	form := make([]formPart, 0, len(input.form)+1)
	hasFile, hasModel := false, false
	for _, part := range input.form {
		switch part.name {
		case "file":
			if hasFile || len(part.body) == 0 {
				return "", nil, "", errors.New("Provide one nonempty audio file")
			}
			hasFile = true
		case "model":
			hasModel = true
		case "language", "temperature", "timestamp_granularities", "timestamp_granularities[]", "response_format", "diarize":
		default:
			continue
		}
		form = append(form, part)
	}
	if !hasFile {
		return "", nil, "", errors.New("An audio file is required")
	}
	if !hasModel {
		form = append(form, formPart{name: "model", header: map[string][]string{"Content-Disposition": {`form-data; name="model"`}}})
	}
	input.form = form
	data, media, err := input.encodeMultipart(candidate.ModelID, false)
	return strings.TrimRight(candidate.APIBase, "/") + "/audio/transcriptions", data, media, err
}

func mistralTranscriptionResponse(raw []byte) ([]byte, error) {
	var original map[string]json.RawMessage
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, err
	}
	text, _ := original["text"]
	if len(text) == 0 || string(text) == "null" {
		text = json.RawMessage(`""`)
	}
	result := map[string]json.RawMessage{"text": text, "usage": json.RawMessage("null")}
	for _, field := range []string{"segments", "language"} {
		if value, found := original[field]; found {
			result[field] = value
		}
	}
	return json.Marshal(result)
}
