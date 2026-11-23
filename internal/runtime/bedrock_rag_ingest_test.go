package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRAGBedrockKnowledgeBaseIngestion(t *testing.T) {
	const dataSource = "/knowledgebases/KB1/datasources/"
	var uploads, starts, polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := "bedrock"
		if r.URL.Path == "/fixture-bucket/documents/fixture.txt" {
			service = "s3"
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/"+service+"/aws4_request") {
			t.Errorf("missing %s signature", service)
		}
		switch r.Method + " " + r.URL.Path {
		case "POST " + dataSource:
			io.WriteString(w, `{"dataSourceSummaries":[{"dataSourceId":"DS1"}]}`)
		case "GET " + dataSource + "DS1":
			io.WriteString(w, `{"dataSource":{"dataSourceConfiguration":{"s3Configuration":{"bucketArn":"arn:aws:s3:::fixture-bucket","inclusionPrefixes":["documents/"]}}}}`)
		case "PUT /fixture-bucket/documents/fixture.txt":
			uploads.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != "fixture document" {
				t.Errorf("incorrect document %q", body)
			}
		case "PUT " + dataSource + "DS1/ingestionjobs/":
			starts.Add(1)
			var payload map[string]string
			if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload["clientToken"]) < 33 {
				t.Error("missing ingestion client token")
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"ingestionJob":{"ingestionJobId":"JOB1","status":"STARTING"}}`)
		case "GET " + dataSource + "DS1/ingestionjobs/JOB1":
			polls.Add(1)
			io.WriteString(w, `{"ingestionJob":{"ingestionJobId":"JOB1","status":"COMPLETE"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "bedrock", ModelID: "fixture", Endpoint: "bedrock-converse", DeploymentID: "fixture", Bedrock: &BedrockConfig{
		Region: "us-east-1", AgentControlBase: upstream.URL, S3Endpoint: upstream.URL, AccessKey: "fixture", SecretKey: "secret",
	}}
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}}})
	upload := func(options string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("ingest_options", options); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "fixture.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "fixture document"); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/rag/ingest", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		return result
	}
	success := upload(`{"vector_store":{"model":"fixture","custom_llm_provider":"bedrock","vector_store_id":"KB1","wait_for_ingestion":true}}`)
	var response map[string]string
	if success.Code != http.StatusOK || json.Unmarshal(success.Body.Bytes(), &response) != nil ||
		response["vector_store_id"] != "KB1" || response["file_id"] != "documents/fixture.txt" || response["status"] != "completed" {
		t.Fatalf("unexpected ingestion result %d %s", success.Code, success.Body)
	}
	rejected := upload(`{"vector_store":{"model":"fixture","custom_llm_provider":"bedrock","knowledge_base_id":"KB1","data_source_id":"DS1","s3_prefix":"unrelated/"}}`)
	if rejected.Code != http.StatusBadRequest || uploads.Load() != 1 || starts.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("wrong ingestion sequence or rejection %d %s", rejected.Code, rejected.Body)
	}
}

func TestRAGBedrockCreatesKnowledgeBaseUsingConfiguredInfrastructure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		switch r.Method + " " + r.URL.Path {
		case "PUT /knowledgebases/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), "foundation-model/amazon.titan-embed-text-v2:0") ||
				!strings.Contains(string(raw), `"vectorIndexName":"fixture-index"`) {
				t.Errorf("incorrect knowledge base payload %s", raw)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"knowledgeBase":{"knowledgeBaseId":"NEW1","status":"CREATING"}}`)
		case "GET /knowledgebases/NEW1":
			io.WriteString(w, `{"knowledgeBase":{"knowledgeBaseId":"NEW1","status":"ACTIVE"}}`)
		case "PUT /knowledgebases/NEW1/datasources/":
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), `"bucketArn":"arn:aws:s3:::fixture-bucket"`) {
				t.Errorf("incorrect data source payload %s", raw)
			}
			io.WriteString(w, `{"dataSource":{"dataSourceId":"DS1"}}`)
		case "GET /knowledgebases/NEW1/datasources/DS1":
			io.WriteString(w, `{"dataSource":{"dataSourceConfiguration":{"s3Configuration":{"bucketArn":"arn:aws:s3:::fixture-bucket","inclusionPrefixes":["data/"]}}}}`)
		case "PUT /fixture-bucket/data/fixture.txt":
		case "PUT /knowledgebases/NEW1/datasources/DS1/ingestionjobs/":
			io.WriteString(w, `{"ingestionJob":{"ingestionJobId":"JOB1","status":"STARTING"}}`)
		default:
			t.Errorf("unexpected provisioning request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "bedrock", ModelID: "fixture", Endpoint: "bedrock-converse", DeploymentID: "fixture", Bedrock: &BedrockConfig{
		Region: "us-east-1", AgentControlBase: upstream.URL, S3Endpoint: upstream.URL, AccessKey: "fixture", SecretKey: "secret",
	}}
	server := NewServer(Config{AuthorizeInference: true, Token: "master", Routes: []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}},
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"fixture"}, key == "fixture" }})
	for _, token := range []string{"fixture", "master"} {
		body := `{"file":{"filename":"fixture.txt","content":"Zml4dHVyZQ=="},"ingest_options":{"name":"fixture-kb","vector_store":{"custom_llm_provider":"bedrock","s3_bucket":"fixture-bucket","knowledge_base_role_arn":"arn:aws:iam::123456789012:role/fixture","opensearch_collection_arn":"arn:aws:aoss:us-east-1:123456789012:collection/fixture","opensearch_index_name":"fixture-index"}}}`
		req := httptest.NewRequest(http.MethodPost, "/rag/ingest", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if token == "master" && (result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"vector_store_id":"NEW1"`)) ||
			token == "fixture" && result.Code != http.StatusForbidden {
			t.Fatalf("unexpected Bedrock create result %d %s", result.Code, result.Body)
		}
	}
}
