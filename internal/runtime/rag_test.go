package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRAGGeminiResumableDocumentIngestion(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1beta/fileSearchStores":
			io.WriteString(w, `{"name":"fileSearchStores/demo","displayName":"fixture"}`)
		case "POST /upload/v1beta/fileSearchStores/demo:uploadToFileSearchStore":
			if r.Header.Get("x-goog-api-key") != "fixture" || r.Header.Get("X-Goog-Upload-Command") != "start" {
				t.Error("unexpected upload initiation")
			}
			w.Header().Set("X-Goog-Upload-Url", upstream.URL+"/fixture-session")
		case "PUT /fixture-session":
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" || string(body) != "reference" ||
				r.Header.Get("x-goog-api-key") != "" {
				t.Error("unexpected Gemini document upload")
			}
			io.WriteString(w, `{"name":"fileSearchStores/demo/operations/uploaded"}`)
		default:
			t.Errorf("unexpected Gemini path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "gemini", ModelID: "gemini-2.5-flash", Endpoint: "gemini-generate", APIBase: upstream.URL, APIKey: "fixture", DeploymentID: "gemini"}
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{{Alias: "fixture", Candidates: []Candidate{candidate}}}})
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	if err := form.WriteField("ingest_options", `{"vector_store":{"model":"fixture","custom_llm_provider":"gemini"}}`); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(part, "reference"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/rag/ingest", &data)
	request.Header.Set("Content-Type", form.FormDataContentType())
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "fileSearchStores/demo/operations/uploaded") {
		t.Fatalf("unexpected Gemini ingestion %d %s", result.Code, result.Body)
	}
}

func TestRAGIngestAndQueryThroughConfiguredRoutes(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/vector_stores":
			io.WriteString(w, `{"id":"vs-native","object":"vector_store"}`)
		case "POST /v1/files":
			file, _, err := r.FormFile("file")
			if err != nil || r.FormValue("purpose") != "assistants" {
				t.Errorf("invalid RAG file upload: %v", err)
			}
			if file != nil {
				defer file.Close()
				data, _ := io.ReadAll(file)
				if string(data) != "reference information" {
					t.Error("wrong uploaded content")
				}
			}
			io.WriteString(w, `{"id":"file-native","object":"file"}`)
		case "POST /v1/vector_stores/vs-native/files":
			data, _ := io.ReadAll(r.Body)
			if string(data) != `{"file_id":"file-native"}` {
				t.Errorf("wrong file reference: %s", data)
			}
			io.WriteString(w, `{"id":"file-native","object":"vector_store.file"}`)
		case "POST /v1/vector_stores/vs-native/search":
			io.WriteString(w, `{"object":"vector_store.search_results.page","data":[{"content":[{"type":"text","text":"first"}]},{"content":[{"type":"text","text":"reference information"}]}]}`)
		case "POST /v1/rerank":
			io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.9}]}`)
		case "POST /v1/chat/completions":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), `"content":"Context:\n\nreference information\n\n"`) {
				t.Errorf("invalid RAG messages: %s", data)
			}
			io.WriteString(w, `{"object":"chat.completion","model":"chat","choices":[{"message":{"role":"assistant","content":"used reference"}}]}`)
		default:
			t.Errorf("unexpected RAG upstream request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	routes := []Route{
		{Alias: "chat", Candidates: []Candidate{{Provider: "openai", Endpoint: "chat-completions", ModelID: "chat", DeploymentID: "chat", APIBase: provider.URL}}},
		{Alias: "rank", Candidates: []Candidate{{Provider: "openai", Endpoint: "rerank", ModelID: "rank", DeploymentID: "rank", APIBase: provider.URL}}},
	}
	server := NewServer(Config{Routes: routes, AuthorizeInference: true, Token: "master", AuthorizeKey: func(key string) ([]string, bool) {
		return []string{"rank"}, key == "limited"
	}})
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	if err := writer.WriteField("ingest_options", `{"vector_store":{"model":"chat","custom_llm_provider":"openai"}}`); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "reference information"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ingest := httptest.NewRequest("POST", "/rag/ingest", &upload)
	ingest.Header.Set("Authorization", "Bearer master")
	ingest.Header.Set("Content-Type", writer.FormDataContentType())
	result := httptest.NewRecorder()
	server.ServeHTTP(result, ingest)
	var metadata struct {
		Vector string `json:"vector_store_id"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &metadata) != nil || !strings.HasPrefix(metadata.Vector, "minato_") {
		t.Fatalf("unexpected ingestion response %d %s", result.Code, result.Body)
	}
	body := `{"model":"chat","messages":[{"role":"user","content":"question"}],"retrieval_config":{"vector_store_id":"` + metadata.Vector + `","custom_llm_provider":"openai"},"rerank":{"enabled":true,"model":"rank","top_n":1}}`
	for _, fixture := range []struct {
		key    string
		status int
	}{{"limited", 403}, {"master", 200}} {
		query := httptest.NewRequest("POST", "/v1/rag/query", strings.NewReader(body))
		query.Header.Set("Authorization", "Bearer "+fixture.key)
		result = httptest.NewRecorder()
		server.ServeHTTP(result, query)
		if result.Code != fixture.status || (fixture.status == 200 && (!strings.Contains(result.Body.String(), `"search_results"`) ||
			!strings.Contains(result.Body.String(), `"rerank_results"`) || !strings.Contains(result.Body.String(), "used reference"))) {
			t.Fatalf("unexpected RAG query response %d %s", result.Code, result.Body)
		}
	}
}
