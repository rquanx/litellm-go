package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeOpenAPIDescribesEnabledAPIs(t *testing.T) {
	for _, settings := range []struct{ provider, management bool }{{true, true}, {false, true}, {true, false}, {false, false}} {
		handler := withNativeOpenAPI(http.NotFoundHandler(), settings.provider, settings.management)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/openapi.json", nil))
		var document struct {
			Version string `json:"openapi"`
			Paths map[string]map[string]json.RawMessage `json:"paths"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &document) != nil || document.Version != "3.1.0" {
			t.Fatalf("invalid OpenAPI document, status %d", response.Code)
		}
		if document.Paths["/v1/chat/completions"]["post"] == nil || document.Paths["/v1/models"]["get"] == nil {
			t.Fatal("core endpoints are missing")
		}
		if (document.Paths["/v1/assistants"]["post"] != nil) != settings.provider {
			t.Fatal("OpenAPI did not respect the provider API switch")
		}
		if (document.Paths["/v2/rerank"]["post"] != nil) != settings.provider ||
			(document.Paths["/containers/{container_id}/files/{file_id}/content"]["get"] != nil) != settings.provider {
			t.Fatal("OpenAPI did not respect the provider API switch for aliases")
		}
		if (document.Paths["/model/new"]["post"] != nil) != settings.management {
			t.Fatal("OpenAPI did not respect the management API switch")
		}
		if (document.Paths["/organization/daily/activity"]["get"] != nil) != settings.management ||
			(document.Paths["/organization/info"]["post"] != nil) != settings.management {
			t.Fatal("OpenAPI did not respect the management API switch for organizations")
		}
		if (document.Paths["/fallback"]["post"] != nil) != settings.management ||
			(document.Paths["/fallback/{model}"]["get"] != nil) != settings.management ||
			(document.Paths["/fallback/{model}"]["delete"] != nil) != settings.management {
			t.Fatal("OpenAPI did not respect the management API switch for fallbacks")
		}
		if document.Paths["/ui"] != nil {
			t.Fatal("the native OpenAPI document advertised an upstream-only path")
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/routes", nil))
		var catalog struct {
			Routes []struct {
				Path string `json:"path"`
				Methods []string `json:"methods"`
			} `json:"routes"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil {
			t.Fatalf("invalid route catalog, status %d", response.Code)
		}
		routes := make(map[string]bool)
		for _, entry := range catalog.Routes {
			for _, method := range entry.Methods {
				routes[method+" "+entry.Path] = true
			}
		}
		if !routes["GET /routes"] || !routes["POST /v1/chat/completions"] ||
			routes["POST /model/new"] != settings.management || routes["POST /v2/rerank"] != settings.provider {
			t.Fatal("route catalog did not match the enabled APIs")
		}
	}
}
