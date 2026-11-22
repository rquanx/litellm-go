package runtime

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"
)

type RedisCacheAdmin interface {
	PingCache(context.Context) error
	DeleteCache(context.Context, []string) error
	FlushCache(context.Context) error
	CacheInfo(context.Context) (map[string]any, error)
}

func (s *Server) serveInferenceCacheAdmin(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /cache/ping", "POST /cache/delete", "POST /cache/flushall", "GET /cache/redis/info":
	default:
		return false
	}
	status := func(code int, detail string) { writeJSON(w, code, map[string]string{"detail": detail}) }
	if !s.authorized(r) {
		status(http.StatusUnauthorized, "Runtime token required")
		return true
	}
	if !s.config.InferenceCache.Enabled {
		status(http.StatusServiceUnavailable, "Cache not initialized. litellm.cache is None")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	redis, shared := s.config.InferenceCacheStore.(RedisCacheAdmin)
	if r.URL.Path == "/cache/ping" {
		result := map[string]any{"status": "healthy", "cache_type": "local", "litellm_cache_params": "{}"}
		if shared {
			if err := redis.PingCache(ctx); err != nil {
				status(http.StatusServiceUnavailable, "Could not reach Redis cache")
				return true
			}
			result["cache_type"] = "redis"
			result["ping_response"] = true
			result["set_cache_response"] = "success"
		}
		writeJSON(w, http.StatusOK, result)
		return true
	}
	if !shared {
		status(http.StatusInternalServerError, "This operation requires a Redis cache")
		return true
	}
	var err error
	switch r.URL.Path {
	case "/cache/delete":
		var request struct {
			Keys []string `json:"keys"`
		}
		err = decodeBody(r, &request, min(s.config.MaxBodyBytes, 64<<10))
		if err != nil || len(request.Keys) == 0 || len(request.Keys) > 512 {
			status(http.StatusBadRequest, "Provide between 1 and 512 cache keys")
			return true
		}
		for _, key := range request.Keys {
			if len(key) != 64 {
				status(http.StatusBadRequest, "Invalid cache key")
				return true
			}
			if _, err := hex.DecodeString(key); err != nil {
				status(http.StatusBadRequest, "Invalid cache key")
				return true
			}
		}
		err = redis.DeleteCache(ctx, request.Keys)
	case "/cache/flushall":
		err = redis.FlushCache(ctx)
	case "/cache/redis/info":
		var info map[string]any
		info, err = redis.CacheInfo(ctx)
		if err == nil {
			writeJSON(w, http.StatusOK, info)
			return true
		}
	}
	if err != nil {
		status(http.StatusServiceUnavailable, "Redis cache operation failed")
	} else {
		writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
	}
	return true
}
