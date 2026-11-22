package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const mcpClientSessionTTL = 30 * time.Minute

type mcpStreamableContext struct{}

type mcpStreamableSession struct {
	credential [32]byte
	path       string
	protocol   string
	mu         sync.Mutex
	closed     bool
	timer      *time.Timer
	upstreams  map[[32]byte]*mcpSession
}

func (s *Server) streamableMCPRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		if r.Method == http.MethodDelete {
			writeError(w, http.StatusBadRequest, "invalid_request", "Mcp-Session-Id is required")
			return r, false
		}
		return r, true
	}
	s.mu.RLock()
	session := s.mcpStreamableSessions[id]
	s.mu.RUnlock()
	if session == nil || session.path != strings.TrimSuffix(r.URL.Path, "/") ||
		session.credential != sha256.Sum256([]byte(ProxyCredential(r))) {
		writeError(w, http.StatusNotFound, "session_not_found", "MCP session not found")
		return r, false
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && version != session.protocol {
		writeError(w, http.StatusBadRequest, "invalid_protocol_version", "MCP protocol version differs from the initialized session")
		return r, false
	}
	if r.Method == http.MethodDelete {
		s.closeMCPStreamableSession(id)
		w.WriteHeader(http.StatusNoContent)
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), mcpStreamableContext{}, session)), true
}

func (s *Server) initializeMCPStreamableSession(w http.ResponseWriter, r *http.Request, protocol string) bool {
	if _, legacy := w.(*mcpSSECapture); legacy {
		return true
	}
	if r.Header.Get("Mcp-Session-Id") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "MCP session is already initialized")
		return false
	}
	id := newUUID()
	session := &mcpStreamableSession{credential: sha256.Sum256([]byte(ProxyCredential(r))),
		path: strings.TrimSuffix(r.URL.Path, "/"), protocol: protocol, upstreams: make(map[[32]byte]*mcpSession)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mcpStreamableSessions) >= max(64, cap(s.sem)*2) {
		writeError(w, http.StatusServiceUnavailable, "capacity_exceeded", "MCP session limit reached")
		return false
	}
	if s.mcpStreamableSessions == nil {
		s.mcpStreamableSessions = make(map[string]*mcpStreamableSession)
	}
	session.timer = time.AfterFunc(mcpClientSessionTTL, func() { s.closeMCPStreamableSession(id) })
	s.mcpStreamableSessions[id] = session
	w.Header().Set("Mcp-Session-Id", id)
	return true
}

func (s *Server) closeMCPStreamableSession(id string) {
	s.mu.Lock()
	session := s.mcpStreamableSessions[id]
	delete(s.mcpStreamableSessions, id)
	s.mu.Unlock()
	if session == nil {
		return
	}
	session.timer.Stop()
	session.mu.Lock()
	session.closed = true
	upstreams := session.upstreams
	session.upstreams = nil
	session.mu.Unlock()
	for _, upstream := range upstreams {
		upstream.close()
	}
}

func (s *Server) protocolMCPSession(ctx context.Context, server MCPServer) (*mcpSession, func(), error) {
	client, _ := ctx.Value(mcpStreamableContext{}).(*mcpStreamableSession)
	if client == nil || server.Transport == "sse" || server.Command != "" {
		session, err := s.openMCP(ctx, server)
		if err != nil {
			return nil, nil, err
		}
		return session, session.close, nil
	}
	// A change in endpoint or credentials requires a fresh upstream handshake.
	encoded, _ := json.Marshal([]any{server.ID, server.URL, server.Headers})
	key := sha256.Sum256(encoded)
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil, nil, errors.New("MCP session is closed")
	}
	if session := client.upstreams[key]; session != nil && !session.expired.Load() {
		return session, func() {}, nil
	}
	session, err := s.openMCP(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	if client.upstreams[key] == nil && len(client.upstreams) >= 32 {
		return session, session.close, nil
	}
	session.ctx = context.Background()
	client.upstreams[key] = session
	return session, func() {}, nil
}
