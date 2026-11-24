package proxyconfig

import (
	"crypto/subtle"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

const (
	scimUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimResourceSchema = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	scimSchemaSchema = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimConfigSchema = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
)

func (registry *KeyRegistry) registerSCIMRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /scim/v2", registry.scimBase)
	mux.HandleFunc("GET /scim/v2/", registry.scimBase)
	mux.HandleFunc("GET /scim/v2/ResourceTypes", registry.scimResourceTypes)
	mux.HandleFunc("GET /scim/v2/ResourceTypes/{resource_type_id}", registry.scimResourceTypes)
	mux.HandleFunc("GET /scim/v2/Schemas", registry.scimSchemas)
	mux.HandleFunc("GET /scim/v2/Schemas/{schema_id...}", registry.scimSchema)
	mux.HandleFunc("GET /scim/v2/ServiceProviderConfig", registry.scimServiceProviderConfig)
	mux.HandleFunc("GET /scim/v2/Users", registry.scimUsers)
	mux.HandleFunc("GET /scim/v2/Users/{user_id}", registry.scimUser)
	mux.HandleFunc("POST /scim/v2/Users", registry.scimCreateUser)
	mux.HandleFunc("PUT /scim/v2/Users/{user_id}", registry.scimUpdateUser)
	mux.HandleFunc("PATCH /scim/v2/Users/{user_id}", registry.scimPatchUser)
	mux.HandleFunc("DELETE /scim/v2/Users/{user_id}", registry.scimDeleteUser)
	mux.HandleFunc("GET /scim/v2/Groups", registry.scimGroups)
	mux.HandleFunc("GET /scim/v2/Groups/{group_id}", registry.scimGroup)
	mux.HandleFunc("POST /scim/v2/Groups", registry.scimCreateGroup)
	mux.HandleFunc("PUT /scim/v2/Groups/{group_id}", registry.scimUpdateGroup)
	mux.HandleFunc("PATCH /scim/v2/Groups/{group_id}", registry.scimPatchGroup)
	mux.HandleFunc("DELETE /scim/v2/Groups/{group_id}", registry.scimDeleteGroup)
}

func (registry *KeyRegistry) scimAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := registry.identifyReadCaller(r); !ok {
		scimError(w, http.StatusUnauthorized, "Valid proxy credentials are required")
		return false
	}
	return true
}

func (registry *KeyRegistry) scimAdmin(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(registry.master), []byte(runtime.ProxyCredential(r))) == 1 {
		return true
	}
	scimError(w, http.StatusUnauthorized, "Proxy master key required")
	return false
}

func scimError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"},
		"detail": detail,
		"status": strconv.Itoa(status),
	})
}

func scimJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (registry *KeyRegistry) scimBase(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas": []string{scimListSchema},
		"totalResults": 2,
		"Resources": registry.scimResourceTypeValues(r),
	})
}

func (registry *KeyRegistry) scimResourceTypes(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	values := registry.scimResourceTypeValues(r)
	if id := r.PathValue("resource_type_id"); id != "" {
		for _, value := range values {
			if value["id"] == id {
				scimJSON(w, http.StatusOK, value)
				return
			}
		}
		scimError(w, http.StatusNotFound, "ResourceType not found: "+id)
		return
	}
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(values), "Resources": values})
}

func (registry *KeyRegistry) scimResourceTypeValues(r *http.Request) []map[string]any {
	base := strings.TrimRight(requestBaseURL(r), "/") + "/scim/v2"
	return []map[string]any{
		{"schemas": []string{scimResourceSchema}, "id": "User", "name": "User", "endpoint": base + "/Users", "schema": scimUserSchema},
		{"schemas": []string{scimResourceSchema}, "id": "Group", "name": "Group", "endpoint": base + "/Groups", "schema": scimGroupSchema},
	}
}

func (registry *KeyRegistry) scimSchemas(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	values := []map[string]any{scimUserSchemaValue(), scimGroupSchemaValue()}
	if id := r.PathValue("schema_id"); id != "" {
		for _, value := range values {
			if value["id"] == id {
				scimJSON(w, http.StatusOK, value)
				return
			}
		}
		scimError(w, http.StatusNotFound, "Schema not found: "+id)
		return
	}
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(values), "Resources": values})
}

