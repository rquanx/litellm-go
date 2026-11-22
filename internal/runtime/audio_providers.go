package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

func nativeAudioDeployment(candidate Candidate) bool {
	return (candidate.Provider == "elevenlabs" && (candidate.Endpoint == "speech" || candidate.Endpoint == "transcription")) ||
		(candidate.Provider == "deepgram" && candidate.Endpoint == "transcription")
}

var elevenVoiceIDs = map[string]string{
	"alloy": "21m00Tcm4TlvDq8ikWAM", "amber": "5Q0t7uMcjvnagumLfvZi", "ash": "AZnzlk1XvdvUeBnXmlld",
	"august": "D38z5RcWu1voky8WS1ja", "blue": "2EiwWnXFnvU5JabPnv8n", "coral": "9BWtsMINqrJLrRacOk9x",
	"lily": "EXAVITQu4vr4xnSDxMaL", "onyx": "29vD33N1CtxCmqQRPOHJ", "sage": "CwhRBWXzGAHq8TQ4Fs17", "verse": "CYw3kZ02Hs0563khs1Fj",
}

func elevenSpeech(input map[string]any, candidate Candidate, base string) (string, []byte, string, error) {
	voice, _ := input["voice"].(string)
	if object, ok := input["voice"].(map[string]any); ok {
		for _, key := range []string{"voice_id", "id", "name"} {
			if voice, _ = object[key].(string); strings.TrimSpace(voice) != "" {
				break
			}
		}
	}
	if voice == "" {
		voice, _ = input["voice_id"].(string)
	}
	voice = strings.TrimSpace(voice)
	if known := elevenVoiceIDs[strings.ToLower(voice)]; known != "" {
		voice = known
	}
	text, ok := input["input"].(string)
	if !validResourceSegment(voice) || !ok {
		return "", nil, "", errors.New("ElevenLabs speech requires input and a voice")
	}
	query := make(url.Values)
	format, _ := input["response_format"].(string)
	if known := map[string]string{"mp3": "mp3_44100_128", "pcm": "pcm_44100", "opus": "opus_48000_128"}[format]; known != "" {
		format = known
	}
	if format == "" {
		format = "pcm_44100"
	}
	if custom, _ := input["output_format"].(string); custom != "" {
		format = custom
	}
	query.Set("output_format", format)
	payload := map[string]any{"text": text, "model_id": candidate.ModelID}
	for _, key := range []string{"language_code", "pronunciation_dictionary_locators", "seed", "previous_text", "next_text", "previous_request_ids", "next_request_ids"} {
		if value := input[key]; value != nil {
			payload[key] = value
		}
	}
	settings, _ := input["voice_settings"].(map[string]any)
	if speed, ok := input["speed"].(float64); ok {
		if settings == nil {
			settings = make(map[string]any)
		} else {
			settings = clone(settings)
		}
		settings["speed"] = speed
	}
	if settings != nil {
		payload["voice_settings"] = settings
	}
	data, err := json.Marshal(payload)
	return base + "/v1/text-to-speech/" + url.PathEscape(voice) + "?" + query.Encode(), data, "application/json", err
}

