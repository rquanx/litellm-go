package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const evalPath = "/v1/evals"

func isEvalPath(path string) bool {
	return path == evalPath || strings.HasPrefix(path, evalPath+"/")
}

func evalOperation(r *http.Request, deployment *string) (string, string, error) {
	path := r.URL.EscapedPath()
	method := r.Method
	if path == evalPath && (method == http.MethodGet || method == http.MethodPost) {
		return path, "evals", nil
	}
	parts := strings.Split(strings.TrimPrefix(path, evalPath+"/"), "/")
	eval, err := evalSegment(parts[0], "evals", deployment)
	if err != nil {
		return "", "", err
	}
	path = evalPath + "/" + url.PathEscape(eval)
	if len(parts) == 1 && (method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete) {
		return path, "evals", nil
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "cancel":
			if method == http.MethodPost {
				return path + "/cancel", "evals", nil
			}
		case "runs":
			if method == http.MethodPost || method == http.MethodGet {
				return path + "/runs", "eval_runs", nil
			}
		}
	}
	if len(parts) == 3 && parts[1] == "runs" &&
		(method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete) {
		run, err := evalSegment(parts[2], "eval_runs", deployment)
		if err != nil {
			return "", "", err
		}
		target := path + "/runs/" + url.PathEscape(run)
		if method == http.MethodPost {
			target += "/cancel"
		}
		return target, "eval_runs", nil
	}
	return "", "", errors.New("unsupported eval operation")
}

func evalSegment(encoded, family string, deployment *string) (string, error) {
	id, err := url.PathUnescape(encoded)
	if err != nil {
		return "", err
	}
	return nativeResourceID(id, family, deployment)
}

func (s *Server) evalResource(w http.ResponseWriter, r *http.Request, scope []string) {
	var deployment string
	path, family, err := evalOperation(r, &deployment)
	if err != nil {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown eval operation")
		return
	}
	query := r.URL.Query()
	for _, field := range []string{"after", "before"} {
		for _, id := range query[field] {
			if _, err := nativeResourceID(id, family, &deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid eval cursor")
				return
			}
		}
	}
	alias := query.Get("model")
	if alias == "" {
		alias = r.Header.Get("X-LiteLLM-Model")
	}
	var body []byte
	var contentType string
	if r.Method == http.MethodPost {
		body, err = readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Eval request body is too large")
			return
		}
		if len(body) > 0 {
			payload, err := decodeJSONPayload(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Expected an eval JSON request")
				return
			}
			if model, _ := payload.fields["model"].(string); model != "" && (!(family == "eval_runs" && strings.HasSuffix(path, "/runs")) || alias == "") {
				alias = model
			}
			delete(payload.fields, "model")
			delete(payload.fields, "custom_llm_provider")
			if err := rewriteEvalFiles(payload.fields, &deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			body, _ = json.Marshal(payload.fields)
			contentType = "application/json"
		}
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, "openai", scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured OpenAI eval deployment")
		return
	}
	s.forwardOpenAIResource(w, r, candidate, family, path, contentType, body)
}

func rewriteEvalFiles(fields map[string]any, deployment *string) error {
	for _, name := range []string{"data_source_config", "data_source"} {
		config, _ := fields[name].(map[string]any)
		if err := rewriteVectorStoreFiles(config, deployment); err != nil {
			return err
		}
		if source, ok := config["source"].(map[string]any); ok {
			if err := rewriteVectorStoreFiles(source, deployment); err != nil {
				return err
			}
		}
	}
	return nil
}
