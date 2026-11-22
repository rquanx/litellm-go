package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const pollingIDPrefix = "litellm_poll_"

type ResponsePollingConfig struct {
	All                bool
	Providers, Native  []string
	TTL                time.Duration
}

type PollingSnapshot struct {
	Owner [32]byte `json:"owner"`
	Model string `json:"model"`
	UpstreamID string `json:"upstream_id,omitempty"`
	Body map[string]any `json:"body"`
}

type PollingStore interface {
	Save(ctx context.Context, id string, snapshot PollingSnapshot, ttl time.Duration, create bool) (bool, error)
	Read(ctx context.Context, id string) (*PollingSnapshot, error)
	Cancel(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}

type responsePolling struct {
	mu      sync.Mutex
	entries map[string]*pollingEntry
}

type pollingEntry struct {
	owner   [32]byte
	model   string
	expires time.Time
	cancel  context.CancelFunc
	timer   *time.Timer
	upstreamID string
	body    map[string]any
}

func (entry *pollingEntry) snapshot() PollingSnapshot {
	return PollingSnapshot{Owner: entry.owner, Model: entry.model, UpstreamID: entry.upstreamID, Body: entry.body}
}

func (s *Server) shouldPollResponse(route Route, input map[string]any) bool {
	config := s.config.ResponsePolling
	return s.polling != nil && inputBool(input, "background") && !slices.Contains(config.Native, route.Alias) &&
		(config.All || slices.ContainsFunc(route.Candidates, func(candidate Candidate) bool {
			return slices.Contains(config.Providers, candidate.Provider)
		}))
}

func (s *Server) startResponsePolling(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	payload, err := decodeJSONPayload(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Expected a response request")
		return
	}
	delete(payload.fields, "background")
	payload.fields["stream"] = true
	metadata := payload.fields["metadata"]
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, _ := json.Marshal(payload.fields)
	ttl := s.config.ResponsePolling.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	now := time.Now()
	id := pollingIDPrefix + newUUID()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ttl)
	initial := map[string]any{
		"id": id, "object": "response", "status": "queued", "created_at": now.Unix(),
		"output": []any{}, "metadata": metadata, "usage": nil, "error": nil,
	}
	entry := &pollingEntry{
		owner: sha256.Sum256([]byte(ProxyCredential(r))), model: model, expires: now.Add(ttl),
		cancel: cancel, body: clone(initial),
	}
	cache := s.polling
	cache.mu.Lock()
	for key, other := range cache.entries {
		if !now.Before(other.expires) {
			delete(cache.entries, key)
			other.cancel()
			other.timer.Stop()
		}
	}
	if len(cache.entries) >= 128 {
		cache.mu.Unlock()
		cancel()
		writeError(w, http.StatusServiceUnavailable, "polling_capacity", "Response polling capacity reached")
		return
	}
	cache.entries[id] = entry
	work := r.Clone(ctx)
	work.Header.Del("Content-Length")
	work.Header.Set("Accept", "text/event-stream")
	work.Header.Set("Content-Type", "application/json")
	work.Body = io.NopCloser(bytes.NewReader(encoded))
	work.ContentLength = int64(len(encoded))
	entry.timer = time.AfterFunc(time.Until(entry.expires), func() {
		cache.mu.Lock()
		if cache.entries[id] == entry {
			delete(cache.entries, id)
		}
		cache.mu.Unlock()
	})
	cache.mu.Unlock()
	if store := s.config.PollingStore; store != nil {
		saved, err := store.Save(r.Context(), id, entry.snapshot(), ttl, true)
		if err != nil || !saved {
			cache.mu.Lock()
			delete(cache.entries, id)
			entry.timer.Stop()
			cache.mu.Unlock()
			cancel()
			writeError(w, http.StatusServiceUnavailable, "polling_unavailable", "Could not initialize response polling")
			return
		}
	}
	go func() {
		defer cancel()
		if store := s.config.PollingStore; store != nil {
			go watchPollingCancellation(ctx, store, id, cancel)
		}
		sink := &pollingWriter{cache: cache, id: id, entry: entry, store: s.config.PollingStore, ctx: ctx,
			headers: make(http.Header), limit: int(min(s.config.MaxBodyBytes, 512<<10))}
		s.ServeHTTP(sink, work)
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.entries[id] == entry && (entry.body["status"] == "queued" || entry.body["status"] == "in_progress") {
			entry.body["status"] = "failed"
			entry.body["error"] = map[string]string{"code": "upstream_error", "message": "The upstream stream ended without a final response"}
			_, _ = sink.persist(0)
		}
	}()
	writeJSON(w, http.StatusOK, initial)
}