func audioProviderRequest(input requestPayload, candidate Candidate) (string, []byte, string, error) {
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	if candidate.Endpoint == "speech" {
		return elevenSpeech(input.fields, candidate, base)
	}
	fields := make(map[string]string)
	var file *formPart
	for i := range input.form {
		part := &input.form[i]
		if part.name == "file" {
			if file != nil {
				return "", nil, "", errors.New("Multiple audio files are unsupported")
			}
			file = part
			continue
		}
		fields[part.name] = string(part.body)
	}
	if file == nil || len(file.body) == 0 {
		return "", nil, "", errors.New("An audio file is required")
	}
	if candidate.Provider == "deepgram" {
		options := make(url.Values)
		options.Set("model", candidate.ModelID)
		for _, name := range []string{"language", "detect_language", "diarize", "paragraphs", "punctuate", "smart_format", "utterances", "numerals", "keywords"} {
			if value := fields[name]; value != "" {
				options.Set(name, value)
			}
		}
		media := file.header.Get("Content-Type")
		if media == "" || media == "application/octet-stream" {
			media = http.DetectContentType(file.body)
		}
		return base + "/v1/listen?" + options.Encode(), file.body, media, nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model_id", candidate.ModelID); err != nil {
		return "", nil, "", err
	}
	if fields["language_code"] == "" {
		fields["language_code"] = fields["language"]
	}
	for _, name := range []string{"language_code", "temperature", "diarize", "num_speakers", "tag_audio_events", "timestamps_granularity", "file_format"} {
		if value := fields[name]; value != "" {
			if err := writer.WriteField(name, value); err != nil {
				return "", nil, "", err
			}
		}
	}
	part, err := writer.CreatePart(file.header)
	if err != nil {
		return "", nil, "", err
	}
	if _, err := part.Write(file.body); err != nil {
		return "", nil, "", err
	}
	if err := writer.Close(); err != nil {
		return "", nil, "", err
	}
	return base + "/v1/speech-to-text", body.Bytes(), writer.FormDataContentType(), nil
}

type audioWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

func providerTranscription(data []byte, provider string) ([]byte, error) {
	result := map[string]any{"task": "transcribe"}
	var words []audioWord
	if provider == "elevenlabs" {
		var response struct {
			Text     string `json:"text"`
			Language string `json:"language_code"`
			Words    []struct {
				Text  string  `json:"text"`
				Type  string  `json:"type"`
				Start float64 `json:"start"`
				End   float64 `json:"end"`
			} `json:"words"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		result["text"], result["language"] = response.Text, response.Language
		if response.Language == "" {
			result["language"] = "unknown"
		}
		if response.Words != nil {
			words = make([]audioWord, 0, len(response.Words))
			for _, item := range response.Words {
				if item.Type == "word" {
					words = append(words, audioWord{item.Text, item.Start, item.End})
				}
			}
		}
	} else {
		var response struct {
			Metadata struct {
				Duration float64 `json:"duration"`
			} `json:"metadata"`
			Results struct {
				Channels []struct {
					Language     string `json:"detected_language"`
					Alternatives []struct {
						Transcript string `json:"transcript"`
						Paragraphs *struct {
							Transcript string `json:"transcript"`
						} `json:"paragraphs"`
						Words []struct {
							audioWord
							Speaker    *int   `json:"speaker"`
							Punctuated string `json:"punctuated_word"`
						} `json:"words"`
					} `json:"alternatives"`
				} `json:"channels"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		if len(response.Results.Channels) == 0 || len(response.Results.Channels[0].Alternatives) == 0 {
			return nil, errors.New("Deepgram returned no transcript alternatives")
		}
		channel := response.Results.Channels[0]
		alternative := channel.Alternatives[0]
		result["text"], result["language"], result["duration"] = alternative.Transcript, channel.Language, response.Metadata.Duration
		if channel.Language == "" {
			result["language"] = "en"
		}
		if len(alternative.Words) > 0 && alternative.Words[0].Speaker != nil {
			if alternative.Paragraphs != nil {
				result["text"] = alternative.Paragraphs.Transcript
			} else {
				var segments, current []string
				prev := *alternative.Words[0].Speaker
				for _, word := range alternative.Words {
					if word.Speaker != nil && *word.Speaker != prev {
						segments = append(segments, fmt.Sprintf("Speaker %d: %s", prev, strings.Join(current, " ")))
						prev, current = *word.Speaker, nil
					}
					if word.Punctuated != "" {
						current = append(current, word.Punctuated)
					} else {
						current = append(current, word.Word)
					}
				}
				segments = append(segments, fmt.Sprintf("\nSpeaker %d: %s\n", prev, strings.Join(current, " ")))
				result["text"] = strings.Join(segments, "\n")
			}
		}
		if alternative.Words != nil {
			words = make([]audioWord, 0, len(alternative.Words))
			for _, item := range alternative.Words {
				words = append(words, item.audioWord)
			}
		}
	}
	if words != nil {
		result["words"] = words
	}
	return json.Marshal(result)
}
