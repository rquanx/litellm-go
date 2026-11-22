package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type internalUser struct {
	ID       string         `json:"user_id"`
	Alias    string         `json:"user_alias"`
	Email    string         `json:"user_email"`
	Role     string         `json:"user_role"`
	Models   []string       `json:"models"`
	Metadata map[string]any `json:"metadata"`
}

func (registry *KeyRegistry) registerUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /user/new", func(w http.ResponseWriter, r *http.Request) {
		registry.writeUser(w, r, false)
	})
	mux.HandleFunc("POST /user/update", func(w http.ResponseWriter, r *http.Request) {
		registry.writeUser(w, r, true)
	})
	mux.HandleFunc("GET /user/info", registry.userInfo)
	mux.HandleFunc("GET /user/list", registry.listUsers)
	mux.HandleFunc("POST /user/delete", registry.deleteUsers)
}

func (registry *KeyRegistry) writeUser(w http.ResponseWriter, r *http.Request, update bool) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		ID            string          `json:"user_id"`
		Alias         *string         `json:"user_alias"`
		Email         *string         `json:"user_email"`
		Role          *string         `json:"user_role"`
		Models        *[]string       `json:"models"`
		Metadata      *map[string]any `json:"metadata"`
		AutoCreateKey *bool           `json:"auto_create_key"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		(input.Models != nil && !validOwnerModels(*input.Models)) ||
		(input.Role != nil && *input.Role != "internal_user" && *input.Role != "internal_user_viewer") ||
		(update && input.AutoCreateKey != nil) {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported user configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if input.ID == "" && update && input.Email != nil && *input.Email != "" {
		for id, user := range registry.state.Users {
			if strings.EqualFold(user.Email, *input.Email) {
				input.ID = id
				break
			}
		}
	}
	current, exists := registry.state.Users[input.ID]
	if update && !exists {
		modelError(w, http.StatusNotFound, "User not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusBadRequest, "User ID already exists")
		return
	}
	if !exists {
		if input.ID == "" {
			input.ID = newEntityID()
		}
		current = internalUser{ID: input.ID, Role: "internal_user", Models: []string{}, Metadata: map[string]any{}}
	}
	if input.Email != nil && *input.Email != "" {
		for _, user := range registry.state.Users {
			if user.ID != current.ID && strings.EqualFold(user.Email, *input.Email) {
				modelError(w, http.StatusBadRequest, "User email already exists")
				return
			}
		}
	}
	if input.Email != nil {
		current.Email = *input.Email
	}
	if input.Alias != nil {
		current.Alias = *input.Alias
	}
	if input.Role != nil {
		current.Role = *input.Role
	}
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}

	next := registry.state
	next.Users = maps.Clone(next.Users)
	if next.Users == nil {
		next.Users = make(map[string]internalUser)
	}
	next.Users[current.ID] = current
	var token string
	if !update && (input.AutoCreateKey == nil || *input.AutoCreateKey) {
		token = newKeyToken()
		now := time.Now().UTC()
		next.Keys = maps.Clone(next.Keys)
		next.Keys[keyDigest(token)] = virtualKey{UserID: current.ID, Models: []string{}, CreatedAt: &now}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, struct {
			internalUser
			Key string `json:"key,omitempty"`
		}{internalUser: current, Key: token})
	}
}

func (registry *KeyRegistry) userInfo(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	id := r.URL.Query().Get("user_id")
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply a user ID")
		return
	}
	registry.mu.RLock()
	user, exists := registry.state.Users[id]
	keys := make([]keyRecord, 0)
	for digest, key := range registry.state.Keys {
		if key.UserID == id {
			keys = append(keys, keyRecord{Token: digest, virtualKey: key})
		}
	}
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "User not found")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"user_id": id, "user_info": user, "keys": keys, "teams": []team{}})
}

func (registry *KeyRegistry) listUsers(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	query := r.URL.Query()
	for field := range query {
		switch field {
		case "role", "user_ids", "user_email", "page", "page_size", "sort_by", "sort_order":
		default:
			modelError(w, http.StatusBadRequest, "Unsupported user list filter")
			return
		}
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 25)
	column, order := query.Get("sort_by"), query.Get("sort_order")
	if pageErr != nil || sizeErr != nil || size > 100 ||
		(column != "" && column != "user_id" && column != "user_email") ||
		(order != "" && order != "asc" && order != "desc") {
		modelError(w, http.StatusBadRequest, "Invalid user list parameters")
		return
	}
	role, email := query.Get("role"), strings.ToLower(query.Get("user_email"))
	rawIDs := query.Get("user_ids")
	ids := strings.Split(rawIDs, ",")
	registry.mu.RLock()
	users := make([]internalUser, 0, len(registry.state.Users))
	for _, user := range registry.state.Users {
		if (role == "" || role == user.Role) && (rawIDs == "" || slices.Contains(ids, user.ID)) &&
			strings.Contains(strings.ToLower(user.Email), email) {
			users = append(users, user)
		}
	}
	registry.mu.RUnlock()
	slices.SortFunc(users, func(a, b internalUser) int {
		result := strings.Compare(a.ID, b.ID)
		if column == "user_email" && a.Email != b.Email {
			result = strings.Compare(a.Email, b.Email)
		}
		if order == "desc" {
			return -result
		}
		return result
	})
	total := len(users)
	pages := (total + size - 1) / size
	if page > pages {
		users = users[:0]
	} else {
		start := (page - 1) * size
		users = users[start:min(start+size, total)]
	}
	modelJSON(w, http.StatusOK, map[string]any{"users": users, "total": total, "page": page, "page_size": size, "total_pages": pages})
}

func (registry *KeyRegistry) deleteUsers(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"user_ids"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		len(input.IDs) == 0 || slices.Contains(input.IDs, "") {
		modelError(w, http.StatusBadRequest, "Supply user IDs to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Users = maps.Clone(next.Users)
	next.Keys = maps.Clone(next.Keys)
	count := 0
	for _, id := range input.IDs {
		if _, exists := next.Users[id]; exists {
			delete(next.Users, id)
			count++
		}
	}
	for digest, key := range next.Keys {
		if slices.Contains(input.IDs, key.UserID) {
			delete(next.Keys, digest)
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"count": count})
	}
}
