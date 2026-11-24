package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func assistantFamily(path string) string {
	for _, family := range []string{"assistants", "threads"} {
		base := "/v1/" + family
		if path == base || strings.HasPrefix(path, base+"/") {
			return family
		}
	}
	return ""
}

func assistantOperation(path, method string, deployment *string) (string, string, error) {
	family := assistantFamily(path)
	base := "/v1/" + family
	if path == base {
		if method == http.MethodPost || (family == "assistants" && method == http.MethodGet) {
			return base, family, nil
		}
		return "", "", errors.New("unsupported assistant operation")
	}
	parts := strings.Split(strings.TrimPrefix(path, base+"/"), "/")
	id, err := decodeResourceSegment(parts[0], family, deployment)
	if err != nil {
		return "", "", err
	}
	target := base + "/" + url.PathEscape(id)
	if len(parts) == 1 && ((family == "assistants" && method == http.MethodDelete) ||
		(family == "threads" && method == http.MethodGet)) {
		return target, family, nil
	}
	if family == "threads" && len(parts) == 2 {
		switch parts[1] {
		case "messages":
			if method == http.MethodGet || method == http.MethodPost {
				return target + "/messages", "thread_messages", nil
			}
		case "runs":
			if method == http.MethodPost {
				return target + "/runs", "thread_runs", nil
			}
		}
	}
	return "", "", errors.New("unsupported assistant operation")
}

func (s *Server) assistantResource(w http.ResponseWriter, r *http.Request, scope []string) {
	var deployment string
	path, family, err := assistantOperation(r.URL.EscapedPath(), r.Method, &deployment)
	if err != nil {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown assistant operation")
		return
	}
	query := r.URL.Query()
	for _, name := range []string{"after", "before"} {
		for _, value := range query[name] {
			if _, err := nativeResourceID(value, family, &deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid assistant cursor")
				return
			}
		}
	}
	alias, provider := query.Get("model"), query.Get("custom_llm_provider")
	if alias == "" {
		alias = r.Header.Get("X-LiteLLM-Model")
	}
	var payload map[string]any
	if r.Method == http.MethodPost {
		body, err := readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Assistant request body is too large")
			return
		}
		if len(body) == 0 && family == "threads" {
			payload = map[string]any{}
		} else if json.Unmarshal(body, &payload) != nil || payload == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Expected an assistant JSON request")
			return
		}
		if model, _ := payload["model"].(string); model != "" {
			alias = model
		}
		if configured, _ := payload["custom_llm_provider"].(string); configured != "" {
			provider = configured
		}
		if family == "thread_runs" {
			id, _ := payload["assistant_id"].(string)
			native, err := nativeResourceID(id, "assistants", &deployment)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid assistant ID or deployment mismatch")
				return
			}
			payload["assistant_id"] = native
		}
		delete(payload, "custom_llm_provider")
	}
	if provider == "" && alias == "" && deployment == "" {
		provider = "openai"
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, provider, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured Assistants deployment")
		return
	}
	var body []byte
	contentType := ""
	if payload != nil {
		switch family {
		case "assistants":
			payload["model"] = candidate.ModelID
		case "thread_runs":
			if _, exists := payload["model"]; exists {
				payload["model"] = candidate.ModelID
			}
		default:
			delete(payload, "model")
		}
		body, _ = json.Marshal(payload)
		contentType = "application/json"
	}
	if family == "thread_runs" {
		s.runAssistantThread(w, r, candidate, alias, path, payload, body)
		return
	}
	s.forwardOpenAIResource(w, r, candidate, family, path, contentType, body)
}

func (s *Server) runAssistantThread(w http.ResponseWriter, r *http.Request, candidate Candidate, alias, path string, payload map[string]any, body []byte) {
	stream, _ := payload["stream"].(bool)
	if !stream {
		timeout := candidate.Timeout
		if timeout <= 0 {
			timeout = s.config.HTTPClient.Timeout
		}
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	budget, _ := r.Context().Value(admittedResourceBudget{}).(KeyBudget)
	admission := keyAdmission{}
	if budget.hasTokenLimit() {
		admission.estimated = max(1, estimatedInputTokens(payload))
		limits := socketTokenLimits(budget)
		for _, limit := range limits {
			if limit.TPM == nil {
				continue
			}
			limit.Parallel = nil
			if !s.reserveInferenceLimits(w, r, limit, &admission) {
				admission.finish(nil)
				return
			}
		}
	}
	if s.config.Callback == nil && len(admission.settlements) == 0 {
		s.forwardOpenAIResource(w, r, candidate, "thread_runs", path, "application/json", body)
		return
	}
	if alias == "" {
		alias = candidate.RoutingGroup
	}
	event := newInferenceEvent("arun_thread", alias, stream)
	budget.apply(&event)
	if credential := ProxyCredential(r); credential != "" && !s.authorized(r) {
		event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(credential)))
	}
	event.EndUser = requestCustomerID(r, requestPayload{fields: payload})
	recorder := &callbackResponse{ResponseWriter: w, event: event, capture: s.config.LogMessages}
	writeRuntimeMeta(recorder, candidate, 1, nil)
	persistedID := ""
	observe := func(initial []byte) {
		if persistedID != "" || s.config.AssistantRuns == nil {
			return
		}
		var pending struct{ ID, Status string }
		if json.Unmarshal(initial, &pending) != nil || !assistantRunPending(pending.Status) || !validResourceSegment(pending.ID) {
			return
		}
		deadline, ok := r.Context().Deadline()
		available := time.Now().Add(135 * time.Second)
		if ok && deadline.After(time.Now()) {
			available = deadline.Add(135 * time.Second)
		}
		persistedID = s.persistAssistantRun(candidate, path, pending.ID, recorder.event, available)
	}
	continued := false
	resume := assistantRunContinuation(func(upstream *http.Request, initial []byte, release func()) bool {
		var pending struct{ ID, Status string }
		if json.Unmarshal(initial, &pending) != nil || !assistantRunPending(pending.Status) || !validResourceSegment(pending.ID) {
			return false
		}
		continued = true
		accounting := *recorder
		accounting.ResponseWriter = nil
		accounting.event.Model, accounting.event.Provider, accounting.event.ModelID = candidate.ModelID, candidate.Provider, candidate.DeploymentID
		observe(initial)
		persisted := persistedID
		go func() {
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			status, _, body, err := s.pollAssistantRun(upstream.WithContext(ctx), http.StatusOK, nil, initial)
			if err != nil {
				status = http.StatusBadGateway
			}
			accounting.status = status
			if status/100 == 2 {
				accounting.recordJSON(wrapOpenAIResource(body, "thread_runs", candidate.DeploymentID))
			}
			if persisted != "" && status/100 == 2 {
				accounting.event.ID = persisted
			}
			s.finishAssistantRun(&accounting, candidate, admission)
			if persisted != "" && status/100 == 2 {
				completeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = s.config.AssistantRuns.Complete(completeCtx, persisted, accounting.event)
			}
		}()
		return true
	})
	r = r.WithContext(context.WithValue(r.Context(), assistantRunContinuationKey{}, resume))
	r = r.WithContext(context.WithValue(r.Context(), assistantRunObserverKey{}, observe))
	s.forwardOpenAIResource(recorder, r, candidate, "thread_runs", path, "application/json", body)
	if continued {
		return
	}
	if recorder.status == 0 {
		admission.finish(nil)
		return
	}
	if persistedID != "" && recorder.status/100 == 2 {
		recorder.event.ID = persistedID
	}
	s.finishAssistantRun(recorder, candidate, admission)
	if persistedID != "" && recorder.status/100 == 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.config.AssistantRuns.Complete(ctx, persistedID, recorder.event)
	}
}

