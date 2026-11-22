package proxyconfig

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPostgresHealthHistoryScopeAndLatest(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TEMP TABLE minato_proxy_health (
		id TEXT PRIMARY KEY, model_name TEXT NOT NULL, model_id TEXT NOT NULL, status TEXT NOT NULL,
		healthy_count INTEGER NOT NULL, unhealthy_count INTEGER NOT NULL,
		response_ms DOUBLE PRECISION NOT NULL, checked_at TIMESTAMPTZ NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := OpenHealthHistory(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store.Record(ctx, []runtime.ModelHealthRecord{
		{ID: "old", Model: "allowed", ModelID: "a", Status: "healthy", HealthyCount: 1, CheckedAt: now.Add(-time.Minute)},
		{ID: "new", Model: "allowed", ModelID: "a", Status: "unhealthy", UnhealthyCount: 1, CheckedAt: now},
		{ID: "private", Model: "other", ModelID: "b", Status: "healthy", HealthyCount: 1, CheckedAt: now},
	})
	for _, tc := range []struct {
		query runtime.HealthHistoryQuery
		want string
	}{
		{runtime.HealthHistoryQuery{Scope: []string{"allowed"}, Latest: true}, "new"},
		{runtime.HealthHistoryQuery{Scope: []string{"allowed"}, Status: "healthy", Limit: 1}, "old"},
		{runtime.HealthHistoryQuery{Scope: []string{}, Latest: true}, ""},
	} {
		rows, err := store.Read(ctx, tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && len(rows) != 0 || tc.want != "" && (len(rows) != 1 || rows[0].ID != tc.want) {
			t.Fatalf("unexpected health history: %+v", rows)
		}
	}
}
