package proxyconfig

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

func keyExpiry(now time.Time, duration string) (*time.Time, error) {
	if duration == "" || duration == "-1" {
		return nil, nil
	}
	if strings.HasSuffix(duration, "mo") {
		months, err := strconv.Atoi(strings.TrimSuffix(duration, "mo"))
		if err != nil || months < 0 || months > (9999-now.Year())*12+12-int(now.Month()) {
			return nil, errors.New("invalid key duration")
		}
		target := now.AddDate(0, months, 1-now.Day())
		lastDay := target.AddDate(0, 1, -1).Day()
		expires := target.AddDate(0, 0, min(now.Day(), lastDay)-1)
		return &expires, nil
	}
	if strings.HasSuffix(duration, "d") || strings.HasSuffix(duration, "w") {
		count, err := strconv.Atoi(duration[:len(duration)-1])
		if err != nil || count < 0 || count > 3000000 {
			return nil, errors.New("invalid key duration")
		}
		days := count
		if strings.HasSuffix(duration, "w") {
			days *= 7
		}
		expires := now.AddDate(0, 0, days)
		if expires.Year() > 9999 {
			return nil, errors.New("invalid key duration")
		}
		return &expires, nil
	}
	interval, err := time.ParseDuration(duration)
	if err != nil || interval < 0 {
		return nil, errors.New("invalid key duration")
	}
	expires := now.Add(interval)
	return &expires, nil
}

func (registry *KeyRegistry) resolveKey(reference string) string {
	if len(reference) == 64 {
		if _, err := hex.DecodeString(reference); err == nil {
			normalized := strings.ToLower(reference)
			if _, exists := registry.state.Keys[normalized]; exists {
				return normalized
			}
		}
	}
	return keyDigest(reference)
}

func (registry *KeyRegistry) validKey(w http.ResponseWriter, key virtualKey) bool {
	if key.UserID != "" {
		if _, exists := registry.state.Users[key.UserID]; !exists {
			modelError(w, http.StatusBadRequest, "User not found")
			return false
		}
	}
	if key.OrgID != "" {
		if _, exists := registry.state.Organizations[key.OrgID]; !exists {
			modelError(w, http.StatusBadRequest, "Organization not found")
			return false
		}
	}
	if key.TeamID != "" {
		team, exists := registry.state.Teams[key.TeamID]
		if !exists {
			modelError(w, http.StatusBadRequest, "Team not found")
			return false
		}
		if key.OrgID != "" && team.OrgID != key.OrgID {
			modelError(w, http.StatusBadRequest, "Team belongs to a different organization")
			return false
		}
	} else if slices.Contains(key.Models, "all-team-models") {
		modelError(w, http.StatusBadRequest, "Team model scope requires a team ID")
		return false
	}
	if slices.Contains(key.Models, "all-proxy-models") {
		modelError(w, http.StatusBadRequest, "Unsupported dynamic proxy model scope")
		return false
	}
	return true
}

