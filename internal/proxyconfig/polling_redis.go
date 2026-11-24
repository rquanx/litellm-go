package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/litellm-go/internal/runtime"
)

const pollingKeyPrefix = "minato:polling:response:"

type redisPollingStore struct{ *redis.Client }

func OpenRedisPollingStore(ctx context.Context, address string) (*redisPollingStore, error) {
	options, err := redis.ParseURL(address)
	if err != nil {
		return nil, errors.New("invalid Responses polling Redis URL")
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, errors.New("could not connect to Responses polling Redis")
	}
	return &redisPollingStore{client}, nil
}

var savePolling = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'status')
if ARGV[3] == '1' then
  if state then return 0 end
else
  if state ~= 'queued' and state ~= 'in_progress' then return 0 end
end
redis.call('HSET', KEYS[1], 'status', ARGV[1], 'snapshot', ARGV[2])
if ARGV[3] == '1' then redis.call('PEXPIRE', KEYS[1], ARGV[4]) end
return 1
`)

func (store *redisPollingStore) Save(ctx context.Context, id string, snapshot runtime.PollingSnapshot, ttl time.Duration, create bool) (bool, error) {
	status, ok := snapshot.Body["status"].(string)
	if !ok || status == "" {
		return false, errors.New("invalid polling status")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	creating := "0"
	if create {
		creating = "1"
	}
	saved, err := savePolling.Run(ctx, store.Client, []string{pollingKeyPrefix + id}, status, encoded, creating, ttl.Milliseconds()).Int()
	return saved == 1, err
}

func (store *redisPollingStore) Read(ctx context.Context, id string) (*runtime.PollingSnapshot, error) {
	fields, err := store.HMGet(ctx, pollingKeyPrefix+id, "status", "snapshot").Result()
	if err != nil || len(fields) != 2 || fields[0] == nil {
		return nil, err
	}
	data, _ := fields[1].(string)
	var snapshot runtime.PollingSnapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil || snapshot.Body == nil {
		return nil, errors.New("invalid Redis polling snapshot")
	}
	snapshot.Body["status"] = fields[0]
	return &snapshot, nil
}

var cancelPolling = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'status')
if state == 'queued' or state == 'in_progress' then
  redis.call('HSET', KEYS[1], 'status', 'cancelled')
end
return 1
`)

func (store *redisPollingStore) Cancel(ctx context.Context, id string) error {
	return cancelPolling.Run(ctx, store.Client, []string{pollingKeyPrefix + id}).Err()
}

func (store *redisPollingStore) Delete(ctx context.Context, id string) error {
	return store.Del(ctx, pollingKeyPrefix+id).Err()
}
