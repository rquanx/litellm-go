package runtime

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCacheResponse = 1 << 20

type InferenceCacheConfig struct {
	Enabled   bool
	OptIn     bool
	TTL       time.Duration
	Namespace string
	Endpoints map[string]bool
}

type InferenceCacheStore interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte, time.Duration) error
}

type memoryInferenceCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	bytes   int
}

type cacheEntry struct {
	key     string
	body    []byte
	expires time.Time
}

func newMemoryInferenceCache() *memoryInferenceCache {
	return &memoryInferenceCache{entries: make(map[string]*list.Element)}
}

func (store *memoryInferenceCache) Get(_ context.Context, key string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	element := store.entries[key]
	if element == nil {
		return nil, nil
	}
	entry := element.Value.(cacheEntry)
	if !time.Now().Before(entry.expires) {
		store.remove(element)
		return nil, nil
	}
	store.order.MoveToFront(element)
	return entry.body, nil
}

func (store *memoryInferenceCache) Set(_ context.Context, key string, body []byte, ttl time.Duration) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if existing := store.entries[key]; existing != nil {
		store.remove(existing)
	}
	entry := cacheEntry{key: key, body: body, expires: time.Now().Add(ttl)}
	store.entries[key] = store.order.PushFront(entry)
	store.bytes += len(body)
	for len(store.entries) > 1024 || store.bytes > 16<<20 {
		store.remove(store.order.Back())
	}
	return nil
}

func (store *memoryInferenceCache) remove(element *list.Element) {
	entry := element.Value.(cacheEntry)
	delete(store.entries, entry.key)
	store.bytes -= len(entry.body)
	store.order.Remove(element)
}

type inferenceCacheRequest struct {
	keyParts    []byte
	ttl         time.Duration
	maxAge      *time.Duration
	read, write bool
}

func (s *Server) prepareInferenceCache(r *http.Request, endpoint, alias string, stream bool, payload *requestPayload) inferenceCacheRequest {
	config := s.config.InferenceCache
	if !config.Enabled || !config.Endpoints[endpoint] {
		return inferenceCacheRequest{}
	}
	directives := make(map[string]any)
	if body, ok := payload.fields["cache"].(map[string]any); ok {
		maps.Copy(directives, body)
	}
	optIn, explicit := payload.fields["caching"].(bool)
	for _, header := range r.Header.Values("Cache-Control") {
		for _, directive := range strings.Split(header, ",") {
			name, value, assigned := strings.Cut(strings.TrimSpace(directive), "=")
			name = strings.ToLower(name)
			if assigned {
				directives[name] = strings.Trim(strings.TrimSpace(value), "\"")
			} else {
				directives[name] = true
			}
		}
	}
	delete(payload.fields, "cache")
	delete(payload.fields, "caching")
	if stream || payload.form != nil || ((config.OptIn || explicit) && !optIn) || r.Header.Get("X-OpenCode-Session") != "" {
		return inferenceCacheRequest{}
	}
	policy := inferenceCacheRequest{ttl: config.TTL, read: directives["no-cache"] != true, write: directives["no-store"] != true}
	if ttl, ok := cacheSeconds(directives["ttl"]); ok {
		policy.ttl = ttl
	}
	for _, name := range []string{"s-maxage", "s-max-age", "max-age"} {
		if age, ok := cacheSeconds(directives[name]); ok {
			policy.maxAge = &age
			break
		}
	}
	namespace, _ := directives["namespace"].(string)
	owner := sha256.Sum256([]byte(ProxyCredential(r)))
	policy.keyParts, _ = json.Marshal([]any{config.Namespace, namespace, owner, endpoint, alias, r.Header.Values("Anthropic-Beta"),
		r.Header.Values("Anthropic-Version"), payload.fields})
	return policy
}

func cacheSeconds(value any) (time.Duration, bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(cacheValue(value)), 64)
	if err != nil || !(seconds >= 0 && seconds <= float64((365*24*time.Hour)/time.Second)) {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

func cacheValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func (policy inferenceCacheRequest) key(candidate Candidate) string {
	if len(policy.keyParts) == 0 || (!policy.read && !policy.write) || candidate.Bedrock != nil ||
		candidate.Vertex != nil || candidate.WatsonX != nil {
		return ""
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	deployment, err := json.Marshal([]any{candidate, key, candidate.AzureADToken})
	if err != nil {
		return ""
	}
	hash := sha256.New()
	hash.Write(policy.keyParts)
	hash.Write(deployment)
	return hex.EncodeToString(hash.Sum(nil))
}

type cachedInference struct {
	Created time.Time       `json:"created"`
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body"`
}

func (s *Server) serveCachedInference(w http.ResponseWriter, r *http.Request, policy inferenceCacheRequest, key string, candidate Candidate, attempts []map[string]any) bool {
	if !policy.read || (policy.maxAge != nil && *policy.maxAge == 0) {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	defer cancel()
	data, err := s.config.InferenceCacheStore.Get(ctx, key)
	var entry cachedInference
	if err != nil || len(data) > maxCacheResponse+1024 || json.Unmarshal(data, &entry) != nil || entry.Status < 200 ||
		entry.Status >= 300 || entry.Created.IsZero() || len(entry.Body) == 0 ||
		(policy.maxAge != nil && time.Since(entry.Created) > *policy.maxAge) {
		return false
	}
	writeRuntimeMeta(w, candidate, len(attempts), attempts)
	if recorder, ok := w.(*callbackResponse); ok {
		recorder.event.CacheHit = true
	}
	w.Header().Set("X-LiteLLM-Cache-Key", key)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	return true
}

func (s *Server) storeInference(r *http.Request, policy inferenceCacheRequest, key string, status int, body []byte) {
	if !policy.write || policy.ttl <= 0 || len(body) > maxCacheResponse || !json.Valid(body) {
		return
	}
	data, err := json.Marshal(cachedInference{Created: time.Now(), Status: status, Body: body})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	defer cancel()
	_ = s.config.InferenceCacheStore.Set(ctx, key, data, policy.ttl)
}
