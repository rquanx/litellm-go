package runtime

import (
	"regexp"
	"slices"
)

func filterTaggedDeployments(candidates []Candidate, input map[string]any, userAgent string, matchAll bool) []Candidate {
	var tags []string
	for _, key := range []string{"metadata", "litellm_metadata"} {
		if metadata, ok := input[key].(map[string]any); ok {
			if raw, ok := metadata["tags"].([]any); ok {
				for _, value := range raw {
					if tag, ok := value.(string); ok && tag != "" && !slices.Contains(tags, tag) {
						tags = append(tags, tag)
					}
				}
			}
		}
	}
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
