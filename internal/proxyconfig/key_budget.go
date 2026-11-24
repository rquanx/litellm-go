package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

type keyBudgetLimit struct {
	Duration string     `json:"budget_duration"`
	Max      *float64   `json:"max_budget"`
	ResetAt  *time.Time `json:"reset_at,omitempty"`
	StartsAt *time.Time `json:"starts_at,omitempty"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
}

type importedKeySpend struct {
	Amount float64 `json:"amount"`
	AsOf time.Time `json:"as_of"`
	ResetAt *time.Time `json:"reset_at,omitempty"`
	Adjustment *keySpendAdjustment `json:"adjustment,omitempty"`
}

type keySpendAdjustment struct {
	Start  time.Time `json:"start"`
	Amount float64   `json:"amount"`
}

func keySpendDiscount(key virtualKey, start time.Time) float64 {
	if base := key.ImportedSpend; base != nil && base.Adjustment != nil && base.Adjustment.Start.Equal(start) {
		return base.Adjustment.Amount
	}
	return 0
}

func publicBudgetLimits(limits []keyBudgetLimit) []keyBudgetLimit {
	for _, limit := range limits {
		if limit.ImportedSpend == nil {
			continue
		}
		public := slices.Clone(limits)
		for i := range public {
			public[i].ImportedSpend = nil
		}
		return public
	}
	return limits
}

type ownerRates struct {
	RPM *int `json:"rpm_limit,omitempty"`
	TPM *int `json:"tpm_limit,omitempty"`
	Parallel *int `json:"max_parallel_requests,omitempty"`
}

type ownerRateChanges struct {
	RPM json.RawMessage `json:"rpm_limit"`
	TPM json.RawMessage `json:"tpm_limit"`
	Parallel json.RawMessage `json:"max_parallel_requests"`
}

func (changes ownerRateChanges) apply(rates *ownerRates) error {
	for _, field := range []struct {
		name string
		raw json.RawMessage
		value **int
	}{{"rpm_limit", changes.RPM, &rates.RPM}, {"tpm_limit", changes.TPM, &rates.TPM},
		{"max_parallel_requests", changes.Parallel, &rates.Parallel}} {
		if field.raw != nil {
			var limit *int
			if json.Unmarshal(field.raw, &limit) != nil || (limit != nil && *limit < 0) {
				return fmt.Errorf("%s must be a non-negative integer or null", field.name)
			}
			*field.value = limit
		}
	}
	return nil
}

func applyOwnerLimits(result *runtime.KeyBudget, user internalUser, owner team) {
	for _, scope := range []struct {
		id string
		rates ownerRates
	}{{"user:" + user.ID, user.ownerRates}, {"team:" + owner.ID, owner.ownerRates}} {
		if scope.rates.RPM != nil || scope.rates.TPM != nil || scope.rates.Parallel != nil {
			result.OwnerLimits = append(result.OwnerLimits, runtime.OwnerRateLimits{
				Identity: scope.id, RPM: scope.rates.RPM, TPM: scope.rates.TPM, Parallel: scope.rates.Parallel,
			})
		}
	}
}

func (keys *KeyRegistry) validateBudgetID(ctx context.Context, raw json.RawMessage) (string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", errors.New("invalid budget_id")
	}
	if id == "" {
		return "", nil
	}
	if keys.budgetMaximum == nil {
		return "", errors.New("budget_id requires PostgreSQL spend tracking")
	}
	if _, err := keys.budgetMaximum(ctx, id); err != nil {
		return "", errors.New("could not use the specified budget")
	}
	return id, nil
}

func (keys *KeyRegistry) updateBudget(maximum, duration json.RawMessage, max **float64, period *string, reset **time.Time) error {
	if maximum != nil {
		var limit *float64
		if json.Unmarshal(maximum, &limit) != nil || (limit != nil && (*limit < 0 || keys.keySpend == nil)) {
			return errors.New("max_budget must be non-negative and requires PostgreSQL spend tracking")
		}
		*max = limit
	}
	if duration != nil {
		var value string
		if json.Unmarshal(duration, &value) != nil {
			return errors.New("Invalid budget_duration")
		}
		key := virtualKey{BudgetDuration: *period, BudgetResetAt: *reset}
		if err := keys.setKeyBudgetDuration(&key, value, time.Now()); err != nil {
			return err
		}
		*period, *reset = key.BudgetDuration, key.BudgetResetAt
	}
	return nil
}

func (keys *KeyRegistry) SetBudgetTimezone(name string) {
	keys.budgetZone = budgetZone(name)
}

func (keys *KeyRegistry) keyBudgetZone() *time.Location {
	if keys.budgetZone != nil {
		return keys.budgetZone
	}
	return time.UTC
}

func (keys *KeyRegistry) setKeyBudgetDuration(key *virtualKey, duration string, now time.Time) error {
	if duration == "" {
		key.BudgetDuration, key.BudgetResetAt = "", nil
		return nil
	}
	if keys.keySpend == nil {
		return errors.New("budget_duration requires PostgreSQL spend tracking")
	}
	if key.BudgetDuration == duration && key.BudgetResetAt != nil {
		return nil
	}
	reset, err := nextBudgetReset(now.In(keys.keyBudgetZone()), duration)
	if err != nil {
		return err
	}
	key.BudgetDuration, key.BudgetResetAt = duration, &reset
	return nil
}

func (keys *KeyRegistry) keySpendWindow(key virtualKey, now time.Time) (time.Time, *time.Time, float64) {
	return keys.spendWindow(key.BudgetDuration, key.BudgetResetAt, key.ImportedSpend, now)
}

func (keys *KeyRegistry) keySpendWindowWithTier(ctx context.Context, key virtualKey, now time.Time) (time.Time, *time.Time, float64, error) {
	start, reset, baseline := keys.keySpendWindow(key, now)
	if key.BudgetID == "" || key.BudgetDuration != "" || keys.budgetPeriod == nil {
		return start, reset, baseline - keySpendDiscount(key, start), nil
	}
	tier, err := keys.budgetPeriod(ctx, key.BudgetID)
	if tier.Start.After(start) {
		start, baseline = tier.Start, 0
	}
	return start, reset, baseline - keySpendDiscount(key, start), err
}

func (keys *KeyRegistry) spendWindow(duration string, anchor *time.Time, imported *importedKeySpend, now time.Time) (time.Time, *time.Time, float64) {
	start, reset := keys.budgetWindow(duration, anchor, now)
	// The verification token already accounts for spend before the snapshot,
	// including historical logs that may be imported separately.
	if imported != nil && (imported.ResetAt == nil || now.Before(*imported.ResetAt)) &&
		(start.IsZero() || start.Before(imported.AsOf)) {
		return imported.AsOf, reset, imported.Amount
	}
	return start, reset, 0
}

func (keys *KeyRegistry) budgetWindow(duration string, reset *time.Time, now time.Time) (time.Time, *time.Time) {
	if duration == "" || reset == nil || now.Before(*reset) {
		return time.Time{}, reset
	}
	zone := keys.keyBudgetZone()
	anchor := reset.In(zone)
	local := now.In(zone)
	parts := budgetDurationPattern.FindStringSubmatch(duration)
	if parts == nil {
		return time.Time{}, reset
	}
	cadence, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || cadence <= 0 {
		return time.Time{}, reset
	}
	if parts[2] == "mo" || (parts[2] == "d" && cadence == 30) {
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, zone)
		next := start.AddDate(0, 1, 0).UTC()
		return start.UTC(), &next
	}
	if parts[2] == "d" || parts[2] == "w" {
		if parts[2] == "w" {
			cadence *= 7
		}
		dayNumber := func(t time.Time) int64 {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
		}
		days := dayNumber(local) - dayNumber(anchor)
		start := anchor.AddDate(0, 0, int(days-days%cadence))
		next := start.AddDate(0, 0, int(cadence)).UTC()
		return start.UTC(), &next
	}
	unit := map[string]int64{"s": 1, "m": 60, "h": 3600}[parts[2]]
	seconds := cadence * unit
	delta := now.Unix() - anchor.Unix()
	start := time.Unix(now.Unix()-delta%seconds, 0).UTC()
	next := time.Unix(start.Unix()+seconds, 0).UTC()
	return start, &next
}

func (keys *KeyRegistry) setBudgetLimits(previousLimits *[]keyBudgetLimit, limits []keyBudgetLimit, now time.Time) error {
	if len(limits) != 0 && keys.keySpend == nil {
		return errors.New("budget_limits requires PostgreSQL spend tracking")
	}
	previous := make(map[string]keyBudgetLimit, len(*previousLimits))
	for _, limit := range *previousLimits {
		previous[limit.Duration] = limit
	}
	seen := make(map[string]bool, len(limits))
	start := now.UTC()
	for i, limit := range limits {
		if limit.Max == nil || *limit.Max < 0 || seen[limit.Duration] {
			return errors.New("budget_limits requires a non-negative max_budget and unique durations")
		}
		seen[limit.Duration] = true
		reset, err := nextBudgetReset(now.In(keys.keyBudgetZone()), limit.Duration)
		if err != nil {
			return err
		}
		limits[i].StartsAt, limits[i].ResetAt = &start, &reset
		limits[i].ImportedSpend = nil
		if old, ok := previous[limit.Duration]; ok {
			limits[i].StartsAt, limits[i].ResetAt = old.StartsAt, old.ResetAt
			limits[i].ImportedSpend = old.ImportedSpend
		}
	}
	*previousLimits = limits
	return nil
}

func (keys *KeyRegistry) budgetLimitWindows(limits []keyBudgetLimit, now time.Time) ([]time.Time, []float64) {
	starts := make([]time.Time, 0, len(limits))
	balances := make([]float64, 0, len(limits))
	for _, limit := range limits {
		start, _, balance := keys.spendWindow(limit.Duration, limit.ResetAt, limit.ImportedSpend, now)
		if start.IsZero() && limit.StartsAt != nil {
			start = *limit.StartsAt
		}
		starts = append(starts, start)
		balances = append(balances, balance)
	}
	return starts, balances
}

func (keys *KeyRegistry) keyForBudget(token string) (string, virtualKey, internalUser, team, project, organization) {
	digest := keyDigest(token)
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	key := keys.state.Keys[digest]
	if key.RotationTarget != "" {
		digest = key.RotationTarget
		key = keys.state.Keys[digest]
	}
	workspace := keys.state.Projects[key.ProjectID]
	if key.TeamID == "" {
		key.TeamID = workspace.TeamID
	}
	owner := keys.state.Teams[key.TeamID]
	user := keys.state.Users[key.UserID]
	if key.OrgID == "" {
		key.OrgID = owner.OrgID
		if key.OrgID == "" {
			key.OrgID = user.DefaultOrgID
		}
	}
	return digest, key, user, owner, workspace, keys.state.Organizations[key.OrgID]
}

func (keys *KeyRegistry) CheckKeyBudgetWithoutStore(_ context.Context, token string) (runtime.KeyBudget, error) {
	digest, key, user, owner, workspace, org := keys.keyForBudget(token)
	hasMemberBudget := owner.memberBudget(key.UserID).MaxBudget != nil
	if key.MaxBudget != nil || key.BudgetID != "" || len(key.BudgetLimits) != 0 || (key.TeamID == "" && user.MaxBudget != nil) ||
		owner.MaxBudget != nil || len(owner.BudgetLimits) != 0 || hasMemberBudget || workspace.BudgetID != "" || org.BudgetID != "" {
		return runtime.KeyBudget{}, errors.New("spend tracking is disabled")
	}
	result := runtime.KeyBudget{SpendIdentity: keyBudgetSpendIdentity(key, digest), RPMLimit: key.RPMLimit,
		TPMLimit: key.TPMLimit, ParallelLimit: key.ParallelLimit, RouterSettings: scopedRouterSettings(key, owner)}
	applyOwnerLimits(&result, user, owner)
	result.ModelPolicy = keyModelPolicy(key, result.SpendIdentity, nil)
	if digest != "" {
		result.ModelRates = keyModelRateLimits(key, owner, workspace, org, result.SpendIdentity)
	}
	return result, nil
}

func keyBudgetSpendIdentity(key virtualKey, digest string) string {
	if key.SpendIdentity != "" {
		return key.SpendIdentity
	}
	return digest
}

func (log *SpendLog) CheckKeyBudget(ctx context.Context, token string) (runtime.KeyBudget, error) {
	digest, key, user, owner, workspace, org := log.keys.keyForBudget(token)
	result := runtime.KeyBudget{SpendIdentity: keyBudgetSpendIdentity(key, digest), RPMLimit: key.RPMLimit,
		TPMLimit: key.TPMLimit, ParallelLimit: key.ParallelLimit, RouterSettings: scopedRouterSettings(key, owner)}
	applyOwnerLimits(&result, user, owner)
	result.ModelPolicy = keyModelPolicy(key, result.SpendIdentity, log)
	if digest != "" {
		result.ModelRates = keyModelRateLimits(key, owner, workspace, org, result.SpendIdentity)
	}
	result.Attribution = &runtime.SpendAttribution{KeyAlias: key.Alias, UserID: key.UserID, TeamID: key.TeamID, ProjectID: key.ProjectID, OrgID: key.OrgID}
	// Team-bound keys are subject to team policies. Their usage can still
	// contribute to the user's recorded balance, so settle it on completion.
	result.Tracked = key.MaxBudget != nil || len(key.BudgetLimits) != 0 || user.MaxBudget != nil || owner.MaxBudget != nil || len(owner.BudgetLimits) != 0
	if org.BudgetID != "" {
		tier, err := log.budgetPeriod(ctx, org.BudgetID)
		if err != nil {
			return result, err
		}
		if limit := tier.Max; limit != nil && *limit > 0 {
			result.Tracked = true
			start, baseline := tier.Start, 0.0
			if imported := org.ImportedSpend; imported != nil && start.Before(imported.AsOf) {
				start, baseline = imported.AsOf, imported.Amount
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			var total float64
			err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend),0) FROM minato_proxy_spend
				WHERE organization_id=$1 AND started_at >= $2`, org.ID, start).Scan(&total)
			if err != nil {
				return result, err
			}
			if total += baseline; total >= *limit {
				result.ErrorMessage = fmt.Sprintf("Budget has been exceeded! Organization=%s Current cost: %g, Max budget: %g", org.ID, total, *limit)
				return result, nil
			}
		}
	}
	if workspace.BudgetID != "" {
		limit, err := log.budgetMaximum(ctx, workspace.BudgetID)
		if err != nil {
			return result, err
		}
		if limit != nil {
			result.Tracked = true
			start, _, baseline := log.keys.spendWindow("", nil, workspace.ImportedSpend, time.Now())
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			var spend float64
			err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend),0) FROM minato_proxy_spend
				WHERE project_id=$1 AND started_at >= $2`, workspace.ID, start).Scan(&spend)
			if err != nil {
				return result, err
			}
			spend += baseline
			if spend > *limit {
				result.ErrorMessage = fmt.Sprintf("Budget has been exceeded! Project=%s Current cost: %g, Max budget: %g", workspace.ID, spend, *limit)
				return result, nil
			}
		}
	}
	if member := owner.memberBudget(key.UserID); member.MaxBudget != nil {
		result.Tracked = true
		limit := *member.MaxBudget
		start, _, baseline := log.keys.spendWindow(member.BudgetDuration, member.BudgetResetAt, member.ImportedSpend, time.Now())
		spent, err := log.teamMemberSpendSince(ctx, owner.ID, key.UserID, start)
		if err != nil {
			return result, err
		}
		spent += baseline
		if spent >= limit {
			result.ErrorMessage = fmt.Sprintf("ExceededBudget: Team member %s in team %s. Spend=%g, Budget=%g", key.UserID, owner.ID, spent, limit)
			return result, nil
		}
	}
	if owner.MaxBudget != nil {
		start, _, baseline := log.keys.spendWindow(owner.BudgetDuration, owner.BudgetResetAt, owner.ImportedSpend, time.Now())
		spent, err := log.TeamSpend(ctx, owner.ID, start)
		if err != nil {
			return result, err
		}
		spent += baseline
		if spent > *owner.MaxBudget {
			result.ErrorMessage = fmt.Sprintf("Budget has been exceeded! Team=%s Current cost: %g, Max budget: %g", owner.ID, spent, *owner.MaxBudget)
			return result, nil
		}
	}
	if len(owner.BudgetLimits) != 0 {
		starts, balances := log.keys.budgetLimitWindows(owner.BudgetLimits, time.Now())
		totals, err := log.budgetTotals(ctx, teamBudgetIdentity, owner.ID, starts)
		if err != nil {
			return result, err
		}
		for i, limit := range owner.BudgetLimits {
			totals[i] += balances[i]
			if limit.Max != nil && totals[i] >= *limit.Max {
				result.ErrorMessage = fmt.Sprintf("ExceededBudget: Team=%s over %s budget. Spend=$%.4f, Limit=$%.2f", owner.ID, limit.Duration, totals[i], *limit.Max)
				return result, nil
			}
		}
	}
	if key.TeamID == "" && user.MaxBudget != nil {
		start, _, baseline := log.keys.spendWindow(user.BudgetDuration, user.BudgetResetAt, user.ImportedSpend, time.Now())
		spent, err := log.UserSpend(ctx, user.ID, start)
		if err != nil {
			return result, err
		}
		spent += baseline
		if spent >= *user.MaxBudget {
			result.ErrorMessage = fmt.Sprintf("ExceededBudget: User=%s over budget. Spend=%g, Budget=%g", user.ID, spent, *user.MaxBudget)
			return result, nil
		}
	}
	var tierStart time.Time
	maximum := key.MaxBudget
	if key.BudgetID != "" {
		tier, err := log.budgetPeriod(ctx, key.BudgetID)
		if err != nil {
			return result, err
		}
		tierStart = tier.Start
		if maximum == nil {
			maximum = tier.Max
		}
		if result.RPMLimit == nil {
			result.RPMLimit = tier.RPM
		}
		if result.TPMLimit == nil {
			result.TPMLimit = tier.TPM
		}
		if result.ParallelLimit == nil {
			result.ParallelLimit = tier.Parallel
		}
	}
	if maximum == nil && len(key.BudgetLimits) == 0 {
		return result, nil
	}
	result.Tracked = true
	now := time.Now()
	starts := make([]time.Time, 0, len(key.BudgetLimits)+1)
	var baseline float64
	if maximum != nil {
		start, _, imported := log.keys.keySpendWindow(key, now)
		if key.BudgetDuration == "" && tierStart.After(start) {
			start, imported = tierStart, 0
		}
		baseline = imported - keySpendDiscount(key, start)
		starts = append(starts, start)
	}
	windowStarts, balances := log.keys.budgetLimitWindows(key.BudgetLimits, now)
	starts = append(starts, windowStarts...)
	totals, err := log.budgetTotals(ctx, keyBudgetIdentity, result.SpendIdentity, starts)
	if err != nil {
		return result, err
	}
	if maximum != nil {
		if spent := baseline + totals[0]; spent >= *maximum {
			result.ErrorMessage = fmt.Sprintf("Budget has been exceeded! Current cost: %g, Max budget: %g", spent, *maximum)
			return result, nil
		}
		totals = totals[1:]
	}
	for i, limit := range key.BudgetLimits {
		totals[i] += balances[i]
		if limit.Max != nil && totals[i] >= *limit.Max {
			result.ErrorMessage = fmt.Sprintf("ExceededBudget: Key over %s budget. Spend=$%.4f, Limit=$%.2f", limit.Duration, totals[i], *limit.Max)
			break
		}
	}
	return result, nil
}

const keyBudgetIdentity = "COALESCE(NULLIF(log.key_spend_identity,''),log.api_key)"
const teamBudgetIdentity = "log.team_id"

func (log *SpendLog) budgetTotals(ctx context.Context, identityColumn, identity string, starts []time.Time) ([]float64, error) {
	if len(starts) == 1 && identityColumn == keyBudgetIdentity {
		total, err := log.KeySpend(ctx, identity, starts[0])
		return []float64{total}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, `SELECT COALESCE(SUM(log.spend),0)
		FROM unnest($2::timestamptz[]) WITH ORDINALITY AS period(started_at, ordinal)
		LEFT JOIN minato_proxy_spend AS log ON log.spend IS NOT NULL
			AND `+identityColumn+`=$1 AND log.started_at >= period.started_at
		GROUP BY period.ordinal ORDER BY period.ordinal`, identity, pq.Array(starts))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := make([]float64, 0, len(starts))
	for rows.Next() {
		var total float64
		if err := rows.Scan(&total); err != nil {
			return nil, err
		}
		totals = append(totals, total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(totals) != len(starts) {
		return nil, errors.New("could not resolve all key budget windows")
	}
	return totals, nil
}

func (log *SpendLog) KeySpend(ctx context.Context, identity string, start time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var spent float64
	statement := `SELECT COALESCE(SUM(spend),0) FROM minato_proxy_spend
		WHERE spend IS NOT NULL AND COALESCE(NULLIF(key_spend_identity,''),api_key)=$1`
	args := []any{identity}
	if !start.IsZero() {
		statement += " AND started_at >= $2"
		args = append(args, start)
	}
	err := log.db.QueryRowContext(ctx, statement, args...).Scan(&spent)
	return spent, err
}
