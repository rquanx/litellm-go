package runtime

import (
	"encoding/json"
	"errors"
	"strings"
)

func multipartAudioDeployment(candidate Candidate) bool {
	return candidate.Endpoint == "transcription" && (candidate.Provider == "mistral" || candidate.Provider == "ovhcloud" || candidate.Provider == "scaleway")
}

func multipartAudioRequest(input requestPayload, candidate Candidate) (string, []byte, string, error) {
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
		case "language", "temperature", "timestamp_granularities", "timestamp_granularities[]", "response_format":
		case "diarize":
			if candidate.Provider != "mistral" {
				continue
			}
		case "prompt":
			if candidate.Provider == "mistral" {
				continue
			}
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

func multipartAudioResponse(raw []byte, media, provider string) ([]byte, error) {
	if provider == "scaleway" && !strings.Contains(strings.ToLower(media), "application/json") {
		return json.Marshal(map[string]any{"text": string(raw), "usage": nil})
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, err
	}
	text := original["text"]
	if provider == "ovhcloud" && (len(text) == 0 || string(text) == "null" || string(text) == `""`) {
		text = original["transcript"]
	}
	if len(text) == 0 || string(text) == "null" {
		text = json.RawMessage(`""`)
	}
	result := map[string]json.RawMessage{"text": text, "usage": json.RawMessage("null")}
	if provider == "ovhcloud" {
		return json.Marshal(result)
	}
	for _, field := range []string{"segments", "language"} {
		if value, found := original[field]; found {
			result[field] = value
		}
	}
	return json.Marshal(result)
}
