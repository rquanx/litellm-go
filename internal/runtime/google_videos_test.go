package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGoogleVeoCreatePollAndDownload(t *testing.T) {
	for _, fixtureName := range []string{"gemini", "vertex_ai", "vertex_ai_gcs"} {
		t.Run(fixtureName, func(t *testing.T) {
			const model = "veo-test"
			vertex := fixtureName != "gemini"
			stored := fixtureName == "vertex_ai_gcs"
			provider := "gemini"
			if vertex {
				provider = "vertex_ai"
			}
			modelPath := "/v1beta/models/" + model
			operation := "models/" + model + "/operations/demo"
			if vertex {
				operation = "projects/fixture/locations/us-central1/publishers/google/models/" + model + "/operations/demo"
				modelPath = "/v1/" + strings.TrimSuffix(operation, "/operations/demo")
			}
			var fixture *httptest.Server
			fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if vertex && r.Header.Get("Authorization") != "Bearer fixture" || !vertex && r.Header.Get("x-goog-api-key") != "fixture" {
					t.Error("missing Veo provider credential")
				}
				switch r.Method + " " + r.URL.Path {
				case "POST " + modelPath + ":predictLongRunning":
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"durationSeconds":5`) || !strings.Contains(string(body), `"aspectRatio":"9:16"`) {
						t.Errorf("unexpected Veo body: %s", body)
					}
					fmt.Fprintf(w, `{"name":%q}`, operation)
				case "GET /v1beta/" + operation:
					fmt.Fprintf(w, `{"name":%q,"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":%q}}]}}}`, operation, fixture.URL+"/v1beta/files/demo:download?alt=media")
				case "POST " + modelPath + ":fetchPredictOperation":
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), operation) {
						t.Errorf("operation name missing %s", body)
					}
					if stored {
						fmt.Fprintf(w, `{"name":%q,"done":true,"response":{"videos":[{"mimeType":"video/mp4","gcsUri":"gs://fixture/videos/demo.mp4"}]}}`, operation)
					} else {
						fmt.Fprintf(w, `{"name":%q,"done":true,"response":{"videos":[{"mimeType":"video/mp4","bytesBase64Encoded":"Zml4dHVyZQ=="}]}}`, operation)
					}
				case "GET /storage/v1/b/fixture/o/videos/demo.mp4":
					if !stored || r.URL.Query().Get("alt") != "media" || !strings.Contains(r.URL.EscapedPath(), "videos%2Fdemo.mp4") {
						t.Errorf("unexpected GCS media request: %s", r.URL)
					}
					w.Header().Set("Content-Type", "video/mp4")
					fmt.Fprint(w, "fixture")
				case "GET /v1beta/files/demo:download":
					w.Header().Set("Content-Type", "video/mp4")
					fmt.Fprint(w, "fixture")
				default:
					t.Errorf("unexpected Veo request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer fixture.Close()
			candidate := Candidate{Provider: provider, ModelID: model, Endpoint: "gemini-generate", DeploymentID: provider,
				APIBase: fixture.URL, APIKey: "fixture"}
			if vertex {
				candidate.Vertex = &VertexConfig{Project: "fixture", Location: "us-central1"}
				if stored {
					candidate.Vertex.Bucket = "fixture/videos"
					candidate.Vertex.StorageBase = fixture.URL
				}
			}
			server := NewServer(Config{AuthorizeInference: true, Token: "master", Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
			call := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer master")
				w := httptest.NewRecorder()
				server.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
				}
				return w
			}
			created := call("POST", "/videos", `{"model":"demo","prompt":"fixture","seconds":"5","size":"720x1280"}`)
			var reply struct{ ID, Status string }
			if json.Unmarshal(created.Body.Bytes(), &reply) != nil || reply.ID == "" || reply.Status != "processing" {
				t.Fatalf("unexpected video creation: %s", created.Body)
			}
			path := "/v1/videos/" + reply.ID
			if status := call("GET", path, ""); !strings.Contains(status.Body.String(), `"status":"completed"`) {
				t.Fatal(status.Body)
			}
			legacy := "video_" + base64.StdEncoding.EncodeToString([]byte("litellm:custom_llm_provider:"+provider+";model_id:"+model+";video_id:"+operation))
			if status := call("GET", "/v1/videos/"+url.PathEscape(legacy), ""); !strings.Contains(status.Body.String(), `"status":"completed"`) {
				t.Fatal(status.Body)
			}
			content := call("GET", path+"/content", "")
			if content.Body.String() != "fixture" || content.Header().Get("Content-Type") != "video/mp4" {
				t.Fatalf("unexpected content %s %v", content.Body, content.Header())
			}
		})
	}
}
