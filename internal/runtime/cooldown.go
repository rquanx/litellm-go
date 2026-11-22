package runtime

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type cooldownWindow struct {
	failures int
	reset    time.Time
	until    time.Time
}

func (s *Server) admitDeployment(ctx context.Context, candidate Candidate, payload requestPayload) (*capacityReservation, bool) {
	track := s.tracksCapacity(candidate)
	if store := s.config.RoutingState; store != nil && candidate.DeploymentID != "" && (track || s.config.Cooldown > 0) {
		var rpm, tpm, tokens int
		if track {
			if candidate.RPM != nil {
				rpm = *candidate.RPM
			}
			if candidate.TPM != nil {
				tpm = *candidate.TPM
			}
			tokens = estimatedInputTokens(payload.fields)
		}
		minute := time.Now().Unix() / 60
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		admitted, err := store.Reserve(bounded, candidate.DeploymentID, minute, rpm, tpm, tokens, track, s.config.Cooldown > 0)
		cancel()
		if err == nil {
			if !admitted || !track {
				return nil, admitted
			}
			return &capacityReservation{deployment: candidate.DeploymentID, minute: minute, tokens: tokens, shared: true}, true
		}
	}
	if s.config.Cooldown > 0 && candidate.DeploymentID != "" {
		s.trafficMu.Lock()
		blocked := time.Now().Before(s.deploymentCooldown[candidate.DeploymentID].until)
		s.trafficMu.Unlock()
		if blocked {
			return nil, false
		}
	}
	return s.reserveCapacity(candidate, payload)
}

func (s *Server) recordCooldown(ctx context.Context, candidate Candidate, status int, retryable bool, headers http.Header) {
	if s.config.Cooldown <= 0 || candidate.DeploymentID == "" || !retryable ||
		(status > 0 && status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests && status < 500) {
		return
	}
	now := time.Now()
	delay := deploymentCooldownDelay(s.config.Cooldown, headers, now)
	if store := s.config.RoutingState; store != nil {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingStateTimeout)
		err := store.Cooldown(bounded, candidate.DeploymentID, s.config.AllowedFails, delay)
		cancel()
		if err == nil {
			return
		}
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.deploymentCooldown == nil {
		s.deploymentCooldown = make(map[string]cooldownWindow)
	}
	entry := s.deploymentCooldown[candidate.DeploymentID]
	if now.After(entry.reset) {
		entry = cooldownWindow{reset: now.Add(time.Minute)}
	}
	entry.failures++
	if entry.failures > s.config.AllowedFails {
		entry.until = now.Add(delay)
		entry.reset = entry.until
	}
	s.deploymentCooldown[candidate.DeploymentID] = entry
}

func deploymentCooldownDelay(delay time.Duration, headers http.Header, now time.Time) time.Duration {
	raw := headers.Get("Retry-After")
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return max(delay, time.Duration(min(seconds, 3600))*time.Second)
	}
	if until, err := http.ParseTime(raw); err == nil {
		return max(delay, min(time.Hour, until.Sub(now)))
	}
	return delay
}
