package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

const containerPath = "/v1/containers"

type ContainerOwners interface {
	Read(ctx context.Context, deployment string, ids []string) (map[string]string, error)
	Record(ctx context.Context, deployment, id, owner string) error
	Delete(ctx context.Context, deployment, id string) error
}

type containerKey struct{ deployment, id string }

type memoryContainerOwners struct {
	mu     sync.RWMutex
	owners map[containerKey]string
}

func (store *memoryContainerOwners) Read(_ context.Context, deployment string, ids []string) (map[string]string, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		if owner, exists := store.owners[containerKey{deployment, id}]; exists {
			result[id] = owner
		}
	}
	return result, nil
}

func (store *memoryContainerOwners) Record(_ context.Context, deployment, id, owner string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := containerKey{deployment, id}
	if existing, exists := store.owners[key]; exists && existing != owner {
		return errors.New("container already has an owner")
	}
	store.owners[key] = owner
	return nil
}

func (store *memoryContainerOwners) Delete(_ context.Context, deployment, id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.owners, containerKey{deployment, id})
	return nil
}

func isContainerPath(path string) bool {
	return path == containerPath || strings.HasPrefix(path, containerPath+"/")
}

type containerOperation struct {
	path, family, id, deployment string
}

func parseContainerOperation(r *http.Request) (op containerOperation, err error) {
	path := r.URL.EscapedPath()
	op.path, op.family = containerPath, "containers"
	if path == containerPath && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		return op, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, containerPath+"/"), "/")
	op.id, err = url.PathUnescape(parts[0])
	if err == nil {
		op.id, err = decodeContainerReference(op.id, &op.deployment)
	}
	if err != nil {
		return op, err
	}
	op.path += "/" + url.PathEscape(op.id)
	if len(parts) == 1 && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
		return op, nil
	}
	if len(parts) >= 2 && parts[1] == "files" {
		op.path += "/files"
		op.family = "container_files"
		if len(parts) == 2 && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
			return op, nil
		}
		if len(parts) == 3 || len(parts) == 4 {
			file, err := url.PathUnescape(parts[2])
			if err == nil {
				file, err = nativeResourceID(file, op.family, &op.deployment)
			}
			if err != nil {
				return op, err
			}
			op.path += "/" + url.PathEscape(file)
			if len(parts) == 3 && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
				return op, nil
			}
			if len(parts) == 4 && parts[3] == "content" && r.Method == http.MethodGet {
				op.path += "/content"
				return op, nil
			}
		}
	}
	return op, errors.New("unsupported container operation")
}

