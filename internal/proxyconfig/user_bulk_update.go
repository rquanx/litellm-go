package proxyconfig

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
)

type userBulkResult struct {
	ID string `json:"user_id"`
	Email string `json:"user_email"`
	Success bool `json:"success"`
	Updated *internalUser `json:"updated_user,omitempty"`
	Error string `json:"error,omitempty"`
}

func (registry *KeyRegistry) bulkUpdateUsers(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Users []userWriteInput `json:"users"`
		All bool `json:"all_users"`
		Updates *userWriteInput `json:"user_updates"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		(input.All && (input.Updates == nil || len(input.Users) != 0)) ||
		(!input.All && (input.Updates != nil || len(input.Users) == 0 || len(input.Users) > 500)) {
		modelError(w, http.StatusBadRequest, "Supply up to 500 users or a set of changes for all users")
		return
	}
	results := []userBulkResult{}
	if input.All {
		var err error
		results, err = registry.updateAllUsers(*input.Updates)
		if err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		for _, user := range input.Users {
			results = append(results, registry.updateOneUser(user))
		}
	}
	successes := 0
	for _, item := range results {
		if item.Success {
			successes++
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"results": results, "total_requested": len(results),
		"successful_updates": successes, "failed_updates": len(results) - successes})
}

func (registry *KeyRegistry) updateAllUsers(input userWriteInput) ([]userBulkResult, error) {
	input.ID, input.Email = "", nil
	hash, err := bulkUserPassword(input)
	if err != nil {
		return nil, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.state.Users) == 0 || len(registry.state.Users) > 500 {
		return nil, errors.New("Bulk update requires between 1 and 500 users")
	}
	next := registry.state
	next.Users = maps.Clone(next.Users)
	results := make([]userBulkResult, 0, len(next.Users))
	for _, id := range slices.Sorted(maps.Keys(next.Users)) {
		user := next.Users[id]
		if err := registry.applyUserFields(&user, input); err != nil {
			return nil, err
		}
		next.Users[id] = user
		if input.Password != nil {
			next.setDashboardPassword(id, hash)
		}
		results = append(results, updatedBulkUser(user))
	}
	if err := registry.commitState(next); err != nil {
		return nil, errors.New("Could not save bulk user changes")
	}
	return results, nil
}

func (registry *KeyRegistry) updateOneUser(input userWriteInput) userBulkResult {
	result := userBulkResult{ID: input.ID}
	if input.Email != nil {
		result.Email = *input.Email
	}
	hash, err := bulkUserPassword(input)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if input.ID == "" && result.Email != "" {
		for _, user := range registry.state.Users {
			if strings.EqualFold(user.Email, result.Email) {
				input.ID = user.ID
				break
			}
		}
	}
	user, exists := registry.state.Users[input.ID]
	if !exists {
		result.Error = "User not found"
		return result
	}
	if err := registry.applyUserFields(&user, input); err != nil {
		result.Error = err.Error()
		return result
	}
	next := registry.state
	next.Users = maps.Clone(next.Users)
	next.Users[user.ID] = user
	if input.Password != nil {
		next.setDashboardPassword(user.ID, hash)
	}
	if err := registry.commitState(next); err != nil {
		result.Error = "Could not save user changes"
		return result
	}
	return updatedBulkUser(user)
}

func bulkUserPassword(input userWriteInput) (string, error) {
	if !input.valid(true) {
		return "", errors.New("Invalid or unsupported user configuration")
	}
	if input.Password == nil || *input.Password == "" {
		return "", nil
	}
	return hashDashboardPassword(*input.Password)
}

func updatedBulkUser(user internalUser) userBulkResult {
	user.ImportedSpend = nil
	return userBulkResult{ID: user.ID, Email: user.Email, Success: true, Updated: &user}
}
