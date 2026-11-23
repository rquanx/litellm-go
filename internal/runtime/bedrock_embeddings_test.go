package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBedrockTitanEmbeddingsAggregatesInputOrderAndUsage(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Text string `json:"inputText"`
			Dimensions int `json:"dimensions"`
			Types []string `json:"embeddingTypes"`
		}
		index := calls.Add(1)
		if r.URL.Path != "/model/amazon.titan-embed-text-v2:0/invoke" || r.Header.Get("Authorization") != "Bearer fixture" ||
			json.NewDecoder(r.Body).Decode(&input) != nil || input.Text != fmt.Sprintf("item%d", index) ||
			input.Dimensions != 256 || len(input.Types) != 1 || input.Types[0] != "binary" {
			t.Errorf("unexpected Titan call: %s, %+v", r.URL, input)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"embeddingsByType":{"binary":[%d]},"inputTextTokenCount":2}`, index)
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: "amazon.titan-embed-text-v2:0", Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"demo","input":["item1","item2"],"dimensions":256,"encoding_format":"base64"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || calls.Load() != 2 || !strings.Contains(response.Body.String(), `"total_tokens":4`) ||
		!strings.Contains(response.Body.String(), `"embedding":[2],"index":1`) {
		t.Fatalf("unexpected embeddings response: %d %s", response.Code, response.Body)
	}
}

func TestBedrockCohereEmbeddingsUseBatchAndBilledTokens(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Texts []string `json:"texts"`
			Dimensions int `json:"output_dimension"`
			Type string `json:"input_type"`
		}
		if r.URL.Path != "/model/cohere.embed-v4:0/invoke" || r.Header.Get("Authorization") != "Bearer fixture" ||
			json.NewDecoder(r.Body).Decode(&input) != nil || strings.Join(input.Texts, ",") != "one,two" ||
			input.Type != "search_query" || input.Dimensions != 256 {
			t.Errorf("unexpected Cohere invocation: %s %+v", r.URL, input)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"embeddings":{"float":[[1],[2]]},"meta":{"billed_units":{"input_tokens":7}}}`)
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: "cohere.embed-v4:0", Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"demo","input":["one","two"],"dimensions":256,"input_type":"search_query"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || calls.Load() != 1 || !strings.Contains(response.Body.String(), `"total_tokens":7`) ||
		!strings.Contains(response.Body.String(), `"embedding":[2],"index":1`) {
		t.Fatalf("unexpected embeddings response: %d %s", response.Code, response.Body)
	}
}

func TestBedrockTitanImageEmbeddings(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Image string `json:"inputImage"`
			Text string `json:"inputText"`
			Config struct {
				Length int `json:"outputEmbeddingLength"`
			} `json:"embeddingConfig"`
		}
		index := calls.Add(1)
		if r.URL.Path != "/model/amazon.titan-embed-image-v1/invoke" || json.NewDecoder(r.Body).Decode(&input) != nil ||
			input.Config.Length != 256 || (index == 1 && (input.Image != "Zml4dHVyZQ==" || input.Text != "")) ||
			(index == 2 && (input.Image != "" || input.Text != "fixture")) {
			t.Errorf("unexpected Titan multimodal call: %s %+v", r.URL, input)
		}
		fmt.Fprint(w, `{"embedding":[1],"inputTextTokenCount":2}`)
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: "amazon.titan-embed-image-v1", Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"demo","input":["data:image/png;base64,Zml4dHVyZQ==","fixture"],"dimensions":256}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || calls.Load() != 2 || !strings.Contains(response.Body.String(), `"image_count":1`) {
		t.Fatalf("unexpected embeddings response: %d %s", response.Code, response.Body)
	}
}

func TestBedrockNovaEmbeddingsSyncAndAsync(t *testing.T) {
	const model = "amazon.nova-2-multimodal-embeddings-v1:0"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid Nova request JSON")
		}
		switch r.URL.Path {
		case "/model/" + model + "/invoke":
			if string(payload["taskType"]) != `"SINGLE_EMBEDDING"` ||
				!strings.Contains(string(payload["singleEmbeddingParams"]), `"embeddingDimension":256`) {
				t.Errorf("unexpected Nova input %s", payload)
			}
			fmt.Fprint(w, `{"embeddings":[{"embedding":[1],"truncatedCharLength":12}]}`)
		case "/async-invoke":
			if string(payload["modelId"]) != `"`+model+`"` || !strings.Contains(string(payload["modelInput"]), "SEGMENTED_EMBEDDING") ||
				!strings.Contains(string(payload["outputDataConfig"]), "s3://fixture/output/") {
				t.Errorf("unexpected Nova invocation %s", payload)
			}
			fmt.Fprint(w, `{"invocationArn":"arn:aws:bedrock:us-east-1:123:async-invoke/fixture"}`)
		default:
			t.Errorf("unexpected Nova path %s", r.URL.Path)
		}
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: model, Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	async := candidate
	async.ModelID = "async_invoke/" + model
	server := NewServer(Config{Routes: []Route{{Alias: "sync", Candidates: []Candidate{candidate}}, {Alias: "async", Candidates: []Candidate{async}}}})
	requests := []struct {
		body string
		want string
	}{
		{`{"model":"sync","input":"fixture","dimensions":256}`, `"total_tokens":3`},
		{`{"model":"async","input":"fixture","output_s3_uri":"s3://fixture/output/"}`, `"embedding":[]`},
	}
	for _, tc := range requests {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(tc.body)))
		if response.Code != 200 || !strings.Contains(response.Body.String(), tc.want) {
			t.Fatalf("unexpected Nova result %d %s", response.Code, response.Body)
		}
		if strings.Contains(tc.body, `"async"`) && !strings.HasSuffix(response.Header().Get("X-LiteLLM-Invocation-ARN"), "async-invoke/fixture") {
			t.Fatal("missing invocation ARN")
		}
	}
}

