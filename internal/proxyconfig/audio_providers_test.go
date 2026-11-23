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

func TestNativeSpeechAndTranscriptionProviders(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/text-to-speech/21m00Tcm4TlvDq8ikWAM":
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("xi-api-key") != "fixture" || r.URL.Query().Get("output_format") != "mp3_44100_128" ||
				!strings.Contains(string(body), `"voice_settings":{"speed":1.2}`) || !strings.Contains(string(body), `"text":"hello"`) {
				t.Errorf("incorrect speech request: %s %s", r.URL.RawQuery, body)
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "fixture audio")
		case "/v1/speech-to-text":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.Header.Get("xi-api-key") != "fixture" || r.FormValue("model_id") != "scribe_v2" || r.FormValue("language_code") != "en" {
				t.Errorf("incorrect ElevenLabs request: %v", r.Form)
			}
			fmt.Fprint(w, `{"text":"eleven fixture","language_code":"en","words":[{"text":"eleven","type":"word","start":0,"end":1},{"text":" ","type":"spacing"}]}`)
		case "/v1/listen":
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "Token fixture" || r.URL.Query().Get("model") != "nova-3" ||
				r.URL.Query().Get("language") != "en" || string(body) != "fixture bytes" {
				t.Errorf("incorrect Deepgram request: %s %s", r.URL.RawQuery, body)
			}
			fmt.Fprint(w, `{"metadata":{"duration":1.3},"results":{"channels":[{"alternatives":[{"transcript":"deepgram fixture","words":[{"word":"deepgram","start":0,"end":1}]}]}]}}`)
		default:
			t.Errorf("unexpected audio path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fixture.Close()
	t.Setenv("AUDIO_TEST_BASE", fixture.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: speech
    litellm_params: {model: elevenlabs/eleven_multilingual_v2, api_key: fixture, api_base: os.environ/AUDIO_TEST_BASE}
  - model_name: eleven
    litellm_params: {model: elevenlabs/scribe_v2, api_key: fixture, api_base: os.environ/AUDIO_TEST_BASE}
  - model_name: deep
    litellm_params: {model: deepgram/nova-3, api_key: fixture, api_base: os.environ/AUDIO_TEST_BASE}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	speech := httptest.NewRecorder()
	server.ServeHTTP(speech, httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"speech","input":"hello","voice":"alloy","response_format":"mp3","speed":1.2}`)))
	if speech.Code != 200 || speech.Header().Get("Content-Type") != "audio/mpeg" || speech.Body.String() != "fixture audio" {
		t.Fatal(speech.Body)
	}
	for alias, want := range map[string]string{"eleven": "eleven fixture", "deep": "deepgram fixture"} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("model", alias); err != nil {
			t.Fatal(err)
		}
		if err := form.WriteField("language", "en"); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "audio.wav")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(file, "fixture bytes")
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), want) || !strings.Contains(response.Body.String(), `"words"`) {
			t.Fatalf("%s returned %d: %s", alias, response.Code, response.Body)
		}
	}
}

func TestAzureSpeechModelUsesSSMLWithoutOpenAIVersion(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/cognitiveservices/v1" || r.Header.Get("Content-Type") != "application/ssml+xml" ||
			r.Header.Get("Ocp-Apim-Subscription-Key") != "fixture" ||
			r.Header.Get("X-Microsoft-OutputFormat") != "riff-24khz-16bit-mono-pcm" ||
			!strings.Contains(string(body), "en-US-JennyNeural") || !strings.Contains(string(body), "+50%") ||
			!strings.Contains(string(body), "&lt;tag&gt; &amp; content") {
			t.Errorf("incorrect Azure speech request: %s %v %s", r.URL.Path, r.Header, body)
		}
		w.Header().Set("Content-Type", "audio/wav")
		fmt.Fprint(w, "fixture-wav")
	}))
	defer fixture.Close()
	t.Setenv("AUDIO_TEST_BASE", fixture.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: speech
    litellm_params: {model: azure/speech/neural, api_key: fixture, api_base: os.environ/AUDIO_TEST_BASE}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"speech","input":"<tag> & content","voice":"alloy","response_format":"wav","speed":1.5}`)))
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/wav" || w.Body.String() != "fixture-wav" {
		t.Fatal(w.Body)
	}
}

func TestVertexSpeechDecodesAudio(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/text:synthesize" || r.Header.Get("Authorization") != "Bearer fixture" ||
			r.Header.Get("X-Goog-User-Project") != "fixture" || !strings.Contains(string(body), `"audioEncoding":"OGG_OPUS"`) ||
			!strings.Contains(string(body), `"name":"en-US-Studio-O"`) || !strings.Contains(string(body), `"ssml"`) {
			t.Errorf("incorrect Vertex speech request: %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"audioContent":"Zml4dHVyZQ=="}`)
	}))
	defer fixture.Close()
	t.Setenv("AUDIO_TEST_BASE", fixture.URL+"/v1/text:synthesize")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: speech
    litellm_params: {model: vertex_ai/tts, api_key: fixture, api_base: os.environ/AUDIO_TEST_BASE, vertex_project: fixture}
    model_info: {mode: audio_speech}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"speech","input":"<speak>hello</speak>","voice":"alloy","response_format":"opus"}`)))
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/ogg" || w.Body.String() != "fixture" {
		t.Fatal(w.Body)
	}
}
