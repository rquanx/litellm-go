package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProjectKeyRetainsTeamAndProjectScopesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	const master = "fixture-master"
	registry, err := LoadKeys(master, path)
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(http.NotFoundHandler())
	call := func(endpoint, body string, status int) []byte {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+master)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != status {
			t.Fatalf("%s returned %d: %s", endpoint, result.Code, result.Body.String())
		}
		return result.Body.Bytes()
	}
	call("/team/new", `{"team_id":"team","models":["shared","team-only"]}`, 200)
	call("/project/new", `{"project_id":"project","team_id":"team","models":["shared","project-only"]}`, 200)
	call("/key/generate", `{"project_id":"project","team_id":"another"}`, 400)
	var issued struct { Key string `json:"key"` }
	if json.Unmarshal(call("/key/generate", `{"project_id":"project","models":["all-team-models"]}`, 200), &issued) != nil || issued.Key == "" {
		t.Fatal("virtual key could not be generated")
	}
	registry, err = LoadKeys(master, path)
	if err != nil {
		t.Fatal(err)
	}
	handler = registry.Handler(http.NotFoundHandler())
	if scope, valid := registry.Authorize(issued.Key); !valid || !slices.Equal(scope, []string{"shared"}) {
		t.Fatalf("unexpected project key scope %+v, valid %v", scope, valid)
	}
	call("/project/update", `{"project_id":"project","blocked":true}`, 200)
	if _, valid := registry.Authorize(issued.Key); valid {
		t.Fatal("blocked project key still works")
	}
	call("/team/delete", `{"team_ids":["team"]}`, 200)
	if len(registry.state.Projects)+len(registry.state.Keys) != 0 {
		t.Fatal("team deletion left a project or assigned key")
	}
}
