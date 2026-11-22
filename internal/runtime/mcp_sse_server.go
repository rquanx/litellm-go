package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const mcpSSEMessages = "/mcp/sse/messages"

type clientMCPSession struct {
	credential [32]byte
	ctx context.Context
	responses chan []byte
}

func (s *Server) serveMCPClientSSE(w http.ResponseWriter, r *http.Request) bool {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/sse", "/mcp/sse":
		if r.Method != http.MethodGet {
			return false
		}
	case mcpSSEMessages, "/sse/messages":
		if r.Method != http.MethodPost {
			return false
		}
		s.submitMCPSSE(w, r)
		return true
	default:
		return false
	}
	if _, valid := s.mcpAccess(r); !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return true
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unavailable", "Could not open event stream")
		return true
	}
	id := newUUID()
	session := &clientMCPSession{credential: sha256.Sum256([]byte(ProxyCredential(r))), ctx: r.Context(), responses: make(chan []byte, 8)}
	s.mu.Lock()
	if len(s.mcpSSESessions) >= cap(s.sem) {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "capacity_exceeded", "MCP session limit reached")
		return true
	}
	if s.mcpSSESessions == nil {
		s.mcpSSESessions = make(map[string]*clientMCPSession)
	}
	s.mcpSSESessions[id] = session
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.mcpSSESessions, id)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "event: endpoint\ndata: %s?session_id=%s\n\n", mcpSSEMessages, id)
	flush.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return true
		case data := <-session.responses:
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data); err != nil {
				return true
			}
			flush.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return true
			}
			flush.Flush()
		}
	}
}

func (s *Server) submitMCPSSE(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	s.mu.RLock()
	session := s.mcpSSESessions[id]
	s.mu.RUnlock()
	if session == nil || session.ctx.Err() != nil || session.credential != sha256.Sum256([]byte(ProxyCredential(r))) {
		writeError(w, http.StatusNotFound, "session_not_found", "MCP session not found")
		return
	}
	response := &mcpSSECapture{limit: s.config.MaxBodyBytes}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	s.serveMCP(response, r.WithContext(ctx))
	if response.status >= 400 {
		writeRawResponse(w, response.status, response.Header(), response.body.Bytes())
		return
	}
	if response.body.Len() > 0 {
		select {
		case session.responses <- response.body.Bytes():
		case <-ctx.Done():
			writeError(w, http.StatusServiceUnavailable, "session_closed", "MCP session is closed")
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

type mcpSSECapture struct {
	headers http.Header
	body bytes.Buffer
	status int
	limit int64
}

func (response *mcpSSECapture) Header() http.Header {
	if response.headers == nil {
		response.headers = make(http.Header)
	}
	return response.headers
}

func (response *mcpSSECapture) WriteHeader(status int) {
	response.status = status
}

func (response *mcpSSECapture) Write(body []byte) (int, error) {
	if int64(response.body.Len()+len(body)) > response.limit {
		response.status = http.StatusBadGateway
		response.body.Reset()
		return 0, errors.New("MCP response is too large")
	}
	return response.body.Write(body)
}
