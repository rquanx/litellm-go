package runtime

import (
	"context"
	"time"
)

// RoutingState coordinates admission and usage across proxy instances.
type RoutingState interface {
	Reserve(ctx context.Context, deployment string, minute int64, rpm, tpm, tokens int, track, checkCooldown bool) (bool, error)
	Adjust(ctx context.Context, deployment string, minute int64, delta int) error
	Usage(ctx context.Context, deployments []string, minute int64) ([]int, error)
	RecordLatency(ctx context.Context, deployment string, stream bool, elapsed time.Duration, samples int) error
	Latencies(ctx context.Context, deployments []string, stream bool) ([]time.Duration, error)
	Busy(ctx context.Context, deployments []string) ([]int, error)
	Track(ctx context.Context, deployment, lease string, active bool) error
	Cooldown(ctx context.Context, deployment string, allowedFailures int, delay time.Duration) error
}

const routingStateTimeout = 150 * time.Millisecond
