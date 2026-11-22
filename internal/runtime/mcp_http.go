package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

type MCPServer struct {
	ID, Name, Alias, URL, Description string
	PublicID                         string
	Transport                        string
	Headers                          http.Header
	AllowAllKeys                     bool
	AllowedTools                     []string
	DisallowedTools                  []string
	AllowedParams                    map[string][]string
	AccessGroups                     []string
	Command                          string
	Args, Env                        []string
}

type mcpSession struct {
	ctx context.Context
	client http.Client
	server MCPServer
	id, protocol string
	limit int64
	process *exec.Cmd
	input *json.Encoder
	output *bufio.Scanner
	cancel context.CancelFunc
	closePipes func() bool
	expired atomic.Bool
	sseBody io.ReadCloser
	sseScanner *bufio.Scanner
	postURL string
}

func (s *Server) openMCP(ctx context.Context, server MCPServer) (*mcpSession, error) {
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	session := &mcpSession{ctx: ctx, client: client, server: server, limit: s.config.MaxBodyBytes, protocol: "2025-03-26"}
	if server.Command != "" {
		if err := session.startProcess(); err != nil {
			return nil, err
		}
	}
	if server.Transport == "sse" {
		if err := session.openLegacySSE(); err != nil {
			return nil, err
		}
	}
	raw, err := session.rpc("initialize", map[string]any{"protocolVersion": session.protocol, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "minato-runtime", "version": "1"}})
	if err != nil {
		session.close()
		return nil, err
	}
	var hello struct { Version string `json:"protocolVersion"` }
	if json.Unmarshal(raw, &hello) != nil || hello.Version == "" {
		session.close()
		return nil, errors.New("invalid MCP initialize response")
	}
	session.protocol = hello.Version
	initialized := map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}
	if session.input != nil {
		if session.input.Encode(initialized) == nil {
			return session, nil
		}
		session.close()
		return nil, errors.New("MCP process rejected initialization")
	}
	response, err := session.send(ctx, http.MethodPost, initialized)
	if err == nil {
		response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return session, nil
		}
	}
	session.close()
	return nil, errors.New("MCP server rejected initialization")
}

func (session *mcpSession) send(ctx context.Context, method string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	address := session.server.URL
	if method == http.MethodPost && session.postURL != "" {
		address = session.postURL
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, err
	}
	request.Header = session.server.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodGet && session.server.Transport == "sse" {
		request.Header.Set("Accept", "text/event-stream")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("MCP-Protocol-Version", session.protocol)
	if session.id != "" {
		request.Header.Set("Mcp-Session-Id", session.id)
	}
	return session.client.Do(request)
}

func (session *mcpSession) close() {
	if session.sseBody != nil {
		session.sseBody.Close()
	}
	if session.process != nil {
		session.cancel()
		_ = session.process.Wait()
		session.closePipes()
		return
	}
	if session.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session.ctx), time.Second)
	defer cancel()
	if response, err := session.send(ctx, http.MethodDelete, nil); err == nil {
		response.Body.Close()
	}
}

func (session *mcpSession) rpc(method string, params any) (json.RawMessage, error) {
	return session.rpcContext(session.ctx, method, params)
}

func (session *mcpSession) rpcContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := newUUID()
	payload := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}
	if session.input != nil {
		if err := session.input.Encode(payload); err != nil {
			return nil, err
		}
		for session.output.Scan() {
			if result, found, err := parseMCPReply(session.output.Bytes(), id); found {
				return result, err
			}
		}
		return nil, errors.New("MCP process closed before its response")
	}
	response, err := session.send(ctx, http.MethodPost, payload)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusNotFound && session.id != "" {
			session.expired.Store(true)
		}
		return nil, errors.New("MCP upstream request failed")
	}
	if method == "initialize" {
		session.id = response.Header.Get("Mcp-Session-Id")
	}
	if session.sseScanner != nil {
		for {
			_, data, ok := nextMCPEvent(session.sseScanner, session.limit)
			if !ok {
				return nil, errors.New("MCP stream closed before its response")
			}
			if result, found, err := parseMCPReply(data, id); found {
				return result, err
			}
		}
	}
	reader := io.LimitReader(response.Body, session.limit+1)
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		body, err := io.ReadAll(reader)
		if err == nil && int64(len(body)) <= session.limit {
			if result, found, err := parseMCPReply(body, id); found {
				return result, err
			}
		}
		return nil, errors.New("invalid MCP response")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), int(session.limit))
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if result, found, err := parseMCPReply(data.Bytes(), id); found {
				return result, err
			}
			data.Reset()
		} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
			data.WriteByte('\n')
		}
	}
	return nil, errors.New("MCP stream closed before its response")
}

func parseMCPReply(data []byte, id string) (json.RawMessage, bool, error) {
	var message struct {
		ID string `json:"id"`
		Result json.RawMessage `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &message) != nil || message.ID != id {
		return nil, false, nil
	}
	if message.Error != nil || message.Result == nil {
		return nil, true, errors.New("MCP upstream returned an RPC error")
	}
	return message.Result, true, nil
}

func (session *mcpSession) startProcess() error {
	timeout := session.client.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(session.ctx, timeout)
	process := exec.CommandContext(ctx, session.server.Command, session.server.Args...)
	process.Env = make([]string, 0, 8+len(session.server.Env))
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "WINDIR", "TMP", "TEMP", "LANG"} {
		if value, exists := os.LookupEnv(name); exists {
			process.Env = append(process.Env, name+"="+value)
		}
	}
	process.Env = append(process.Env, session.server.Env...)
	in, err := process.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	out, err := process.StdoutPipe()
	if err != nil {
		in.Close()
		cancel()
		return err
	}
	if err := process.Start(); err != nil {
		in.Close()
		out.Close()
		cancel()
		return err
	}
	session.process, session.cancel = process, cancel
	session.closePipes = context.AfterFunc(ctx, func() {
		in.Close()
		out.Close()
	})
	session.input = json.NewEncoder(in)
	session.output = bufio.NewScanner(out)
	session.output.Buffer(make([]byte, 4096), int(session.limit))
	return nil
}

func (s *Server) mcpTools(ctx context.Context, server MCPServer) ([]map[string]any, error) {
	session, release, err := s.protocolMCPSession(ctx, server)
	if err != nil {
		return nil, err
	}
	defer release()
	var tools []map[string]any
	params := make(map[string]string)
	for page := 0; page < 32; page++ {
		body, err := session.rpcContext(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Tools []map[string]any `json:"tools"`
			Next string `json:"nextCursor"`
		}
		if json.Unmarshal(body, &result) != nil {
			return nil, errors.New("invalid MCP tool list")
		}
		for _, tool := range result.Tools {
			name, _ := tool["name"].(string)
			if name == "" || !server.allowsTool(name) {
				continue
			}
			tool["mcp_info"] = map[string]string{"server_id": server.ID, "server_name": server.Name, "description": server.Description}
			tools = append(tools, tool)
		}
		if result.Next == "" {
			return tools, nil
		}
		if params["cursor"] == result.Next {
			break
		}
		params["cursor"] = result.Next
	}
	return nil, errors.New("MCP pagination did not finish")
}

func (s *Server) ProbeMCP(ctx context.Context, server MCPServer, listTools bool) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if listTools {
		return s.mcpTools(ctx, server)
	}
	session, err := s.openMCP(ctx, server)
	if err != nil {
		return nil, err
	}
	session.close()
	return nil, nil
}
