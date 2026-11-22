package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
