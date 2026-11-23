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
