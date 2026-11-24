package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

type PendingAssistantRun struct {
	ID, Deployment, Provider, Path, TargetHash, RunID string
	Event InferenceEvent
	KeyDigest, KeySpendIdentity, ModelBudgetDuration string
	Attribution *SpendAttribution
	UsagePrice [2]*float64
	CacheReadPrice, CacheCreationPrice *float64
	AvailableAt time.Time
}

type AssistantRunPersistence interface {
	Schedule(context.Context, PendingAssistantRun) error
	Complete(context.Context, string, InferenceEvent) error
}

func (s *Server) RecoverAssistantRun(ctx context.Context, job PendingAssistantRun) (InferenceEvent, error) {
	candidate, allowed := s.selectResourceCandidate("", job.Deployment, job.Provider, nil)
	if !allowed {
		return InferenceEvent{}, fmt.Errorf("assistant run deployment is unavailable")
	}
	target := resourceUpstreamURL(candidate, job.Path)
	if fmt.Sprintf("%x", sha256.Sum256([]byte(target))) != job.TargetHash {
		return InferenceEvent{}, fmt.Errorf("assistant run deployment target changed")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return InferenceEvent{}, err
	}
	upstream.Header.Set("OpenAI-Beta", "assistants=v2")
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if err := providerKeyHeaders(ctx, upstream.Header, candidate, key); err != nil {
		return InferenceEvent{}, err
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return InferenceEvent{}, ctx.Err()
	}
	initial, _ := json.Marshal(map[string]string{"id": job.RunID, "status": "in_progress"})
	status, _, data, err := s.pollAssistantRun(upstream, http.StatusOK, nil, initial)
	if err != nil || status/100 != 2 {
		return InferenceEvent{}, fmt.Errorf("could not recover assistant run state")
	}
	recorder := callbackResponse{event: job.Event, status: status, capture: s.config.LogMessages,
		price: job.UsagePrice, cachePrice: job.CacheReadPrice, cacheCreationPrice: job.CacheCreationPrice}
	recorder.event.KeyDigest, recorder.event.KeySpendIdentity = job.KeyDigest, job.KeySpendIdentity
	recorder.event.KeyModelBudgetDuration = job.ModelBudgetDuration
	recorder.event.SpendAttribution = job.Attribution
	if origin, err := url.Parse(candidate.APIBase); err == nil {
		recorder.event.APISource = origin.Scheme + "://" + origin.Host
	}
	recorder.recordJSON(data)
	recorder.finish()
	recorder.event.ID = job.ID
	return recorder.event, nil
}

func (s *Server) persistAssistantRun(candidate Candidate, path, runID string, event InferenceEvent, availableAt time.Time) string {
	if s.config.AssistantRuns == nil {
		return ""
	}
	target := resourceUpstreamURL(candidate, path)
	id := assistantSpendID(candidate.DeploymentID, runID)
	job := PendingAssistantRun{ID: id, Deployment: candidate.DeploymentID, Provider: candidate.Provider,
		Path: path, TargetHash: fmt.Sprintf("%x", sha256.Sum256([]byte(target))), RunID: runID, Event: event,
		KeyDigest: event.KeyDigest, KeySpendIdentity: event.KeySpendIdentity, ModelBudgetDuration: event.KeyModelBudgetDuration,
		Attribution: event.SpendAttribution, UsagePrice: candidate.UsagePrice, CacheReadPrice: candidate.CacheReadPrice,
		CacheCreationPrice: candidate.CacheCreationPrice, AvailableAt: availableAt}
	job.Event.Messages, job.Event.Response = nil, nil
	job.Event.Model = candidate.ModelID
	job.Event.APISource = ""
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.config.AssistantRuns.Schedule(ctx, job) != nil {
		return ""
	}
	return id
}

func assistantSpendID(deployment, runID string) string {
	return fmt.Sprintf("assistant-run-%x", sha256.Sum256([]byte(deployment+"\x00"+runID)))
}

