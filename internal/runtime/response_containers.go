package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
)

func decodeContainerReference(id string, deployment *string) (string, error) {
	if encoded, ok := strings.CutPrefix(id, "cntr_"); ok && len(encoded) <= 8192 {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil {
			value, managed := strings.CutPrefix(string(raw), "litellm:custom_llm_provider:")
			if managed {
				parts := strings.SplitN(value, ";", 3)
				if len(parts) != 3 {
					return "", errors.New("invalid LiteLLM container ID")
				}
				source, validModel := strings.CutPrefix(parts[1], "model_id:")
				original, found := strings.CutPrefix(parts[2], "container_id:")
				if !validModel || !found || !validResourceSegment(original) || (source != "" && *deployment != "" && source != *deployment) {
					return "", errors.New("invalid or mismatched LiteLLM container ID")
				}
				if source != "" {
					*deployment = source
				}
				return original, nil
			}
		}
	}
	return nativeResourceID(id, "containers", deployment)
}

func rewriteResponseContainers(input map[string]any, deployment *string) ([]string, error) {
	tools, _ := input["tools"].([]any)
	var referenced []string
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != "code_interpreter" {
			continue
		}
		switch container := tool["container"].(type) {
		case string:
			id, err := decodeContainerReference(container, deployment)
			if err != nil {
				return nil, err
			}
			tool["container"] = id
			if !slices.Contains(referenced, id) {
				referenced = append(referenced, id)
			}
		case map[string]any:
			if err := rewriteVectorStoreFiles(container, deployment); err != nil {
				return nil, err
			}
		}
	}
	return referenced, nil
}

func (s *Server) authorizeResponseContainers(r *http.Request, route *Route, deployment string, ids []string) (int, string) {
	if len(ids) == 0 || s.authorized(r) {
		return 0, ""
	}
	if deployment == "" {
		for _, candidate := range route.Candidates {
			if candidate.Endpoint != "responses" || candidate.DeploymentID == "" {
				continue
			}
			if deployment != "" && deployment != candidate.DeploymentID {
				return http.StatusBadRequest, "Select one deployment for the container"
			}
			deployment = candidate.DeploymentID
		}
		if deployment == "" || !route.pinResponseDeployment("", deployment) {
			return http.StatusBadRequest, "Select one deployment for the container"
		}
	}
	return s.checkResponseContainerOwners(r, deployment, ids)
}

func (s *Server) checkResponseContainerOwners(r *http.Request, deployment string, ids []string) (int, string) {
	if len(ids) == 0 || s.authorized(r) {
		return 0, ""
	}
	scopes := s.containerOwnerScopes(r, false)
	owners, err := s.config.ContainerOwners.Read(r.Context(), deployment, ids)
	if err != nil {
		return http.StatusServiceUnavailable, "Could not check container ownership"
	}
	for _, id := range ids {
		if !slices.Contains(scopes, owners[id]) {
			return http.StatusNotFound, "Container not found"
		}
	}
	return 0, ""
}

func (s *Server) responseContainerEncoder(r *http.Request, candidate Candidate) func(string) string {
	scope := s.containerOwnerScopes(r, s.authorized(r))[0]
	encoded := map[string]string{}
	return func(id string) string {
		if value, exists := encoded[id]; exists {
			return value
		}
		if !validResourceSegment(id) || strings.HasPrefix(id, "minato_") {
			return id
		}
		ref := encodeResourceID("containers", candidate.DeploymentID, id)
		if len(encoded) >= 1024 {
			clear(encoded)
		}
		encoded[id] = ref
		_ = s.config.ContainerOwners.Record(context.WithoutCancel(r.Context()), candidate.DeploymentID, id, scope)
		return ref
	}
}

func encodeOutputContainerIDs(item map[string]any, encode func(string) string) bool {
	if item == nil || encode == nil {
		return false
	}
	changed := false
	wrap := func(value map[string]any) {
		if id, ok := value["container_id"].(string); ok && id != "" {
			value["container_id"] = encode(id)
			changed = true
		}
	}
	wrap(item)
	if call, ok := item["code_interpreter_call"].(map[string]any); ok {
		wrap(call)
	}
	if item["type"] == "message" {
		content, _ := item["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			annotations, _ := part["annotations"].([]any)
			for _, raw := range annotations {
				annotation, _ := raw.(map[string]any)
				if annotation["type"] == "container_file_citation" {
					wrap(annotation)
				}
			}
		}
	}
	return changed
}

func encodeResponseContainerIDs(payload map[string]any, encode func(string) string) bool {
	changed := false
	items, _ := payload["output"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if encodeOutputContainerIDs(item, encode) {
			changed = true
		}
	}
	return changed
}
