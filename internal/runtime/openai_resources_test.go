package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFilesAndBatchesShareUpstreamDeployment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Error("provider credential was missing")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/files":
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("after") != "file-in" {
					t.Error("wrong file pagination cursor")
				}
				_, _ = io.WriteString(w, `{"data":[{"id":"file-out"}],"last_id":"file-out"}`)
				return
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "synthetic input" || r.FormValue("purpose") != "batch" ||
				r.FormValue("target_model_names") != "" {
				t.Error("unexpected file payload")
			}
			_, _ = io.WriteString(w, `{"id":"file-in","object":"file"}`)
		case "/v1/batches":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["input_file_id"] != "file-in" ||
				payload["custom_llm_provider"] != nil {
				t.Errorf("batch did not receive its upstream input file: %#v", payload)
			}
			_, _ = io.WriteString(w, `{"id":"batch-job","input_file_id":"file-in","output_file_id":"file-out"}`)
		case "/v1/batches/batch-job/cancel":
			if r.Method != http.MethodPost {
				t.Error("batch cancel method changed")
			}
			_, _ = io.WriteString(w, `{"id":"batch-job","status":"cancelling"}`)
		case "/v1/files/file-out/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0xff, 0x00, 0x02})
		default:
			t.Errorf("unknown provider path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master", Routes: []Route{
		{Alias: "demo", Candidates: []Candidate{{Provider: "openai", DeploymentID: "dep",
			ModelID: "fixture", APIBase: upstream.URL, APIKey: "fixture-provider", Endpoint: "chat-completions", RoutingGroup: "demo"}}},
	}})
	send := func(method, path, contentType string, body io.Reader) string {
		t.Helper()
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer master")
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", method, path, result.Code, result.Body.String())
		}
		return result.Body.String()
	}
	var buffer bytes.Buffer
	form := multipart.NewWriter(&buffer)
	_ = form.WriteField("target_model_names", "demo")
	_ = form.WriteField("purpose", "batch")
	file, err := form.CreateFormFile("file", "input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "synthetic input")
	_ = form.Close()
	input := encodeResourceID("files", "dep", "file-in")
	if body := send("POST", "/openai/v1/files", form.FormDataContentType(), &buffer); !strings.Contains(body, input) {
		t.Fatal("file upload did not return a managed ID")
	}
	output := encodeResourceID("files", "dep", "file-out")
	if body := send("GET", "/v1/files?after="+url.QueryEscape(input), "", nil); !strings.Contains(body, output) {
		t.Fatal("file pagination did not return the next managed ID")
	}
	batch := encodeResourceID("batches", "dep", "batch-job")
	body := send("POST", "/openai/v1/batches", "application/json",
		strings.NewReader(`{"custom_llm_provider":"vertex_ai","input_file_id":"`+input+`"}`))
	if !strings.Contains(body, batch) || !strings.Contains(body, output) {
		t.Fatal("batch response lost the deployment IDs")
	}
	send("POST", "/v1/batches/"+batch+"/cancel", "", nil)
	if body := send("GET", "/openai/v1/files/"+output+"/content", "", nil); body != "\xff\x00\x02" {
		t.Fatal("file content was modified")
	}
	mismatched := httptest.NewRequest("GET", "/azure/v1/files/"+output+"/content", nil)
	mismatched.Header.Set("Authorization", "Bearer master")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, mismatched)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("provider path accepted a different provider's resource: %d %s", response.Code, response.Body)
	}
	liteLLMID := func(prefix, original string) string {
		return prefix + base64.RawURLEncoding.EncodeToString([]byte("litellm:" + original + ";model,demo"))
	}
	legacyInput := liteLLMID("file-", "file-in")
	if body := send("GET", "/v1/files?after="+url.QueryEscape(legacyInput), "", nil); !strings.Contains(body, output) {
		t.Fatal("LiteLLM cursor did not select the expected file deployment")
	}
	send("POST", "/v1/batches", "application/json", strings.NewReader(`{"input_file_id":"`+legacyInput+`"}`))
	send("POST", "/v1/batches/"+liteLLMID("batch_", "batch-job")+"/cancel", "", nil)
	if body := send("GET", "/v1/files/"+liteLLMID("file-", "file-out")+"/content", "", nil); body != "\xff\x00\x02" {
		t.Fatal("LiteLLM file ID did not resolve to the expected content")
	}
}