func (registry *KeyRegistry) scimSchema(w http.ResponseWriter, r *http.Request) {
	registry.scimSchemas(w, r)
}

func scimUserSchemaValue() map[string]any {
	return map[string]any{"schemas": []string{scimSchemaSchema}, "id": scimUserSchema, "name": "User", "description": "User account", "attributes": []map[string]any{
		{"name": "userName", "type": "string", "required": true, "uniqueness": "server"},
		{"name": "name", "type": "complex", "subAttributes": []map[string]any{{"name": "givenName", "type": "string"}, {"name": "familyName", "type": "string"}}},
		{"name": "displayName", "type": "string"},
		{"name": "active", "type": "boolean"},
		{"name": "emails", "type": "complex", "multiValued": true},
	}}
}

func scimGroupSchemaValue() map[string]any {
	return map[string]any{"schemas": []string{scimSchemaSchema}, "id": scimGroupSchema, "name": "Group", "description": "Team group", "attributes": []map[string]any{
		{"name": "displayName", "type": "string", "required": true},
		{"name": "members", "type": "complex", "multiValued": true},
	}}
}

func (registry *KeyRegistry) scimServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas": []string{scimConfigSchema},
		"patch": map[string]any{"supported": false},
		"bulk": map[string]any{"supported": false},
		"filter": map[string]any{"supported": true, "maxResults": 100},
		"changePassword": map[string]any{"supported": false},
		"sort": map[string]any{"supported": false},
		"etag": map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{"type": "httpbasic", "name": "Bearer", "description": "Bearer proxy credentials"}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": r.URL.String()},
	})
}

func scimPage(r *http.Request) (int, int) {
	start, count := 1, 10
	if value, err := strconv.Atoi(r.URL.Query().Get("startIndex")); err == nil && value > 0 {
		start = value
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && value > 0 && value <= 100 {
		count = value
	}
	return start, count
}

func scimSlice[T any](values []T, start, count int) []T {
	if start > len(values) {
		return []T{}
	}
	end := min(len(values), start-1+count)
	return values[start-1:end]
}

func (registry *KeyRegistry) scimUsers(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	start, count := scimPage(r)
	filter := strings.ToLower(r.URL.Query().Get("filter"))
	registry.mu.RLock()
	values := make([]map[string]any, 0, len(registry.state.Users))
	for _, user := range registry.state.Users {
		externalID, _ := user.Metadata["scim_external_id"].(string)
		if filter != "" && !scimFilterMatches(filter, user.ID, user.Email, externalID) {
			continue
		}
		values = append(values, scimUserValue(user, registry.state.Teams))
	}
	registry.mu.RUnlock()
	slices.SortFunc(values, func(a, b map[string]any) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
	page := scimSlice(values, start, count)
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(values), "startIndex": start, "itemsPerPage": len(page), "Resources": page})
}

func scimFilterMatches(filter, id, email, externalID string) bool {
	parts := strings.Fields(filter)
	if len(parts) < 3 || strings.ToLower(parts[1]) != "eq" {
		return false
	}
	value := strings.Trim(parts[2], "'\"")
	switch strings.ToLower(parts[0]) {
	case "username", "userName":
		return strings.EqualFold(value, id) || strings.EqualFold(value, email)
	case "emails.value":
		return strings.EqualFold(value, email)
	case "externalid":
		return strings.EqualFold(value, externalID)
	default:
		return false
	}
}

func scimUserValue(user internalUser, teams map[string]team) map[string]any {
	groups := []map[string]any{}
	for id, team := range teams {
		if memberIndex(team, user.ID) >= 0 {
			groups = append(groups, map[string]any{"value": id, "display": firstNonEmpty(team.Alias, id), "type": "direct"})
		}
	}
	active := true
	if value, ok := user.Metadata["scim_active"].(bool); ok {
		active = value
	}
	result := map[string]any{"schemas": []string{scimUserSchema}, "id": user.ID, "userName": user.ID, "displayName": firstNonEmpty(user.Alias, user.ID), "active": active, "groups": groups}
	if externalID, ok := user.Metadata["scim_external_id"].(string); ok && externalID != "" { result["externalId"] = externalID }
	if user.Email != "" {
		result["emails"] = []map[string]any{{"value": user.Email, "type": "work", "primary": true}}
	}
	if metadata, ok := user.Metadata["scim_metadata"].(map[string]any); ok {
		result["name"] = metadata
	}
	return result
}

