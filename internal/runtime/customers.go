package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type CustomerPolicy struct {
	Blocked bool
	BudgetError string
	Region string
	DefaultModel string
	ModelPolicy func(context.Context, string) (KeyModelPolicy, error)
}

func customerRegionCandidates(candidates []Candidate, region string) []Candidate {
	if region == "" || candidates == nil {
		return candidates
	}
	selected := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Region == region {
			selected = append(selected, candidate)
		}
	}
	return selected
}

func requestCustomerID(r *http.Request, payload requestPayload) string {
	for _, header := range []string{"X-LiteLLM-Customer-ID", "X-LiteLLM-End-User-ID"} {
		if id := r.Header.Get(header); strings.TrimSpace(id) != "" {
			return id
		}
	}
	fields := payload.fields
	if len(payload.form) > 0 {
		fields = clone(fields)
		for _, part := range payload.form {
			switch part.name {
			case "user", "metadata", "litellm_metadata", "safety_identifier":
				fields[part.name] = string(part.body)
			}
		}
	}
	if id, _ := fields["user"].(string); id != "" {
		return id
	}
	for _, source := range [][2]string{{"litellm_metadata", "user"}, {"metadata", "user_id"}} {
		metadata, _ := fields[source[0]].(map[string]any)
		if raw, ok := fields[source[0]].(string); ok {
			_ = json.Unmarshal([]byte(raw), &metadata)
		}
		if id, _ := metadata[source[1]].(string); id != "" {
			return id
		}
	}
	id, _ := fields["safety_identifier"].(string)
	return id
}
