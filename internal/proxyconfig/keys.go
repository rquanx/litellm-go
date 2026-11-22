package proxyconfig

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"os"
	"sync"
	"time"
)

type virtualKey struct {
	Alias   string     `json:"key_alias,omitempty"`
	Models  []string   `json:"models"`
	Expires *time.Time `json:"expires,omitempty"`
}

type KeyRegistry struct {
	master string
	path   string
	mu     sync.RWMutex
	keys   map[string]virtualKey
}

func LoadKeys(master, path string) (*KeyRegistry, error) {
	if master == "" {
		return nil, errors.New("virtual keys require a proxy master key")
	}
	registry := &KeyRegistry{master: master, path: path, keys: make(map[string]virtualKey)}
	if path == "" {
		return registry, nil
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := decodeModelJSON(file, &registry.keys); err != nil || registry.keys == nil {
		return nil, errors.New("invalid virtual key state")
	}
	return registry, nil
}

func (registry *KeyRegistry) Authorize(token string) ([]string, bool) {
	if token == "" {
		return nil, false
	}
	registry.mu.RLock()
	key, exists := registry.keys[keyDigest(token)]
	registry.mu.RUnlock()
	return key.Models, exists && (key.Expires == nil || time.Now().Before(*key.Expires))
}

func (registry *KeyRegistry) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /key/generate", registry.generate)
	mux.HandleFunc("GET /key/info", registry.info)
	mux.HandleFunc("POST /key/delete", registry.remove)
	mux.Handle("/", next)
	return mux
}

func (registry *KeyRegistry) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte("Bearer "+registry.master), []byte(r.Header.Get("Authorization"))) == 1 {
		return true
	}
	modelError(w, http.StatusUnauthorized, "Proxy master key required")
	return false
}

func (registry *KeyRegistry) generate(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Alias    string   `json:"key_alias"`
		Duration string   `json:"duration"`
		Models   []string `json:"models"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key configuration")
		return
	}
	key := virtualKey{Alias: input.Alias, Models: append([]string{}, input.Models...)}
	if input.Duration != "" {
		duration, err := time.ParseDuration(input.Duration)
		if err != nil || duration <= 0 {
			modelError(w, http.StatusBadRequest, "Invalid key duration")
			return
		}
		expires := time.Now().Add(duration).UTC()
		key.Expires = &expires
	}
	var random [32]byte
	rand.Read(random[:])
	token := "sk-" + hex.EncodeToString(random[:])
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := maps.Clone(registry.keys)
	digest := keyDigest(token)
	next[digest] = key
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{"key": token, "token_id": digest, "expires": key.Expires, "key_alias": key.Alias})
	}
}

func (registry *KeyRegistry) info(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	token := r.URL.Query().Get("key")
	registry.mu.RLock()
	key, exists := registry.keys[keyDigest(token)]
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"key": keyDigest(token), "info": key})
}

func (registry *KeyRegistry) remove(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Keys []string `json:"keys"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.Keys) == 0 {
		modelError(w, http.StatusBadRequest, "Supply virtual keys to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := maps.Clone(registry.keys)
	removed := 0
	for _, token := range input.Keys {
		digest := keyDigest(token)
		if _, exists := next[digest]; exists {
			delete(next, digest)
			removed++
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"deleted_keys": removed})
	}
}

func (registry *KeyRegistry) apply(w http.ResponseWriter, next map[string]virtualKey) bool {
	if registry.path != "" && saveJSONState(registry.path, next) != nil {
		modelError(w, http.StatusInternalServerError, "Could not save virtual key state")
		return false
	}
	registry.keys = next
	return true
}

func keyDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