func (s *Server) containerResource(w http.ResponseWriter, r *http.Request, scope []string) {
	op, err := parseContainerOperation(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown container operation")
		return
	}
	query := r.URL.Query()
	provider := r.Header.Get("custom-llm-provider")
	if provider == "" {
		provider = query.Get("custom_llm_provider")
	}
	alias := query.Get("model")
	var body []byte
	contentType := r.Header.Get("Content-Type")
	if r.Method == http.MethodPost {
		body, err = readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Container request body is too large")
			return
		}
		if op.family == "containers" {
			payload, err := decodeJSONPayload(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Expected a container JSON request")
				return
			}
			if provider == "" {
				provider, _ = payload.fields["custom_llm_provider"].(string)
			}
			if alias == "" {
				alias, _ = payload.fields["model"].(string)
			}
			delete(payload.fields, "model")
			delete(payload.fields, "custom_llm_provider")
			if err := rewriteVectorStoreFiles(payload.fields, &op.deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			body, _ = json.Marshal(payload.fields)
			contentType = "application/json"
		} else {
			payload, err := resourceForm(contentType, body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Expected a multipart container file request")
				return
			}
			for index := range payload.form {
				part := &payload.form[index]
				if part.name != "file_id" {
					continue
				}
				id, err := nativeResourceID(string(part.body), "files", &op.deployment)
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_request", "Invalid container input file")
					return
				}
				part.body = []byte(id)
			}
			body, contentType, err = payload.encodeForm("", false, true)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid container file form")
				return
			}
		}
	}
	var cursors []string
	for _, field := range []string{"after", "before"} {
		for _, cursor := range query[field] {
			var id string
			var err error
			if op.family == "containers" {
				id, err = decodeContainerReference(cursor, &op.deployment)
			} else {
				id, err = nativeResourceID(cursor, op.family, &op.deployment)
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid container cursor")
				return
			}
			cursors = append(cursors, id)
		}
	}
	if provider == "azure_text" {
		provider = "azure"
	}
	candidate, ok := s.selectResourceCandidate(alias, op.deployment, provider, scope)
	if !ok && provider == "" && alias == "" && op.deployment == "" {
		candidate, ok = s.selectResourceCandidate("", "", "openai", scope)
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured container deployment")
		return
	}
	store := s.config.ContainerOwners
	admin := s.authorized(r)
	ownerScopes := s.containerOwnerScopes(r, admin)
	protectedIDs := []string{}
	if op.id != "" {
		protectedIDs = append(protectedIDs, op.id)
	} else {
		protectedIDs = cursors
	}
	if !admin && len(protectedIDs) > 0 {
		owners, err := store.Read(r.Context(), candidate.DeploymentID, protectedIDs)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check container ownership")
			return
		}
		for _, id := range protectedIDs {
			if !slices.Contains(ownerScopes, owners[id]) {
				writeError(w, http.StatusNotFound, "resource_not_found", "Container not found")
				return
			}
		}
	}
	transform := func(body []byte) ([]byte, error) {
		ctx, deployment := r.Context(), candidate.DeploymentID
		switch {
		case op.id == "" && r.Method == http.MethodPost:
			var response struct{ ID string `json:"id"` }
			if json.Unmarshal(body, &response) != nil || !validResourceSegment(response.ID) {
				return nil, errors.New("provider did not return a container ID")
			}
			if err := store.Record(context.WithoutCancel(ctx), deployment, response.ID, ownerScopes[0]); err != nil {
				return nil, err
			}
		case op.id == "" && r.Method == http.MethodGet && !admin:
			var err error
			body, err = filterContainerList(ctx, store, deployment, ownerScopes, body)
			if err != nil {
				return nil, err
			}
		case op.family == "containers" && r.Method == http.MethodDelete:
			if err := store.Delete(context.WithoutCancel(ctx), deployment, op.id); err != nil {
				return nil, err
			}
		}
		return wrapOpenAIResource(body, op.family, deployment), nil
	}
	s.forwardResource(w, r, candidate, op.family, op.path, contentType, body, transform)
}

func (s *Server) containerOwnerScopes(r *http.Request, admin bool) []string {
	if admin {
		return []string{"admin:master"}
	}
	token := ProxyCredential(r)
	if s.config.ResourceOwnerScopes != nil {
		if scopes := s.config.ResourceOwnerScopes(token); len(scopes) > 0 {
			return scopes
		}
	}
	digest := sha256.Sum256([]byte(token))
	return []string{"key:" + hex.EncodeToString(digest[:])}
}

func filterContainerList(ctx context.Context, store ContainerOwners, deployment string, scopes []string, body []byte) ([]byte, error) {
	var response map[string]json.RawMessage
	var entries []json.RawMessage
	if json.Unmarshal(body, &response) != nil || json.Unmarshal(response["data"], &entries) != nil || entries == nil {
		return nil, errors.New("provider did not return a container list")
	}
	ids := make([]string, len(entries))
	for i, entry := range entries {
		var row struct{ ID string `json:"id"` }
		if json.Unmarshal(entry, &row) == nil {
			ids[i] = row.ID
		}
	}
	owners, err := store.Read(ctx, deployment, ids)
	if err != nil {
		return nil, err
	}
	data := make([]json.RawMessage, 0, len(entries))
	var first, last string
	for i, id := range ids {
		if id != "" && slices.Contains(scopes, owners[id]) {
			data = append(data, entries[i])
			if first == "" {
				first = id
			}
			last = id
		}
	}
	response["data"], _ = json.Marshal(data)
	for key, id := range map[string]string{"first_id": first, "last_id": last} {
		if _, exists := response[key]; exists {
			response[key] = json.RawMessage("null")
			if id != "" {
				response[key], _ = json.Marshal(id)
			}
		}
	}
	return json.Marshal(response)
}
