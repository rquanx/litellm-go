package runtime

import (
	"context"
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

func (s *Server) recordLatency(ctx context.Context, candidate Candidate, stream bool, elapsed time.Duration) {
	if s.config.RoutingStrategy != "latency-based-routing" || candidate.DeploymentID == "" {
		return
	}
	size := s.config.LatencySamples
	if size <= 0 {
		size = 10
	}
	if store := s.config.RoutingState; store != nil {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingStateTimeout)
		err := store.RecordLatency(bounded, candidate.DeploymentID, stream, elapsed, size)
		cancel()
		if err == nil {
			return
		}
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

func (s *Server) lowestLatency(ctx context.Context, group []Candidate, stream bool) int {
	if store := s.config.RoutingState; store != nil {
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		values, err := store.Latencies(bounded, routingDeploymentIDs(group), stream)
		cancel()
		if err == nil && len(values) == len(group) {
			return selectLatency(values, s.config.LatencyBuffer)
		}
	}
	values := make([]time.Duration, len(group))
	s.trafficMu.Lock()
	for i, candidate := range group {
		values[i] = s.averageLatency(candidate.DeploymentID, stream)
	}
	s.trafficMu.Unlock()
	return selectLatency(values, s.config.LatencyBuffer)
}

func selectLatency(values []time.Duration, buffer float64) int {
	lowest := values[0]
	for _, value := range values[1:] {
		lowest = min(lowest, value)
	}
	threshold := float64(lowest) * (1 + buffer)
	selected, tied := 0, 0
	for index, latency := range values {
		if float64(latency) <= threshold {
			tied++
			if rand.IntN(tied) == 0 {
				selected = index
			}
		}
	}
	return selected
}
