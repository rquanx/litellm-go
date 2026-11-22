package runtime

import "math/rand/v2"

func (s *Server) orderDeployments(candidates []Candidate) {
	if s.config.RoutingStrategy == "" {
		return
	}
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].RoutingGroup == candidates[start].RoutingGroup {
			end++
		}
		if candidates[start].RoutingGroup != "" && end-start > 1 {
			group := candidates[start:end]
			selected := chooseWeighted(group)
			if s.config.RoutingStrategy == "least-busy" {
				selected = s.leastBusy(group)
			}
			first := group[selected]
			copy(group[1:selected+1], group[:selected])
			group[0] = first
		}
		start = end
	}
}

func chooseWeighted(group []Candidate) int {
	first := group[0]
	total := 0.0
	for _, candidate := range group {
		total += routingWeight(first, candidate)
	}
	if total <= 0 {
		return rand.IntN(len(group))
	}
	remaining := rand.Float64() * total
	for index, candidate := range group {
		remaining -= routingWeight(first, candidate)
		if remaining < 0 {
			return index
		}
	}
	return 0
}

func routingWeight(first, candidate Candidate) float64 {
	switch {
	case first.Weight != nil && candidate.Weight != nil:
		return *candidate.Weight
	case first.Weight == nil && first.RPM != nil && candidate.RPM != nil:
		return float64(*candidate.RPM)
	case first.Weight == nil && first.RPM == nil && first.TPM != nil && candidate.TPM != nil:
		return float64(*candidate.TPM)
	default:
		return 0
	}
}

func (s *Server) leastBusy(group []Candidate) int {
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	best := rand.IntN(len(group))
	for index, candidate := range group {
		if s.deploymentTraffic[candidate.DeploymentID] < s.deploymentTraffic[group[best].DeploymentID] {
			best = index
		}
	}
	return best
}

func (s *Server) trackDeployment(candidate Candidate) func() {
	id := candidate.DeploymentID
	if id == "" || s.config.RoutingStrategy != "least-busy" {
		return func() {}
	}
	s.trafficMu.Lock()
	if s.deploymentTraffic == nil {
		s.deploymentTraffic = make(map[string]int)
	}
	s.deploymentTraffic[id]++
	s.trafficMu.Unlock()
	return func() {
		s.trafficMu.Lock()
		if s.deploymentTraffic[id] <= 1 {
			delete(s.deploymentTraffic, id)
		} else {
			s.deploymentTraffic[id]--
		}
		s.trafficMu.Unlock()
	}
}