func (s *Server) pollingResource(w http.ResponseWriter, r *http.Request, scope []string) bool {
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, responseResourcePrefix), "/")
	if !strings.HasPrefix(id, pollingIDPrefix) {
		return false
	}
	if s.polling == nil {
		writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling response")
		return true
	}
	if s.config.PollingStore != nil {
		s.sharedPollingResource(w, r, id, action, scope)
		return true
	}
	cache := s.polling
	cache.mu.Lock()
	entry := cache.entries[id]
	if entry == nil || !time.Now().Before(entry.expires) || !modelAllowed(scope, entry.model) ||
		(!s.authorized(r) && entry.owner != sha256.Sum256([]byte(ProxyCredential(r)))) {
		cache.mu.Unlock()
		writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling response")
		return true
	}
	var result []byte
	switch {
	case r.Method == http.MethodGet && action == "":
		result, _ = json.Marshal(entry.body)
	case r.Method == http.MethodPost && action == "cancel":
		if entry.body["status"] == "queued" || entry.body["status"] == "in_progress" {
			entry.body["status"] = "cancelled"
			entry.cancel()
		}
		result, _ = json.Marshal(entry.body)
	case r.Method == http.MethodDelete && action == "":
		delete(cache.entries, id)
		entry.cancel()
		entry.timer.Stop()
		result, _ = json.Marshal(map[string]any{"id": id, "object": "response", "deleted": true})
	default:
		cache.mu.Unlock()
		writeError(w, http.StatusNotFound, "response_not_found", "Unknown polling operation")
		return true
	}
	cache.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)
	return true
}

func (s *Server) resolvePollingParent(r *http.Request, id string, scope []string) string {
	if s.polling == nil {
		return ""
	}
	if store := s.config.PollingStore; store != nil {
		entry, err := store.Read(r.Context(), id)
		if err == nil && s.pollingSnapshotAllowed(r, scope, entry) && entry.Body["status"] == "completed" {
			return entry.UpstreamID
		}
		return ""
	}
	s.polling.mu.Lock()
	defer s.polling.mu.Unlock()
	entry := s.polling.entries[id]
	if entry == nil || !time.Now().Before(entry.expires) || !modelAllowed(scope, entry.model) ||
		(!s.authorized(r) && entry.owner != sha256.Sum256([]byte(ProxyCredential(r)))) {
		return ""
	}
	if entry.body["status"] != "completed" {
		return ""
	}
	return entry.upstreamID
}

type pollingWriter struct {
	cache   *responsePolling
	id      string
	entry   *pollingEntry
	headers http.Header
	status  int
	limit   int
	store   PollingStore
	ctx     context.Context
}

func (w *pollingWriter) Header() http.Header     { return w.headers }
func (w *pollingWriter) Flush()                  {}
func (w *pollingWriter) WriteHeader(status int)   { w.status = status }

