package proxyconfig

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

type inferenceCacheParams struct {
	Type      string   `yaml:"type"`
	Mode      string   `yaml:"mode"`
	TTL       *float64 `yaml:"ttl"`
	MemoryTTL *float64 `yaml:"default_in_memory_ttl"`
	RedisTTL  *float64 `yaml:"default_redis_ttl"`
	Namespace string   `yaml:"namespace"`
	CallTypes []string `yaml:"supported_call_types"`
	RedisURL  string   `yaml:"url"`
	Host      string   `yaml:"host"`
	Port      string   `yaml:"port"`
	Password  string   `yaml:"password"`
}

func (params inferenceCacheParams) apply(config *Config) error {
	cache := runtime.InferenceCacheConfig{Enabled: true, Namespace: params.Namespace, TTL: time.Hour}
	switch params.Mode {
	case "default_off":
		cache.OptIn = true
	case "", "default_on":
	default:
		return errors.New("unsupported inference cache mode")
	}
	ttl := params.TTL
	switch params.Type {
	case "local":
		if ttl == nil {
			ttl = params.MemoryTTL
		}
	case "", "redis":
		if ttl == nil {
			ttl = params.RedisTTL
		}
		options, err := params.redisOptions()
		if err != nil {
			return err
		}
		config.InferenceCacheRedis = options
	default:
		return errors.New("unsupported inference cache type")
	}
	if ttl != nil {
		if math.IsNaN(*ttl) || *ttl < 0 || *ttl > float64((365*24*time.Hour)/time.Second) {
			return errors.New("invalid inference cache TTL")
		}
		cache.TTL = time.Duration(*ttl * float64(time.Second))
	}
	cache.Endpoints = make(map[string]bool)
	callTypes := params.CallTypes
	if callTypes == nil {
		callTypes = []string{"acompletion", "aembedding", "atext_completion", "arerank", "atranscription", "aresponses"}
	}
	for _, call := range callTypes {
		switch call {
		case "acompletion":
			cache.Endpoints["chat-completions"] = true
		case "aembedding":
			cache.Endpoints["embeddings"] = true
		case "atext_completion":
			cache.Endpoints["completions"] = true
		case "arerank":
			cache.Endpoints["rerank"] = true
		case "atranscription":
			cache.Endpoints["transcription"] = true
		case "aresponses":
			cache.Endpoints["responses"] = true
		case "completion", "embedding", "text_completion", "rerank", "transcription", "responses":
			// The HTTP proxy dispatches asynchronous call types.
		default:
			return errors.New("unsupported inference cache call type")
		}
	}
	config.InferenceCache = cache
	return nil
}

func (params inferenceCacheParams) redisOptions() (*redis.Options, error) {
	address, err := resolveEnv(params.RedisURL)
	if err != nil {
		return nil, err
	}
	if address == "" && params.Host == "" {
		address = os.Getenv("REDIS_URL")
	}
	if address != "" {
		options, err := redis.ParseURL(address)
		if err != nil {
			return nil, errors.New("invalid inference cache Redis URL")
		}
		return options, nil
	}
	values := []*string{&params.Host, &params.Port, &params.Password}
	for index, env := range []string{"REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD"} {
		value, err := resolveEnv(*values[index])
		if err != nil {
			return nil, err
		}
		if value == "" {
			value = os.Getenv(env)
		}
		*values[index] = value
	}
	if params.Host == "" {
		return nil, errors.New("inference cache Redis host or URL is required")
	}
	if params.Port == "" {
		params.Port = "6379"
	}
	return &redis.Options{Addr: net.JoinHostPort(params.Host, params.Port), Password: params.Password}, nil
}

type redisInferenceCache struct {
	*redis.Client
	prefix string
}

func OpenRedisInferenceCache(ctx context.Context, options *redis.Options, namespace string) (*redisInferenceCache, error) {
	configured := *options
	configured.ContextTimeoutEnabled = true
	client := redis.NewClient(&configured)
	if client.Ping(ctx).Err() != nil {
		client.Close()
		return nil, errors.New("could not connect to inference cache Redis")
	}
	return &redisInferenceCache{Client: client, prefix: fmt.Sprintf("minato:inference:%x:", sha256.Sum256([]byte(namespace)))}, nil
}

func (store *redisInferenceCache) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := store.Client.Get(ctx, store.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return data, err
}

func (store *redisInferenceCache) Set(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	return store.Client.Set(ctx, store.prefix+key, data, max(ttl, time.Millisecond)).Err()
}

func (store *redisInferenceCache) PingCache(ctx context.Context) error {
	if err := store.Ping(ctx).Err(); err != nil {
		return err
	}
	return store.Client.Set(ctx, store.prefix+"health", "ok", time.Second).Err()
}

func (store *redisInferenceCache) DeleteCache(ctx context.Context, keys []string) error {
	names := make([]string, len(keys))
	for i, key := range keys {
		names[i] = store.prefix + key
	}
	return store.Del(ctx, names...).Err()
}

func (store *redisInferenceCache) FlushCache(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := store.Scan(ctx, cursor, store.prefix+"*", 256).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := store.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

func (store *redisInferenceCache) CacheInfo(ctx context.Context) (map[string]any, error) {
	raw, err := store.Info(ctx, "stats", "memory", "keyspace").Result()
	if err != nil {
		return nil, err
	}
	sections := make(map[string]any)
	for _, line := range strings.Split(raw, "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			sections[key] = number
		} else {
			sections[key] = value
		}
	}
	return map[string]any{"num_clients": []int{-1}, "clients": []string{}, "info": sections}, nil
}
