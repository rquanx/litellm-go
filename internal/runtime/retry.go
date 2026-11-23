package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RetryPolicy map[string]int

func (policy RetryPolicy) Valid() bool {
	for name, count := range policy {
		if count < 0 || count > 8 {
			return false
		}
		switch name {
		case "BadRequestErrorRetries", "AuthenticationErrorRetries", "TimeoutErrorRetries",
			"RateLimitErrorRetries", "ContentPolicyViolationErrorRetries", "InternalServerErrorRetries":
		default:
			return false
		}
	}
	return true
}

func retryCategory(status int, kind string) string {
	switch {
	case kind == "policy":
		return "ContentPolicyViolationErrorRetries"
	case status == http.StatusBadRequest:
		return "BadRequestErrorRetries"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "AuthenticationErrorRetries"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "TimeoutErrorRetries"
	case status == http.StatusTooManyRequests:
		return "RateLimitErrorRetries"
	case status >= 500:
		return "InternalServerErrorRetries"
	}
	return ""
}

type attemptPlan struct {
	index     int
	retries   int
	used      int
	remaining int
	specialized bool
	maxFallbacks *int
	fallbacks int
	policy RetryPolicy
	modelPolicy RetryPolicy
}

func (s *Server) attemptPlan(alias string) attemptPlan {
	return attemptPlan{retries: s.config.NumRetries, remaining: s.config.NumRetries, maxFallbacks: s.config.MaxFallbacks,
		policy: s.config.RetryPolicy, modelPolicy: s.config.ModelRetryPolicies[alias]}
}

func (plan *attemptPlan) next(ctx context.Context, status int, headers http.Header, retryable bool, body []byte, route *Route, admitted bool) bool {
	if !admitted {
		return plan.advance(ctx, status, headers, retryable, route.Candidates)
	}
	kind := ""
	if len(body) > 0 && !plan.specialized && route.Candidates[plan.index].RoutingGroup != "" {
		kind = specializedFailure(status, body)
		var candidates []Candidate
		switch kind {
		case "context":
			candidates = route.ContextFallback
		case "policy":
			candidates = route.PolicyFallback
		}
		if candidates != nil {
			if ctx.Err() != nil || len(candidates) == 0 || !plan.canFallback() {
				return false
			}
			plan.fallbacks++
			route.Candidates = candidates
			plan.index = 0
			plan.used = 0
			plan.specialized = true
			return true
		}
		retryable = retryable || kind != ""
	}
	if name := retryCategory(status, kind); name != "" && plan.used <= 8 {
		limit, set := plan.modelPolicy[name]
		if !set {
			limit, set = plan.policy[name]
		}
		if set {
			if plan.used < limit && ctx.Err() == nil && waitForRetry(ctx, headers, plan.used) {
				plan.used++
				return true
			}
			if ctx.Err() != nil || !plan.canAdvance(route.Candidates) {
				return false
			}
			plan.advanceIndex(route.Candidates)
			return true
		}
	}
	return plan.advance(ctx, status, headers, retryable, route.Candidates)
}

func specializedFailure(status int, body []byte) string {
	var payload struct {
		Error struct {
			Code any `json:"code"`
			Type string `json:"type"`
			Message string `json:"message"`
			Inner struct {
				Code string `json:"code"`
			} `json:"innererror"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	code, _ := payload.Error.Code.(string)
	for _, value := range []string{code, payload.Error.Type, payload.Error.Inner.Code} {
		switch value {
		case "context_length_exceeded", "context_window_exceeded":
			return "context"
		case "content_policy_violation", "content_filter", "ResponsibleAIPolicyViolation":
			return "policy"
		}
	}
	if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge {
		message := strings.ToLower(payload.Error.Message)
		for _, marker := range []string{"prompt is too long", "prompt: length", "maximum context length", "context length exceeded"} {
			if strings.Contains(message, marker) {
				return "context"
			}
		}
		if strings.Contains(message, "content policy violation") {
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

func (plan *attemptPlan) advance(ctx context.Context, status int, headers http.Header, retryable bool, candidates []Candidate) bool {
	previous := plan.index
	if !plan.advanceWithBackup(ctx, status, headers, retryable, plan.canAdvance(candidates)) {
		return false
	}
	if plan.index != previous && differentFallbackGroup(candidates[previous], candidates[plan.index]) {
		plan.fallbacks++
	}
	return true
}

func (plan *attemptPlan) advanceWithBackup(ctx context.Context, status int, headers http.Header, retryable, hasBackup bool) bool {
	if !retryable || ctx.Err() != nil {
		return false
	}
	if plan.remaining > 0 && plan.used < plan.retries && (status == 0 || retryableStatus(status)) &&
		(status != http.StatusTooManyRequests || !hasBackup) {
		if waitForRetry(ctx, headers, plan.used) {
			plan.used++
			plan.remaining--
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	if !hasBackup {
		return false
	}
	plan.index++
	plan.used = 0
	return true
}

func waitForRetry(ctx context.Context, headers http.Header, used int) bool {
	wait := retryDelay(headers, used)
	if wait > 8*time.Second {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
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

func (plan *attemptPlan) canFallback() bool {
	return plan.maxFallbacks == nil || plan.fallbacks < *plan.maxFallbacks
}

func (plan *attemptPlan) canAdvance(candidates []Candidate) bool {
	return plan.index+1 < len(candidates) &&
		(!differentFallbackGroup(candidates[plan.index], candidates[plan.index+1]) || plan.canFallback())
}

func (plan *attemptPlan) advanceIndex(candidates []Candidate) {
	if differentFallbackGroup(candidates[plan.index], candidates[plan.index+1]) {
		plan.fallbacks++
	}
	plan.index++
	plan.used = 0
}

func differentFallbackGroup(from, to Candidate) bool {
	return from.RoutingGroup != to.RoutingGroup
}
