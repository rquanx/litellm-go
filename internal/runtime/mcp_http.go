package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type MCPServer struct {
	ID, Name, Alias, URL, Description string
	Headers                          http.Header
	AllowAllKeys                     bool
	AllowedTools                     []string
}

type mcpSession struct {
	ctx context.Context
	client http.Client
	server MCPServer
	id, protocol string
	limit int64
}

func (s *Server) openMCP(ctx context.Context, server MCPServer) (*mcpSession, error) {
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	session := &mcpSession{ctx: ctx, client: client, server: server, limit: s.config.MaxBodyBytes, protocol: "2025-03-26"}
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
	response, err := session.send(ctx, http.MethodPost, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
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
	request, err := http.NewRequestWithContext(ctx, method, session.server.URL, body)
	if err != nil {
		return nil, err
	}
	request.Header = session.server.Headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
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
	id := newUUID()
	response, err := session.send(session.ctx, http.MethodPost, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("MCP upstream request failed")
	}
	if method == "initialize" {
		session.id = response.Header.Get("Mcp-Session-Id")
	}
	parse := func(data []byte) (json.RawMessage, bool, error) {
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
	reader := io.LimitReader(response.Body, session.limit+1)
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		body, err := io.ReadAll(reader)
		if err == nil && int64(len(body)) <= session.limit {
			if result, found, err := parse(body); found {
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
			if result, found, err := parse(data.Bytes()); found {
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

func (s *Server) mcpTools(ctx context.Context, server MCPServer) ([]map[string]any, error) {
	session, err := s.openMCP(ctx, server)
	if err != nil {
		return nil, err
	}
	defer session.close()
	var tools []map[string]any
	params := make(map[string]string)
	for page := 0; page < 32; page++ {
		body, err := session.rpc("tools/list", params)
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
