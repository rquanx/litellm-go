package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var genericPromptVariable = regexp.MustCompile(`\{\{[a-zA-Z_][a-zA-Z0-9_]*\}\}|\{[a-zA-Z_][a-zA-Z0-9_]*\}`)

var genericPromptHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func renderGenericPrompt(r *http.Request, fields map[string]any, record promptRecord) (int, error) {
	base, _ := record.Params["api_base"].(string)
	base, err := resolveEnv(base)
	if err != nil {
		return http.StatusBadRequest, errors.New("Could not resolve prompt manager URL")
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return http.StatusBadRequest, errors.New("Invalid prompt manager URL")
	}
	key, _ := record.Params["api_key"].(string)
	key, err = resolveEnv(key)
	if err != nil {
		return http.StatusBadRequest, errors.New("Could not resolve prompt manager credentials")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/beta/litellm_prompt_management"
	target.RawPath = ""
	query := target.Query()
	id, _ := record.Params["prompt_id"].(string)
	if id == "" {
		id = record.PromptID
	}
	query.Set("prompt_id", id)
	options, _ := record.Params["provider_specific_query_params"].(map[string]any)
	for name, value := range options {
		if values, ok := value.([]any); ok {
			query.Del(name)
			for _, item := range values {
				query.Add(name, fmt.Sprint(item))
			}
		} else {
			query.Set(name, fmt.Sprint(value))
		}
	}
	target.RawQuery = query.Encode()
	timeout := 30 * time.Second
	if seconds, ok := record.Params["timeout"].(float64); ok && seconds >= 1 && seconds <= 30 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return http.StatusBadRequest, errors.New("Invalid prompt manager request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := genericPromptHTTPClient.Do(request)
	if err != nil {
		return http.StatusBadGateway, errors.New("Could not reach prompt manager")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return http.StatusBadGateway, errors.New("Prompt manager rejected the request")
	}
	const maxPromptBytes = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPromptBytes+1))
	var template struct {
		Messages []map[string]any `json:"prompt_template"`
		Model    string           `json:"prompt_template_model"`
		Options  map[string]any   `json:"prompt_template_optional_params"`
	}
	if err != nil || len(raw) > maxPromptBytes || json.Unmarshal(raw, &template) != nil || len(template.Messages) == 0 {
		return http.StatusBadGateway, errors.New("Invalid prompt manager response")
	}
	variables, _ := fields["prompt_variables"].(map[string]any)
	remaining := maxPromptBytes - len(raw)
	for _, message := range template.Messages {
		content, ok := message["content"].(string)
		if !ok {
			continue
		}
		message["content"] = genericPromptVariable.ReplaceAllStringFunc(content, func(marker string) string {
			value, ok := variables[strings.Trim(marker, "{}")]
			if !ok {
				return marker
			}
			text := fmt.Sprint(value)
			remaining -= len(text) - len(marker)
			if remaining < 0 {
				return ""
			}
			return text
		})
		if remaining < 0 {
			return http.StatusBadRequest, errors.New("Rendered prompt exceeds the size limit")
		}
	}
	metadata := map[string]any{}
	maps.Copy(metadata, template.Options)
	metadata["model"] = template.Model
	applyStoredPrompt(fields, record.Params, metadata, template.Messages, "generic_prompt/")
	return 0, nil
}
