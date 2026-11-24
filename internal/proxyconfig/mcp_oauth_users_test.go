package proxyconfig

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestMCPUserOAuthStoreUsesUpstreamTable(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("LITELLM_SALT_KEY", "fixture-secret")
	store, err := OpenMCPUserCredentialStore(db, "fixture-master")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, server := "fixture-"+newEntityID(), "fixture"
	token := runtime.MCPUserOAuthToken{AccessToken: "sensitive-fixture", RefreshToken: "refresh", ConnectedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Save(ctx, user, server, token); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := db.QueryRow(`SELECT credential_b64 FROM "LiteLLM_MCPUserCredentials" WHERE user_id=$1`, user).Scan(&encrypted); err != nil ||
		strings.Contains(encrypted, token.AccessToken) || encrypted == "" {
		t.Fatal("MCP token was not encrypted at rest")
	}
	if found, err := store.Read(ctx, user, server); err != nil || found.AccessToken != token.AccessToken || found.RefreshToken != token.RefreshToken {
		t.Fatalf("could not restore token: %v", err)
	}
	if _, err := store.Read(ctx, user+"-other", server); err == nil {
		t.Fatal("token visible to another user")
	}
	if _, err := db.Exec(`UPDATE "LiteLLM_MCPUserCredentials" SET credential_b64='legacy-byok' WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, user, server, token); err == nil {
		t.Fatal("interactive OAuth overwrote an existing BYOK credential")
	}
}