func (registry *KeyRegistry) updateKey(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Key      string          `json:"key"`
		Alias    *string         `json:"key_alias"`
		Models   *[]string       `json:"models"`
		TeamID   *string         `json:"team_id"`
		OrgID    *string         `json:"organization_id"`
		UserID   *string         `json:"user_id"`
		Blocked  *bool           `json:"blocked"`
		Metadata *map[string]any `json:"metadata"`
		Duration json.RawMessage `json:"duration"`
		ObjectPermission json.RawMessage `json:"object_permission"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.Key == "" {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key update")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	digest := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[digest]
	if !exists || key.RotationTarget != "" {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	if input.Alias != nil {
		key.Alias = *input.Alias
	}
	if input.Models != nil {
		key.Models = *input.Models
	}
	if input.TeamID != nil {
		key.TeamID = *input.TeamID
	}
	if input.OrgID != nil {
		key.OrgID = *input.OrgID
	}
	if input.UserID != nil {
		key.UserID = *input.UserID
	}
	if input.Blocked != nil {
		key.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		key.Metadata = *input.Metadata
	}
	if input.ObjectPermission != nil {
		var permission *searchPermission
		if err := decodeModelJSON(strings.NewReader(string(input.ObjectPermission)), &permission); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid or unsupported object permission")
			return
		}
		key.ObjectPermission = permission
	}
	if input.Duration != nil {
		var duration string
		if err := json.Unmarshal(input.Duration, &duration); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid key duration")
			return
		}
		expires, err := keyExpiry(time.Now().UTC(), duration)
		if err != nil {
			modelError(w, http.StatusBadRequest, "Invalid key duration")
			return
		}
		key.Expires = expires
	}
	if registry.validKey(w, key) {
		registry.saveKey(w, digest, key, true)
	}
}

func (registry *KeyRegistry) setKeyBlocked(w http.ResponseWriter, r *http.Request, blocked bool) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.Key == "" {
		modelError(w, http.StatusBadRequest, "Supply a virtual key")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	digest := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[digest]
	if !exists {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	key.Blocked = blocked
	registry.saveKey(w, digest, key, false)
}

type keyRecord struct {
	Token string `json:"token"`
	virtualKey
}

func (registry *KeyRegistry) saveKey(w http.ResponseWriter, digest string, key virtualKey, update bool) {
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	next.Keys[digest] = key
	if registry.apply(w, next) {
		if update {
			modelJSON(w, http.StatusOK, struct {
				Key string `json:"key"`
				keyRecord
			}{Key: digest, keyRecord: keyRecord{Token: digest, virtualKey: key}})
			return
		}
		modelJSON(w, http.StatusOK, keyRecord{Token: digest, virtualKey: key})
	}
}

func keyListInt(query url.Values, name string, fallback int) (int, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(query.Get(name))
	if err != nil || value <= 0 {
		return 0, errors.New("invalid pagination parameter")
	}
	return value, nil
}

func (registry *KeyRegistry) listKeys(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	query := r.URL.Query()
	for field := range query {
		switch field {
		case "page", "size", "key_hash", "team_id", "organization_id", "user_id", "key_alias", "return_full_object", "sort_by", "sort_order":
		default:
			modelError(w, http.StatusBadRequest, "Unsupported key list filter")
			return
		}
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "size", 10)
	full, fullErr := strconv.ParseBool(query.Get("return_full_object"))
	if !query.Has("return_full_object") {
		fullErr = nil
	}
	order, column := query.Get("sort_order"), query.Get("sort_by")
	if pageErr != nil || sizeErr != nil || fullErr != nil || size > 100 ||
		(order != "" && order != "asc" && order != "desc") ||
		(column != "" && column != "created_at" && column != "key_alias") {
		modelError(w, http.StatusBadRequest, "Invalid key list parameters")
		return
	}
	teamID, digest, alias := query.Get("team_id"), query.Get("key_hash"), strings.ToLower(query.Get("key_alias"))
	orgID := query.Get("organization_id")
	userID := strings.ToLower(query.Get("user_id"))
	registry.mu.RLock()
	keys := make([]keyRecord, 0, len(registry.state.Keys))
	for token, key := range registry.state.Keys {
		if (orgID == "" || key.OrgID == orgID || registry.state.Teams[key.TeamID].OrgID == orgID) &&
			(teamID == "" || key.TeamID == teamID) && (digest == "" || token == digest) &&
			strings.Contains(strings.ToLower(key.UserID), userID) &&
			strings.Contains(strings.ToLower(key.Alias), alias) {
			keys = append(keys, keyRecord{Token: token, virtualKey: key})
		}
	}
	registry.mu.RUnlock()
	slices.SortFunc(keys, func(a, b keyRecord) int {
		byColumn := cmp.Compare(a.Alias, b.Alias)
		if column != "key_alias" {
			var left, right time.Time
			if a.CreatedAt != nil {
				left = *a.CreatedAt
			}
			if b.CreatedAt != nil {
				right = *b.CreatedAt
			}
			byColumn = left.Compare(right)
		}
		if byColumn == 0 {
			byColumn = cmp.Compare(a.Token, b.Token)
		}
		if order != "asc" {
			return -byColumn
		}
		return byColumn
	})
	total := len(keys)
	pages := (total + size - 1) / size
	if page > pages {
		keys = keys[:0]
	} else {
		start := (page - 1) * size
		keys = keys[start:min(start+size, total)]
	}
	var result any = keys
	if !full {
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			ids = append(ids, key.Token)
		}
		result = ids
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"keys": result, "total_count": total, "current_page": page, "total_pages": pages, "size": size,
	})
}
