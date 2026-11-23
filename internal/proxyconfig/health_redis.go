package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

var acquireHealth = redis.NewScript(`
local snapshot = redis.call('GET', KEYS[1])
if snapshot then return snapshot end
if redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', ARGV[2]) then return 'acquired' end
return ''
`)

var publishHealth = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
redis.call('DEL', KEYS[2])
return 1
`)

func (store *redisRoutingState) healthKeys(key string) []string {
	prefix := "minato:health:{" + store.namespace + ":" + key + "}:"
	return []string{prefix + "snapshot", prefix + "lease"}
}

func (store *redisRoutingState) AcquireHealth(ctx context.Context, key, token string, lease time.Duration) (*runtime.SharedHealthResult, bool, error) {
	value, err := acquireHealth.Run(ctx, store.Client, store.healthKeys(key), token, lease.Milliseconds()).Text()
	if err != nil || value == "" {
		return nil, false, err
	}
	if value == "acquired" {
		return nil, true, nil
	}
	var snapshot runtime.SharedHealthResult
	if json.Unmarshal([]byte(value), &snapshot) != nil || snapshot.Record.ID == "" {
		return nil, false, errors.New("invalid shared health result")
	}
	return &snapshot, false, nil
}

func (store *redisRoutingState) PublishHealth(ctx context.Context, key, token string, snapshot runtime.SharedHealthResult, ttl time.Duration) error {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return publishHealth.Run(ctx, store.Client, store.healthKeys(key), token, body, max(ttl.Milliseconds(), 1)).Err()
}

func (store *redisRoutingState) PingHealth(ctx context.Context) error {
	return store.Ping(ctx).Err()
}
