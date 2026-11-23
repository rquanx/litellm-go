package proxyconfig

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func renderLangfusePrompt(r *http.Request, fields map[string]any, record promptRecord) (int, error) {
	configuredHost, _ := record.Params["api_base"].(string)
	if configuredHost == "" {
		configuredHost, _ = record.Params["langfuse_host"].(string)
	}
	host, err := resolveEnv(configuredHost)
	if err != nil {
		return http.StatusBadRequest, errors.New("Could not resolve Langfuse host")
	}
	if host == "" {
		host = os.Getenv("LANGFUSE_HOST")
	}
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	target, err := url.Parse(host)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return http.StatusBadRequest, errors.New("Invalid Langfuse host")
	}
	public, _ := record.Params["langfuse_public_key"].(string)
	secret, _ := record.Params["langfuse_secret_key"].(string)
	if secret == "" {
		secret, _ = record.Params["langfuse_secret"].(string)
	}
	if secret == "" {
		secret, _ = record.Params["api_key"].(string)
	}
	public, publicErr := resolveEnv(public)
	secret, secretErr := resolveEnv(secret)
	if configuredHost == "" {
		if public == "" {
			public = os.Getenv("LANGFUSE_PUBLIC_KEY")
		}
		if secret == "" {
			secret = os.Getenv("LANGFUSE_SECRET_KEY")
		}
	}
	if publicErr != nil || secretErr != nil || public == "" || secret == "" {
		return http.StatusBadRequest, errors.New("Langfuse credentials are required")
	}
	id, _ := record.Params["prompt_id"].(string)
	if id == "" {
		id = record.PromptID
	}
	target.RawPath = strings.TrimRight(target.EscapedPath(), "/") + "/api/public/v2/prompts/" + url.PathEscape(id)
	target.Path = strings.TrimRight(target.Path, "/") + "/api/public/v2/prompts/" + id
	query := target.Query()
	if version, ok := fields["prompt_version"].(float64); ok && version > 0 && version <= 2147483647 && version == float64(int(version)) {
		query.Set("version", strconv.Itoa(int(version)))
	} else if label, _ := fields["prompt_label"].(string); label != "" {
		query.Set("label", label)
	}
	target.RawQuery = query.Encode()
	var template struct {
		Prompt json.RawMessage `json:"prompt"`
		Config map[string]any  `json:"config"`
	}
	size, err := fetchManagedPrompt(r.Context(), target, "Basic "+base64.StdEncoding.EncodeToString([]byte(public+":"+secret)), record.Params, &template)
	if err != nil {
		return http.StatusBadGateway, errors.New("Could not fetch Langfuse prompt")
	}
	var messages []map[string]any
	if json.Unmarshal(template.Prompt, &messages) != nil {
		var content string
		if json.Unmarshal(template.Prompt, &content) == nil && content != "" {
			messages = []map[string]any{{"role": "user", "content": content}}
		}
	}
	if len(messages) == 0 {
		return http.StatusBadGateway, errors.New("Invalid Langfuse prompt response")
	}
	if err := substituteManagedPrompt(messages, fields, size, dotpromptVariable); err != nil {
		return http.StatusBadRequest, err
	}
	applyStoredPrompt(fields, record.Params, template.Config, messages, "langfuse/")
	return 0, nil
}
