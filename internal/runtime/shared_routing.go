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
	Cooldown(ctx context.Context, deployment string, allowedFailures int, delay time.Duration) error
}

const routingStateTimeout = 150 * time.Millisecond
