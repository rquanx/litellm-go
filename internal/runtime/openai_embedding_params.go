package runtime

import (
	"errors"
	"slices"
	"strings"
)

func prepareOpenAIEmbeddingParams(payload map[string]any, candidate Candidate) error {
	allowed := parameterList(payload, "allowed_openai_params", candidate.AllowedOpenAIParams)
	for _, name := range parameterList(payload, "additional_drop_params", candidate.AdditionalDropParams) {
		switch name {
		case "dimensions", "encoding_format", "user":
			delete(payload, name)
		}
	}
	delete(payload, "drop_params")
	delete(payload, "allowed_openai_params")
	delete(payload, "additional_drop_params")
	if payload["dimensions"] != nil && !strings.Contains(candidate.ModelID, "text-embedding-3") &&
		!slices.Contains(allowed, "dimensions") {
		return unsupportedProviderParams{errors.New("Setting dimensions is not supported for OpenAI `text-embedding-3` and later models. To drop it from the call, set `litellm.drop_params = True`.")}
	}
	return nil
}
