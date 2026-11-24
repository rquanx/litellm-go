package proxyconfig

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestRedisRoutingCoordinatesTwoProxies(t *testing.T) {
	address := os.Getenv("MINATO_TEST_REDIS_URL")
	if address == "" {
		t.Skip("MINATO_TEST_REDIS_URL is required")
	}
	options, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("routing-fixture-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/unavailable/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"fixture unavailable"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"fixture","usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
	}))
	defer upstream.Close()
	limit := 1
	build := func() (*runtime.Server, *redisRoutingState) {
		t.Helper()
		store, err := OpenRedisRoutingState(ctx, options, namespace)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return runtime.NewServer(runtime.Config{
			RoutingStrategy: "usage-based-routing-v2", RoutingState: store, Cooldown: time.Minute,
			Routes: []runtime.Route{
				{Alias: "demo", Candidates: []runtime.Candidate{{DeploymentID: "limited", APIBase: upstream.URL, Endpoint: "chat-completions", ModelID: "fixture", RPM: &limit}}},
				{Alias: "down", Candidates: []runtime.Candidate{{DeploymentID: "unavailable", APIBase: upstream.URL + "/unavailable/v1", Endpoint: "chat-completions", ModelID: "fixture"}}},
			},
		}), store
	}
	first, writer := build()
	second, observer := build()
	call := func(server *runtime.Server, model string, status int) {
		t.Helper()
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"fixture"}]}`))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, request)
		if w.Code != status {
			t.Fatalf("%s returned %d, expected %d: %s", model, w.Code, status, w.Body)
		}
	}
	minute := time.Now().Unix() / 60
	if time.Now().Second() >= 58 {
		t.Skip("fixture is too close to the end of the routing bucket")
	}
	call(first, "demo", 200)
	call(second, "demo", 429)
	usage, err := observer.Usage(ctx, []string{"limited", "unknown"}, minute)
	if err != nil || len(usage) != 2 || usage[0] != 4 || usage[1] != 0 {
		t.Fatalf("unexpected shared usage %v: %v", usage, err)
	}
	call(first, "down", 503)
	call(second, "down", 429)
	if err := writer.RecordLatency(ctx, "limited", false, 15*time.Millisecond, 2); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordLatency(ctx, "limited", false, 25*time.Millisecond, 2); err != nil {
		t.Fatal(err)
	}
	if latency, err := observer.Latencies(ctx, []string{"limited", "unknown"}, true); err != nil || len(latency) != 2 || latency[0] != 20*time.Millisecond || latency[1] != 0 {
		t.Fatalf("unexpected shared latency: %v: %v", latency, err)
	}
	if err := writer.Track(ctx, "limited", "fixture-lease", true); err != nil {
		t.Fatal(err)
	}
	if busy, err := observer.Busy(ctx, []string{"limited", "unknown"}); err != nil || len(busy) != 2 || busy[0] != 1 || busy[1] != 0 {
		t.Fatalf("unexpected shared traffic: %v: %v", busy, err)
	}
	if err := writer.Track(ctx, "limited", "fixture-lease", false); err != nil {
		t.Fatal(err)
	}
	if busy, err := observer.Busy(ctx, []string{"limited"}); err != nil || len(busy) != 1 || busy[0] != 0 {
		t.Fatalf("lease remained active: %v: %v", busy, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected upstream calls: %d", calls.Load())
	}
}
