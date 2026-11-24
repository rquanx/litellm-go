package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestSupportedOpenAIParamsResolvesProviderPrefixedCustomModels(t *testing.T) {
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		query string
		status int
		text string
	}{
		{"?model=openai_like%2Fcustom-model", 200, "temperature"},
		{"?model=openai%2Fgpt-4o", 200, "response_format"},
		{"?model=unrecognized-fixture", 400, "Could not map model"},
		{"", 422, "Field required"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/utils/supported_openai_params"+scenario.query, nil)
		request.Header.Set("Authorization", "Bearer master")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != scenario.status || !strings.Contains(response.Body.String(), scenario.text) {
			t.Fatalf("%s response = %d %s", scenario.query, response.Code, response.Body)
		}
	}
}
