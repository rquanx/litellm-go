package proxyconfig

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/litellm-go/internal/runtime"
)

type redisRoutingState struct {
	*redis.Client
	namespace string
}

func OpenRedisRoutingState(ctx context.Context, options *redis.Options, namespace string) (*redisRoutingState, error) {
	configured := *options
	configured.MaxRetries = -1
	configured.ContextTimeoutEnabled = true
	client := redis.NewClient(&configured)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, errors.New("could not connect to routing state Redis")
	}
	return &redisRoutingState{Client: client, namespace: fmt.Sprintf("%x", sha256.Sum256([]byte(namespace)))}, nil
}

func (store *redisRoutingState) prefix(deployment string) string {
	return fmt.Sprintf("minato:routing:{%s:%x}:", store.namespace, sha256.Sum256([]byte(deployment)))
}

var reserveRouting = redis.NewScript(`
if ARGV[5] == '1' and redis.call('EXISTS', KEYS[2]) == 1 then return 0 end
if ARGV[4] == '0' then return 1 end
local usage = redis.call('HMGET', KEYS[1], 'requests', 'tokens')
local rpm, tpm, tokens = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
if rpm > 0 and tonumber(usage[1] or '0') >= rpm then return 0 end
if tpm > 0 and tonumber(usage[2] or '0') + tokens > tpm then return 0 end
redis.call('HINCRBY', KEYS[1], 'requests', 1)
redis.call('HINCRBY', KEYS[1], 'tokens', tokens)
if not usage[1] then redis.call('EXPIRE', KEYS[1], 120) end
return 1
`)

func (store *redisRoutingState) Reserve(ctx context.Context, deployment string, minute int64, rpm, tpm, tokens int, track, checkCooldown bool) (bool, error) {
	prefix := store.prefix(deployment)
	trackArg, checkArg := "0", "0"
	if track {
		trackArg = "1"
	}
	if checkCooldown {
		checkArg = "1"
	}
	result, err := reserveRouting.Run(ctx, store.Client, []string{
		prefix + strconv.FormatInt(minute, 10), prefix + "cooldown",
	}, rpm, tpm, tokens, trackArg, checkArg).Int()
	return result == 1, err
}

var reserveKeyRPM = redis.NewScript(`
local retry = 0
for i, key in ipairs(KEYS) do
    local limit = tonumber(ARGV[i])
    if limit <= 0 then return 60 end
    local count = tonumber(redis.call('GET', key) or '0')
    if count >= limit then retry = math.max(retry, 1, redis.call('TTL', key)) end
end
if retry > 0 then return retry end
for _, key in ipairs(KEYS) do
    if redis.call('INCR', key) == 1 then redis.call('EXPIRE', key, 60) end
end
return 0
`)

func (store *redisRoutingState) ReserveRequestRates(ctx context.Context, scopes []runtime.OwnerRateLimits) (int64, error) {
	keys := make([]string, 0, len(scopes))
	limits := make([]any, 0, len(scopes))
	for _, scope := range scopes {
		keys = append(keys, store.prefix(scope.Identity)+"key-rpm")
		limits = append(limits, *scope.RPM)
	}
	return reserveKeyRPM.Run(ctx, store.Client, keys, limits...).Int64()
}

var reserveKeyTokens = redis.NewScript(`
local window = redis.call('HGET', KEYS[1], 'window')
local tokens = tonumber(redis.call('HGET', KEYS[1], 'tokens') or '0')
if tokens + tonumber(ARGV[2]) > tonumber(ARGV[1]) then
    return {'', math.max(1, redis.call('TTL', KEYS[1]))}
end
if not window then
    window = ARGV[3]
    redis.call('HSET', KEYS[1], 'window', window, 'tokens', ARGV[2])
    redis.call('EXPIRE', KEYS[1], 60)
else
    redis.call('HINCRBY', KEYS[1], 'tokens', ARGV[2])
end
return {window, 0}
`)

func (store *redisRoutingState) ReserveKeyTokens(ctx context.Context, identity string, limit, estimated int) (string, int64, error) {
	window := newEntityID()
	values, err := reserveKeyTokens.Run(ctx, store.Client, []string{store.prefix(identity) + "key-tpm"}, limit, estimated, window).Slice()
	if err != nil {
		return "", 0, err
	}
	if len(values) != 2 {
		return "", 0, errors.New("invalid key token reservation")
	}
	window, windowOK := values[0].(string)
	retry, retryOK := values[1].(int64)
	if !windowOK || !retryOK {
		return "", 0, errors.New("invalid key token reservation")
	}
	return window, retry, nil
}

var adjustKeyTokens = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'window') == ARGV[1] then
    local tokens = tonumber(redis.call('HGET', KEYS[1], 'tokens'))
    redis.call('HSET', KEYS[1], 'tokens', math.max(0, tokens + tonumber(ARGV[2])))
