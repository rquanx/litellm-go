package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestBedrockAssumeRoleAndWebIdentity(t *testing.T) {
	for _, webIdentity := range []bool{false, true} {
		name := "AssumeRole"
		if webIdentity {
			name += "WithWebIdentity"
		}
		t.Run(name, func(t *testing.T) {
			var exchanges atomic.Int32
			sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("Action") != name || r.Form.Get("RoleArn") != "arn:aws:iam::123456789012:role/fixture" ||
					r.Form.Get("RoleSessionName") != "fixture-session" {
					t.Error("unexpected AWS role request")
				}
				if webIdentity {
					if r.Form.Get("WebIdentityToken") != "fixture-identity-token" || r.Header.Get("Authorization") != "" {
						t.Error("unexpected AWS web identity request")
					}
				} else if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-base/") || r.Form.Get("ExternalId") != "fixture-external" {
					t.Error("unexpected AWS source credentials")
				}
				exchanges.Add(1)
				w.Header().Set("Content-Type", "text/xml")
				fmt.Fprintf(w, `<%[1]sResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><%[1]sResult><Credentials><AccessKeyId>fixture-assumed</AccessKeyId><SecretAccessKey>fixture-assumed-secret</SecretAccessKey><SessionToken>fixture-assumed-session</SessionToken><Expiration>%[2]s</Expiration></Credentials></%[1]sResult></%[1]sResponse>`,
					name, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			}))
			defer sts.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-assumed/") ||
					r.Header.Get("X-Amz-Security-Token") != "fixture-assumed-session" {
					t.Error("request did not use assumed role credentials")
				}
				fmt.Fprint(w, `{"output":{"message":{"role":"assistant","content":[{"text":"signed"}]}}}`)
			}))
			defer upstream.Close()
			t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "fixture-bearer-unused")
			t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
			auth := "      aws_access_key_id: fixture-base\n      aws_secret_access_key: fixture-secret\n      aws_external_id: fixture-external"
			if webIdentity {
				auth = "      aws_web_identity_token: fixture-identity-token"
			}
			config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: bedrock/converse/fixture
      api_base: %q
      aws_sts_endpoint: %q
      aws_role_name: arn:aws:iam::123456789012:role/fixture
      aws_region_name: us-east-1
      aws_session_name: fixture-session
%s
`, upstream.URL, sts.URL, auth)))
			if err != nil {
				t.Fatal(err)
			}
			server := runtime.NewServer(runtime.Config{Routes: config.Routes})
			for range 2 {
				request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("unexpected status %d: %s", response.Code, response.Body)
				}
			}
			if exchanges.Load() != 1 {
				t.Fatalf("STS exchanges = %d, expected 1", exchanges.Load())
			}
		})
	}
}

func TestBedrockUsesSelectedAWSSharedProfile(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(shared, []byte("[fixture]\naws_access_key_id = fixture-profile\naws_secret_access_key = fixture-secret\naws_session_token = fixture-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", shared)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-profile/") ||
			r.Header.Get("X-Amz-Security-Token") != "fixture-session" {
			t.Error("request did not use temporary profile credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":{"message":{"role":"assistant","content":[{"text":"signed"}]}},"stopReason":"end_turn"}`)
	}))
	defer upstream.Close()
	proxy, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: bedrock/converse/fixture
      api_base: %q
      aws_region_name: us-east-1
      aws_profile_name: fixture
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: proxy.Routes})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "signed") {
		t.Fatalf("Bedrock proxy request failed: %d %s", response.Code, response.Body)
	}
}
