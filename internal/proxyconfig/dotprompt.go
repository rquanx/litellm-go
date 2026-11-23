package proxyconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var dotpromptHeader = regexp.MustCompile(`(?s)^---[^\S\n]*\n(.*?)\n---[^\S\n]*\n(.*)$`)
var dotpromptVariable = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*\}\}`)

func splitDotprompt(source string) (map[string]any, string, error) {
	if !utf8.ValidString(source) {
		return nil, "", errors.New("Prompt must be UTF-8")
	}
	metadata := make(map[string]any)
	source = strings.ReplaceAll(source, "\r\n", "\n")
	parts := dotpromptHeader.FindStringSubmatch(source)
	if len(parts) == 0 {
		return metadata, strings.TrimSpace(source), nil
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil {
		return nil, "", errors.New("Invalid prompt frontmatter")
	}
	return metadata, strings.TrimSpace(parts[2]), nil
}

func dotpromptMessages(content string, variables map[string]any) ([]map[string]any, error) {
	residue := dotpromptVariable.ReplaceAllString(content, "")
	if strings.Contains(residue, "{{") || strings.Contains(residue, "{%") || strings.Contains(residue, "{#") {
		rendered, err := renderDotpromptJinja(content, variables)
		if err != nil {
			return nil, err
		}
		return splitDotpromptMessages(rendered), nil
	}
	remaining := 2<<20 - len(residue)
	content = dotpromptVariable.ReplaceAllStringFunc(content, func(marker string) string {
		name := dotpromptVariable.FindStringSubmatch(marker)[1]
		var value any = variables
		for _, property := range strings.Split(name, ".") {
			fields, _ := value.(map[string]any)
			value = fields[property]
		}
		if value == nil {
			return ""
		}
		if remaining < 0 {
			return ""
		}
		text, ok := value.(string)
		if !ok {
			raw, _ := json.Marshal(value)
			text = string(raw)
		}
		remaining -= len(text)
		if remaining < 0 {
			return ""
		}
		return text
	})
	if remaining < 0 {
		return nil, errors.New("Rendered prompt exceeds the size limit")
	}
	return splitDotpromptMessages(content), nil
}

func validateDotpromptInput(metadata, variables map[string]any) error {
	input, _ := metadata["input"].(map[string]any)
	schema, _ := input["schema"].(map[string]any)
	for name, kind := range schema {
		value, present := variables[name]
		if !present {
			continue
		}
		typeName, ok := kind.(string)
		if !ok || !dotpromptValueMatches(value, strings.ToLower(typeName)) {
			return fmt.Errorf("Invalid type for prompt variable %q", name)
		}
	}
	return nil
}

func dotpromptValueMatches(value any, kind string) bool {
	switch kind {
	case "number", "integer", "int", "float":
		switch number := value.(type) {
		case int:
			return kind != "float"
		case float64:
			return kind == "number" || kind == "float" ||
				((kind == "integer" || kind == "int") && math.Trunc(number) == number)
		case bool:
			return kind != "float"
		default:
			return false
		}
	case "boolean", "bool":
		_, ok := value.(bool)
		return ok
	case "array", "list":
		_, ok := value.([]any)
		return ok
	case "object", "dict":
		_, ok := value.(map[string]any)
		return ok
	default:
		_, ok := value.(string)
		return ok
	}
}

func splitDotpromptMessages(content string) []map[string]any {
	messages := make([]map[string]any, 0, 4)
	role := "user"
	var lines []string
	flush := func() {
		if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		prefix, text, found := strings.Cut(line, ":")
		if found && (prefix == "System" || prefix == "User" || prefix == "Assistant") {
			flush()
			role = strings.ToLower(prefix)
			lines = []string{strings.TrimSpace(text)}
		} else {
			lines = append(lines, line)
		}
	}
	flush()
	return messages
}

func (api promptAPI) testPrompt(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if _, ok := api.keys.identifyResourceCaller(r); !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	var input struct {
		Content   string           `json:"dotprompt_content"`
		Variables map[string]any   `json:"prompt_variables"`
		History   []map[string]any `json:"conversation_history"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid prompt test request")
		return
	}
	metadata, content, err := splitDotprompt(input.Content)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	model, _ := metadata["model"].(string)
	if model == "" {
		modelError(w, http.StatusBadRequest, "Model is required in dotprompt metadata")
		return
	}
	if err := validateDotpromptInput(metadata, input.Variables); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	messages, err := dotpromptMessages(content, input.Variables)
	if err != nil || len(messages) == 0 {
		modelError(w, http.StatusBadRequest, "Unsupported or empty prompt template")
		return
	}
	if len(input.History) != 0 {
		history := make([]map[string]any, 0, len(messages)+len(input.History))
		for _, message := range messages {
			if message["role"] == "system" {
				history = append(history, message)
			}
		}
		messages = append(history, input.History...)
	}
	fields := map[string]any{"model": model, "messages": messages, "stream": true}
	applyDotpromptOptions(fields, metadata)
	body, err := json.Marshal(fields)
	if err != nil {
		modelError(w, http.StatusBadRequest, "Invalid prompt parameters")
		return
	}
	request := r.Clone(r.Context())
	request.URL = &url.URL{Path: "/v1/chat/completions"}
	request.RequestURI = request.URL.Path
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/json")
	next.ServeHTTP(w, request)
}

func (api promptAPI) convertDotprompt(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.keys.identifyResourceCaller(r); !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	reader, err := r.MultipartReader()
	if err != nil {
		modelError(w, http.StatusBadRequest, "Expected a prompt upload")
		return
	}
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		name := part.FileName()
		raw, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil || !strings.HasSuffix(name, ".prompt") {
			modelError(w, http.StatusBadRequest, "File must have .prompt extension")
			return
		}
		metadata, content, err := splitDotprompt(string(raw))
		if err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
		modelJSON(w, http.StatusOK, map[string]any{"prompt_id": strings.TrimSuffix(path.Base(name), ".prompt"),
			"json_data": map[string]any{"content": content, "metadata": metadata}})
		return
	}
	modelError(w, http.StatusBadRequest, "A .prompt file is required")
}

func applyDotpromptOptions(fields, metadata map[string]any) {
	for _, key := range []string{"temperature", "max_tokens", "max_completion_tokens", "top_p", "frequency_penalty", "presence_penalty", "stop", "seed", "tools", "tool_choice", "response_format", "reasoning_effort"} {
		if value := metadata[key]; value != nil {
			fields[key] = value
		}
	}
}
