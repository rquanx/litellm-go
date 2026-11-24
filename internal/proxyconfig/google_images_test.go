package proxyconfig

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestGoogleImageGenerationModes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.Contains(path, "/projects/") {
			if r.Header.Get("Authorization") != "Bearer fixture-vertex" || !strings.Contains(path, "/locations/us-central1/publishers/google/") {
				t.Errorf("invalid Vertex request: %s %v", path, r.Header)
			}
		} else if r.Header.Get("x-goog-api-key") != "fixture-gemini" {
			t.Error("Gemini credential missing")
		}
		switch {
		case strings.HasSuffix(path, ":predict"):
			if !strings.Contains(string(body), `"aspectRatio":"16:9"`) || !strings.Contains(string(body), `"sampleCount":2`) ||
				!strings.Contains(string(body), `"instances":[{"prompt":"image fixture"}]`) {
				t.Errorf("incorrect Imagen body: %s", body)
			}
			fmt.Fprint(w, `{"predictions":[{"bytesBase64Encoded":"AQID"}]}`)
		case strings.HasSuffix(path, ":generateContent"):
			if !strings.Contains(string(body), `"text":"image fixture"`) {
				t.Errorf("incorrect Gemini image body: %s", body)
			}
			if strings.Contains(path, "/projects/") {
				if !strings.Contains(string(body), `"responseModalities":["IMAGE"]`) {
					t.Errorf("incorrect Vertex Gemini image body: %s", body)
				}
			} else if !strings.Contains(string(body), `"response_modalities":["IMAGE","TEXT"]`) {
				t.Errorf("incorrect Gemini image body: %s", body)
			}
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"caption"},{"inlineData":{"data":"AQID"},"thoughtSignature":"fixture-signature"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5,"promptTokensDetails":[{"modality":"IMAGE","tokenCount":1},{"modality":"TEXT","tokenCount":1}]}}`)
		default:
			t.Errorf("unexpected image path: %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_GOOGLE_IMAGE_URL", upstream.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: gemini-image
    litellm_params: {model: gemini/gemini-2.5-flash-image, api_base: os.environ/TEST_GOOGLE_IMAGE_URL, api_key: fixture-gemini}
    model_info: {mode: image_generation}
  - model_name: imagen
    litellm_params: {model: gemini/imagen-4.0-generate-001, api_base: os.environ/TEST_GOOGLE_IMAGE_URL, api_key: fixture-gemini}
  - model_name: vertex-imagen
    litellm_params: {model: vertex_ai/imagen-4.0-generate-001, api_base: os.environ/TEST_GOOGLE_IMAGE_URL, api_key: fixture-vertex, vertex_project: fixture, vertex_location: us-central1}
  - model_name: vertex-gemini
    litellm_params: {model: vertex_ai/gemini-2.5-flash-image, api_base: os.environ/TEST_GOOGLE_IMAGE_URL, api_key: fixture-vertex, vertex_project: fixture, vertex_location: us-central1}
    model_info: {supported_endpoints: [/v1/images/generations]}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	for _, alias := range []string{"gemini-image", "imagen", "vertex-imagen", "vertex-gemini"} {
		request := httptest.NewRequest("POST", "/v1/images/generations",
			strings.NewReader(`{"model":"`+alias+`","prompt":"image fixture","size":"1792x1024","n":2}`))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		result := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(result, `"b64_json":"AQID"`) {
			t.Fatalf("%s returned %d: %s", alias, recorder.Code, result)
		}
		if strings.Contains(alias, "gemini") && (!strings.Contains(result, `"thought_signature":"fixture-signature"`) ||
			!strings.Contains(result, `"total_tokens":5`)) {
			t.Errorf("Gemini image metadata missing: %s", result)
		}
	}
}

func TestGoogleImageEditMultipart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, ":predict"):
			if !strings.Contains(string(body), `"referenceType":"REFERENCE_TYPE_RAW"`) ||
				!strings.Contains(string(body), `"referenceType":"REFERENCE_TYPE_MASK"`) ||
				!strings.Contains(string(body), `"sampleCount":2`) {
				t.Errorf("unexpected Imagen edit request: %s", body)
			}
			fmt.Fprint(w, `{"predictions":[{"bytesBase64Encoded":"AQID"}]}`)
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			if !strings.Contains(string(body), `"inlineData":{"data":"AQID","mimeType":"image/png"}`) ||
				!strings.Contains(string(body), `"16:9"`) {
				t.Errorf("unexpected Gemini edit request: %s", body)
			}
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"AQID"}}]}}]}`)
		default:
			t.Errorf("unexpected edit path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("TEST_GOOGLE_EDIT_URL", upstream.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: gemini-edit
    litellm_params: {model: gemini/gemini-2.5-flash-image, api_base: os.environ/TEST_GOOGLE_EDIT_URL, api_key: fixture-gemini}
    model_info: {mode: image_edit}
  - model_name: vertex-edit
    litellm_params: {model: vertex_ai/imagen-3.0-capability-001, api_base: os.environ/TEST_GOOGLE_EDIT_URL, api_key: fixture-vertex, vertex_project: fixture, vertex_location: us-central1}
    model_info: {supported_endpoints: [/v1/images/edits]}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	for _, alias := range []string{"gemini-edit", "vertex-edit"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for key, value := range map[string]string{"model": alias, "prompt": "edit fixture", "size": "1792x1024", "n": "2"} {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"image", "mask"} {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Type", "image/png")
			header.Set("Content-Disposition", `form-data; name="`+name+`"; filename="fixture.png"`)
			part, err := writer.CreatePart(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write([]byte{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/v1/images/edits", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"b64_json":"AQID"`) {
			t.Fatalf("%s returned %d: %s", alias, recorder.Code, recorder.Body)
		}
	}
}
