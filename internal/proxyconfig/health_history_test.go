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
		want  string
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

func TestImportLiteLLMHealthIsIdempotent(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE "LiteLLM_HealthCheckTable" (
		health_check_id TEXT PRIMARY KEY, model_name TEXT, model_id TEXT, status TEXT, healthy_count INT,
		unhealthy_count INT, response_time_ms DOUBLE PRECISION, checked_at TIMESTAMP, error_message TEXT);
		INSERT INTO "LiteLLM_HealthCheckTable" VALUES
		('legacy-health','legacy-model',NULL,'healthy',1,0,NULL,'2026-01-01 00:00:00','provider fixture detail');`); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1, 0} {
		count, err := ImportLiteLLMHealth(db)
		if err != nil || count != want {
			t.Fatalf("imported %d want %d: %v", count, want, err)
		}
	}
	history, err := OpenHealthHistory(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := history.Read(context.Background(), runtime.HealthHistoryQuery{Scope: []string{"legacy-model"}, Latest: true})
	if err != nil || len(rows) != 1 || rows[0].ModelID != "legacy-model" || rows[0].CheckedAt.Unix() != 1767225600 || rows[0].ErrorMessage != nil {
		t.Fatalf("unexpected imported health results %+v %v", rows, err)
	}
}
