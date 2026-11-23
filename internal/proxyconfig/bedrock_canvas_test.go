package proxyconfig

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestBedrockCanvasImageGenerationAndEdits(t *testing.T) {
	bodies := make(chan string, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/amazon.nova-canvas-v1:0/invoke" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=fixture/") {
			t.Errorf("unexpected Nova Canvas upstream request: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		io.WriteString(w, `{"images":["AQID"]}`)
	}))
	defer upstream.Close()
	t.Setenv("MINATO_CANVAS_FIXTURE_URL", upstream.URL)
	proxy, err := Load(strings.NewReader(`
model_list:
  - model_name: canvas
    litellm_params:
      model: bedrock/amazon.nova-canvas-v1:0
      api_base: os.environ/MINATO_CANVAS_FIXTURE_URL
      aws_region_name: us-east-1
      aws_access_key_id: fixture
      aws_secret_access_key: fixture-secret
    model_info: {supported_endpoints: [/v1/images/generations, /v1/images/edits]}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: proxy.Routes})
	check := func(request *http.Request, expected string) {
		t.Helper()
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"b64_json":"AQID"`) {
			t.Fatalf("status %d: %s", response.Code, response.Body)
		}
		select {
		case payload := <-bodies:
			var body json.RawMessage
			if json.Unmarshal([]byte(payload), &body) != nil || string(body) != expected {
				t.Fatalf("request body = %s, want %s", payload, expected)
			}
		default:
			t.Fatal("provider request was missing")
		}
	}
	check(httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"canvas","prompt":"example","size":"512x768","n":2,"quality":"hd"}`)),
		`{"imageGenerationConfig":{"height":768,"numberOfImages":2,"quality":"premium","width":512},"taskType":"TEXT_IMAGE","textToImageParams":{"text":"example"}}`)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for key, value := range map[string]string{"model": "canvas", "prompt": "paint", "n": "2"} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"image", "mask"} {
		part, err := writer.CreateFormFile(field, field+".png")
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
	request := httptest.NewRequest("POST", "/v1/images/edits", &form)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	check(request, `{"imageGenerationConfig":{"numberOfImages":2},"inPaintingParams":{"image":"AQID","maskImage":"AQID","text":"paint"},"taskType":"INPAINTING"}`)
	check(httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(`{"model":"canvas","taskType":"BACKGROUND_REMOVAL","image":"AQID","n":2}`)),
		`{"backgroundRemovalParams":{"image":"AQID"},"taskType":"BACKGROUND_REMOVAL"}`)
}
