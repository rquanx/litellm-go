package runtime

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type admittedResourceBudget struct{}

type keyAdmission struct {
	releases []func()
	settlements []func(int)
	estimated int
}

func (s *Server) serveRateLimitedResource(w http.ResponseWriter, r *http.Request, scope []string,
	serve func(http.ResponseWriter, *http.Request, []string)) {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		// Cancellation stays available when an allowance is exhausted.
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			budget, allowed := s.checkKeyBudget(w, r)
			if !allowed {
				return
			}
			admission, allowed := s.admitKeyLimits(w, r, budget, -1)
			if !allowed {
				return
			}
			defer admission.finish(nil)
			r = r.WithContext(context.WithValue(r.Context(), admittedResourceBudget{}, budget))
		}
	}
	serve(w, r, scope)
}

func (admission keyAdmission) finish(recorder *callbackResponse) {
	for _, release := range admission.releases {
		defer release()
	}
	actual := 0
	if recorder != nil {
		actual = recorder.event.Total
		if actual == 0 && recorder.status >= 200 && recorder.status < 300 {
			actual = admission.estimated
		}
	}
	for _, settle := range admission.settlements {
		settle(actual)
	}
}

func (s *Server) admitKeyLimits(w http.ResponseWriter, r *http.Request, budget KeyBudget, estimated int) (keyAdmission, bool) {
	admission := keyAdmission{estimated: estimated}
	key := OwnerRateLimits{Identity: budget.SpendIdentity, RPM: budget.RPMLimit, TPM: budget.TPMLimit, Parallel: budget.ParallelLimit}
	if !s.reserveInferenceLimits(w, r, key, &admission) {
		admission.finish(nil)
		return keyAdmission{}, false
	}
	for _, owner := range budget.OwnerLimits {
		if !s.reserveInferenceLimits(w, r, owner, &admission) {
			admission.finish(nil)
			return keyAdmission{}, false
		}
	}
	return admission, true
}

func (s *Server) reserveInferenceLimits(w http.ResponseWriter, r *http.Request, limit OwnerRateLimits, admission *keyAdmission) bool {
	if limit.Parallel != nil {
		release, err := s.acquireKeyParallel(r.Context(), limit.Identity, *limit.Parallel)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "rate_limit_unavailable", "Could not read concurrency limit")
			return false
		}
		if release == nil {
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Key parallel request limit exceeded")
			return false
		}
		admission.releases = append(admission.releases, release)
	}
	if limit.TPM == nil || admission.estimated < 0 {
		return true
	}
	settle, retry, err := s.reserveKeyTokens(r.Context(), limit.Identity, *limit.TPM, admission.estimated, time.Now().Unix())
	if err == nil && retry == 0 {
		admission.settlements = append(admission.settlements, settle)
		return true
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "rate_limit_unavailable", "Could not read key token limit")
	} else {
		w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
		writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Key token limit exceeded")
	}
	return false
}

func (budget KeyBudget) hasTokenLimit() bool {
	if budget.TPMLimit != nil {
		return true
	}
	for _, owner := range budget.OwnerLimits {
		if owner.TPM != nil {
			return true
		}
	}
	return false
}

func (s *Server) admitRequestRateLimits(w http.ResponseWriter, r *http.Request, budget KeyBudget) bool {
	retryAfter, err := s.reserveRequestRates(r.Context(), budget)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "rate_limit_unavailable", "Could not read key rate limit")
		return false
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Key request limit exceeded")
		return false
	}
	return true
}

func (s *Server) deploymentModelRateDefaults(alias string) OwnerRateLimits {
	s.mu.RLock()
	route, found := s.routes[alias]
	if !found {
		route = s.configuredRoutes[alias]
	}
	s.mu.RUnlock()

	var defaults OwnerRateLimits
	for _, candidate := range route.Candidates {
		if candidate.RoutingGroup != "" && candidate.RoutingGroup != alias {
			continue
		}
		if limit := candidate.DefaultKeyRPM; limit != nil && (defaults.RPM == nil || *limit < *defaults.RPM) {
			defaults.RPM = limit
		}
		if limit := candidate.DefaultKeyTPM; limit != nil && (defaults.TPM == nil || *limit < *defaults.TPM) {
			defaults.TPM = limit
		}
	}
	return defaults
}

