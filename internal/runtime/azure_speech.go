package runtime

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

func azureSpeechDeployment(candidate Candidate) bool {
	return candidate.Provider == "azure" && candidate.Endpoint == "speech" && strings.HasPrefix(candidate.ModelID, "speech/")
}

func azureSpeechTarget(base string) (string, error) {
	address, err := url.Parse(base)
	if err != nil || address.Hostname() == "" {
		return "", errors.New("Invalid Azure speech endpoint")
	}
	if region, ok := strings.CutSuffix(address.Hostname(), ".api.cognitive.microsoft.com"); ok {
		address.Scheme = "https"
		address.Host = region + ".tts.speech.microsoft.com"
		address.Path = ""
	}
	address.Path = strings.TrimRight(address.Path, "/")
	if !strings.HasSuffix(address.Path, "/cognitiveservices/v1") {
		address.Path += "/cognitiveservices/v1"
	}
	address.RawPath = ""
	return address.String(), nil
}

func azureSpeechRequest(input map[string]any) ([]byte, string, error) {
	text, ok := input["input"].(string)
	if !ok || text == "" {
		return nil, "", errors.New("Azure speech requires text or SSML")
	}
	format, _ := input["response_format"].(string)
	if known := map[string]string{"mp3": "audio-24khz-48kbitrate-mono-mp3", "aac": "audio-24khz-48kbitrate-mono-mp3", "flac": "audio-24khz-48kbitrate-mono-mp3",
		"opus": "ogg-48khz-16bit-mono-opus", "wav": "riff-24khz-16bit-mono-pcm", "pcm": "raw-24khz-16bit-mono-pcm"}[format]; known != "" {
		format = known
	}
	if format == "" {
		format = "audio-24khz-48kbitrate-mono-mp3"
	}
	if strings.ContainsAny(format, "\r\n") {
		return nil, "", errors.New("Invalid Azure speech output format")
	}
	if raw := strings.TrimSpace(text); strings.HasPrefix(raw, "<speak ") || strings.HasPrefix(raw, "<speak>") {
		return []byte(text), format, nil
	}
	voice, _ := input["voice"].(string)
	if object, ok := input["voice"].(map[string]any); ok {
		voice, _ = object["name"].(string)
	}
	if known := map[string]string{"alloy": "en-US-JennyNeural", "echo": "en-US-GuyNeural", "fable": "en-GB-RyanNeural",
		"onyx": "en-US-DavisNeural", "nova": "en-US-AmberNeural", "shimmer": "en-US-AriaNeural"}[voice]; known != "" {
		voice = known
	}
	if voice == "" {
		voice = "en-US-AriaNeural"
	}
	speed := 1.0
	if supplied, ok := input["speed"].(float64); ok {
		if supplied < 0.25 || supplied > 4 {
			return nil, "", errors.New("Azure speech speed must be between 0.25 and 4")
		}
		speed = supplied
	}
	escaped := func(value string) string {
		var result bytes.Buffer
		_ = xml.EscapeText(&result, []byte(value))
		return result.String()
	}
	prosody := fmt.Sprintf("<prosody rate='%+d%%'>%s</prosody>", int((speed-1)*100), escaped(text))
	attrs := ""
	ns := ""
	for _, name := range []string{"style", "styledegree", "role"} {
		if value, _ := input[name].(string); value != "" {
			attrs += " " + name + "='" + escaped(value) + "'"
		}
	}
	if attrs != "" {
		ns = " xmlns:mstts='https://www.w3.org/2001/mstts'"
		prosody = "<mstts:express-as" + attrs + ">" + prosody + "</mstts:express-as>"
	}
	lang := ""
	if selected, _ := input["lang"].(string); selected != "" {
		lang = " xml:lang='" + escaped(selected) + "'"
	}
	ssml := fmt.Sprintf("<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis'%s xml:lang='en-US'><voice name='%s'%s>%s</voice></speak>",
		ns, escaped(voice), lang, prosody)
	return []byte(ssml), format, nil
}