func (w *pollingWriter) Write(data []byte) (int, error) {
	w.cache.mu.Lock()
	defer w.cache.mu.Unlock()
	entry := w.entry
	if w.cache.entries[w.id] != entry || entry.body["status"] == "cancelled" || entry.body["status"] == "failed" {
		return 0, context.Canceled
	}
	if entry.body["status"] == "completed" || entry.body["status"] == "incomplete" {
		return len(data), nil
	}
	if w.status >= 400 {
		entry.body["status"] = "failed"
		var failure map[string]any
		if len(data) <= w.limit && json.Unmarshal(data, &failure) == nil {
			entry.body["error"] = failure["error"]
		}
		return w.persist(len(data))
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		raw, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		if len(raw) > w.limit {
			return w.tooLarge()
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		applyPollingEvent(entry.body, event)
		if info, ok := event["response"].(map[string]any); ok {
			if original, ok := info["id"].(string); ok {
				entry.upstreamID = original
			}
		}
		entry.body["id"] = w.id
		entry.body["object"] = "response"
		if encoded, _ := json.Marshal(entry.body); len(encoded) > w.limit {
			return w.tooLarge()
		}
	}
	return w.persist(len(data))
}

func (w *pollingWriter) persist(size int) (int, error) {
	if w.store == nil {
		return size, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 2*time.Second)
	defer cancel()
	snapshot := w.entry.snapshot()
	w.cache.mu.Unlock()
	saved, err := w.store.Save(ctx, w.id, snapshot, 0, false)
	w.cache.mu.Lock()
	if err != nil || !saved {
		w.entry.cancel()
		return 0, context.Canceled
	}
	return size, nil
}

func (w *pollingWriter) tooLarge() (int, error) {
	w.entry.body = map[string]any{
		"id": w.id, "object": "response", "status": "failed", "created_at": w.entry.body["created_at"],
		"output": []any{}, "usage": nil,
		"error": map[string]string{"code": "response_too_large", "message": "The response exceeds the polling cache limit"},
	}
	w.entry.cancel()
	_, _ = w.persist(0)
	return 0, errors.New("response exceeds polling cache limit")
}

func applyPollingEvent(state, event map[string]any) {
	if response, ok := event["response"].(map[string]any); ok {
		for field, value := range response {
			if field != "created_at" {
				state[field] = value
			}
		}
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled":
		state["status"] = strings.TrimPrefix(kind, "response.")
		return
	case "error":
		state["status"], state["error"] = "failed", event
		return
	}
	state["status"] = "in_progress"
	switch kind {
	case "response.output_item.added", "response.output_item.done", "response.output_text.delta",
		"response.content_part.added", "response.content_part.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
	default:
		return
	}
	output, _ := state["output"].([]any)
	index, ok := event["output_index"].(float64)
	if !ok {
		if id, exists := event["item_id"].(string); exists {
			for i, raw := range output {
				item, _ := raw.(map[string]any)
				if item["id"] == id {
					index, ok = float64(i), true
					break
				}
			}
		}
	}
	if !ok || index < 0 || index >= 512 || float64(int(index)) != index {
		return
	}
	for len(output) <= int(index) {
		output = append(output, nil)
	}
	if item, ok := event["item"].(map[string]any); ok {
		output[int(index)] = item
	} else {
		item, _ := output[int(index)].(map[string]any)
		if kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done" {
			if item != nil && item["type"] == "function_call" {
				if kind == "response.function_call_arguments.done" {
					item["arguments"] = event["arguments"]
				} else {
					value, _ := item["arguments"].(string)
					delta, _ := event["delta"].(string)
					item["arguments"] = value + delta
				}
			}
			return
		}
		if item == nil {
			item = map[string]any{"type": "message", "role": "assistant"}
		}
		contentIndex, _ := event["content_index"].(float64)
		if contentIndex < 0 || contentIndex >= 512 || float64(int(contentIndex)) != contentIndex {
			return
		}
		content, _ := item["content"].([]any)
		for len(content) <= int(contentIndex) {
			content = append(content, nil)
		}
		if kind == "response.content_part.added" || kind == "response.content_part.done" {
			if part, ok := event["part"].(map[string]any); ok {
				content[int(contentIndex)] = part
				item["content"] = content
				output[int(index)] = item
				state["output"] = output
			}
			return
		}
		part, _ := content[int(contentIndex)].(map[string]any)
		if part == nil {
			part = map[string]any{"type": "output_text", "text": ""}
		}
		text, _ := part["text"].(string)
		delta, _ := event["delta"].(string)
		part["text"] = text + delta
		content[int(contentIndex)] = part
		item["content"] = content
		output[int(index)] = item
	}
	state["output"] = output
}
