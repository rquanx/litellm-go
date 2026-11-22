package runtime

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"time"
)

func capacityError() (int, []byte, http.Header) {
	return http.StatusTooManyRequests,
		[]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Deployment capacity exhausted"}}`),
		http.Header{"Content-Type": {"application/json"}}
}

type capacityUsage struct {
	requests int
	tokens   int
}

type capacityReservation struct {
	deployment string
	minute     int64
	tokens     int
	shared     bool
}

func (s *Server) lowestUsage(ctx context.Context, candidates []Candidate) int {
	if store := s.config.RoutingState; store != nil {
		deployments := make([]string, len(candidates))
		for i, candidate := range candidates {
			deployments[i] = candidate.DeploymentID
		}
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		values, err := store.Usage(bounded, deployments, time.Now().Unix()/60)
		cancel()
		if err == nil && len(values) == len(candidates) {
			selected := rand.IntN(len(values))
			for i, count := range values {
				if count < values[selected] {
					selected = i
				}
			}
			return selected
		}
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	s.rollCapacityWindow(time.Now().Unix() / 60)
	selected := rand.IntN(len(candidates))
	for index, candidate := range candidates {
		if s.deploymentCapacity[candidate.DeploymentID].tokens < s.deploymentCapacity[candidates[selected].DeploymentID].tokens {
			selected = index
		}
	}
	return selected
}

func (s *Server) rollCapacityWindow(minute int64) {
	if s.deploymentCapacity == nil || s.capacityMinute != minute {
		s.deploymentCapacity = make(map[string]capacityUsage)
		s.capacityMinute = minute
	}
}

func (s *Server) tracksCapacity(candidate Candidate) bool {
	if candidate.DeploymentID == "" {
		return false
	}
	switch s.config.RoutingStrategy {
	case "latency-based-routing", "cost-based-routing":
		if candidate.RPM == nil && candidate.TPM == nil {
			return false
		}
	case "usage-based-routing", "usage-based-routing-v2":
	default:
		return false
	}
	return true
}

func (s *Server) reserveCapacity(candidate Candidate, payload requestPayload) (*capacityReservation, bool) {
	if !s.tracksCapacity(candidate) {
		return nil, true
	}
	tokens := estimatedInputTokens(payload.fields)
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	minute := time.Now().Unix() / 60
	s.rollCapacityWindow(minute)
	usage := s.deploymentCapacity[candidate.DeploymentID]
	if (candidate.RPM != nil && *candidate.RPM > 0 && usage.requests >= *candidate.RPM) ||
		(candidate.TPM != nil && *candidate.TPM > 0 && (usage.tokens > *candidate.TPM || tokens > *candidate.TPM-usage.tokens)) {
		return nil, false
	}
	usage.requests++
	usage.tokens += tokens
	s.deploymentCapacity[candidate.DeploymentID] = usage
	return &capacityReservation{deployment: candidate.DeploymentID, minute: minute, tokens: tokens}, true
}

func (s *Server) settleCapacity(ctx context.Context, reservation *capacityReservation, tokens int) {
	if tokens <= 0 || tokens == reservation.tokens {
		return
	}
	if reservation.shared {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingStateTimeout)
		defer cancel()
		if s.config.RoutingState.Adjust(bounded, reservation.deployment, reservation.minute, tokens-reservation.tokens) == nil {
			reservation.tokens = tokens
		}
		return
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.capacityMinute != reservation.minute {
		return
	}
	usage := s.deploymentCapacity[reservation.deployment]
	usage.tokens += tokens - reservation.tokens
	reservation.tokens = tokens
	s.deploymentCapacity[reservation.deployment] = usage
}

func estimatedInputTokens(fields map[string]any) int {
	size := 0
	for _, key := range []string{"messages", "input", "prompt", "instructions", "tools", "contents", "query", "documents"} {
		if value, exists := fields[key]; exists {
			body, err := json.Marshal(value)
			if err == nil {
				size += len(body)
			}
		}
	}
	return (size + 3) / 4
}

func usageFromJSON(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	type usage struct {
		TotalTokens        int `json:"total_tokens"`
		BedrockTotalTokens int `json:"totalTokens"`
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
	}
	var result struct {
		Usage    usage `json:"usage"`
		Response struct {
			Usage usage `json:"usage"`
		} `json:"response"`
		UsageMetadata struct {
			Total int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(body, &result) != nil {
		return 0
	}
	read := result.Usage
	if result.Response.Usage != (usage{}) {
		read = result.Response.Usage
	}
	return max(read.TotalTokens, read.BedrockTotalTokens, read.InputTokens+read.OutputTokens, result.UsageMetadata.Total)
}
