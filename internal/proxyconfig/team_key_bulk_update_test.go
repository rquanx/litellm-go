package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBulkTeamKeyUpdateKeepsTeamScope(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = map[string]internalUser{"member": {ID: "member", Role: "internal_user"}}
	entry := team{ID: "one", Members: []teamMembership{{TeamID: "one", UserID: "member", Role: "user"}}}
	keys.state.Teams["one"] = entry
	keys.state.Teams["two"] = team{ID: "two"}
	keys.state.Keys[keyDigest("caller")] = virtualKey{UserID: "member"}
	keys.state.Keys[keyDigest("target")] = virtualKey{TeamID: "one"}
	keys.state.Keys[keyDigest("outsider")] = virtualKey{TeamID: "two", Alias: "private"}
	keys.state.Keys[keyDigest("blocked")] = virtualKey{TeamID: "one", Blocked: true}
	expired := time.Now().Add(-time.Hour)
	keys.state.Keys[keyDigest("expired")] = virtualKey{TeamID: "one", Expires: &expired}
	handler := keys.Handler(http.NotFoundHandler())

	call := func(path, body string, expected int) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer caller")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("got `d, want `d: `s", response.Code, expected, response.Body)
		}
		return response.Body.String()
	}
	bulk := "/team/key/bulk_update"
	all := `{ "team_id":"one", "all_keys_in_team":true, "update_fields":{"rpm_limit":9} }`
	call(bulk, all, 401)
	permissions := []string{"/key/update"}
	entry.MemberPermissions = &permissions
	keys.state.Teams["one"] = entry
	call(bulk, `{"team_id":"two","all_keys_in_team":true,"update_fields":{"rpm_limit":9}}`, 401)
	call(bulk, `{"team_id":"one","all_keys_in_team":true,"update_fields":{"team_id":"two"}}`, 400)
	call(bulk, `{"team_id":"one","all_keys_in_team":true,"update_fields":{"metadata":{"allowed_passthrough_routes":["/"]}}}`, 403)
	result := call(bulk, `{"team_id":"one","key_ids":["target","`+keyDigest("target")+`","outsider"],"update_fields":{"rpm_limit":7}}`, 200)
	var batch struct {
		Total int `json:"total_requested"`
		Success []bulkKeyResult `json:"successful_updates"`
		Failed []bulkKeyResult `json:"failed_updates"`
	}
	if err := json.Unmarshal([]byte(result), &batch); err != nil || batch.Total != 2 ||
		len(batch.Success) != 1 || len(batch.Failed) != 1 || batch.Failed[0].Info != nil {
		t.Fatalf("unexpected cross-team result: `s (`v)", result, err)
	}
	call(bulk, all, 200)
	for _, untouched := range []string{"outsider", "blocked", "expired"} {
		if keys.state.Keys[keyDigest(untouched)].RPMLimit != nil {
			t.Fatalf("unexpected write to `s", untouched)
		}
	}
	target := keys.state.Keys[keyDigest("target")]
	if target.RPMLimit == nil || *target.RPMLimit != 9 {
		t.Fatalf("unexpected team key limit: `v", target.RPMLimit)
	}
	call("/key/block", `{"key":"target"}`, 404)
	permissions = append(permissions, "/key/block", "/key/unblock")
	keys.state.Teams["one"] = entry
	call("/key/block", `{"key":"outsider"}`, 404)
	call("/key/block", `{"key":"target"}`, 200)
	if !keys.state.Keys[keyDigest("target")].Blocked {
		t.Fatal("team key was not blocked")
	}
	call("/key/unblock", `{"key":"target"}`, 200)
	if keys.state.Keys[keyDigest("target")].Blocked {
		t.Fatal("team key was not unblocked")
	}
}