end
return 0
`)

func (store *redisRoutingState) AdjustKeyTokens(ctx context.Context, identity, window string, delta int) error {
	return adjustKeyTokens.Run(ctx, store.Client, []string{store.prefix(identity) + "key-tpm"}, window, delta).Err()
}

var acquireKeyParallel = redis.NewScript(`
local now = tonumber(redis.call('TIME')[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], now + 600, ARGV[1])
redis.call('EXPIRE', KEYS[1], 600)
return 1
`)

func (store *redisRoutingState) AcquireKeyParallel(ctx context.Context, identity, lease string, limit int) (bool, error) {
	result, err := acquireKeyParallel.Run(ctx, store.Client, []string{store.prefix(identity) + "key-parallel"}, lease, limit).Int()
	return result == 1, err
}

var renewKeyParallel = redis.NewScript(`
local now = tonumber(redis.call('TIME')[1])
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not score or tonumber(score) <= now then return 0 end
redis.call('ZADD', KEYS[1], 'XX', now + 600, ARGV[1])
redis.call('EXPIRE', KEYS[1], 600)
return 1
`)

func (store *redisRoutingState) RenewKeyParallel(ctx context.Context, identity, lease string) (bool, error) {
	result, err := renewKeyParallel.Run(ctx, store.Client, []string{store.prefix(identity) + "key-parallel"}, lease).Int()
	return result == 1, err
}

func (store *redisRoutingState) ReleaseKeyParallel(ctx context.Context, identity, lease string) error {
	return store.ZRem(ctx, store.prefix(identity)+"key-parallel", lease).Err()
}

var adjustRouting = redis.NewScript(`
local current = redis.call('HGET', KEYS[1], 'tokens')
if not current then return 0 end
redis.call('HSET', KEYS[1], 'tokens', math.max(0, tonumber(current) + tonumber(ARGV[1])))
return 1
`)

func (store *redisRoutingState) Adjust(ctx context.Context, deployment string, minute int64, delta int) error {
	return adjustRouting.Run(ctx, store.Client, []string{store.prefix(deployment) + strconv.FormatInt(minute, 10)}, delta).Err()
}

func (store *redisRoutingState) Usage(ctx context.Context, deployments []string, minute int64) ([]int, error) {
	pipeline := store.Pipeline()
	bucket := strconv.FormatInt(minute, 10)
	reads := make([]*redis.StringCmd, len(deployments))
	for index, id := range deployments {
		reads[index] = pipeline.HGet(ctx, store.prefix(id)+bucket, "tokens")
	}
	if _, err := pipeline.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	result := make([]int, len(reads))
	for index, read := range reads {
		count, err := read.Int()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		result[index] = count
	}
	return result, nil
}

var cooldownRouting = redis.NewScript(`
local failures = redis.call('INCR', KEYS[1])
if failures == 1 then redis.call('EXPIRE', KEYS[1], 60) end
if failures > tonumber(ARGV[1]) then
  local delay = math.max(tonumber(ARGV[2]), redis.call('PTTL', KEYS[2]))
  redis.call('SET', KEYS[2], '1', 'PX', delay)
  redis.call('PEXPIRE', KEYS[1], delay)
end
return 1
`)

func (store *redisRoutingState) Cooldown(ctx context.Context, deployment string, allowedFailures int, delay time.Duration) error {
	prefix := store.prefix(deployment)
	return cooldownRouting.Run(ctx, store.Client, []string{prefix + "failures", prefix + "cooldown"}, allowedFailures, max(1, delay.Milliseconds())).Err()
}

func (store *redisRoutingState) latencyKey(deployment string, stream bool) string {
	return store.prefix(deployment) + "latency:" + strconv.FormatBool(stream)
}

func (store *redisRoutingState) RecordLatency(ctx context.Context, deployment string, stream bool, elapsed time.Duration, samples int) error {
	key := store.latencyKey(deployment, stream)
	_, err := store.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.LPush(ctx, key, elapsed.Nanoseconds())
		pipe.LTrim(ctx, key, 0, int64(samples-1))
		pipe.Expire(ctx, key, time.Hour)
		return nil
	})
	return err
}

func (store *redisRoutingState) Latencies(ctx context.Context, deployments []string, stream bool) ([]time.Duration, error) {
	pipeline := store.Pipeline()
	reads := make([]*redis.StringSliceCmd, len(deployments))
	fallbacks := make([]*redis.StringSliceCmd, len(deployments))
	for i, id := range deployments {
		reads[i] = pipeline.LRange(ctx, store.latencyKey(id, stream), 0, 999)
		if stream {
			fallbacks[i] = pipeline.LRange(ctx, store.latencyKey(id, false), 0, 999)
		}
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		return nil, err
	}
	result := make([]time.Duration, len(deployments))
	for i, read := range reads {
		values := read.Val()
		if len(values) == 0 && stream {
			values = fallbacks[i].Val()
		}
		for _, value := range values {
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, err
			}
			result[i] += time.Duration(number)
		}
		if len(values) > 0 {
			result[i] /= time.Duration(len(values))
		}
	}
	return result, nil
}

const routingBusyLease = 10 * time.Minute

func (store *redisRoutingState) Track(ctx context.Context, deployment, lease string, active bool) error {
	key := store.prefix(deployment) + "busy"
	if !active {
		return store.ZRem(ctx, key, lease).Err()
	}
	now := time.Now()
	_, err := store.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(now.UnixMilli(), 10))
		pipe.ZAdd(ctx, key, redis.Z{Score: float64(now.Add(routingBusyLease).UnixMilli()), Member: lease})
		pipe.Expire(ctx, key, routingBusyLease)
		return nil
	})
	return err
}

func (store *redisRoutingState) Busy(ctx context.Context, deployments []string) ([]int, error) {
	pipeline := store.Pipeline()
	minimum := "(" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	reads := make([]*redis.IntCmd, len(deployments))
	for i, id := range deployments {
		reads[i] = pipeline.ZCount(ctx, store.prefix(id)+"busy", minimum, "+inf")
	}
	if _, err := pipeline.Exec(ctx); err != nil {
		return nil, err
	}
	result := make([]int, len(reads))
	for i, read := range reads {
		result[i] = int(read.Val())
	}
	return result, nil
}
