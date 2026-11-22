package runtime

import (
	"net/http"
	"strconv"
	"time"
)

type cooldownWindow struct {
	failures int
	reset    time.Time
	until    time.Time
}

func (s *Server) admitDeployment(candidate Candidate, payload requestPayload) (*capacityReservation, bool) {
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

func (s *Server) recordCooldown(candidate Candidate, status int, retryable bool, headers http.Header) {
	if s.config.Cooldown <= 0 || candidate.DeploymentID == "" || !retryable ||
		(status > 0 && status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests && status < 500) {
		return
	}
	now := time.Now()
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
		delay := s.config.Cooldown
		raw := headers.Get("Retry-After")
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
			delay = max(delay, time.Duration(min(seconds, 3600))*time.Second)
		} else if until, err := http.ParseTime(raw); err == nil {
			delay = max(delay, min(time.Hour, until.Sub(now)))
		}
		entry.until = now.Add(delay)
		entry.reset = entry.until
	}
	s.deploymentCooldown[candidate.DeploymentID] = entry
}
