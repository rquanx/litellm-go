package proxyconfig

import (
	"context"
	"crypto/tls"
	"errors"
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed cache_settings_fields.json
var cacheSettingsFields json.RawMessage

func (manager *cacheSettingsManager) Handler(next http.Handler, keys *KeyRegistry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cache/settings", func(w http.ResponseWriter, r *http.Request) {
		if !keys.requireAdminRead(w, r) {
			return
		}
		var response struct {
			Fields []map[string]any `json:"fields"`
			CurrentValues map[string]any `json:"current_values"`
			RedisTypes map[string]string `json:"redis_type_descriptions"`
		}
		if err := json.Unmarshal(cacheSettingsFields, &response); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read cache settings catalog")
			return
		}
		response.CurrentValues = publicCacheSettings(manager.currentConfig())
		for _, field := range response.Fields {
			name, _ := field["field_name"].(string)
			if value, present := response.CurrentValues[name]; present {
				field["field_value"] = value
			}
		}
		modelJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("POST /cache/settings/test", func(w http.ResponseWriter, r *http.Request) {
		if keys.requireMaster(w, r) {
			testCacheSettings(w, r)
		}
	})
	mux.HandleFunc("POST /cache/settings", func(w http.ResponseWriter, r *http.Request) {
		if keys.requireMaster(w, r) {
			manager.update(w, r)
		}
	})
	mux.Handle("/", next)
	return mux
}

func publicCacheSettings(config Config) map[string]any {
	values := map[string]any{}
	if !config.InferenceCache.Enabled {
		return values
	}
	values["type"] = "local"
	values["namespace"] = config.InferenceCache.Namespace
	values["ttl"] = config.InferenceCache.TTL.Seconds()
	values["mode"] = "default_on"
	if config.InferenceCache.OptIn {
		values["mode"] = "default_off"
	}
	if options := config.InferenceCacheRedis; options != nil {
		values["type"] = "redis"
		values["redis_type"] = "node"
		if host, port, err := net.SplitHostPort(options.Addr); err == nil {
			values["host"], values["port"] = host, port
		}
		if options.Password != "" {
			values["password"] = "****"
		}
		values["db"] = options.DB
		values["ssl"] = options.TLSConfig != nil
		if options.PoolSize > 0 {
			values["max_connections"] = options.PoolSize
		}
		if options.Username != "" {
			values["username"] = options.Username
		}
	}
	if topology := config.inferenceCacheTopology; topology != nil {
		values["type"], values["redis_type"] = "redis", topology.redisTopology()
		values["ssl"], values["db"], values["max_connections"] = topology.SSL, topology.DB, topology.PoolSize
		values["username"] = topology.Username
		if topology.Password != "" {
			values["password"] = "****"
		}
		if topology.redisTopology() == "cluster" {
			values["redis_startup_nodes"] = topology.StartupNodes
		} else {
			values["sentinel_nodes"], values["service_name"] = topology.SentinelNodes, topology.ServiceName
			if topology.SentinelPassword != "" {
				values["sentinel_password"] = "****"
			}
		}
	}
	return values
}

type cacheConnectionSettings struct {
	Type string `json:"type"`
	RedisType string `json:"redis_type"`
	StartupNodes []cacheRedisNode `json:"redis_startup_nodes"`
	SentinelNodes [][]json.RawMessage `json:"sentinel_nodes"`
	SentinelPassword string `json:"sentinel_password"`
	ServiceName string `json:"service_name"`
	URL string `json:"url"`
	Host string `json:"host"`
	Port json.RawMessage `json:"port"`
	Password string `json:"password"`
	Username string `json:"username"`
	DB int `json:"db"`
	SSL bool `json:"ssl"`
	PoolSize int `json:"max_connections"`
	Mode string `json:"mode"`
	TTL *float64 `json:"ttl"`
	MemoryTTL *float64 `json:"default_in_memory_ttl"`
	RedisTTL *float64 `json:"default_redis_ttl"`
	Namespace string `json:"namespace"`
	CallTypes []string `json:"supported_call_types"`
}

func (input cacheConnectionSettings) options() (*redis.Options, error) {
	if input.Type != "redis" || (input.RedisType != "" && input.RedisType != "node") {
		return nil, errors.New("Only standalone Redis cache connections are supported")
	}
	if input.PoolSize < 0 {
		return nil, errors.New("Invalid maximum Redis connections")
	}
	var options *redis.Options
	host := input.Host
	if input.URL != "" {
		var err error
		options, err = redis.ParseURL(input.URL)
		if err != nil || options.Network != "tcp" {
			return nil, errors.New("Invalid Redis connection URL")
		}
		host, _, _ = net.SplitHostPort(options.Addr)
	} else {
		port := "6379"
		if len(input.Port) > 0 && string(input.Port) != "null" {
			port = strings.Trim(string(input.Port), "\\\"")
		}
		number, err := strconv.Atoi(port)
		if input.Host == "" || err != nil || number < 1 || number > 65535 || input.DB < 0 {
			return nil, errors.New("Invalid Redis host, port or database")
		}
		options = &redis.Options{Addr: net.JoinHostPort(input.Host, port), Username: input.Username, Password: input.Password, DB: input.DB}
	}
	if input.SSL && options.TLSConfig == nil {
		options.TLSConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	if input.PoolSize > 0 {
		options.PoolSize = input.PoolSize
	}
	return options, nil
}

func testCacheSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Settings cacheConnectionSettings `json:"cache_settings"`
	}
	fail := func(message string) {
		modelJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": message, "error": nil})
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &request); err != nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid cache settings")
		return
	}
	client, err := request.Settings.newRedisClient()
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		fail("Could not connect to Redis")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Redis connection successful", "error": nil})
}
