package runtime

import (
	"regexp"
	"testing"
)

func TestDeploymentTagsAndStrictRegex(t *testing.T) {
	group := []Candidate{
		{ModelID: "tier", RoutingGroup: "demo", Tags: []string{"team", "region"}, TagRegex: []*regexp.Regexp{regexp.MustCompile("^User-Agent: fixture")}},
		{ModelID: "default", RoutingGroup: "demo", Tags: []string{"default"}},
		{ModelID: "fallback", RoutingGroup: "fallback", Tags: []string{"default"}},
	}
	tags := func(values ...any) map[string]any { return map[string]any{"metadata": map[string]any{"tags": values}} }
	assert := func(input map[string]any, agent string, strict bool, want string) {
		t.Helper()
		result := filterTaggedDeployments(group, input, agent, strict)
		if len(result) != 2 || result[0].ModelID != want || result[1].ModelID != "fallback" {
			t.Fatalf("unexpected filtered routes: %+v", result)
		}
	}
	assert(nil, "", false, "default")
	assert(nil, "fixture client", false, "tier")
	assert(nil, "fixture client", true, "default")
	assert(tags("team", "extra"), "fixture client", true, "default")
	assert(tags("team", "region"), "", true, "tier")
	assert(tags("team", "extra"), "", false, "tier")
}