func (registry *KeyRegistry) scimUser(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	id := r.PathValue("user_id")
	registry.mu.RLock()
	user, exists := registry.state.Users[id]
	teams := registry.state.Teams
	registry.mu.RUnlock()
	if !exists {
		scimError(w, http.StatusNotFound, "User not found with ID: "+id)
		return
	}
	scimJSON(w, http.StatusOK, scimUserValue(user, teams))
}

func (registry *KeyRegistry) scimGroups(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	start, count := scimPage(r)
	filter := strings.ToLower(r.URL.Query().Get("filter"))
	registry.mu.RLock()
	values := make([]map[string]any, 0, len(registry.state.Teams))
	for id, team := range registry.state.Teams {
		if filter != "" {
			parts := strings.SplitN(filter, "eq", 2)
			if len(parts) != 2 { continue }
			value := strings.Trim(strings.TrimSpace(parts[1]), " '\"")
			externalID, _ := team.Metadata["scim_external_id"].(string)
			switch strings.TrimSpace(parts[0]) {
			case "displayname":
				if !strings.EqualFold(value, firstNonEmpty(team.Alias, id)) { continue }
			case "externalid":
				if !strings.EqualFold(value, externalID) { continue }
			default:
				continue
			}
		}
		members := []map[string]any{}
		for _, member := range team.Members {
			user := registry.state.Users[member.UserID]
			members = append(members, map[string]any{"value": member.UserID, "display": firstNonEmpty(user.Email, member.UserID)})
		}
		values = append(values, scimGroupValue(team, registry.state.Users))
	}
	registry.mu.RUnlock()
	slices.SortFunc(values, func(a, b map[string]any) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
	page := scimSlice(values, start, count)
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(values), "startIndex": start, "itemsPerPage": len(page), "Resources": page})
}

func (registry *KeyRegistry) scimGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAuthorized(w, r) {
		return
	}
	id := r.PathValue("group_id")
	registry.mu.RLock()
	team, exists := registry.state.Teams[id]
	members := slices.Clone(team.Members)
	users := registry.state.Users
	registry.mu.RUnlock()
	if !exists {
		scimError(w, http.StatusNotFound, "Group not found with ID: "+id)
		return
	}
	entries := []map[string]any{}
	for _, member := range members {
		user := users[member.UserID]
		entries = append(entries, map[string]any{"value": member.UserID, "display": firstNonEmpty(user.Email, member.UserID)})
	}
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimGroupSchema}, "id": id, "displayName": firstNonEmpty(team.Alias, id), "members": entries})
}

type scimPatchOperation struct {
	Op string `json:"op"`
	Path string `json:"path"`
	Value json.RawMessage `json:"value"`
}

type scimPatchDocument struct {
	Operations []scimPatchOperation `json:"Operations"`
}

func decodeSCIMPatch(w http.ResponseWriter, r *http.Request) ([]scimPatchOperation, bool) {
	var document scimPatchDocument
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &document) != nil || len(document.Operations) == 0 {
		scimError(w, http.StatusBadRequest, "Invalid SCIM patch document")
		return nil, false
	}
	return document.Operations, true
}

