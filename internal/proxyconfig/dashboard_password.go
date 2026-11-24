package proxyconfig

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

func (state *accessState) setDashboardPassword(userID, hash string) {
	passwords := make(map[string]string, len(state.Passwords)+1)
	maps.Copy(passwords, state.Passwords)
	if hash == "" {
		delete(passwords, userID)
	} else {
		passwords[userID] = hash
	}
	state.Passwords = passwords
	state.Keys = maps.Clone(state.Keys)
	for digest, key := range state.Keys {
		if key.UserID == userID && key.Alias == "ui-password:"+userID {
			delete(state.Keys, digest)
		}
	}
}

func hashDashboardPassword(password string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt[:], 16384, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return "scrypt:" + base64.StdEncoding.EncodeToString(append(salt[:], key...)), nil
}

func validDashboardPasswordHash(stored string) bool {
	if encoded, ok := strings.CutPrefix(stored, "scrypt:"); ok {
		data, err := base64.StdEncoding.DecodeString(encoded)
		return err == nil && len(data) == 48
	}
	data, err := hex.DecodeString(stored)
	return err == nil && len(data) == sha256.Size && stored == strings.ToLower(stored)
}

func verifyDashboardPassword(password, stored string) bool {
	if !validDashboardPasswordHash(stored) {
		return false
	}
	if encoded, ok := strings.CutPrefix(stored, "scrypt:"); ok {
		data, _ := base64.StdEncoding.DecodeString(encoded)
		key, err := scrypt.Key([]byte(password), data[:16], 16384, 8, 1, 32)
		return err == nil && subtle.ConstantTimeCompare(key, data[16:]) == 1
	}
	sum := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(stored)) == 1
}

func (login dashboardLogin) issuePasswordSession(w http.ResponseWriter, request *http.Request, username, password string) (string, time.Time, bool) {
	keys := login.keys
	if _, postgres := keys.store.(*postgresState); postgres {
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		err := keys.refresh(ctx)
		cancel()
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read Dashboard account state")
			return "", time.Time{}, false
		}
	}
	keys.mu.RLock()
	var user internalUser
	for _, candidate := range keys.state.Users {
		if candidate.Email != "" && strings.EqualFold(candidate.Email, username) {
			if user.ID != "" {
				user = internalUser{}
				break
			}
			user = candidate
		}
	}
	stored := keys.state.Passwords[user.ID]
	keys.mu.RUnlock()
	if stored == "" || !dashboardUserRole(user.Role) {
		modelError(w, http.StatusUnauthorized, "Invalid username or password")
		return "", time.Time{}, false
	}
	select {
	case keys.passwordSlots <- struct{}{}:
		defer func() { <-keys.passwordSlots }()
	default:
		modelError(w, http.StatusTooManyRequests, "Too many concurrent password checks")
		return "", time.Time{}, false
	}
	if !verifyDashboardPassword(password, stored) {
		modelError(w, http.StatusUnauthorized, "Invalid username or password")
		return "", time.Time{}, false
	}
	var upgraded string
	if !strings.HasPrefix(stored, "scrypt:") {
		var err error
		upgraded, err = hashDashboardPassword(password)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not upgrade password hash")
			return "", time.Time{}, false
		}
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	current := keys.state.Users[user.ID]
	if current.Email != user.Email || !dashboardUserRole(current.Role) || keys.state.Passwords[user.ID] != stored {
		modelError(w, http.StatusUnauthorized, "Invalid username or password")
		return "", time.Time{}, false
	}
	next := keys.state
	if upgraded != "" {
		next.Passwords = maps.Clone(next.Passwords)
		next.Passwords[user.ID] = upgraded
	}
	return keys.issueUserDashboardSession(w, next, current, "ui-password:", "username_password")
}
