package proxyconfig

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRedisCacheTopologies(t *testing.T) {
	for _, topology := range []string{"CLUSTER", "SENTINEL"} {
		t.Run(topology, func(t *testing.T) {
			raw := os.Getenv("MINATO_TEST_REDIS_" + topology)
			if raw == "" {
				t.Skip("Redis topology fixture is unset")
			}
			var settings cacheConnectionSettings
			if err := json.Unmarshal([]byte(raw), &settings); err != nil {
				t.Fatal(err)
			}
			settings.Namespace = "topology-regression"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, store, err := settings.prepare(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			rawStore := store.UniversalClient
			if err := rawStore.Set(ctx, "unrelated", "keep", time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
			defer rawStore.Del(ctx, "unrelated")
			for _, key := range []string{"one", "two", "three"} {
				if err := store.Set(ctx, key, []byte("cached"), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.DeleteCache(ctx, []string{"one", "two"}); err != nil {
				t.Fatal(err)
			}
			if err := store.FlushCache(ctx); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"one", "two", "three"} {
				if data, err := store.Get(ctx, key); err != nil || data != nil {
					t.Fatalf("expected %s to be removed: %q %v", key, data, err)
				}
			}
			if value, err := rawStore.Get(ctx, "unrelated").Result(); err != nil || value != "keep" {
				t.Fatalf("unrelated key was changed: %q %v", value, err)
			}
		})
	}
}

