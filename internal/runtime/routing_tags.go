package runtime

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

func inferenceTags(fields map[string]any) []string {
	var tags []string
	seen := make(map[string]struct{})
	value := fields["metadata"]
	if alternate, exists := fields["litellm_metadata"]; exists {
		value = alternate
	}
	metadata, _ := value.(map[string]any)
	if text, ok := value.(string); ok {
		_ = json.Unmarshal([]byte(text), &metadata)
	}
	for _, list := range []any{metadata["tags"], fields["tags"]} {
		items, _ := list.([]any)
		for _, item := range items {
			if tag, ok := item.(string); ok && tag != "" {
				if _, exists := seen[tag]; !exists {
					tags = append(tags, tag)
					seen[tag] = struct{}{}
				}
			}
		}
	}
	return tags
}

func withDeploymentTags(candidates []Candidate, managed map[string][]string) []Candidate {
	if len(managed) == 0 {
		return candidates
	}
	candidates = slices.Clone(candidates)
	for index := range candidates {
		candidate := &candidates[index]
		for _, target := range []string{candidate.RoutingGroup, candidate.DeploymentID} {
			added := managed[target]
			if len(added) == 0 {
				continue
			}
			candidate.Tags = slices.Clone(candidate.Tags)
			for _, tag := range added {
				if !slices.Contains(candidate.Tags, tag) {
					candidate.Tags = append(candidate.Tags, tag)
				}
			}
		}
	}
	return candidates
}

func filterTaggedDeployments(candidates []Candidate, input map[string]any, userAgent string, matchAll bool) []Candidate {
	tags := inferenceTags(input)
	result := make([]Candidate, 0, len(candidates))
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].RoutingGroup == candidates[start].RoutingGroup {
			end++
		}
		result = append(result, matchDeploymentTags(candidates[start:end], tags, userAgent, matchAll)...)
		start = end
	}
	return result
}

func matchDeploymentTags(group []Candidate, requestTags []string, userAgent string, matchAll bool) []Candidate {
	var defaults, matching []Candidate
	tagged := len(requestTags) != 0
	header := "User-Agent: " + userAgent
	for _, deployment := range group {
		if slices.Contains(deployment.Tags, "default") {
			defaults = append(defaults, deployment)
		}
		if userAgent != "" && len(deployment.TagRegex) > 0 {
			tagged = true
		}
		match := len(requestTags) > 0 && len(deployment.Tags) > 0
		if match {
			if matchAll {
				match = !slices.ContainsFunc(requestTags, func(tag string) bool { return !slices.Contains(deployment.Tags, tag) })
			} else {
				match = slices.ContainsFunc(requestTags, func(tag string) bool { return slices.Contains(deployment.Tags, tag) })
			}
		}
		if !match && userAgent != "" && (!matchAll || len(deployment.Tags) == 0) {
			match = slices.ContainsFunc(deployment.TagRegex, func(pattern *regexp.Regexp) bool { return pattern.MatchString(header) })
		}
		if match {
			matching = append(matching, deployment)
		}
	}
	if len(matching) > 0 {
		return matching
	}
	if tagged || len(defaults) > 0 {
		return defaults
	}
	return group
}

func appendAnalyticsUserAgent(tags []string, agent string) []string {
	if agent == "" || len(agent) > 1024 {
		return tags
	}
	result := slices.Clone(tags)
	short, _, separated := strings.Cut(agent, "/")
	if separated && short != "" && !slices.Contains(result, "User-Agent: "+short) {
		result = append(result, "User-Agent: "+short)
	}
	if !slices.Contains(result, "User-Agent: "+agent) {
		result = append(result, "User-Agent: "+agent)
	}
	return result
}