func TestBedrockMarengoImageEmbeddings(t *testing.T) {
	const model = "twelvelabs.marengo-embed-2-7-v1:0"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Kind   string `json:"inputType"`
			Source struct {
				Inline string `json:"base64String"`
			} `json:"mediaSource"`
		}
		if r.URL.Path != "/model/"+model+"/invoke" || json.NewDecoder(r.Body).Decode(&input) != nil ||
			input.Kind != "image" || input.Source.Inline != "Zml4dHVyZQ==" {
			t.Errorf("unexpected Marengo request: %s %+v", r.URL, input)
		}
		fmt.Fprint(w, `{"data":[{"embedding":[1,2,3,4]}]}`)
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: model, Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(
		`{"model":"demo","input":"data:image/png;base64,Zml4dHVyZQ==","input_type":"image"}`)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"embedding":[1,2,3,4]`) ||
		!strings.Contains(response.Body.String(), `"total_tokens":1`) {
		t.Fatalf("unexpected Marengo result %d %s", response.Code, response.Body)
	}
}

func TestBedrockAsyncBatchStatusUsesSubmissionDeploymentAndOwner(t *testing.T) {
	const id = "arn:aws:bedrock:us-east-1:123456789012:async-invoke/fixture"
	const model = "async_invoke/amazon.nova-2-multimodal-embeddings-v1:0"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=fixture/") {
			t.Error("request was not signed for Bedrock")
		}
		switch r.Method {
		case http.MethodPost:
			fmt.Fprintf(w, `{"invocationArn":%q}`, id)
		case http.MethodGet:
			if r.URL.Path != "/async-invoke/"+id || !strings.HasSuffix(r.URL.EscapedPath(), "async-invoke%2Ffixture") {
				t.Errorf("unexpected status path %s", r.URL.EscapedPath())
			}
			fmt.Fprintf(w, `{"invocationArn":%q,"modelArn":"fixture","status":"Completed","submitTime":"2026-01-01T01:02:03Z","endTime":"2026-01-01T01:02:05Z","outputDataConfig":{"s3OutputDataConfig":{"s3Uri":"s3://fixture/output/"}}}`, id)
		default:
			t.Error("unexpected provider operation")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{APIBase: upstream.URL, ModelID: model, Endpoint: "bedrock-embeddings", DeploymentID: "fixture",
		Provider: "bedrock", Bedrock: &BedrockConfig{Region: "us-east-1", AccessKey: "fixture", SecretKey: "fixture-secret"}}
	server := NewServer(Config{
		Token: "master", AuthorizeInference: true,
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"async"}, key == "owner" || key == "other" },
		Routes: []Route{{Alias: "async", Candidates: []Candidate{candidate}}},
	})
	send := func(method, path, token, body string, expected int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s %s status = %d, expected %d: %s", method, path, response.Code, expected, response.Body)
		}
		return response
	}
	batch := "/v1/batches/" + id
	send("GET", batch, "owner", "", 404)
	start := send("POST", "/v1/embeddings", "owner", `{"model":"async","input":"example","output_s3_uri":"s3://fixture/output/"}`, 200)
	if start.Header().Get("X-LiteLLM-Invocation-ARN") != id {
		t.Fatal("submission did not return its invocation ARN")
	}
	send("GET", batch, "other", "", 404)
	send("GET", batch+"?model=another", "owner", "", 404)
	response := send("GET", strings.Replace(batch, "async-invoke/fixture", "async-invoke%2Ffixture", 1), "owner", "", 200)
	for _, expected := range []string{`"status":"completed"`, `"request_counts":{"completed":1,"failed":0,"total":1}`, `"output_file_id":"s3://fixture/output/"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("expected %s in %s", expected, response.Body)
		}
	}
}
