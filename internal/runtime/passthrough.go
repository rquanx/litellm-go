package runtime

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type PassThroughRoute struct {
	Path string
	Target url.URL
	Headers http.Header
	DefaultQuery url.Values
	Methods []string
	Public, IncludeSubpath, ForwardHeaders, MergeQuery bool
	Cost *float64
}

func IsBuiltInProxyPath(path string) bool {
	if proxyAlias(path) || EndpointFromPath(path) != "" {
		return true
	}
	if _, endpoint := geminiMethod(path); endpoint != "" {
		return true
	}
	if _, endpoint := deploymentEndpoint(path); endpoint != "" {
		return true
	}
	for _, root := range []string{"/health", "/healthz", "/internal", "/v1/models", "/v1/responses", "/v1/files", "/v1/batches", "/v1/messages",
		"/v1/search", "/search", "/v1/realtime", "/realtime", "/v1/vector_stores", "/v1/videos", "/openai", "/anthropic",
		"/model", "/v1/model", "/key", "/team", "/user", "/organization", "/credential", "/router", "/config",
		"/spend", "/search_tools", "/model_group", "/utils"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func (s *Server) ReplacePassThrough(routes []PassThroughRoute) {
	s.mu.Lock()
	s.passThrough = routes
	s.mu.Unlock()
}

func (s *Server) servePassThrough(w http.ResponseWriter, r *http.Request) bool {
	s.mu.RLock()
	routes := s.passThrough
	s.mu.RUnlock()
	if len(routes) == 0 || IsBuiltInProxyPath(r.URL.Path) {
		return false
	}
	var selected *PassThroughRoute
	for i := range routes {
		route := &routes[i]
		if !slices.Contains(route.Methods, r.Method) ||
			(r.URL.Path != route.Path && !(route.IncludeSubpath && strings.HasPrefix(r.URL.Path, route.Path+"/"))) {
			continue
		}
		if selected == nil || len(route.Path) > len(selected.Path) {
			selected = route
		}
	}
	if selected == nil {
		return false
	}
	if !selected.Public && !s.authorized(r) {
		valid := false
		if s.config.AuthorizeKey != nil {
			_, valid = s.config.AuthorizeKey(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
	}
	s.forwardPassThrough(w, r, *selected)
	return true
}

func (s *Server) forwardPassThrough(w http.ResponseWriter, r *http.Request, route PassThroughRoute) {
	target := route.Target
	if suffix := strings.TrimPrefix(r.URL.Path, route.Path); suffix != "" {
		escapedPrefix := (&url.URL{Path: route.Path}).EscapedPath()
		escapedSuffix := strings.TrimPrefix(r.URL.EscapedPath(), escapedPrefix)
		if !strings.HasPrefix(escapedSuffix, "/") || strings.Contains(suffix, "\\") {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid pass-through path")
			return
		}
		for _, segment := range strings.Split(suffix, "/") {
			if segment == "." || segment == ".." {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid pass-through path")
				return
			}
		}
		target.RawPath = strings.TrimRight(target.EscapedPath(), "/") + escapedSuffix
		target.Path = strings.TrimRight(target.Path, "/") + suffix
	}
	query := make(url.Values, len(route.DefaultQuery))
	for k, v := range route.DefaultQuery {
		query[k] = v
	}
	for k, v := range target.Query() {
		query[k] = v
	}
	if route.MergeQuery {
		for k, v := range r.URL.Query() {
			query[k] = v
		}
	}
	target.RawQuery = query.Encode()
	body, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid pass-through request")
		return
	}
	if route.ForwardHeaders {
		for name, values := range r.Header {
			if !protectedPassHeader(name) && !connectionHeader(r.Header, name) && !strings.HasPrefix(strings.ToLower(name), "x-pass-") {
				upstream.Header[name] = values
			}
		}
	}
	if value := r.Header.Get("Content-Type"); value != "" {
		upstream.Header.Set("Content-Type", value)
	}
	for name, values := range route.Headers {
		upstream.Header[name] = values
	}
	for name, values := range r.Header {
		if actual, ok := strings.CutPrefix(strings.ToLower(name), "x-pass-"); ok && actual != "" && !protectedPassHeader(actual) {
			upstream.Header[http.CanonicalHeaderKey(actual)] = values
		}
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}
	event := newInferenceEvent("pass_through_endpoint", route.Path, false)
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach pass-through provider")
		s.recordPassThrough(r, event, http.StatusBadGateway, false, route.Cost)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if !hopHeader(name) && !connectionHeader(response.Header, name) {
			w.Header()[name] = values
		}
	}
	stream := strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
	w.WriteHeader(response.StatusCode)
	if stream {
		_, _ = io.Copy(passThroughStream{w}, response.Body)
	} else {
		_, _ = io.Copy(w, response.Body)
	}
	s.recordPassThrough(r, event, response.StatusCode, stream, route.Cost)
}

func protectedPassHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "api-key", "x-api-key", "x-goog-api-key", "ocp-apim-subscription-key", "x-litellm-api-key", "host", "content-length":
		return true
	}
	return hopHeader(name) || strings.HasPrefix(lower, "x-amz-")
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func connectionHeader(headers http.Header, name string) bool {
	for _, value := range headers.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), name) {
				return true
			}
		}
	}
	return false
}

type passThroughStream struct{ http.ResponseWriter }

func (writer passThroughStream) Write(body []byte) (int, error) {
	n, err := writer.ResponseWriter.Write(body)
	if flush, ok := writer.ResponseWriter.(http.Flusher); ok {
		flush.Flush()
	}
	return n, err
}

func (s *Server) recordPassThrough(r *http.Request, event InferenceEvent, status int, stream bool, cost *float64) {
	if s.config.Callback == nil {
		return
	}
	if key := ProxyCredential(r); key != "" && !s.authorized(r) {
		event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	}
	event.Stream = stream
	event.Status = "failure"
	if status >= 200 && status < 300 {
		event.Status, event.Spend = "success", cost
	}
	event.EndTime = float64(time.Now().UnixNano()) / float64(time.Second)
	s.config.Callback(event)
}
