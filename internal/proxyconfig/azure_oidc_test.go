package proxyconfig

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAzureIdentityUsesOIDCAndPasswordCredentials(t *testing.T) {
	t.Setenv("AZURE_CREDENTIAL", "")
	t.Setenv("AZURE_CLIENT_SECRET", "")
	for _, settings := range []string{
		"azure_ad_token: oidc/env/MINATO_OIDC_FIXTURE",
		"azure_username: fixture@example.com\n      azure_password: fixture-password",
	} {
		config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: fixture
    litellm_params:
      model: azure/text-embedding-3-small
      api_base: https://fixture.openai.azure.com/openai/v1
      tenant_id: fixture-tenant
      client_id: 11111111-1111-1111-1111-111111111111
      %s
`, settings)))
		if err != nil {
			t.Fatal(err)
		}
		candidate := config.Routes[0].Candidates[0]
		if candidate.AzureADToken != "" || candidate.AzureToken == nil || candidate.AzureIdentityKey == "" {
			t.Fatal("Azure credential did not bind a token provider")
		}
	}
}

func TestAzureOIDCSourceReadsCurrentAssertion(t *testing.T) {
	t.Setenv("MINATO_OIDC_FIXTURE", "first")
	source, err := azureOIDCSource("oidc/env/MINATO_OIDC_FIXTURE")
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source(context.Background()); err != nil || token != "first" {
		t.Fatalf("initial assertion missing: %v", err)
	}
	t.Setenv("MINATO_OIDC_FIXTURE", "second")
	if token, err := source(context.Background()); err != nil || token != "second" {
		t.Fatalf("rotated assertion missing: %v", err)
	}
	root := t.TempDir()
	inside := filepath.Join(root, "token")
	outside := filepath.Join(t.TempDir(), "token")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LITELLM_OIDC_ALLOWED_CREDENTIAL_DIRS", root)
	for _, path := range []string{inside, outside} {
		source, err := azureOIDCSource("oidc/file/" + path)
		if err != nil {
			t.Fatal(err)
		}
		token, err := source(context.Background())
		if path == inside && (err != nil || token != "fixture") {
			t.Fatalf("could not read allowed assertion: %v", err)
		}
		if path == outside && err == nil {
			t.Fatal("assertion outside credential directory was accepted")
		}
	}
}

func TestAzureOIDCFallsBackToManagedIdentity(t *testing.T) {
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resource") != "https://fixture.example" || r.URL.Query().Get("client_id") != "fixture-client" {
			t.Errorf("wrong Azure OIDC fallback request %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"fixture-assertion","token_type":"Bearer","expires_on":"%d","resource":"https://fixture.example"}`, time.Now().Add(time.Hour).Unix())
	}))
	defer identity.Close()
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	t.Setenv("AZURE_CREDENTIAL", "ManagedIdentityCredential")
	t.Setenv("AZURE_CLIENT_ID", "fixture-client")
	t.Setenv("IDENTITY_ENDPOINT", identity.URL)
	t.Setenv("IDENTITY_HEADER", "fixture")
	t.Setenv("IDENTITY_SERVER_THUMBPRINT", "")
	t.Setenv("MSI_ENDPOINT", "")
	source, err := azureOIDCSource("oidc/azure/https://fixture.example/.default")
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source(context.Background()); err != nil || token != "fixture-assertion" {
		t.Fatalf("Azure managed identity did not return an assertion: %v", err)
	}
}
