package proxyconfig

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestMistralVoxtralMultipartTranscription(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Errorf("unexpected Voxtral target %s", r.URL)
		}
		f, info, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture form error", http.StatusBadRequest)
			return
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if string(data) != "audio fixture" || info.Filename != "fixture.wav" || r.FormValue("model") != "voxtral-mini-latest" ||
			r.FormValue("diarize") != "true" || r.FormValue("prompt") != "" || r.FormValue("language") != "en" {
			t.Errorf("unexpected Voxtral multipart request: %v", r.MultipartForm.Value)
		}
		fmt.Fprint(w, `{"text":"fixture text","language":"en","segments":[{"speaker_id":"spk_0"}],"extra":"hidden"}`)
	}))
	defer provider.Close()
	cfg, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: voxtral
    litellm_params:
      model: mistral/voxtral-mini-latest
      api_base: %q
      api_key: fixture
`, provider.URL+"/prefix/v1")))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: cfg.Routes})
	var request bytes.Buffer
	multipartWriter := multipart.NewWriter(&request)
	for k, v := range map[string]string{"model": "voxtral", "diarize": "true", "language": "en", "prompt": "ignored"} {
		if err := multipartWriter.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	file, err := multipartWriter.CreateFormFile("file", "fixture.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "audio fixture"); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &request)
	r.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, r)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"language":"en","segments":[{"speaker_id":"spk_0"}],"text":"fixture text","usage":null}` {
		t.Fatalf("unexpected Voxtral response %d %s", response.Code, response.Body)
	}
}

func TestWatsonXTranscriptionUsesIAMAndProject(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if r.ParseForm() != nil || r.Form.Get("apikey") != "fixture-iam" {
				t.Error("incorrect IAM exchange")
			}
			fmt.Fprint(w, `{"access_token":"fixture-token","expires_in":3600}`)
		case "/ml/v1/audio/transcriptions":
			if r.Header.Get("Authorization") != "Bearer fixture-token" || r.URL.Query().Get("version") != "2024-03-13" ||
				r.FormValue("project_id") != "fixture-project" || r.FormValue("space_id") != "" || r.FormValue("model") != "whisper" {
				t.Errorf("incorrect transcription request: %s", r.URL)
			}
			fmt.Fprint(w, `{"text":"fixture","model":"whisper","duration":1.5}`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	t.Setenv("WATSONX_IAM_URL", fixture.URL+"/token")
	t.Setenv("WATSONX_TOKEN", "")
	t.Setenv("WATSONX_ZENAPIKEY", "")
	cfg, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: watsonx/whisper
      api_base: %q
      api_key: fixture-iam
      watsonx_project: fixture-project
`, fixture.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Routes: cfg.Routes})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", "demo"); err != nil {
		t.Fatal(err)
	}
	file, err := form.CreateFormFile("file", "fixture.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"duration":1.5,"text":"fixture","usage":null}` {
		t.Fatalf("unexpected transcription output %d %s", w.Code, w.Body)
	}
}
