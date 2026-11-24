package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

type Agent struct {
	AgentCore                         *BedrockConfig
	AgentJWT, AgentSession, AgentUser string
	Pydantic                          bool
	Completion                        *Candidate
	CompletionOptions                 map[string]any
	ID, Name, URL                     string
	Card                              map[string]any
	Headers                           http.Header
	ExtraHeaders                      []string
	AccessGroups                      []string
	RequireTraceID                    bool
	Public                            bool
}

func agentCard(r *http.Request, agent Agent, selector string) map[string]any {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	card := maps.Clone(agent.Card)
	endpoint := url.URL{Scheme: scheme, Host: r.Host, Path: "/a2a/" + selector}
	card["url"] = endpoint.String()
	delete(card, "additionalInterfaces")
	return card
}

func (s *Server) ReplaceAgents(agents []Agent) {
	s.mu.Lock()
	s.agents = slices.Clone(agents)
	s.mu.Unlock()
}

func (s *Server) serveAgent(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/a2a/") && !strings.HasPrefix(path, "/v1/a2a/") &&
		!strings.HasPrefix(path, "/v1/agents") && path != "/public/agent_hub" {
		return false
	}
	s.mu.RLock()
	agents := s.agents
	s.mu.RUnlock()
	if path == "/public/agent_hub" && r.Method == http.MethodGet {
		cards := make([]map[string]any, 0)
		for _, agent := range agents {
			if agent.Public {
				cards = append(cards, agentCard(r, agent, agent.ID))
			}
		}
		writeJSON(w, http.StatusOK, cards)
		return true
	}
	isList := path == "/v1/agents" && r.Method == http.MethodGet
	id, isDetail := strings.CutPrefix(path, "/v1/agents/")
	a2a, isA2A := strings.CutPrefix(strings.TrimPrefix(path, "/v1"), "/a2a/")
	if !isList && !isDetail && !isA2A {
		return false
	}
	master := s.authorized(r)
	if !master {
		valid := false
		if s.config.AuthorizeKey != nil {
			_, valid = s.config.AuthorizeKey(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
	}
	var granted func(Agent) bool
	if !master && s.config.AuthorizeAgent != nil {
		granted = s.config.AuthorizeAgent(ProxyCredential(r))
	}
	allowed := func(agent Agent) bool {
		if master {
			return true
		}
		if granted != nil {
			return granted(agent)
		}
		return agent.Public
	}
	describe := func(agent Agent) map[string]any {
		return map[string]any{"agent_id": agent.ID, "agent_name": agent.Name, "agent_card_params": agentCard(r, agent, agent.ID),
			"litellm_params": map[string]bool{"is_public": agent.Public}}
	}
	if isList {
		visible := slices.DeleteFunc(slices.Clone(agents), func(agent Agent) bool { return !allowed(agent) })
		if r.URL.Query().Get("health_check") == "true" {
			visible = s.reachableAgents(r.Context(), visible)
		}
		reply := make([]map[string]any, 0, len(visible))
		for _, agent := range visible {
			reply = append(reply, describe(agent))
		}
		writeJSON(w, http.StatusOK, reply)
		return true
	}
	var action string
	if isA2A {
		id, action, _ = strings.Cut(a2a, "/")
	}
	index := slices.IndexFunc(agents, func(agent Agent) bool { return agent.ID == id || agent.Name == id })
	if index < 0 || !allowed(agents[index]) {
		writeError(w, http.StatusNotFound, "agent_not_found", "Agent not found")
		return true
	}
	agent := agents[index]
	switch {
	case !isA2A && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, describe(agent))
	case isA2A && r.Method == http.MethodGet && (action == ".well-known/agent-card.json" || action == ".well-known/agent.json"):
		writeJSON(w, http.StatusOK, agentCard(r, agent, id))
	case isA2A && r.Method == http.MethodPost && (action == "" || action == "message/send"):
		s.relayAgentMessage(w, r, agent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

func (s *Server) relayAgentMessage(w http.ResponseWriter, r *http.Request, agent Agent) {
	if agent.RequireTraceID && strings.TrimSpace(r.Header.Get("X-LiteLLM-Trace-Id")) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "This agent requires the X-LiteLLM-Trace-Id header")
		return
	}
	raw, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Agent input is too large")
		return
	}
	var message mcpRPCRequest
	if json.Unmarshal(raw, &message) != nil {
		writeMCPError(w, http.StatusOK, nil, -32700, "Parse error")
		return
	}
	if message.Version != "2.0" || message.ID == nil {
		writeMCPError(w, http.StatusOK, message.ID, -32600, "Invalid JSON-RPC request")
		return
	}
	if message.Method != "message/send" && message.Method != "message/stream" {
		writeMCPError(w, http.StatusOK, message.ID, -32601, "Method not found")
		return
	}
	budget, allowed := s.checkKeyBudget(w, r)
	if !allowed {
		return
	}
	admission, allowed := s.admitKeyLimits(w, r, budget, (len(message.Params)+3)/4)
	if !allowed {
		return
	}
	var recorder *callbackResponse
	defer func() { admission.finish(recorder) }()
	if s.config.Callback != nil || len(admission.settlements) != 0 {
		call := "asend_message"
		if message.Method == "message/stream" {
			call = "asend_message_streaming"
		}
		event := newInferenceEvent(call, agent.Name, message.Method == "message/stream")
		budget.apply(&event)
		event.Model, event.ModelID, event.Provider = "a2a_agent/"+agent.Name, agent.ID, "a2a_agent"
		if trace := strings.TrimSpace(r.Header.Get("X-LiteLLM-Trace-Id")); trace != "" {
			event.TraceID = trace
		}
		if key := ProxyCredential(r); key != "" && !s.authorized(r) {
			event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		}
		if target, err := url.Parse(agent.URL); err == nil {
			event.APISource = target.Scheme + "://" + target.Host
		}
		recorder = &callbackResponse{ResponseWriter: w, event: event, capture: s.config.LogMessages}
		if recorder.capture {
			recorder.event.Messages = boundedCallbackJSON(message.Params)
		}
		w = recorder
		defer func() {
			if recorder.status != 0 {
				recorder.finish()
				if s.config.Callback != nil {
					s.config.Callback(recorder.event)
				}
			}
		}()
	}
	if agent.Pydantic {
		s.relayPydanticAgent(w, r, agent, message)
		return
	}
	if agent.Completion != nil {
		s.relayAgentCompletion(w, r, agent, message)
		return
	}
	body, _ := json.Marshal(message)
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, agent.URL, bytes.NewReader(body))
	if err != nil {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Invalid agent address")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json, text/event-stream")
	for _, name := range agent.ExtraHeaders {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "X-Api-Key", "Api-Key", "Cookie", "Connection", "Content-Length", "Host":
			continue
		}
		if value := r.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	for _, selector := range []string{agent.ID, agent.Name} {
		prefix := "x-a2a-" + strings.ToLower(selector) + "-"
		for header, values := range r.Header {
			field, ok := strings.CutPrefix(strings.ToLower(header), prefix)
			if !ok || field == "" {
				continue
			}
			switch name := http.CanonicalHeaderKey(field); name {
			case "Connection", "Content-Length", "Host", "Transfer-Encoding", "Upgrade", "Proxy-Authorization":
				continue
			default:
				if len(values) > 0 {
					upstream.Header[name] = slices.Clone(values)
				}
			}
		}
	}
	for name, values := range agent.Headers {
		upstream.Header[name] = slices.Clone(values)
	}
	if agent.AgentCore != nil {
		session := agent.AgentSession
		if session == "" {
			session = "litellm-session-" + newUUID()
		}
		upstream.Header.Set("X-Amzn-Bedrock-AgentCore-Runtime-Session-Id", session)
		if agent.AgentUser != "" {
			upstream.Header.Set("X-Amzn-Bedrock-AgentCore-Runtime-User-Id", agent.AgentUser)
		}
		if agent.AgentJWT != "" {
			upstream.Header.Set("Authorization", "Bearer "+agent.AgentJWT)
		} else {
			upstream.Header.Del("Authorization")
			if err := signAWSRequest(r.Context(), upstream, agent.AgentCore, body, "bedrock-agentcore"); err != nil {
				writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Could not authorize AgentCore request")
				return
			}
		}
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(upstream)
	if err != nil {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Could not reach agent")
		return
	}
	defer response.Body.Close()
	if response.StatusCode/100 == 2 && strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.WriteHeader(response.StatusCode)
		flusher, _ := w.(http.Flusher)
		reader := bufio.NewReader(response.Body)
		for {
			frame, err := readSSEFrame(reader, s.config.MaxBodyBytes)
			if !writeStreamFrame(w, flusher, frame) || err != nil {
				return
			}
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(data)) > s.config.MaxBodyBytes {
		writeMCPError(w, http.StatusBadGateway, message.ID, -32603, "Could not read agent response")
		return
	}
	writeRawResponse(w, response.StatusCode, response.Header, data)
}

func (s *Server) reachableAgents(ctx context.Context, agents []Agent) []Agent {
	if len(agents) == 0 {
		return agents
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	healthy := make([]bool, len(agents))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(agents), s.config.HealthCheckConcurrency) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				address := agents[index].URL
				if address == "" {
					healthy[index] = true
					continue
				}
				healthy[index] = s.probeAgent(ctx, &client, address)
			}
		}()
	}
	for index := range agents {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	result := make([]Agent, 0, len(agents))
	for index, agent := range agents {
		if healthy[index] {
			result = append(result, agent)
		}
	}
	return result
}

func (s *Server) probeAgent(ctx context.Context, client *http.Client, address string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.sem }()
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode < 500
}

func (s *Server) GrantedAgentActivityIDs(r *http.Request) []string {
	ids := []string{}
	if s.config.AuthorizeAgent == nil {
		return ids
	}
	granted := s.config.AuthorizeAgent(ProxyCredential(r))
	if granted == nil {
		return ids
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, agent := range s.agents {
		agent.Public = false
		if granted(agent) {
			ids = append(ids, agent.ID)
		}
	}
	return ids
}