func (registry *KeyRegistry) scimPatchUser(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	operations, ok := decodeSCIMPatch(w, r); if !ok { return }
	id := r.PathValue("user_id")
	registry.mu.Lock(); defer registry.mu.Unlock()
	user, exists := registry.state.Users[id]
	if !exists { scimError(w, http.StatusNotFound, "User not found: "+id); return }
	for _, operation := range operations {
		switch strings.ToLower(operation.Op) {
		case "replace", "add":
			field := strings.ToLower(operation.Path)
			switch field {
			case "active":
				var active bool; if json.Unmarshal(operation.Value, &active) != nil { scimError(w, http.StatusBadRequest, "active must be boolean"); return }
				if user.Metadata == nil { user.Metadata = map[string]any{} }; user.Metadata["scim_active"] = active
			case "displayname":
				var value string; if json.Unmarshal(operation.Value, &value) != nil { scimError(w, http.StatusBadRequest, "displayName must be a string"); return }; user.Alias = value
			case "emails", "emails.value":
				var values []struct{ Value string `json:"value"` }
				if len(operation.Value) > 0 && operation.Value[0] == '{' { var value struct{ Value string `json:"value"` }; if json.Unmarshal(operation.Value, &value) != nil { scimError(w, http.StatusBadRequest, "Invalid email"); return }; values = append(values, value) } else if json.Unmarshal(operation.Value, &values) != nil { scimError(w, http.StatusBadRequest, "Invalid emails"); return }
				if len(values) > 0 { user.Email = values[0].Value }
			default: scimError(w, http.StatusBadRequest, "Unsupported SCIM user patch path: "+operation.Path); return
			}
		case "remove":
			if strings.EqualFold(operation.Path, "emails") || strings.EqualFold(operation.Path, "emails.value") { user.Email = "" } else if strings.EqualFold(operation.Path, "active") { if user.Metadata == nil { user.Metadata = map[string]any{} }; user.Metadata["scim_active"] = false } else { scimError(w, http.StatusBadRequest, "Unsupported SCIM user patch path: "+operation.Path); return }
		default: scimError(w, http.StatusBadRequest, "Unsupported SCIM patch operation: "+operation.Op); return
		}
	}
	next := registry.state; next.Users = maps.Clone(next.Users); next.Users[id] = user
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not patch user"); return }
	scimJSON(w, http.StatusOK, scimUserValue(user, next.Teams))
}

func scimPatchMembers(raw json.RawMessage) ([]string, error) {
	var values []struct{ Value string `json:"value"` }
	if len(raw) > 0 && raw[0] == '{' { var value struct{ Value string `json:"value"` }; if err := json.Unmarshal(raw, &value); err != nil { return nil, err }; return []string{value.Value}, nil }
	if err := json.Unmarshal(raw, &values); err != nil { return nil, err }
	result := make([]string, 0, len(values)); for _, value := range values { result = append(result, value.Value) }; return result, nil
}

func (registry *KeyRegistry) scimPatchGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	operations, ok := decodeSCIMPatch(w, r); if !ok { return }
	id := r.PathValue("group_id")
	registry.mu.Lock(); defer registry.mu.Unlock()
	team, exists := registry.state.Teams[id]; if !exists { scimError(w, http.StatusNotFound, "Group not found: "+id); return }
	for _, operation := range operations {
		field := strings.ToLower(operation.Path)
		if bracket := strings.IndexByte(field, '['); bracket >= 0 {
			field = field[:bracket]
		}
		switch strings.ToLower(operation.Op) {
		case "replace":
			if field == "displayname" { if json.Unmarshal(operation.Value, &team.Alias) != nil { scimError(w, http.StatusBadRequest, "displayName must be a string"); return } } else if field == "members" { values, err := scimPatchMembers(operation.Value); if err != nil { scimError(w, http.StatusBadRequest, "Invalid members"); return }; team.Members = nil; for _, member := range values { if _, ok := registry.state.Users[member]; !ok { scimError(w, http.StatusBadRequest, "User not found: "+member); return }; team.Members = append(team.Members, teamMembership{TeamID:id, UserID:member, Role:"user"}) } } else { scimError(w, http.StatusBadRequest, "Unsupported SCIM group patch path: "+operation.Path); return }
		case "add":
			if field != "members" { scimError(w, http.StatusBadRequest, "Unsupported SCIM group patch path: "+operation.Path); return }; values, err := scimPatchMembers(operation.Value); if err != nil { scimError(w, http.StatusBadRequest, "Invalid members"); return }; for _, member := range values { if _, ok := registry.state.Users[member]; !ok { scimError(w, http.StatusBadRequest, "User not found: "+member); return }; if memberIndex(team, member) < 0 { team.Members = append(team.Members, teamMembership{TeamID:id, UserID:member, Role:"user"}) } }
		case "remove":
			if field != "members" { scimError(w, http.StatusBadRequest, "Unsupported SCIM group patch path: "+operation.Path); return }
			if len(operation.Value) > 0 { values, err := scimPatchMembers(operation.Value); if err == nil { for _, member := range values { team.Members = slices.DeleteFunc(team.Members, func(entry teamMembership) bool { return entry.UserID == member }) } } }
			if bracket := strings.Index(strings.ToLower(operation.Path), "value eq"); bracket >= 0 { expression := strings.Trim(strings.TrimSpace(operation.Path[bracket+len("value eq"):]), "[] \""); team.Members = slices.DeleteFunc(team.Members, func(entry teamMembership) bool { return strings.EqualFold(entry.UserID, expression) }) }
		default: scimError(w, http.StatusBadRequest, "Unsupported SCIM patch operation: "+operation.Op); return
		}
	}
	next := registry.state; next.Teams = maps.Clone(next.Teams); next.Teams[id] = team
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not patch group"); return }
	scimJSON(w, http.StatusOK, scimGroupValue(team, next.Users))
}

