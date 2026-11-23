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

const maxManagedPromptBytes = 2 << 20

var managedPromptHTTPClient = &http.Client{
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
	var auth string
	if key != "" {
		auth = "Bearer " + key
	}
	var template struct {
		Messages []map[string]any `json:"prompt_template"`
		Model    string           `json:"prompt_template_model"`
		Options  map[string]any   `json:"prompt_template_optional_params"`
	}
	size, err := fetchManagedPrompt(r.Context(), target, auth, record.Params, &template)
	if err != nil || len(template.Messages) == 0 {
		return http.StatusBadGateway, errors.New("Invalid prompt manager response")
	}
	if err := substituteManagedPrompt(template.Messages, fields, size, genericPromptVariable); err != nil {
		return http.StatusBadRequest, err
	}
	metadata := map[string]any{}
	maps.Copy(metadata, template.Options)
	metadata["model"] = template.Model
	applyStoredPrompt(fields, record.Params, metadata, template.Messages, "generic_prompt/")
	return 0, nil
}

func fetchManagedPrompt(parent context.Context, target *url.URL, authorization string, params map[string]any, template any) (int, error) {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	if authorization != "" {
		headers.Set("Authorization", authorization)
	}
	raw, _, err := fetchPromptContent(parent, target, headers, params)
	if err != nil {
		return 0, err
	}
	return len(raw), json.Unmarshal(raw, template)
}

func fetchPromptContent(parent context.Context, target *url.URL, headers http.Header, params map[string]any) ([]byte, int, error) {
	timeout := 30 * time.Second
	if seconds, ok := params["timeout"].(float64); ok && seconds >= 1 && seconds <= 30 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header = headers.Clone()
	response, err := managedPromptHTTPClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, response.StatusCode, errors.New("prompt manager rejected the request")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxManagedPromptBytes+1))
	if err != nil || len(raw) > maxManagedPromptBytes {
		return nil, response.StatusCode, errors.New("could not read prompt manager response")
	}
	return raw, response.StatusCode, nil
}

func substituteManagedPrompt(messages []map[string]any, fields map[string]any, size int, pattern *regexp.Regexp) error {
	variables, _ := fields["prompt_variables"].(map[string]any)
	remaining := maxManagedPromptBytes - size
	for _, message := range messages {
		content, ok := message["content"].(string)
		if !ok {
			continue
		}
		message["content"] = pattern.ReplaceAllStringFunc(content, func(marker string) string {
			value, ok := variables[strings.TrimSpace(strings.Trim(marker, "{}"))]
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
			return errors.New("Rendered prompt exceeds the size limit")
		}
	}
	return nil
}
