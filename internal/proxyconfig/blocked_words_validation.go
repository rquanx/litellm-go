package proxyconfig

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.yaml.in/yaml/v3"
)

func (registry *modelRegistry) validateBlockedWordsFile(w http.ResponseWriter, r *http.Request) {
	if registry.keys == nil {
		if !registry.allowed(w, r) {
			return
		}
	} else if _, valid := registry.keys.identifyReadCaller(r); !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	var input map[string]string
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusUnprocessableEntity, "Expected a JSON object containing string values")
		return
	}
	modelJSON(w, http.StatusOK, checkBlockedWordsFile(input["file_content"]))
}

func checkBlockedWordsFile(content string) map[string]any {
	invalid := func(message string) map[string]any {
		return map[string]any{"valid": false, "error": message}
	}
	if content == "" {
		return invalid("No file content provided")
	}
	decoder := yaml.NewDecoder(strings.NewReader(content))
	var data any
	if err := decoder.Decode(&data); err != nil && err != io.EOF {
		return invalid("Invalid YAML syntax: " + err.Error())
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid("Invalid YAML syntax: expected one document")
	}
	fields, isObject := data.(map[string]any)
	value, exists := fields["blocked_words"]
	if !isObject || !exists {
		return invalid("Invalid format: file must contain 'blocked_words' key with a list")
	}
	words, ok := value.([]any)
	if !ok {
		return invalid("'blocked_words' must be a list")
	}
	var errors []string
	for index, word := range words {
		entry, ok := word.(map[string]any)
		if !ok {
			if _, ok = word.(map[any]any); ok {
				entry = make(map[string]any)
				for key, value := range word.(map[any]any) {
					if name, ok := key.(string); ok {
						entry[name] = value
					}
				}
			} else {
				errors = append(errors, fmt.Sprintf("Entry %d: must be an object", index))
				continue
			}
		}
		for _, field := range []string{"keyword", "action", "description"} {
			value, exists := entry[field]
			switch text, stringValue := value.(string); {
			case !exists && field != "description":
				errors = append(errors, fmt.Sprintf("Entry %d: missing '%s' field", index, field))
			case !exists:
			case field == "action" && (!stringValue || (text != "BLOCK" && text != "MASK")):
				errors = append(errors, fmt.Sprintf("Entry %d: action must be 'BLOCK' or 'MASK', got '%v'", index, value))
			case !stringValue:
				errors = append(errors, fmt.Sprintf("Entry %d: '%s' must be a string", index, field))
			}
		}
	}
	if len(errors) > 0 {
		return map[string]any{"valid": false, "errors": errors}
	}
	return map[string]any{"valid": true, "message": fmt.Sprintf("Valid YAML file with %d blocked word(s)", len(words))}
}