type scimWriteUser struct {
	UserName string `json:"userName"`
	ExternalID string `json:"externalId"`
	DisplayName string `json:"displayName"`
	Active *bool `json:"active"`
	Emails []struct { Value string `json:"value"` } `json:"emails"`
	Name map[string]any `json:"name"`
}

func (registry *KeyRegistry) scimCreateUser(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	var input scimWriteUser
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		scimError(w, http.StatusBadRequest, "Invalid SCIM user")
		return
	}
	id := strings.TrimSpace(input.UserName)
	if id == "" { id = newEntityID() }
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.state.Users[id]; exists {
		scimError(w, http.StatusConflict, "User already exists: "+id)
		return
	}
	metadata := map[string]any{}
	if input.Active != nil { metadata["scim_active"] = *input.Active }
	if input.Name != nil { metadata["scim_metadata"] = input.Name }
	if input.ExternalID != "" { metadata["scim_external_id"] = input.ExternalID }
	email := ""
	if len(input.Emails) > 0 { email = input.Emails[0].Value }
	user := internalUser{ID: id, Alias: input.DisplayName, Email: email, Role: "internal_user", Models: []string{}, Metadata: metadata}
	next := registry.state
	next.Users = maps.Clone(next.Users)
	if next.Users == nil { next.Users = map[string]internalUser{} }
	next.Users[id] = user
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not create user"); return }
	scimJSON(w, http.StatusCreated, scimUserValue(user, next.Teams))
}

func (registry *KeyRegistry) scimUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	var input scimWriteUser
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil { scimError(w, http.StatusBadRequest, "Invalid SCIM user"); return }
	id := r.PathValue("user_id")
	registry.mu.Lock()
	defer registry.mu.Unlock()
	user, exists := registry.state.Users[id]
	if !exists { scimError(w, http.StatusNotFound, "User not found: "+id); return }
	if input.DisplayName != "" { user.Alias = input.DisplayName }
	if len(input.Emails) > 0 { user.Email = input.Emails[0].Value }
	if input.Active != nil { if user.Metadata == nil { user.Metadata = map[string]any{} }; user.Metadata["scim_active"] = *input.Active }
	if input.Name != nil { if user.Metadata == nil { user.Metadata = map[string]any{} }; user.Metadata["scim_metadata"] = input.Name }
	if input.ExternalID != "" { if user.Metadata == nil { user.Metadata = map[string]any{} }; user.Metadata["scim_external_id"] = input.ExternalID }
	next := registry.state; next.Users = maps.Clone(next.Users); next.Users[id] = user
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not update user"); return }
	scimJSON(w, http.StatusOK, scimUserValue(user, next.Teams))
}

func (registry *KeyRegistry) scimDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	id := r.PathValue("user_id")
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.state.Users[id]; !exists { scimError(w, http.StatusNotFound, "User not found: "+id); return }
	next := registry.state; next.Users = maps.Clone(next.Users); delete(next.Users, id); next.Keys = maps.Clone(next.Keys)
	for digest, key := range next.Keys { if key.UserID == id { key.Blocked = true; next.Keys[digest] = key } }
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not delete user"); return }
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(http.StatusNoContent)
}