type assistantRunObserverKey struct{}

func observeAssistantRun(request *http.Request, pending []byte) {
	if observe, ok := request.Context().Value(assistantRunObserverKey{}).(func([]byte)); ok {
		observe(pending)
	}
}

type assistantRunContinuationKey struct{}
type assistantRunContinuation func(*http.Request, []byte, func()) bool

func continueAssistantRun(request *http.Request, upstream *http.Request, pending []byte, release func()) bool {
	resume, ok := request.Context().Value(assistantRunContinuationKey{}).(assistantRunContinuation)
	return ok && resume(upstream, pending, release)
}

func (s *Server) finishAssistantRun(recorder *callbackResponse, candidate Candidate, admission keyAdmission) {
	if !strings.HasPrefix(recorder.event.ID, "assistant-run-") && recorder.event.ResponseID != "" && recorder.event.Total > 0 {
		runID := recorder.event.ResponseID
		if resource, ok := decodeResourceID(runID, "thread_runs"); ok {
			runID = resource.Original
		}
		recorder.event.ID = assistantSpendID(candidate.DeploymentID, runID)
	}
	recorder.finish()
	admission.finish(recorder)
	if s.config.Callback != nil {
		s.config.Callback(recorder.event)
	}
}

func (s *Server) pollAssistantRun(request *http.Request, status int, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	var run struct{ ID, Status string }
	if json.Unmarshal(body, &run) != nil || !assistantRunPending(run.Status) {
		return status, headers, body, nil
	}
	if !validResourceSegment(run.ID) {
		return 0, nil, nil, errors.New("invalid provider run ID")
	}
	poll := request.Clone(request.Context())
	poll.Method, poll.Body, poll.GetBody, poll.ContentLength = http.MethodGet, nil, nil, 0
	poll.URL.RawPath = strings.TrimRight(request.URL.EscapedPath(), "/") + "/" + url.PathEscape(run.ID)
	poll.URL.Path = strings.TrimRight(request.URL.Path, "/") + "/" + run.ID
	poll.Header.Del("Content-Type")
	poll.Header.Set("Accept", "application/json")
	poll.Header.Set("X-Stainless-Poll-Helper", "true")

	var interval time.Duration
	runID := run.ID
	for {
		response, err := s.config.HTTPClient.Do(poll)
		if err != nil {
			return 0, nil, nil, err
		}
		body, err = io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
		response.Body.Close()
		if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
			return 0, nil, nil, errors.New("could not read assistant run")
		}
		status, headers = response.StatusCode, response.Header
		if status/100 != 2 {
			return status, headers, body, nil
		}
		run.ID, run.Status = "", ""
		if json.Unmarshal(body, &run) != nil || run.ID != runID || run.Status == "" {
			return 0, nil, nil, errors.New("invalid provider run response")
		}
		if !assistantRunPending(run.Status) {
			return status, headers, body, nil
		}
		if interval == 0 {
			milliseconds, err := strconv.Atoi(headers.Get("OpenAI-Poll-After-Ms"))
			if err != nil || milliseconds < 1 {
				milliseconds = 1000
			}
			interval = time.Duration(min(milliseconds, 60000)) * time.Millisecond
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-request.Context().Done():
			timer.Stop()
			return 0, nil, nil, request.Context().Err()
		}
	}
}

func assistantRunPending(status string) bool {
	return status == "queued" || status == "in_progress" || status == "cancelling"
}

func assistantResourceKind(object string) string {
	switch object {
	case "assistant", "assistant.deleted":
		return "assistants"
	case "thread", "thread.deleted":
		return "threads"
	case "thread.message", "thread.message.delta":
		return "thread_messages"
	case "thread.run", "thread.run.step", "thread.run.step.delta":
		return "thread_runs"
	}
	return ""
}
