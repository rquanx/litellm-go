package proxyconfig

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestUnifiedAccessGroupGrantsFollowAssignments(t *testing.T) {
	keys := organizationFixture(t)
	entry := keys.state.Teams["owned-team"]
	entry.Models = []string{"team-model"}
	keys.state.Teams[entry.ID] = entry
	org := keys.state.Organizations[entry.OrgID]
	org.Models = []string{"team-model", "extra"}
	keys.state.Organizations[org.ID] = org
	keys.state.Keys[keyDigest("team-only")] = virtualKey{TeamID: entry.ID, Models: []string{"team-model"}}
	handler := keys.Handler(http.NotFoundHandler())
	request := func(method, path, token, body string, status int) string {
		t.Helper()
		return organizationRequest(t, handler, method, path, token, body, status)
	}
	const base = "/v1/access_group"
	const master = "fixture-master"
	digest := keyDigest("team-only")
	body := `{"access_group_name":"fixture","access_model_names":["extra","outside-org"],"access_mcp_server_ids":["mcp"],"access_agent_ids":["agent"],"assigned_key_ids":["` + digest + `"]}`
	request("POST", base, "admin", body, 401)
	created := request("POST", base, master, body, 201)
	var group accessGroupView
	if err := json.Unmarshal([]byte(created), &group); err != nil || group.ID == "" || !slices.Equal(group.Keys, []string{digest}) {
		t.Fatalf("unexpected group response: %s", created)
	}
	models, ok := keys.Authorize("team-only")
	if !ok || !slices.Contains(models, "extra") || slices.Contains(models, "outside-org") {
		t.Fatalf("group bypassed its organization or failed to extend the team scope: %v", models)
	}
	mcp, ok := keys.AuthorizeMCP("team-only")
	if !ok || !mcp(runtime.MCPServer{ID: "mcp"}, "fixture") || mcp(runtime.MCPServer{ID: "outside"}, "fixture") {
		t.Fatal("unexpected access group MCP authorization")
	}
	agent := keys.AuthorizeAgent("team-only")
	if agent == nil || !agent(runtime.Agent{ID: "agent"}) || agent(runtime.Agent{ID: "outside"}) {
		t.Fatal("unexpected access group agent authorization")
	}
	path := "/v1/unified_access_group/" + group.ID
	request("PUT", path, master, `{"assigned_key_ids":null,"assigned_team_ids":["owned-team"]}`, 200)
	models, _ = keys.Authorize("team-only")
	if slices.Contains(models, "extra") {
		t.Fatal("a team grant exceeded the explicit key scope")
	}
	request("POST", "/key/update", master, `{"key":"`+digest+`","models":["all-team-models"]}`, 200)
	models, _ = keys.Authorize("team-only")
	if !slices.Contains(models, "extra") {
		t.Fatal("the key did not inherit the team grant")
	}
	request("DELETE", path, master, "", 204)
	models, _ = keys.Authorize("team-only")
	if slices.Contains(models, "extra") {
		t.Fatal("deleted grant remained active")
	}
	if result := request("GET", base, master, "", 200); strings.TrimSpace(result) != "[]" {
		t.Fatalf("deleted group remained listed: %s", result)
	}
}

func TestModelAccessGroupUpdatesScopedKeys(t *testing.T) {
	keys := organizationFixture(t)
	keys.state.Keys[keyDigest("restricted")] = virtualKey{Models: []string{"production"}}
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: static
    litellm_params: {model: openai/static, api_base: http://example.invalid}
    model_info: {access_groups: [static-group]}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Token: "fixture-master", AuthorizeInference: true, AuthorizeKey: keys.Authorize})
	handler, err := WithModelManagement(server, config, "fixture-master", "", keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		body := `{"model_name":"` + name + `","litellm_params":{"model":"openai/` + name + `","api_base":"http://example.invalid"},"model_info":{"id":"` + name + `"}}`
		organizationRequest(t, handler, "POST", "/model/new", "fixture-master", body, 200)
	}
	organizationRequest(t, handler, "POST", "/access_group/new", "fixture-master",
		`{"access_group":"production","model_names":["first"]}`, 200)
	models, _ := keys.Authorize("restricted")
	if !slices.Contains(models, "first") || slices.Contains(models, "second") {
		t.Fatalf("initial access scope was %v", models)
	}
	const path = "/access_group/production"
	organizationRequest(t, handler, "PUT", path+"/update", "fixture-master",
		`{"model_names":["first"],"model_ids":["second"]}`, 200)
	models, _ = keys.Authorize("restricted")
	if !slices.Contains(models, "second") || slices.Contains(models, "first") {
		t.Fatalf("updated access scope was %v", models)
	}
	organizationRequest(t, handler, "DELETE", path+"/delete", "fixture-master", "", 200)
	models, _ = keys.Authorize("restricted")
	if !slices.Equal(models, []string{"production"}) {
		t.Fatalf("deleted access group still expanded the key's scope: %v", models)
	}
	organizationRequest(t, handler, "GET", "/access_group/static-group/info", "fixture-master", "", 200)
}
