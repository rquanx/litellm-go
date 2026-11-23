package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBedrockS3BatchUploadAndDownload(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.Contains(r.Header.Get("Authorization"), "/us-west-2/s3/aws4_request") ||
			r.Header.Get("X-Amz-Content-Sha256") == "" || !strings.HasPrefix(r.URL.Path, "/fixture/batches/litellm-bedrock-files-") {
			t.Errorf("unexpected S3 request: %s", r.URL.Path)
		}
		switch r.Method {
		case "PUT":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), `"recordId":"CallA"`) ||
				!strings.Contains(string(data), `"messages":[{"content":[{"text":"example"}],"role":"user"}]`) {
				t.Errorf("unexpected S3 batch file: %s", data)
			}
		case "GET":
			io.WriteString(w, "fixture content")
		default:
			t.Errorf("unexpected S3 method: %s", r.Method)
		}
	}))
	defer provider.Close()
	candidate := Candidate{Provider: "bedrock", ModelID: "amazon.nova-lite-v1:0", Endpoint: "bedrock-converse", DeploymentID: "fixture",
		Bedrock: &BedrockConfig{Region: "us-east-1", S3Region: "us-west-2", S3Bucket: "fixture/batches", S3Endpoint: provider.URL,
			AccessKey: "fixture", SecretKey: "fixture-secret"}}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"fixture"}, token == "owner" || token == "other" },
		Routes:       []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}},
	})
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	if err := form.WriteField("purpose", "batch"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "fixture.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, `{"custom_id":"CallA","body":{"model":"fixture","messages":[{"role":"user","content":"example"}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/files?custom_llm_provider=bedrock&model=fixture", &data)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	var file struct {
		ID string `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &file) != nil || !strings.HasPrefix(file.ID, "s3://fixture/batches/") {
		t.Fatalf("unexpected upload %d %s", w.Code, w.Body)
	}
	for _, fixture := range []struct {
		id, token string
		status    int
	}{
		{file.ID, "owner", 200},
		{file.ID, "other", 404},
		{"s3://fixture/litellm-bedrock-files-other", "master", 400},
		{"s3://elsewhere/litellm-batch-outputs/result", "master", 400},
	} {
		request := httptest.NewRequest("GET", "/v1/files/"+url.PathEscape(fixture.id)+"/content", nil)
		request.Header.Set("Authorization", "Bearer "+fixture.token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != fixture.status || (fixture.status == 200 && response.Body.String() != "fixture content") {
			t.Fatalf("unexpected download as %s: %d %s", fixture.token, response.Code, response.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected S3 calls %d", calls.Load())
	}
}

func TestBedrockControlBatchCreatesAndRetrievesOwnedJob(t *testing.T) {
	const id = "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/job123"
	var submissions atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=fixture/") {
			t.Error("Bedrock control-plane request was not signed")
		}
		if r.Method == "POST" && r.URL.Path == "/model-invocation-job" {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["modelId"] != "amazon.nova-lite-v1:0" || payload["roleArn"] != "arn:aws:iam::123456789012:role/batch" ||
				payload["clientRequestToken"] != payload["jobName"] {
				t.Errorf("unexpected batch submission %+v", payload)
			}
			submissions.Add(1)
			fmt.Fprintf(w, `{"jobArn":%q}`, id)
			return
		}
		if r.Method != "GET" || r.URL.Path != "/model-invocation-job/"+id || !strings.HasSuffix(r.URL.EscapedPath(), "model-invocation-job%2Fjob123") {
			t.Errorf("unexpected Bedrock status request %s %s", r.Method, r.URL.EscapedPath())
		}
		fmt.Fprintf(w, `{"jobArn":%q,"status":"Completed","jobName":"fixture","submitTime":"2026-01-01T01:02:03Z","endTime":"2026-01-01T01:02:05Z","inputDataConfig":{"s3InputDataConfig":{"s3Uri":"s3://fixture/input.jsonl"}},"outputDataConfig":{"s3OutputDataConfig":{"s3Uri":"s3://fixture/output/"}}}`, id)
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "bedrock", ModelID: "amazon.nova-lite-v1:0", Endpoint: "bedrock-converse", DeploymentID: "fixture",
		APIBase: "https://example.invalid", Bedrock: &BedrockConfig{ControlBase: upstream.URL, Region: "us-east-1", AccessKey: "fixture",
			SecretKey: "fixture-secret", BatchRole: "arn:aws:iam::123456789012:role/batch"}}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"fixture"}, token == "owner" || token == "other" },
		Routes:       []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}},
	})
	send := func(method, path, token, body string, expected int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", "fixture-request")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s %s status = %d, expected %d: %s", method, path, response.Code, expected, response.Body)
		}
		result, _ := io.ReadAll(response.Result().Body)
		return string(result)
	}
	base := "/v1/batches"
	send("GET", base+"/"+id, "owner", "", 404)
	send("POST", base+"?custom_llm_provider=bedrock", "owner", `{"model":"fixture","input_file_id":"https://example.invalid/file"}`, 400)
	result := send("POST", base, "owner", `{"model":"fixture","input_file_id":"s3://fixture/input.jsonl","completion_window":"24h"}`, 200)
	if !strings.Contains(result, id) || !strings.Contains(result, `"status":"validating"`) || submissions.Load() != 1 {
		t.Fatalf("unexpected batch create response %s", result)
	}
	send("GET", base+"/"+id, "other", "", 404)
	result = send("GET", base+"/"+id, "owner", "", 200)
	if !strings.Contains(result, `"output_file_id":"s3://fixture/output/job123/input.jsonl.out"`) ||
		!strings.Contains(result, `"status":"completed"`) {
		t.Fatalf("unexpected batch status response %s", result)
	}
}
