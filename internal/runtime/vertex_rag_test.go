package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestVertexRAGCorpusUploadAndSearch(t *testing.T) {
	const parent = "projects/fixture/locations/us-central1"
	const corpus = parent + "/ragCorpora/123"
	const fileID = corpus + "/ragFiles/456"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vertex-token" {
			t.Error("wrong Vertex credential")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/" + parent + "/ragCorpora":
			io.WriteString(w, `{"name":"`+corpus+`","displayName":"new corpus","createTime":"2026-01-01T00:00:00Z","labels":{"origin":"fixture"}}`)
		case "POST /v1beta1/" + parent + "/ragCorpora":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"displayName":"fixture corpus"`) || !strings.Contains(string(body), `"endpoint":"embedding-model"`) {
				t.Errorf("incorrect corpus configuration %s", body)
			}
			io.WriteString(w, `{"name":"`+parent+`/operations/1"}`)
		case "GET /v1beta1/" + parent + "/operations/1":
			io.WriteString(w, `{"done":true,"response":{"name":"`+corpus+`"}}`)
		case "POST /upload/v1beta1/" + corpus + "/ragFiles:upload":
			if r.Header.Get("X-Goog-Upload-Protocol") != "multipart" {
				t.Error("wrong upload protocol")
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("invalid document upload: %v", err)
			} else {
				defer file.Close()
				data, _ := io.ReadAll(file)
				if string(data) != "fixture doc" {
					t.Errorf("unexpected document %q", data)
				}
			}
			if metadata := r.FormValue("metadata"); !strings.Contains(metadata, `"chunk_size":512`) ||
				!strings.Contains(metadata, `"display_name":"fixture.txt"`) {
				t.Errorf("incorrect chunking or file metadata %s", metadata)
			}
			io.WriteString(w, `{"ragFile":{"name":"`+fileID+`"}}`)
		case "POST /v1beta1/" + corpus + "/ragFiles:import":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			if string(raw) != `{"importRagFilesConfig":{"gcsSource":{"uris":["gs://fixture/documents/import.txt"]},"maxEmbeddingRequestsPerMin":50,"ragFileChunkingConfig":{"chunkOverlap":200,"chunkSize":512}}}` {
				t.Errorf("wrong Vertex import settings %s", raw)
			}
			io.WriteString(w, `{"name":"`+parent+`/operations/import"}`)
		case "POST /v1/" + parent + ":retrieveContexts":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("invalid search request: %v", err)
			}
			want := `{"query":{"rag_retrieval_config":{"top_k":3},"text":"first second"},"vertex_rag_store":{"rag_resources":[{"rag_corpus":"` + corpus + `"}]}}`
			actual, _ := json.Marshal(body)
			if string(actual) != want {
				t.Errorf("incorrect search request %s", actual)
			}
			io.WriteString(w, `{"contexts":{"contexts":[{"text":"result","sourceUri":"gs://fixture/doc","sourceDisplayName":"doc","score":0.8,"pageSpan":{"firstPage":1}}]}}`)
		default:
			t.Errorf("unexpected Vertex request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	candidate := Candidate{Provider: "vertex_ai", ModelID: "gemini-2.5-flash", Endpoint: "gemini-generate", APIBase: upstream.URL, DeploymentID: "vertex", Vertex: &VertexConfig{
		Project: "fixture", Location: "us-central1", Bucket: "fixture/documents", TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "vertex-token"}),
	}}
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{{Alias: "vertex", Candidates: []Candidate{candidate}}}})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("ingest_options", `{"name":"fixture corpus","chunking_strategy":{"chunk_size":512},"vector_store":{"custom_llm_provider":"vertex_ai","embedding_model":"embedding-model","model":"vertex"}}`); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "fixture doc"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/rag/ingest", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	result := httptest.NewRecorder()
	server.ServeHTTP(result, req)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), fileID) {
		t.Fatalf("unexpected ingestion result %d %s", result.Code, result.Body)
	}
	for _, object := range []string{"gs://fixture/documents/import.txt", "gs://fixture/other/import.txt"} {
		result := httptest.NewRecorder()
		body := `{"file_url":"` + object + `","ingest_options":{"chunking_strategy":{"chunk_size":512},"vector_store":{"custom_llm_provider":"vertex_ai","vector_store_id":"123","max_embedding_requests_per_min":50}}}`
		server.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/rag/ingest", strings.NewReader(body)))
		if strings.Contains(object, "/other/") {
			if result.Code != http.StatusBadRequest {
				t.Fatalf("unexpected object outside bucket prefix %d %s", result.Code, result.Body)
			}
		} else if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), parent+"/operations/import") ||
			!strings.Contains(result.Body.String(), `"status":"in_progress"`) {
			t.Fatalf("unexpected import response %d %s", result.Code, result.Body)
		}
	}
	direct := httptest.NewRecorder()
	server.ServeHTTP(direct, httptest.NewRequest(http.MethodPost, "/v1/vector_stores", strings.NewReader(`{"custom_llm_provider":"vertex_ai","name":"requested"}`)))
	if direct.Code != http.StatusOK || !strings.Contains(direct.Body.String(), `"created_at":1767225600`) ||
		!strings.Contains(direct.Body.String(), `"name":"new corpus"`) || !strings.Contains(direct.Body.String(), `"origin":"fixture"`) {
		t.Fatalf("unexpected vector store result %d %s", direct.Code, direct.Body)
	}
	for _, id := range []string{"123", corpus, "projects/other/locations/us-central1/ragCorpora/123"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/vector_stores/"+url.PathEscape(id)+"/search",
			strings.NewReader(`{"query":["first","second"],"custom_llm_provider":"vertex_ai","max_num_results":3}`))
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if strings.HasPrefix(id, "projects/other/") {
			if result.Code != http.StatusBadRequest {
				t.Fatalf("foreign corpus accepted %d %s", result.Code, result.Body)
			}
		} else if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"score":0.8`) ||
			!strings.Contains(result.Body.String(), `"pageSpan":{"firstPage":1}`) {
			t.Fatalf("unexpected search result %d %s", result.Code, result.Body)
		}
	}
}
