package proxyconfig

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

func renderPhoenixPrompt(r *http.Request, fields map[string]any, record promptRecord) (int, error) {
	base, _ := record.Params["api_base"].(string)
	base, err := resolveEnv(base)
	if err != nil {
		return http.StatusBadRequest, errors.New("Could not resolve Phoenix prompt manager URL")
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return http.StatusBadRequest, errors.New("Invalid Phoenix prompt manager URL")
	}
	token, _ := record.Params["api_key"].(string)
	token, err = resolveEnv(token)
	if token == "" && err == nil {
		token = os.Getenv("PHOENIX_API_KEY")
	}
	if token == "" || err != nil {
		return http.StatusBadRequest, errors.New("Phoenix prompt manager credentials are required")
	}
	id, _ := record.Params["prompt_id"].(string)
	if id == "" {
		id = record.PromptID
	}
	target.RawPath = strings.TrimRight(target.EscapedPath(), "/") + "/v1/prompt_versions/" + url.PathEscape(id)
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/prompt_versions/" + id
	var response struct {
		Data struct {
			Model      string                    `json:"model_name"`
			Parameters map[string]map[string]any `json:"invocation_parameters"`
			Template   struct {
				Messages []struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
			} `json:"template"`
		} `json:"data"`
	}
	size, err := fetchManagedPrompt(r.Context(), target, "Bearer "+token, record.Params, &response)
	if err != nil || len(response.Data.Template.Messages) == 0 {
		return http.StatusBadGateway, errors.New("Invalid Phoenix prompt manager response")
	}
	variables, _ := fields["prompt_variables"].(map[string]any)
	messages := make([]map[string]any, 0, len(response.Data.Template.Messages))
	total := 0
	for _, message := range response.Data.Template.Messages {
		texts := make([]string, 0, len(message.Content))
		for _, part := range message.Content {
			if part.Type != "text" {
				return http.StatusBadGateway, errors.New("Unsupported Phoenix prompt content type")
			}
			text := part.Text
			if strings.Contains(text, "{{") || strings.Contains(text, "{%") || strings.Contains(text, "{#") {
				text, err = renderDotpromptJinja(text, variables)
				if err != nil {
					return http.StatusBadRequest, err
				}
			}
			total += len(text) + 1
			if total+size > maxManagedPromptBytes {
				return http.StatusBadRequest, errors.New("Rendered Phoenix prompt exceeds the size limit")
			}
			texts = append(texts, text)
		}
		role := message.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{"role": role, "content": strings.Join(texts, " ")})
	}
	options := map[string]any{"model": response.Data.Model}
	params := response.Data.Parameters
	names := append([]string{"openai", "anthropic"}, slices.Sorted(maps.Keys(params))...)
	for _, name := range names {
		if selected := params[name]; selected != nil {
			maps.Copy(options, selected)
			options["model"] = response.Data.Model
			break
		}
	}
	applyStoredPrompt(fields, record.Params, options, messages, "arize/")
	return 0, nil
}
