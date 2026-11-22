package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLegacyMCPClientSSEBindsMessagePostsToSessionKey(t *testing.T) {
	server := NewServer(Config{AuthorizeInference: true, Token: "fixture-master"})
	proxy := httptest.NewServer(server)
	defer proxy.Close()
	get, _ := http.NewRequest(http.MethodGet, proxy.URL+"/sse", nil)
	get.Header.Set("Authorization", "Bearer fixture-master")
	client := &http.Client{Timeout: 3 * time.Second}
	stream, err := client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatalf("SSE status %d", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	event, address, ok := nextMCPEvent(scanner, 8192)
	if !ok || event != "endpoint" || !strings.HasPrefix(string(address), mcpSSEMessages+"?session_id=") {
		t.Fatalf("invalid MCP event endpoint %q", address)
	}
	for _, tc := range []struct {
		key string
		status int
	}{{"wrong", 404}, {"fixture-master", 202}} {
		post, _ := http.NewRequest(http.MethodPost, proxy.URL+string(address), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		post.Header.Set("Authorization", "Bearer "+tc.key)
		response, err := client.Do(post)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("MCP message POST returned %d, want %d", response.StatusCode, tc.status)
		}
	}
	event, data, ok := nextMCPEvent(scanner, 8192)
	if !ok || event != "message" || !strings.Contains(string(data), `"result":{}`) {
		t.Fatalf("invalid MCP ping reply %q", data)
	}
}

func TestLegacySSEUpstreamMCPTransport(t *testing.T) {
	events := make(chan string, 4)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages?session=fixture\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case response := <-events:
					fmt.Fprint(w, "event: message\ndata: "+response+"\n\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/messages":
			if r.URL.Query().Get("session") != "fixture" {
				t.Error("session endpoint query lost")
			}
			var request mcpRPCRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return
			}
			switch request.Method {
			case "initialize":
				events <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26"}}`, request.ID)
			case "tools/list":
				events <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"sse-fixture","inputSchema":{}}]}}`, request.ID)
			case "notifications/initialized":
			default:
				t.Error("unexpected legacy SSE message")
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()
	server := NewServer(Config{AuthorizeInference: true, HTTPClient: &http.Client{Timeout: 3 * time.Second},
		MCPServers: []MCPServer{{ID: "legacy", Name: "legacy", URL: origin.URL + "/sse", Transport: "sse"}},
	})
	output := httptest.NewRecorder()
	server.ServeHTTP(output, httptest.NewRequest(http.MethodGet, "/mcp-rest/tools/list?server_id=legacy", nil))
	if output.Code != 200 || !strings.Contains(output.Body.String(), "sse-fixture") {
		t.Fatalf("unexpected legacy SSE tool list: %d %s", output.Code, output.Body.String())
	}
}
