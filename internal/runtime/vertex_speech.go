package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

func vertexSpeechDeployment(candidate Candidate) bool {
	return candidate.Provider == "vertex_ai" && candidate.Endpoint == "speech" && candidate.Vertex != nil
}

func vertexSpeechPayload(input map[string]any) ([]byte, string, error) {
	text, ok := input["input"].(string)
	if !ok || text == "" {
		return nil, "", errors.New("Vertex speech requires text or SSML")
	}
	inputType := "text"
	trimmed := strings.TrimSpace(text)
	if inputBool(input, "use_ssml") || strings.HasPrefix(trimmed, "<speak>") || strings.HasPrefix(trimmed, "<speak ") {
		inputType = "ssml"
	}
	voice, _ := input["voice"].(map[string]any)
	if voice == nil {
		name, _ := input["voice"].(string)
		if translated := map[string]string{"alloy": "en-US-Studio-O", "echo": "en-US-Studio-M", "fable": "en-GB-Studio-B",
			"onyx": "en-US-Wavenet-D", "nova": "en-US-Studio-O", "shimmer": "en-US-Wavenet-F"}[name]; translated != "" {
			name = translated
		}
		if name == "" {
			name = "en-US-Studio-O"
		}
		lang := "en-US"
		if parts := strings.SplitN(name, "-", 3); len(parts) >= 2 {
			lang = parts[0] + "-" + parts[1]
		}
		voice = map[string]any{"name": name, "languageCode": lang}
	}
	audio, _ := input["audioConfig"].(map[string]any)
	encoding, _ := audio["audioEncoding"].(string)
	if audio == nil {
		format, _ := input["response_format"].(string)
		encoding = map[string]string{"mp3": "MP3", "opus": "OGG_OPUS", "aac": "MP3", "flac": "FLAC", "wav": "LINEAR16", "pcm": "LINEAR16"}[format]
		if encoding == "" {
			encoding = format
			if encoding == "" {
				encoding = "LINEAR16"
			}
		}
		speed := 1.0
		if raw, ok := input["speed"].(float64); ok {
			if raw < 0.25 || raw > 4 {
				return nil, "", errors.New("Vertex speech speed must be between 0.25 and 4")
			}
			speed = raw
		}
		audio = map[string]any{"audioEncoding": encoding, "speakingRate": speed}
	}
	media := map[string]string{"MP3": "audio/mpeg", "OGG_OPUS": "audio/ogg", "FLAC": "audio/flac", "LINEAR16": "audio/wav",
		"MULAW": "audio/basic", "ALAW": "audio/basic"}[encoding]
	if media == "" {
		media = "application/octet-stream"
	}
	data, err := json.Marshal(map[string]any{"input": map[string]string{inputType: text}, "voice": voice, "audioConfig": audio})
	return data, media, err
}

func vertexSpeechAudio(data []byte) ([]byte, error) {
	var reply struct {
		Audio string `json:"audioContent"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Audio == "" {
		return nil, errors.New("Vertex speech returned no audio content")
	}
	return base64.StdEncoding.DecodeString(reply.Audio)
}
