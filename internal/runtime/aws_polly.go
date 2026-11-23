package runtime

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

func pollyPayload(input map[string]any, model string) ([]byte, error) {
	text, ok := input["input"].(string)
	if !ok || text == "" {
		return nil, errors.New("AWS Polly requires text or SSML")
	}
	voice, _ := input["voice"].(string)
	if object, ok := input["voice"].(map[string]any); ok {
		voice, _ = object["name"].(string)
	}
	if mapped := map[string]string{"alloy": "Joanna", "echo": "Matthew", "fable": "Amy", "onyx": "Brian", "nova": "Ivy", "shimmer": "Kendra"}[voice]; mapped != "" {
		voice = mapped
	}
	if voice == "" {
		voice = "Joanna"
	}
	format, _ := input["response_format"].(string)
	if mapped := map[string]string{"opus": "ogg_vorbis", "aac": "mp3", "flac": "mp3", "wav": "pcm"}[format]; mapped != "" {
		format = mapped
	}
	if format == "" {
		format = "mp3"
	}
	engine := "neural"
	for _, component := range strings.Split(model, "/") {
		if slices.Contains([]string{"standard", "neural", "long-form", "generative"}, component) {
			engine = component
		}
	}
	if supplied, _ := input["engine"].(string); supplied != "" {
		if !slices.Contains([]string{"standard", "neural", "long-form", "generative"}, supplied) {
			return nil, errors.New("invalid AWS Polly engine")
		}
		engine = supplied
	}
	kind := "text"
	clean := strings.TrimSpace(text)
	if strings.HasPrefix(clean, "<speak>") || strings.HasPrefix(clean, "<speak ") {
		kind = "ssml"
	}
	body := map[string]any{"Engine": engine, "OutputFormat": format, "Text": text, "TextType": kind, "VoiceId": voice}
	for from, to := range map[string]string{"language_code": "LanguageCode", "lexicon_names": "LexiconNames", "sample_rate": "SampleRate"} {
		if value := input[from]; value != nil {
			body[to] = value
		}
	}
	return json.Marshal(body)
}
