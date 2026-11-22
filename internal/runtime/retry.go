package runtime

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type attemptPlan struct {
	index     int
	retries   int
	used      int
	remaining int
}

func (plan *attemptPlan) advance(ctx context.Context, status int, headers http.Header, retryable bool, size int) bool {
	if !retryable || ctx.Err() != nil {
		return false
	}
	hasBackup := plan.index+1 < size
	if plan.remaining > 0 && plan.used < plan.retries && (status == 0 || retryableStatus(status)) &&
		(status != http.StatusTooManyRequests || !hasBackup) {
		wait := retryDelay(headers, plan.used)
		if wait <= 8*time.Second {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-timer.C:
				plan.used++
				plan.remaining--
				return true
			case <-ctx.Done():
				return false
			}
		}
	}
	if !hasBackup {
		return false
	}
	plan.index++
	plan.used = 0
	return true
}

func retryDelay(headers http.Header, used int) time.Duration {
	raw := headers.Get("Retry-After")
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		if seconds > 8 {
			return 9 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		return time.Until(when)
	}
	return min(500*time.Millisecond<<used, 8*time.Second)
}
