package proxyconfig

import "strings"

func openAIModelEndpoint(mode, model string) string {
	switch mode {
	case "chat":
		return "chat-completions"
	case "responses":
		return "responses"
	case "completion":
		return "completions"
	case "embedding":
		return "embeddings"
	case "image_generation":
		return "image-generation"
	case "audio_transcription":
		return "transcription"
	case "audio_speech":
		return "speech"
	case "moderation":
		return "moderations"
	}
	switch {
	case strings.HasPrefix(model, "text-embedding-"):
		return "embeddings"
	case strings.HasPrefix(model, "dall-e-"), strings.HasPrefix(model, "gpt-image-"):
		return "image-generation"
	case model == "whisper-1", strings.HasSuffix(model, "-transcribe"):
		return "transcription"
	case strings.HasPrefix(model, "tts-"), strings.HasSuffix(model, "-tts"):
		return "speech"
	case strings.Contains(model, "-moderation"):
		return "moderations"
	}
	return ""
}
