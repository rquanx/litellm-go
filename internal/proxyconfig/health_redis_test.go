package proxyconfig

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestRedisSharesBackgroundHealthAcrossProxies(t *testing.T) {
	address := os.Getenv("MINATO_TEST_REDIS_URL")
	if address == "" {
		t.Skip("MINATO_TEST_REDIS_URL is required")
	}
	options, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	namespace := fmt.Sprintf("health-fixture-%d", time.Now().UnixNano())
	var servers []*runtime.Server
	for range 2 {
		store, err := OpenRedisRoutingState(ctx, options, namespace)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		server := runtime.NewServer(runtime.Config{
			AuthorizeInference: true, SharedHealthChecks: true, HealthCoordinator: store, BackgroundHealthInterval: time.Hour,
			Routes: []runtime.Route{{Alias: "fixture", Candidates: []runtime.Candidate{{
				DeploymentID: "test", Provider: "openai", ModelID: "upstream", APIBase: upstream.URL, Endpoint: "chat-completions",
			}}}},
		})
		servers = append(servers, server)
		workers.Add(1)
		go func() {
			defer workers.Done()
			server.RunBackgroundHealthChecks(ctx)
		}()
	}

	deadline := time.Now().Add(3 * time.Second)
	for _, server := range servers {
		for {
			out := httptest.NewRecorder()
			server.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/health?model=fixture", nil))
			if out.Code == http.StatusOK && strings.Contains(out.Body.String(), `"healthy_count":1`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("shared health did not become ready: %d %s", out.Code, out.Body)
			}
			time.Sleep(10 * time.Millisecond)
		}
		out := httptest.NewRecorder()
		server.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/health/shared-status", nil))
		if !strings.Contains(out.Body.String(), `"redis_connected":true`) {
			t.Fatalf("unexpected Redis status: %s", out.Body)
		}
	}
	if count := calls.Load(); count != 1 {
		t.Fatalf("sent %d upstream probes, want one", count)
	}
}
