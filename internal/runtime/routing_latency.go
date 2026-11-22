package runtime

import (
	"math/rand/v2"
	"time"
)

type latencyKey struct {
	deployment string
	stream     bool
}

type latencyWindow struct {
	samples []time.Duration
	next    int
	sum     time.Duration
}

func (s *Server) recordLatency(candidate Candidate, stream bool, elapsed time.Duration) {
	if s.config.RoutingStrategy != "latency-based-routing" || candidate.DeploymentID == "" {
		return
	}
	size := s.config.LatencySamples
	if size <= 0 {
		size = 10
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.deploymentLatency == nil {
		s.deploymentLatency = make(map[latencyKey]*latencyWindow)
	}
	key := latencyKey{deployment: candidate.DeploymentID, stream: stream}
	window := s.deploymentLatency[key]
	if window == nil {
		window = &latencyWindow{samples: make([]time.Duration, 0, size)}
		s.deploymentLatency[key] = window
	}
	if len(window.samples) < size {
		window.samples = append(window.samples, elapsed)
	} else {
		window.sum -= window.samples[window.next]
		window.samples[window.next] = elapsed
		window.next = (window.next + 1) % size
	}
	window.sum += elapsed
}

func (s *Server) averageLatency(deployment string, stream bool) time.Duration {
	window := s.deploymentLatency[latencyKey{deployment: deployment, stream: stream}]
	if window == nil && stream {
		window = s.deploymentLatency[latencyKey{deployment: deployment}]
	}
	if window == nil {
		return 0
	}
	return window.sum / time.Duration(len(window.samples))
}

func (s *Server) lowestLatency(group []Candidate, stream bool) int {
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	lowest := s.averageLatency(group[0].DeploymentID, stream)
	for _, candidate := range group[1:] {
		lowest = min(lowest, s.averageLatency(candidate.DeploymentID, stream))
	}
	threshold := float64(lowest) * (1 + s.config.LatencyBuffer)
	selected, tied := 0, 0
	for index, candidate := range group {
		if float64(s.averageLatency(candidate.DeploymentID, stream)) <= threshold {
			tied++
			if rand.IntN(tied) == 0 {
				selected = index
			}
		}
	}
	return selected
}
