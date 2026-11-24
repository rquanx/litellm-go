package proxyconfig

import (
	"net/http"
	"strings"
)

func (log *SpendLog) userActivity(w http.ResponseWriter, r *http.Request) {
	caller, ok := log.keys.identifyReadCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	user := r.URL.Query().Get("user_id")
	if !caller.admin {
		if caller.user == "" || (user != "" && user != caller.user) {
			modelError(w, http.StatusForbidden, "You can only view your own spend data")
			return
		}
		user = caller.user
	}
	log.entityActivity(w, r, "user", activityScope{user: user})
}

func (log *SpendLog) teamActivity(w http.ResponseWriter, r *http.Request) {
	scope, ok := log.teamActivityScope(w, r)
	if ok {
		log.entityActivity(w, r, "team", scope)
	}
}

func (log *SpendLog) teamActivityScope(w http.ResponseWriter, r *http.Request) (activityScope, bool) {
	log.keys.mu.RLock()
	defer log.keys.mu.RUnlock()
	caller, ok := log.keys.identifyReadCallerLocked(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return activityScope{}, false
	}
	if !caller.admin && caller.user == "" {
		modelError(w, http.StatusNotFound, "User not found")
		return activityScope{}, false
	}
	scope := activityScope{aliases: make(map[string]string)}
	if !caller.admin {
		scope.ids = []string{}
	}
	memberAdmin := make(map[string]bool)
	for id, team := range log.keys.state.Teams {
		if caller.admin {
			scope.aliases[id] = team.Alias
			continue
		}
		if index := memberIndex(team, caller.user); index >= 0 {
			scope.ids = append(scope.ids, id)
			scope.aliases[id] = team.Alias
			memberAdmin[id] = team.Members[index].Role == "admin" || team.hasMemberPermission("/team/daily/activity")
		}
	}
	if !caller.admin {
		if ids := r.URL.Query().Get("team_ids"); ids != "" {
			scope.ids = strings.Split(ids, ",")
		}
		for _, id := range scope.ids {
			admin, member := memberAdmin[id]
			if !member {
				modelError(w, http.StatusNotFound, "User does not belong to the requested team")
				return activityScope{}, false
			}
			if !admin {
				scope.user = caller.user
			}
		}
	}
	return scope, true
}
