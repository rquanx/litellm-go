package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3VectorsIngestAndSearch(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ocr":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "data:application/pdf;base64,") {
				t.Errorf("unexpected OCR document %s", body)
			}
			io.WriteString(w, `{"pages":[{"markdown":"fixture"}]}`)
			return
		case "/v1/embeddings":
			io.WriteString(w, `{"data":[{"embedding":[0.25,0.75],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/s3vectors/aws4_request") {
			t.Error("missing S3 Vectors signature")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["vectorBucketName"] != "fixture-bucket" {
			t.Errorf("invalid S3 Vectors request: %v %v", request, err)
		}
		switch r.URL.Path {
		case "/GetVectorBucket", "/GetIndex":
			w.WriteHeader(http.StatusNotFound)
		case "/CreateVectorBucket":
			w.WriteHeader(http.StatusConflict)
		case "/CreateIndex":
			if request["dimension"] != float64(2) || request["dataType"] != "float32" {
				t.Errorf("wrong index settings %v", request)
			}
		case "/PutVectors":
			raw, _ := json.Marshal(request["vectors"])
			if string(raw) != `[{"data":{"float32":[0.25,0.75]},"key":"doc.txt_0","metadata":{"chunk_index":"0","filename":"doc.txt","source_text":"fixture"}}]` {
				t.Errorf("unexpected vectors %s", raw)
			}
		case "/QueryVectors":
			if request["topK"] != float64(5) {
				t.Errorf("wrong query limit %v", request)
			}
			io.WriteString(w, `{"vectors":[{"metadata":{"source_text":"fixture","filename":"doc.txt","chunk_index":"0"},"distance":0.25},{"metadata":{}}]}`)
		default:
			t.Errorf("unhandled path %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer cloud.Close()
	routes := []Route{
		{Alias: "aws", Candidates: []Candidate{{Provider: "bedrock", Endpoint: "bedrock-converse", ModelID: "aws", DeploymentID: "aws",
			Bedrock: &BedrockConfig{Region: "us-east-1", AccessKey: "fixture", SecretKey: "secret", VectorBase: cloud.URL}}}},
		{Alias: "ocr", Candidates: []Candidate{{Provider: "mistral", Endpoint: "ocr", ModelID: "mistral-ocr-latest", DeploymentID: "ocr", APIBase: cloud.URL}}},
		{Alias: "embed", Candidates: []Candidate{{Provider: "openai", Endpoint: "embeddings", ModelID: "embed", DeploymentID: "embed", APIBase: cloud.URL}}},
	}
	server := NewServer(Config{AuthorizeInference: true, Routes: routes, Token: "master", VectorRegistryLookup: func(r *http.Request, id string) (string, map[string]any, bool, bool, error) {
		registered := id == "fixture-bucket:fixture-index"
		return "s3_vectors", map[string]any{"model": "aws", "litellm_embedding_model": "embed"}, registered, ProxyCredential(r) == "master", nil
	}, AuthorizeKey: func(key string) ([]string, bool) {
		return []string{"aws"}, key == "scoped"
	}})
	send := func(path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		return result
	}
	upload := send("/v1/rag/ingest", "master", `{"file":{"filename":"doc.txt","content":"Zml4dHVyZQ=="},"ingest_options":{"embedding":{"model":"embed"},"vector_store":{"custom_llm_provider":"s3_vectors","model":"aws","vector_bucket_name":"fixture-bucket","index_name":"fixture-index"}}}`)
	if upload.Code != http.StatusOK || !strings.Contains(upload.Body.String(), "fixture-bucket:fixture-index") {
		t.Fatalf("unexpected S3 ingestion result %d %s", upload.Code, upload.Body)
	}
	ocr := send("/v1/rag/ingest", "master", `{"file":{"filename":"doc.txt","content":"JVBESg==","content_type":"application/pdf"},"ingest_options":{"ocr":{"model":"ocr"},"embedding":{"model":"embed"},"vector_store":{"custom_llm_provider":"s3_vectors","model":"aws","vector_bucket_name":"fixture-bucket","index_name":"fixture-index"}}}`)
	if ocr.Code != http.StatusOK {
		t.Fatalf("unexpected OCR ingestion result %d %s", ocr.Code, ocr.Body)
	}
	retrieve := send("/v1/vector_stores/fixture-bucket:fixture-index", "scoped", `{}`)
	if retrieve.Code != http.StatusNotFound {
		t.Fatalf("registered store ownership bypassed on update: %d %s", retrieve.Code, retrieve.Body)
	}
	for _, key := range []string{"master", "scoped"} {
		result := send("/v1/vector_stores/fixture-bucket:fixture-index/search", key, `{"query":"fixture"}`)
		if key == "scoped" {
			if result.Code != http.StatusNotFound {
				t.Fatalf("wrong store access result %d %s", result.Code, result.Body)
			}
		} else if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"score":0.75`) {
			t.Fatalf("wrong S3 search result %d %s", result.Code, result.Body)
		}
	}
}
