package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type attemptPlan struct {
	index     int
	retries   int
	used      int
	remaining int
	specialized bool
}

func (plan *attemptPlan) next(ctx context.Context, status int, headers http.Header, retryable bool, body []byte, route *Route) bool {
	if len(body) > 0 && !plan.specialized && route.Candidates[plan.index].RoutingGroup != "" {
		kind := specializedFailure(body)
		var candidates []Candidate
		switch kind {
		case "context":
			candidates = route.ContextFallback
		case "policy":
			candidates = route.PolicyFallback
		}
		if candidates != nil {
			if ctx.Err() != nil || len(candidates) == 0 {
				return false
			}
			route.Candidates = candidates
			plan.index = 0
			plan.used = 0
			plan.specialized = true
			return true
		}
		retryable = retryable || kind != ""
	}
	return plan.advance(ctx, status, headers, retryable, len(route.Candidates))
}

func specializedFailure(body []byte) string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	for _, value := range []string{payload.Error.Code, payload.Error.Type} {
		switch value {
		case "context_length_exceeded", "context_window_exceeded":
			return "context"
		case "content_policy_violation", "content_filter", "ResponsibleAIPolicyViolation":
			return "policy"
		}
	}
	return ""
}

func streamErrorBody(frame []byte) []byte {
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			return bytes.TrimSpace(value)
		}
	}
	return nil
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
