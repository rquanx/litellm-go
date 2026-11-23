package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthProbesMediaAndNativeResponses(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected probe method %s", r.Method)
		}
		if r.URL.Path == "/v1/audio/transcriptions" {
			f, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("missing audio fixture: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if header.Filename != "audio_health_check.wav" || string(data) != string(healthProbeWAV) || r.FormValue("model") != "fixture" {
				t.Error("invalid transcription probe")
			}
			io.WriteString(w, `{"text":"test"}`)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["model"] != "fixture" {
			t.Errorf("invalid health probe: %v", payload)
		}
		switch r.URL.Path {
		case "/v1/responses":
			if payload["input"] != "ping" || payload["stream"] != false || payload["max_output_tokens"] != nil {
				t.Errorf("invalid Responses probe: %v", payload)
			}
		case "/v1/images/generations":
			if payload["prompt"] != "ping" {
				t.Errorf("invalid image probe: %v", payload)
			}
		case "/v1/ocr":
			document, _ := payload["document"].(map[string]any)
			if document["document_url"] != healthProbePDF {
				t.Error("missing the local OCR test document")
			}
		default:
			t.Errorf("unexpected probe path %s", r.URL.Path)
		}
		io.WriteString(w, "{}")
	}))
	defer origin.Close()

	var routes []Route
	for _, endpoint := range []string{"responses", "image-generation", "transcription", "ocr"} {
		routes = append(routes, Route{Alias: endpoint, Candidates: []Candidate{{
			APIBase: origin.URL, ModelID: "fixture", DeploymentID: endpoint, Provider: "openai", Endpoint: endpoint,
		}}})
	}
	server := NewServer(Config{AuthorizeInference: true, Routes: routes})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"healthy_count":4`) {
		t.Fatalf("unexpected health response %d %s", response.Code, response.Body)
	}
}
