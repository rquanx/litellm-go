package proxyconfig

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

type spendLogScope struct {
	user string
	teams []string
}

func (log *SpendLog) spendLogScope(w http.ResponseWriter, r *http.Request) (spendLogScope, bool) {
	keys := log.keys
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	caller, ok := keys.identifyReadCallerLocked(r)
	if !ok || (!caller.admin && caller.user == "") {
		modelError(w, http.StatusUnauthorized, "A master key or user credential is required")
		return spendLogScope{}, false
	}
	if caller.admin {
		return spendLogScope{}, true
	}
	scope := spendLogScope{user: caller.user}
	if r.URL.Path != "/spend/logs/v2" && r.URL.Path != "/spend/logs/ui" && r.URL.Path != "/spend/logs/session/ui" &&
		!strings.HasPrefix(r.URL.Path, "/spend/logs/ui/") {
		return scope, true
	}
	for id, team := range keys.state.Teams {
		if member := memberIndex(team, caller.user); member >= 0 &&
			(team.Members[member].Role == "admin" || team.hasMemberPermission("/spend/logs")) {
			scope.teams = append(scope.teams, id)
		}
	}
	if id := r.URL.Query().Get("team_id"); id != "" && !slices.Contains(scope.teams, id) {
		modelError(w, http.StatusForbidden, "Not authorized to view team spend")
		return spendLogScope{}, false
	}
	return scope, true
}

// Request filters only narrow the caller's authorized rows.
func (scope spendLogScope) apply(where string, args []any) (string, []any) {
	if scope.user == "" {
		return where, args
	}
	user := "user_id = $" + strconv.Itoa(len(args)+1)
	args = append(args, scope.user)
	if len(scope.teams) == 0 {
		return where + " AND " + user, args
	}
	team := "team_id = ANY($" + strconv.Itoa(len(args)+1) + "::text[])"
	return where + " AND (" + user + " OR " + team + ")", append(args, pq.Array(scope.teams))
}

func spendLogOrder(query url.Values) (string, bool) {
	var column string
	switch query.Get("sort_by") {
	case "", "startTime":
		column = "started_at"
	case "endTime":
		column = "ended_at"
	case "spend", "total_tokens", "model":
		column = query.Get("sort_by")
	case "request_duration_ms":
		column = "(ended_at - started_at)"
	case "ttft_ms":
		column = "first_output_seconds"
	default:
		return "", false
	}
	switch strings.ToLower(query.Get("sort_order")) {
	case "", "desc":
		return column + " DESC NULLS LAST", true
	case "asc":
		return column + " ASC NULLS LAST", true
	default:
		return "", false
	}
}