type scimWriteGroup struct {
	ID string `json:"id"`
	ExternalID string `json:"externalId"`
	DisplayName string `json:"displayName"`
	Members []struct { Value string `json:"value"` } `json:"members"`
}

func (registry *KeyRegistry) scimCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	var input scimWriteGroup
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || strings.TrimSpace(input.DisplayName) == "" { scimError(w, http.StatusBadRequest, "Invalid SCIM group"); return }
	id := firstNonEmpty(input.ID, input.ExternalID, newEntityID())
	registry.mu.Lock(); defer registry.mu.Unlock()
	if _, exists := registry.state.Teams[id]; exists { scimError(w, http.StatusConflict, "Group already exists: "+id); return }
	members := make([]teamMembership, 0, len(input.Members))
	for _, member := range input.Members { if _, exists := registry.state.Users[member.Value]; !exists { scimError(w, http.StatusBadRequest, "User not found: "+member.Value); return }; members = append(members, teamMembership{TeamID: id, UserID: member.Value, Role: "user"}) }
	metadata := map[string]any{}
	if input.ExternalID != "" { metadata["scim_external_id"] = input.ExternalID }
	next := registry.state; next.Teams = maps.Clone(next.Teams); next.Teams[id] = team{ID: id, Alias: input.DisplayName, Models: []string{}, Metadata: metadata, Members: members}
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not create group"); return }
	scimJSON(w, http.StatusCreated, scimGroupValue(next.Teams[id], next.Users))
}

func (registry *KeyRegistry) scimUpdateGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	var input scimWriteGroup
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil { scimError(w, http.StatusBadRequest, "Invalid SCIM group"); return }
	id := r.PathValue("group_id")
	registry.mu.Lock(); defer registry.mu.Unlock()
	team, exists := registry.state.Teams[id]; if !exists { scimError(w, http.StatusNotFound, "Group not found: "+id); return }
	if input.DisplayName != "" { team.Alias = input.DisplayName }
	if input.ExternalID != "" { if team.Metadata == nil { team.Metadata = map[string]any{} }; team.Metadata["scim_external_id"] = input.ExternalID }
	if input.Members != nil { team.Members = nil; for _, member := range input.Members { if _, ok := registry.state.Users[member.Value]; !ok { scimError(w, http.StatusBadRequest, "User not found: "+member.Value); return }; team.Members = append(team.Members, teamMembership{TeamID: id, UserID: member.Value, Role: "user"}) } }
	next := registry.state; next.Teams = maps.Clone(next.Teams); next.Teams[id] = team
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not update group"); return }
	scimJSON(w, http.StatusOK, scimGroupValue(team, next.Users))
}

func (registry *KeyRegistry) scimDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.scimAdmin(w, r) { return }
	id := r.PathValue("group_id")
	registry.mu.Lock(); defer registry.mu.Unlock()
	if _, exists := registry.state.Teams[id]; !exists { scimError(w, http.StatusNotFound, "Group not found: "+id); return }
	next := registry.state; next.Teams = maps.Clone(next.Teams); delete(next.Teams, id)
	if err := registry.commitState(next); err != nil { scimError(w, http.StatusInternalServerError, "Could not delete group"); return }
	w.Header().Set("Content-Type", "application/scim+json"); w.WriteHeader(http.StatusNoContent)
}

func scimGroupValue(team team, users map[string]internalUser) map[string]any {
	members := make([]map[string]any, 0, len(team.Members)); for _, member := range team.Members { user := users[member.UserID]; members = append(members, map[string]any{"value": member.UserID, "display": firstNonEmpty(user.Email, member.UserID)}) }
	result := map[string]any{"schemas": []string{scimGroupSchema}, "id": team.ID, "displayName": firstNonEmpty(team.Alias, team.ID), "members": members}
	if externalID, ok := team.Metadata["scim_external_id"].(string); ok && externalID != "" { result["externalId"] = externalID }
	return result
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil { scheme = "https" }
	return scheme + "://" + r.Host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
