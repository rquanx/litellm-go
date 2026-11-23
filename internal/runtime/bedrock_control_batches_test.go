package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
