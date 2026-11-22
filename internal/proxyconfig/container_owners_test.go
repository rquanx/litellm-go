package proxyconfig

import (
	"context"
	"os"
	"testing"
)

func TestPostgresContainerOwnershipSurvivesStoreReopen(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE minato_proxy_container_owners (
		deployment_id TEXT NOT NULL, container_id TEXT NOT NULL, owner_scope TEXT NOT NULL,
		PRIMARY KEY (deployment_id, container_id))`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := OpenContainerOwners(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, "deployment", "cntr_fixture", "project:a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, "deployment", "cntr_fixture", "project:b"); err == nil {
		t.Fatal("container ownership was overwritten")
	}
	reopened, err := OpenContainerOwners(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := reopened.Read(ctx, "deployment", []string{"cntr_fixture", "unknown"})
	if err != nil || len(owners) != 1 || owners["cntr_fixture"] != "project:a" {
		t.Fatalf("unexpected persisted owners %v: %v", owners, err)
	}
	if err := reopened.Delete(ctx, "deployment", "cntr_fixture"); err != nil {
		t.Fatal(err)
	}
	owners, err = store.Read(ctx, "deployment", []string{"cntr_fixture"})
	if err != nil || len(owners) != 0 {
		t.Fatalf("unexpected owners after deletion %v: %v", owners, err)
	}
}
