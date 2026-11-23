package runtime

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

func minimaxSpeechFormat(input map[string]any) string {
	switch minimaxAudioFormat(input) {
	case "wav":
		return "audio/wav"
	case "pcm":
		return "application/octet-stream"
	case "flac":
		return "audio/flac"
	default:
		return "audio/mpeg"
	}
}

func minimaxAudioFormat(input map[string]any) string {
	format, _ := input["response_format"].(string)
	if extra, _ := input["extra_body"].(map[string]any); extra != nil {
		if value, ok := extra["format"].(string); ok {
			format = value
		}
	}
	switch format {
	case "wav", "pcm", "flac":
		return format
	}
	return "mp3"
}

func minimaxSpeechRequest(input map[string]any, model string) ([]byte, error) {
	text, ok := input["input"].(string)
	if !ok {
		return nil, errors.New("MiniMax speech requires text input")
	}
	voice, _ := input["voice"].(string)
	if obj, ok := input["voice"].(map[string]any); ok {
		for _, key := range []string{"voice_id", "id", "name"} {
			if voice, _ = obj[key].(string); strings.TrimSpace(voice) != "" {
				break
			}
		}
	}
	if strings.TrimSpace(voice) == "" {
		voice, _ = input["voice_id"].(string)
	}
	voice = strings.TrimSpace(voice)
	if mapped := (map[string]string{"alloy": "male-qn-qingse", "echo": "male-qn-jingying", "fable": "female-shaonv", "onyx": "male-qn-badao", "nova": "female-yujie", "shimmer": "female-tianmei"})[strings.ToLower(voice)]; mapped != "" {
		voice = mapped
	}
	if voice == "" {
		voice = "male-qn-qingse"
	}
	speed := 1.0
	switch n := input["speed"].(type) {
	case float64:
		speed = n
	case string:
		if parsed, err := strconv.ParseFloat(n, 64); err == nil && !math.IsNaN(parsed) {
			speed = parsed
		}
	}
	voiceSettings := map[string]any{"voice_id": voice, "speed": max(0.5, min(2.0, speed)), "vol": 1, "pitch": 0}
	audioSettings := map[string]any{"sample_rate": 32000, "bitrate": 128000, "format": minimaxAudioFormat(input), "channel": 1}
	extra, _ := input["extra_body"].(map[string]any)
	for _, key := range []string{"voice_id", "speed", "vol", "pitch"} {
		if value := extra[key]; value != nil {
			voiceSettings[key] = value
		}
	}
	for _, key := range []string{"sample_rate", "bitrate", "channel"} {
		if value := extra[key]; value != nil {
			audioSettings[key] = value
		}
	}
	for _, key := range []string{"vol", "pitch"} {
		if value := input[key]; value != nil {
			voiceSettings[key] = value
		}
	}
	for _, key := range []string{"sample_rate", "bitrate", "channel"} {
		if value := input[key]; value != nil {
			audioSettings[key] = value
		}
	}
	output := input["output_format"]
	if extra["output_format"] != nil {
		output = extra["output_format"]
	}
	if output != nil && output != "hex" {
		return nil, errors.New("MiniMax speech requires hex audio output")
	}
	return json.Marshal(map[string]any{"model": model, "text": text, "stream": false, "output_format": "hex",
		"voice_setting": voiceSettings, "audio_setting": audioSettings})
}

func minimaxSpeechResponse(raw []byte) ([]byte, error) {
	var body struct {
		Status      int    `json:"status"`
		LegacyAudio string `json:"audio_file"`
		Base        struct {
			Code int `json:"status_code"`
		} `json:"base_resp"`
		Data struct {
			Audio string `json:"audio"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("Invalid MiniMax speech response")
	}
	if body.Status != 0 || body.Base.Code != 0 {
		return nil, errors.New("MiniMax rejected the speech request")
	}
	audio := body.Data.Audio
	if audio == "" {
		audio = body.LegacyAudio
	}
	if audio == "" {
		return nil, errors.New("Missing MiniMax audio")
	}
	decoded, err := hex.DecodeString(audio)
	if err != nil {
		return base64.StdEncoding.DecodeString(audio)
	}
	return decoded, nil
}