func (s *Server) reserveRequestRates(ctx context.Context, budget KeyBudget) (int64, error) {
	var scopes []OwnerRateLimits
	for _, owner := range budget.OwnerLimits {
		if owner.RPM != nil {
			scopes = append(scopes, owner)
		}
	}
	if budget.RPMLimit != nil {
		scopes = append(scopes, OwnerRateLimits{Identity: budget.SpendIdentity, RPM: budget.RPMLimit})
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	if store, ok := s.config.RoutingState.(keyRPMStore); ok {
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		defer cancel()
		return store.ReserveRequestRates(bounded, scopes)
	}
	return s.reserveLocalRequestRates(scopes, time.Now().Unix()), nil
}

type keyRPMStore interface {
	ReserveRequestRates(ctx context.Context, scopes []OwnerRateLimits) (int64, error)
}

type keyTokenStore interface {
	ReserveKeyTokens(ctx context.Context, identity string, limit, estimated int) (string, int64, error)
	AdjustKeyTokens(ctx context.Context, identity, window string, delta int) error
}

type keyParallelStore interface {
	AcquireKeyParallel(ctx context.Context, identity, lease string, limit int) (bool, error)
	ReleaseKeyParallel(ctx context.Context, identity, lease string) error
	RenewKeyParallel(ctx context.Context, identity, lease string) (bool, error)
}

func (s *Server) acquireKeyParallel(ctx context.Context, identity string, limit int) (func(), error) {
	if limit <= 0 {
		return nil, nil
	}
	if store, ok := s.config.RoutingState.(keyParallelStore); ok {
		lease := newUUID()
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		admitted, err := store.AcquireKeyParallel(bounded, identity, lease, limit)
		cancel()
		if err != nil || !admitted {
			return nil, err
		}
		leaseCtx, stopLease := context.WithCancel(ctx)
		var refresh func()
		refresh = func() {
			if leaseCtx.Err() != nil {
				return
			}
			bounded, cancel := context.WithTimeout(leaseCtx, routingStateTimeout)
			active, err := store.RenewKeyParallel(bounded, identity, lease)
			cancel()
			if leaseCtx.Err() == nil && (active || err != nil) {
				time.AfterFunc(3*time.Minute, refresh)
			}
		}
		timer := time.AfterFunc(3*time.Minute, refresh)
		return func() {
			stopLease()
			timer.Stop()
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingStateTimeout)
			defer cancel()
			_ = store.ReleaseKeyParallel(bounded, identity, lease)
		}, nil
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.keyParallel[identity] >= limit {
		return nil, nil
	}
	if s.keyParallel == nil {
		s.keyParallel = make(map[string]int)
	}
	s.keyParallel[identity]++
	return func() {
		s.trafficMu.Lock()
		defer s.trafficMu.Unlock()
		if s.keyParallel[identity] == 1 {
			delete(s.keyParallel, identity)
		} else {
			s.keyParallel[identity]--
		}
	}, nil
}

type keyRPMWindow struct {
	requests int
	expiresAt int64
}

type keyTokenWindow struct {
	tokens int
	expiresAt int64
}

func (s *Server) reserveKeyTokens(ctx context.Context, identity string, limit, estimated int, now int64) (func(int), int64, error) {
	if limit <= 0 || estimated > limit {
		return nil, 60, nil
	}
	if store, ok := s.config.RoutingState.(keyTokenStore); ok {
		bounded, cancel := context.WithTimeout(ctx, routingStateTimeout)
		window, retry, err := store.ReserveKeyTokens(bounded, identity, limit, estimated)
		cancel()
		if err != nil || retry > 0 {
			return nil, retry, err
		}
		return func(actual int) {
			if actual == estimated {
				return
			}
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingStateTimeout)
			defer cancel()
			_ = store.AdjustKeyTokens(bounded, identity, window, actual-estimated)
		}, 0, nil
	}
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.keyTokenWindows == nil {
		s.keyTokenWindows = make(map[string]keyTokenWindow)
	}
	if now >= s.keyTokenPruneAt {
		for key, window := range s.keyTokenWindows {
			if now >= window.expiresAt {
				delete(s.keyTokenWindows, key)
			}
		}
		s.keyTokenPruneAt = now + 60
	}
	window := s.keyTokenWindows[identity]
	if now >= window.expiresAt {
		window = keyTokenWindow{expiresAt: now + 60}
	}
	if window.tokens > limit || estimated > limit-window.tokens {
		return nil, window.expiresAt - now, nil
	}
	window.tokens += estimated
	s.keyTokenWindows[identity] = window
	return func(actual int) {
		s.trafficMu.Lock()
		defer s.trafficMu.Unlock()
		active := s.keyTokenWindows[identity]
		if active.expiresAt == window.expiresAt {
			active.tokens = max(0, active.tokens+actual-estimated)
			s.keyTokenWindows[identity] = active
		}
	}, 0, nil
}

func (s *Server) reserveLocalRequestRates(scopes []OwnerRateLimits, now int64) int64 {
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	if s.keyRPMWindows == nil {
		s.keyRPMWindows = make(map[string]keyRPMWindow)
	}
	if now >= s.keyRPMPruneAt {
		for key, window := range s.keyRPMWindows {
			if now >= window.expiresAt {
				delete(s.keyRPMWindows, key)
			}
		}
		s.keyRPMPruneAt = now + 60
	}
	var retry int64
	for _, scope := range scopes {
		if *scope.RPM <= 0 {
			return 60
		}
		window := s.keyRPMWindows[scope.Identity]
		if now < window.expiresAt && window.requests >= *scope.RPM {
			retry = max(retry, window.expiresAt-now)
		}
	}
	if retry != 0 {
		return retry
	}
	for _, scope := range scopes {
		window := s.keyRPMWindows[scope.Identity]
		if now >= window.expiresAt {
			window = keyRPMWindow{expiresAt: now + 60}
		}
		window.requests++
		s.keyRPMWindows[scope.Identity] = window
	}
	return 0
}
