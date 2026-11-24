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

	"github.com/rquanx/litellm-go/internal/runtime"
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

func TestBedrockTitanAndStabilityImageAdapters(t *testing.T) {
	for _, fixture := range []struct {
		model, path, input, payload, result string
		status                              int
	}{
		{"amazon.titan-image-generator-v2:0", "/v1/images/generations", `"prompt":"example","quality":"high","negativeText":"haze"`,
			`{"imageGenerationConfig":{"quality":"premium"},"taskType":"TEXT_IMAGE","textToImageParams":{"negativeText":"haze","text":"example"}}`, `{"images":["AQID"]}`, 200},
		{"stability.stable-diffusion-xl-v1", "/v1/images/generations", `"prompt":"line\nbreak","size":"512x768","steps":40`,
			`{"height":768,"steps":40,"text_prompts":[{"text":"line break","weight":1}],"width":512}`, `{"artifacts":[{"base64":"AQID"}]}`, 200},
		{"stability.sd3-large-v1:0", "/v1/images/generations", `"prompt":"example","aspect_ratio":"16:9"`,
			`{"aspect_ratio":"16:9","prompt":"example"}`, `{"images":["AQID"],"finish_reasons":[null]}`, 200},
		{"stability.stable-style-transfer-v1:0", "/v1/images/edits", `"image":"AQID","style_image":"BAUG","style_strength":0.5`,
			`{"init_image":"AQID","output_format":"png","style_image":"BAUG","style_strength":0.5}`, `{"finish_reasons":["fixture declined"]}`, 400},
	} {
		t.Run(fixture.model, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/model/"+fixture.model+"/invoke" || string(body) != fixture.payload {
					t.Errorf("unexpected upstream request %s %s", r.URL.Path, body)
				}
				io.WriteString(w, fixture.result)
			}))
			defer upstream.Close()
			t.Setenv("MINATO_BEDROCK_IMAGE_FIXTURE_URL", upstream.URL)
			proxy, err := Load(strings.NewReader(`
model_list:
  - model_name: fixture
    litellm_params:
      model: bedrock/` + fixture.model + `
      api_base: os.environ/MINATO_BEDROCK_IMAGE_FIXTURE_URL
      aws_region_name: us-east-1
      aws_access_key_id: fixture
      aws_secret_access_key: fixture-secret
`))
			if err != nil {
				t.Fatal(err)
			}
			server := runtime.NewServer(runtime.Config{Routes: proxy.Routes})
			request := httptest.NewRequest("POST", fixture.path, strings.NewReader(`{"model":"fixture",`+fixture.input+`}`))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			expected := `"b64_json":"AQID"`
			if fixture.status == 400 {
				expected = `"message":"fixture declined"`
			}
			if response.Code != fixture.status || !strings.Contains(response.Body.String(), expected) {
				t.Fatalf("unexpected result %d %s", response.Code, response.Body)
			}
		})
	}
}
