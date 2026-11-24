package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type cacheSettingsManager struct {
	mu sync.Mutex
	config Config
	base Config
	store *postgresState
	server *runtime.Server
	activeRevision int64
	managed *redisInferenceCache
}

func (config Config) WithCacheSettings(next http.Handler, keys *KeyRegistry, db *sql.DB, server *runtime.Server) (http.Handler, error) {
	manager := &cacheSettingsManager{config: config, base: config, server: server}
	if db != nil {
		manager.store = postgresModels(db, keys.master)
		manager.store.name = "inference-cache"
		if err := manager.refresh(); err != nil {
			return nil, err
		}
		go func() {
			ticker := time.NewTicker(10*time.Second)
			defer ticker.Stop()
			for range ticker.C {
				if err := manager.refresh(); err != nil {
					log.Print("could not refresh managed cache settings")
				}
			}
		}()
	}
	return manager.Handler(next, keys), nil
}

func (manager *cacheSettingsManager) currentConfig() Config {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.config
}

func (manager *cacheSettingsManager) refresh() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var settings cacheConnectionSettings
	changed, err := manager.store.loadAfter(ctx, &settings, manager.activeRevision)
	if err != nil || !changed {
		return err
	}
	config := manager.base
	var backend *redisInferenceCache
	if manager.store.revision == 0 && config.InferenceCacheRedis != nil {
		backend, err = OpenRedisInferenceCache(ctx, config.InferenceCacheRedis, config.InferenceCache.Namespace)
		if err != nil {
			return err
		}
	}
	if manager.store.revision != 0 {
		config, backend, err = settings.prepare(ctx)
		if err != nil {
			return err
		}
	}
	manager.publish(config, backend)
	return nil
}

func (manager *cacheSettingsManager) publish(config Config, backend *redisInferenceCache) {
	previous := manager.managed
	manager.config = config
	manager.managed = backend
	manager.activeRevision = manager.store.revision
	var store runtime.InferenceCacheStore
	if backend != nil {
		store = backend
	}
	manager.server.ReplaceInferenceCache(config.InferenceCache, store)
	if previous != nil {
		previous.Close()
	}
}

func (input cacheConnectionSettings) prepare(ctx context.Context) (Config, *redisInferenceCache, error) {
	var config Config
	switch input.Type {
	case "local":
	case "redis":
		if input.redisTopology() == "node" {
			options, err := input.options()
			if err != nil {
				return config, nil, err
			}
			options.MaxRetries = -1
			config.InferenceCacheRedis = options
		} else {
			config.inferenceCacheTopology = &input
		}
	default:
		return config, nil, errors.New("Cache type must be local or redis")
	}
	params := inferenceCacheParams{Type: input.Type, Mode: input.Mode, TTL: input.TTL, MemoryTTL: input.MemoryTTL,
		RedisTTL: input.RedisTTL, Namespace: input.Namespace, CallTypes: input.CallTypes}
	if err := params.apply(&config); err != nil {
		return Config{}, nil, err
	}
	if config.inferenceCacheTopology != nil {
		client, err := input.newRedisClient()
		if err != nil {
			return Config{}, nil, err
		}
		backend, err := openInferenceCacheClient(ctx, client, input.Namespace)
		return config, backend, err
	}
	if config.InferenceCacheRedis == nil {
		return config, nil, nil
	}
	backend, err := OpenRedisInferenceCache(ctx, config.InferenceCacheRedis, input.Namespace)
	return config, backend, err
}

func (manager *cacheSettingsManager) update(w http.ResponseWriter, r *http.Request) {
	if manager.store == nil {
		modelError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	var request struct {
		Settings cacheConnectionSettings `json:"cache_settings"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &request) != nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid cache settings")
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	input := request.Settings
	if input.Password == "****" {
		current := manager.config.InferenceCacheRedis
		target, err := input.options()
		if input.URL != "" || err != nil || current == nil || target.Addr != current.Addr || target.DB != current.DB ||
			target.Username != current.Username || (target.TLSConfig == nil) != (current.TLSConfig == nil) {
			modelError(w, http.StatusBadRequest, "Supply credentials for the new cache connection")
			return
		}
		input.Password = current.Password
	}
	config, backend, err := input.prepare(ctx)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	manager.store.revision = manager.activeRevision
	var audit *managementAuditEvent
	if manager.base.StoreAuditLogs {
		audit = newSettingsAudit(r, "LiteLLM_CacheConfig", "cache_config", publicCacheSettings(manager.config), publicCacheSettings(config))
	}
	if err := manager.store.saveWithAudit(input, audit); err != nil {
		if backend != nil {
			backend.Close()
		}
		if errors.Is(err, errConcurrentState) {
			modelError(w, http.StatusConflict, "Proxy cache settings were changed by another process")
		} else {
			modelError(w, http.StatusInternalServerError, "Could not save cache settings")
		}
		return
	}
	manager.publish(config, backend)
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Cache settings updated successfully",
		"settings": publicCacheSettings(config)})
}

