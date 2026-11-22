package proxyconfig

import (
	"strings"
	"testing"
)

func TestModelGroupAliasesAndWildcardFallback(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: main
    litellm_params: { model: openai/fixture }
  - model_name: backup
    litellm_params: { model: openai/fallback }
router_settings:
  model_group_alias:
    public: main
    private: {model: main, hidden: true}
  fallbacks:
    - "*": [backup]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range config.Routes {
		switch route.Alias {
		case "backup":
			if len(route.Candidates) != 1 {
				t.Fatal("wildcard fallbacks must skip the model itself")
			}
		case "public", "private":
			if len(route.Candidates) != 2 || route.Candidates[0].RoutingGroup != route.Alias ||
				route.Candidates[1].RoutingGroup != "backup" || route.Hidden != (route.Alias == "private") {
				t.Fatalf("unexpected alias route: %+v", route)
			}
		}
	}
}
