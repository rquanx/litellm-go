package proxyconfig

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func renderRepositoryPrompt(r *http.Request, fields map[string]any, record promptRecord) (int, error) {
	provider, _ := record.Params["prompt_integration"].(string)
	settings, _ := record.Params[provider+"_config"].(map[string]any)
	value := func(key string) string {
		text, _ := settings[key].(string)
		return text
	}
	token, err := resolveEnv(value("access_token"))
	if err != nil || token == "" {
		return http.StatusBadRequest, errors.New("Repository prompt credentials are required")
	}
	id, _ := record.Params["prompt_id"].(string)
	if id == "" {
		id = record.PromptID
	}
	folder := value("prompts_path")
	if folder == "" {
		folder = value("folder")
	}
	file := path.Join(folder, strings.TrimSuffix(id, ".prompt")+".prompt")
	ref, _ := record.Params["git_ref"].(string)
	if ref == "" {
		ref = value("tag")
	}
	if ref == "" {
		ref = value("branch")
	}
	if ref == "" {
		ref = "main"
	}
	base := value("base_url")
	headers := make(http.Header)
	var segments []string
	if provider == "gitlab" {
		if base == "" {
			base = "https://gitlab.com/api/v4"
		}
		project := value("project")
		if project == "" {
			if numeric, ok := settings["project"].(float64); ok && numeric > 0 && numeric == float64(int64(numeric)) {
				project = fmt.Sprint(int64(numeric))
			}
		}
		if project == "" {
			return http.StatusBadRequest, errors.New("GitLab project is required")
		}
		segments = []string{"projects", project, "repository", "files", file, "raw"}
		if value("auth_method") == "oauth" {
			headers.Set("Authorization", "Bearer "+token)
		} else {
			headers.Set("Private-Token", token)
		}
	} else {
		if base == "" {
			base = "https://api.bitbucket.org/2.0"
		}
		workspace, repository := value("workspace"), value("repository")
		if workspace == "" || repository == "" {
			return http.StatusBadRequest, errors.New("Bitbucket workspace and repository are required")
		}
		segments = append([]string{"repositories", workspace, repository, "src", ref}, strings.Split(file, "/")...)
		if value("auth_method") == "basic" && value("username") != "" {
			headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(value("username")+":"+token)))
		} else {
			headers.Set("Authorization", "Bearer "+token)
		}
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return http.StatusBadRequest, errors.New("Invalid repository prompt URL")
	}
	target.RawPath = strings.TrimRight(target.EscapedPath(), "/")
	target.Path = strings.TrimRight(target.Path, "/")
	for _, segment := range segments {
		target.Path += "/" + segment
		target.RawPath += "/" + url.PathEscape(segment)
	}
	if provider == "gitlab" {
		query := target.Query()
		query.Set("ref", ref)
		target.RawQuery = query.Encode()
	}
	raw, status, err := fetchPromptContent(r.Context(), target, headers, settings)
	if provider == "gitlab" && status == http.StatusNotFound {
		target.Path = strings.TrimSuffix(target.Path, "/raw")
		target.RawPath = strings.TrimSuffix(target.RawPath, "/raw")
		raw, status, err = fetchPromptContent(r.Context(), target, headers, settings)
		if err == nil {
			var response struct {
				Content string `json:"content"`
			}
			if err = json.Unmarshal(raw, &response); err == nil {
				raw, err = base64.StdEncoding.DecodeString(response.Content)
			}
		}
	}
	if status == http.StatusNotFound {
		return http.StatusNotFound, errors.New("Repository prompt not found")
	}
	if err != nil {
		return http.StatusBadGateway, errors.New("Could not fetch repository prompt")
	}
	metadata, content, err := splitDotprompt(string(raw))
	if err != nil {
		return http.StatusBadGateway, err
	}
	variables, _ := fields["prompt_variables"].(map[string]any)
	if err := validateDotpromptInput(metadata, variables); err != nil {
		return http.StatusBadRequest, err
	}
	messages, err := dotpromptMessages(content, variables)
	if err != nil || len(messages) == 0 {
		return http.StatusBadRequest, errors.New("Unsupported or empty repository prompt")
	}
	applyStoredPrompt(fields, record.Params, metadata, messages, provider+"/")
	return 0, nil
}
