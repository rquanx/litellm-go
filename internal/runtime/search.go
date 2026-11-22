package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"
)

type SearchTool struct {
	Name, Provider, Description string
	URL, APIKey                 string
	Timeout                     time.Duration
	Retries                     *int
}

func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if path != "/search" && !strings.HasPrefix(path, "/search/") {
		return false
	}
	selector := strings.TrimPrefix(path, "/search/")
	list := path == "/search/tools" && r.Method == http.MethodGet
	post := r.Method == http.MethodPost &&
		(path == "/search" || (strings.HasPrefix(path, "/search/") && selector != "" && !strings.Contains(selector, "/")))
	if !list && !post {
		return false
	}
	var scope []string
	if !s.authorized(r) {
		authorize := s.config.AuthorizeSearch
		if list && authorize == nil {
			authorize = s.config.AuthorizeKey
		}
		var valid bool
		if authorize != nil {
			scope, valid = authorize(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
	}
	if list {
		data := make([]map[string]string, 0, len(s.config.SearchTools))
		for _, tool := range s.config.SearchTools {
			info := map[string]string{"search_tool_name": tool.Name, "search_provider": tool.Provider}
			if tool.Description != "" {
				info["description"] = tool.Description
			}
			data = append(data, info)
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
		return true
	}
	if path == "/search" {
		selector = ""
	}
	s.search(w, r, selector, scope)
	return true
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, name string, scope []string) {
	var input map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes))
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "Expected one JSON search request")
		return
	}
	if name == "" {
		selector, present := input["search_tool_name"]
		if !present {
			selector = input["model"]
		}
		if json.Unmarshal(selector, &name) != nil || name == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "search_tool_name or model is required")
			return
		}
	}
	if scope != nil && !slices.Contains(scope, name) {
		writeError(w, http.StatusForbidden, "key_model_access_denied", "Search tool access denied")
		return
	}
	var query string
	var queries []string
	if (json.Unmarshal(input["query"], &query) != nil || query == "") &&
		(json.Unmarshal(input["query"], &queries) != nil || len(queries) == 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "query must be a string or a list of strings")
		return
	}
	var tools []SearchTool
	for _, tool := range s.config.SearchTools {
		if tool.Name == name {
			tools = append(tools, tool)
		}
	}
	if len(tools) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Search tool not found")
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}
	if len(tools) > 1 {
		rand.Shuffle(len(tools), func(i, j int) { tools[i], tools[j] = tools[j], tools[i] })
	}
	plan := s.attemptPlan(name)
	var recorder *callbackResponse
	var lastProvider string
	if s.config.Callback != nil {
		recorder = &callbackResponse{ResponseWriter: w, event: newInferenceEvent("asearch", name, false), capture: s.config.LogMessages}
		if key := ProxyCredential(r); key != "" && !s.authorized(r) {
			recorder.event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		}
		if recorder.capture {
			recorder.event.Messages = boundedCallbackJSON(input["query"])
		}
		w = recorder
		defer func() {
			if recorder.status != 0 {
				recorder.finish()
				recorder.event.Model = lastProvider + "/search"
				recorder.event.Provider = lastProvider
				s.config.Callback(recorder.event)
			}
		}()
	}
	for {
		tool := tools[plan.index]
		lastProvider = tool.Provider
		payload := searchPayload(tool.Provider, input, query, queries)
		status, headers, body := s.callSearch(r.Context(), tool, payload)
		if status >= 200 && status < 300 {
			if result, ok := normalizeSearch(tool.Provider, body); ok {
				if recorder != nil {
					recorder.event.Spend = searchCost(tool.Provider, input, max(1, len(queries)))
				}
				writeJSON(w, http.StatusOK, map[string]any{"object": "search", "results": result})
				return
			}
			status = http.StatusBadGateway
		}
		if plan.used == 0 {
			plan.retries = s.config.NumRetries
			if tool.Retries != nil {
				plan.retries = *tool.Retries
			}
			plan.remaining = plan.retries
		}
		if !plan.advance(r.Context(), status, headers, retryableStatus(status), len(tools)) {
			writeError(w, status, "search_provider_error", "Search provider request failed")
			return
		}
	}
}

func (s *Server) callSearch(ctx context.Context, tool SearchTool, payload []byte) (int, http.Header, []byte) {
	if tool.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tool.Timeout)
		defer cancel()
	}
	method := http.MethodPost
	var bodyReader io.Reader = bytes.NewReader(payload)
	if tool.Provider == "searxng" {
		method, bodyReader = http.MethodGet, nil
	}
	request, err := http.NewRequestWithContext(ctx, method, tool.URL, bodyReader)
	if err != nil {
		return http.StatusBadGateway, nil, nil
	}
	if tool.Provider == "searxng" {
		var params map[string]json.RawMessage
		if json.Unmarshal(payload, &params) != nil {
			return http.StatusBadGateway, nil, nil
		}
		query := request.URL.Query()
		for key, value := range params {
			var decoded string
			if json.Unmarshal(value, &decoded) == nil {
				query.Set(key, decoded)
			} else {
				query.Set(key, string(value))
			}
		}
		request.URL.RawQuery = query.Encode()
	}
	if tool.Provider == "exa_ai" || tool.Provider == "serper" {
		request.Header.Set("X-API-Key", tool.APIKey)
	} else if tool.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+tool.APIKey)
	}
	request.Header.Set("Content-Type", "application/json")
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return http.StatusGatewayTimeout, nil, nil
		}
		return http.StatusBadGateway, nil, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || (response.StatusCode >= 300 && response.StatusCode < 400) {
		return http.StatusBadGateway, response.Header, nil
	}
	if response.StatusCode >= 400 {
		return response.StatusCode, response.Header, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
		return http.StatusBadGateway, response.Header, nil
	}
	return response.StatusCode, response.Header, body
}
