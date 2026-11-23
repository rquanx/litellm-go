package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

func StoredPromptResolver(db *sql.DB, keys *KeyRegistry) func(*http.Request, map[string]any) (int, error) {
	api := promptAPI{db, keys}
	return api.renderStoredPrompt
}

func (api promptAPI) renderStoredPrompt(r *http.Request, fields map[string]any) (int, error) {
	requested, _ := fields["prompt_id"].(string)
	if requested == "" {
		return http.StatusBadRequest, errors.New("A stored prompt ID is required")
	}
	admin, scope, ok := api.scope(r)
	if !ok {
		return http.StatusUnauthorized, errors.New("Proxy token required")
	}
	id, version := promptIDVersion(requested)
	if explicit, ok := fields["prompt_version"].(float64); ok && explicit > 0 && explicit <= 2147483647 {
		if version != 0 && float64(version) != explicit || explicit != float64(int(explicit)) {
			return http.StatusBadRequest, errors.New("Invalid prompt version")
		}
		version = int(explicit)
	}
	env, _ := fields["prompt_label"].(string)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	prompt, err := scanPrompt(api.db.QueryRowContext(ctx, `SELECT `+promptColumns+` FROM `+promptTable+`
		WHERE prompt_id=$1 AND ($2='' OR environment=$2) AND ($3=0 OR version=$3)
		AND ($4 OR prompt_id=ANY($5::text[]) OR (prompt_id || '.v' || version)=ANY($5::text[]))
		ORDER BY version DESC, (environment='development') DESC, environment LIMIT 1`, id, env, version, admin, pq.Array(scope)))
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, errors.New("Stored prompt not found")
	}
	if err != nil {
		return http.StatusServiceUnavailable, errors.New("Could not load stored prompt")
	}
	if prompt.Params["prompt_integration"] != "dotprompt" {
		return http.StatusBadRequest, errors.New("Unsupported prompt integration")
	}
	templateID, _ := prompt.Params["prompt_id"].(string)
	if templateID == "" {
		templateID = id
	}
	var metadata map[string]any
	var content string
	if source, _ := prompt.Params["dotprompt_content"].(string); source != "" {
		metadata, content, err = splitDotprompt(source)
		if err != nil {
			return http.StatusBadRequest, err
		}
	} else {
		items, _ := prompt.Params["prompt_data"].(map[string]any)
		item, _ := items[templateID].(map[string]any)
		content, _ = item["content"].(string)
		metadata, _ = item["metadata"].(map[string]any)
	}
	variables, _ := fields["prompt_variables"].(map[string]any)
	rendered, err := dotpromptMessages(content, variables)
	if err != nil || len(rendered) == 0 {
		return http.StatusBadRequest, errors.New("Unsupported or empty stored prompt template")
	}
	messages := make([]any, 0, len(rendered))
	for _, message := range rendered {
		messages = append(messages, message)
	}
	if client, ok := fields["messages"].([]any); ok {
		messages = append(messages, client...)
	}
	fields["messages"] = messages
	if ignore, _ := prompt.Params["ignore_prompt_manager_model"].(bool); !ignore {
		if model, _ := metadata["model"].(string); model != "" {
			fields["model"] = model
		} else if model, _ := fields["model"].(string); model != "" {
			fields["model"] = strings.TrimPrefix(model, "dotprompt/")
		}
	}
	if ignore, _ := prompt.Params["ignore_prompt_manager_optional_params"].(bool); !ignore {
		applyDotpromptOptions(fields, metadata)
	}
	for _, key := range []string{"prompt_id", "prompt_variables", "prompt_version", "prompt_label"} {
		delete(fields, key)
	}
	return 0, nil
}
