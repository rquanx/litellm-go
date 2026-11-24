package proxyconfig

import (
	"cmp"
	"crypto/subtle"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (registry *modelRegistry) infoV2(w http.ResponseWriter, r *http.Request) {
	token := runtime.ProxyCredential(r)
	admin := (registry.token != "" && subtle.ConstantTimeCompare([]byte(registry.token), []byte(token)) == 1) ||
		registry.keys.adminViewer(r)
	var scope []string
	if !admin {
		var valid bool
		if registry.keys != nil {
			scope, valid = registry.keys.Authorize(token)
		}
		if !valid {
			modelError(w, http.StatusUnauthorized, "Proxy token required")
			return
		}
	}

	query := r.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "size", 50)
	if pageErr != nil || sizeErr != nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid page or size")
		return
	}
	sortBy, order := query.Get("sortBy"), strings.ToLower(query.Get("sortOrder"))
	includeTeams := false
	if raw := query.Get("include_team_models"); raw != "" {
		var err error
		includeTeams, err = strconv.ParseBool(raw)
		if err != nil {
			modelError(w, http.StatusUnprocessableEntity, "Invalid include_team_models")
			return
		}
	}
	teamFilter := strings.TrimSpace(query.Get("teamId"))
	var caller memoryCaller
	if !admin && (includeTeams || teamFilter != "" || query.Get("user_models_only") != "") {
		caller, _ = registry.keys.identifyResourceCaller(r)
	}
	teams, direct := registry.modelListingTeams(admin, caller, includeTeams, teamFilter)
	var creator string
	if raw := query.Get("user_models_only"); raw != "" {
		onlyUserModels, err := strconv.ParseBool(raw)
		if err != nil {
			modelError(w, http.StatusUnprocessableEntity, "Invalid user_models_only")
			return
		}
		if onlyUserModels {
			if admin {
				creator = "default_user_id"
			} else {
				creator = caller.user
			}
			if creator == "" {
				scope = []string{}
			}
		}
	}
	if sortBy != "" && order != "" && order != "asc" && order != "desc" {
		modelError(w, http.StatusBadRequest, "Invalid sortOrder. Must be 'asc' or 'desc'")
		return
	}
	type listedModel struct {
		deployment
		managed bool
		price   [2]*float64
		direct  bool
		teams   []string
	}
	registry.mu.Lock()
	models := append(slices.Clone(registry.base.models), registry.models...)
	registry.mu.Unlock()
	name, search, id := query.Get("model"), strings.ToLower(query.Get("search")), query.Get("modelId")
	filtered := make([]listedModel, 0, len(models))
	for index, model := range models {
		if (name != "" && model.Alias != name) || (id != "" && model.Info.ID != id) ||
			(search != "" && !strings.Contains(strings.ToLower(model.Alias), search)) ||
			(creator != "" && (index < len(registry.base.models) || model.Info.CreatedBy != creator)) {
			continue
		}
		if scope != nil && !slices.Contains(scope, "*") && !slices.Contains(scope, model.Alias) {
			continue
		}
		if !admin && registry.base.modelGroupAliases[model.Alias].Hidden {
			continue
		}
		directAccess := includeTeams && (admin || slices.Contains(direct, model.Alias))
		if teamFilter != "" {
			models, valid := teams[teamFilter]
			if !valid || (!directAccess && models != nil && !slices.Contains(models, model.Alias)) {
				continue
			}
		}
		var viaTeams []string
		if includeTeams {
			viaTeams = []string{}
			for id, models := range teams {
				if models == nil || slices.Contains(models, model.Alias) {
					viaTeams = append(viaTeams, id)
				}
			}
			if !directAccess && len(viaTeams) == 0 {
				continue
			}
			slices.Sort(viaTeams)
		}
		_, price, _ := tokenPrice(model.Params.Model, model.Params.InputPrice, model.Params.OutputPrice)
		filtered = append(filtered, listedModel{model, index >= len(registry.base.models), price, directAccess, viaTeams})
	}
	var compare func(listedModel, listedModel) int
	switch sortBy {
	case "model_name":
		compare = func(a, b listedModel) int {
			return cmp.Compare(strings.ToLower(a.Alias), strings.ToLower(b.Alias))
		}
	case "status":
		compare = func(a, b listedModel) int {
			if a.managed == b.managed {
				return 0
			}
			if a.managed {
				return 1
			}
			return -1
		}
	case "created_at":
		compare = func(a, b listedModel) int { return compareModelDates(a.Info.CreatedAt, b.Info.CreatedAt, order) }
	case "updated_at":
		compare = func(a, b listedModel) int { return compareModelDates(a.Info.UpdatedAt, b.Info.UpdatedAt, order) }
	case "costs":
		cost := func(model listedModel) float64 {
			var total float64
			for _, value := range model.price {
				if value != nil {
					total += *value
				}
			}
			if total == 0 {
				if order == "desc" {
					return math.Inf(-1)
				}
				return math.Inf(1)
			}
			return total
		}
		compare = func(a, b listedModel) int { return cmp.Compare(cost(a), cost(b)) }
	}
	if compare != nil {
		slices.SortStableFunc(filtered, func(a, b listedModel) int {
			if order == "desc" {
				return compare(b, a)
			}
			return compare(a, b)
		})
	}
	count := len(filtered)
	pages := count / size
	if count%size != 0 {
		pages++
	}
	result := []map[string]any{}
	if page <= pages {
		start := (page - 1) * size
		for _, model := range filtered[start : start+min(count-start, size)] {
			entry := publicModel(model.deployment)
			info := map[string]any{}
			encoded, _ := json.Marshal(model.Info)
			_ = json.Unmarshal(encoded, &info)
			info["db_model"] = model.managed
			if includeTeams {
				info["direct_access"] = model.direct
				info["access_via_team_ids"] = model.teams
			}
			if model.price[0] != nil {
				info["input_cost_per_token"] = *model.price[0]
			}
			if model.price[1] != nil {
				info["output_cost_per_token"] = *model.price[1]
			}
			entry["model_info"] = info
			result = append(result, entry)
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"data": result, "total_count": count, "current_page": page, "total_pages": pages, "size": size,
	})
}

func (registry *modelRegistry) modelListingTeams(admin bool, caller memoryCaller, includeTeams bool, filter string) (map[string][]string, []string) {
	if registry.keys == nil || (!includeTeams && filter == "") {
		return nil, nil
	}
	registry.keys.mu.RLock()
	defer registry.keys.mu.RUnlock()
	state := registry.keys.state
	teams := make(map[string][]string)
	for id, entry := range state.Teams {
		if entry.Blocked || (!includeTeams && id != filter) {
			continue
		}
		index := -1
		if caller.user != "" {
			index = memberIndex(entry, caller.user)
		}
		if !admin && id != caller.team && index < 0 {
			continue
		}
		models := unrestrictedScope(entry.Models)
		if !admin && index >= 0 {
			models = intersectScopes(models, unrestrictedScope(entry.Members[index].Models))
		}
		models = intersectScopes(models, unrestrictedScope(state.Organizations[entry.OrgID].Models))
		teams[id] = models
	}
	return teams, state.Users[caller.user].Models
}

func compareModelDates(a, b *time.Time, order string) int {
	if a != nil && b != nil {
		return a.Compare(*b)
	}
	if a == nil && b == nil {
		return 0
	}
	if (a == nil) == (order == "desc") {
		return -1
	}
	return 1
}
