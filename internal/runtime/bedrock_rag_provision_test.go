package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBedrockRAGProvisionsInfrastructure(t *testing.T) {
	var cloud *httptest.Server
	var bucket string
	cloud = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signed := func(service string) {
			if !strings.Contains(r.Header.Get("Authorization"), "/"+service+"/aws4_request") {
				t.Errorf("missing %s signature on %s", service, r.URL.Path)
			}
		}
		switch {
		case r.URL.Path == "/sts/":
			signed("sts")
			if r.FormValue("Action") != "GetCallerIdentity" {
				t.Error("wrong STS operation")
			}
			io.WriteString(w, "<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:sts::123456789012:assumed-role/fixture/session</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>")
		case r.URL.Path == "/iam/":
			signed("iam")
			switch r.FormValue("Action") {
			case "CreateRole":
				io.WriteString(w, "<CreateRoleResponse><CreateRoleResult><Role><Arn>arn:aws:iam::123456789012:role/knowledge</Arn></Role></CreateRoleResult></CreateRoleResponse>")
			case "PutRolePolicy":
				if !strings.Contains(r.FormValue("PolicyDocument"), "arn:aws:s3:::"+bucket+"/data/*") {
					t.Error("missing scoped object permissions")
				}
			default:
				t.Error("wrong IAM operation")
			}
		case r.URL.Path == "/oss/":
			signed("aoss")
			switch r.Header.Get("X-Amz-Target") {
			case "OpenSearchServerless.CreateSecurityPolicy":
			case "OpenSearchServerless.CreateCollection":
				io.WriteString(w, `{"createCollectionDetail":{"id":"coll"}}`)
			case "OpenSearchServerless.BatchGetCollection":
				fmt.Fprintf(w, `{"collectionDetails":[{"arn":"arn:aws:aoss:us-east-1:123456789012:collection/coll","collectionEndpoint":%q,"status":"ACTIVE"}]}`, cloud.URL)
			case "OpenSearchServerless.CreateAccessPolicy":
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "role/fixture") || !strings.Contains(string(body), "role/knowledge") {
					t.Error("access policy is missing the caller or Bedrock role")
				}
			default:
				t.Error("wrong OpenSearch operation")
			}
		case r.URL.Path == "/bedrock-kb-index":
			signed("aoss")
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["mappings"] == nil {
				t.Error("missing OpenSearch index mapping")
			}
		case strings.HasPrefix(r.URL.Path, "/s3/litellm-kb-"):
			signed("s3")
			bucket = strings.Trim(strings.TrimPrefix(r.URL.Path, "/s3/"), "/")
		default:
			t.Errorf("unknown fixture route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cloud.Close()
	candidate := Candidate{Bedrock: &BedrockConfig{Region: "us-east-1", AccessKey: "fixture", SecretKey: "secret",
		STSBase: cloud.URL + "/sts", IAMBase: cloud.URL + "/iam", OpenSearchBase: cloud.URL + "/oss", S3Endpoint: cloud.URL + "/s3"}}
	server := NewServer(Config{})
	result := httptest.NewRecorder()
	role, collection, storage, ok := server.provisionBedrockRAGInfrastructure(result, httptest.NewRequest(http.MethodPost, "/rag/ingest", nil),
		candidate, nil, "data/", "amazon.titan-embed-text-v2:0", "bedrock-kb-index")
	if !ok || role != "arn:aws:iam::123456789012:role/knowledge" || !strings.HasSuffix(collection, "/coll") || !strings.HasPrefix(storage, "litellm-kb-") {
		t.Fatalf("provisioning failed: %d %s", result.Code, result.Body)
	}
}
