package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestPolicyTemplateLocalEnrichment(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	policies, err := OpenPolicyRegistry(nil, nil)
	if err != nil { t.Fatal(err) }
	handler := policies.Handler(http.NotFoundHandler(), "master", keys, runtime.NewServer(runtime.Config{}))
	req := httptest.NewRequest(http.MethodPost, "/policy/templates/enrich", strings.NewReader(`{"template_id":"competitor-mention-detection","parameters":{"brand_name":"Acme"},"competitors":["Rival One","Rival Two"]}`))
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder(); handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Rival One") || !strings.Contains(w.Body.String(), "guardrailDefinitions") {
		t.Fatalf("unexpected enrichment response %d: %s", w.Code, w.Body.String())
	}
}

func TestPolicyTemplateLocalTestAndSuggest(t *testing.T) {
	keys, err := LoadKeys("master", ""); if err != nil { t.Fatal(err) }
	policies, err := OpenPolicyRegistry(nil, nil); if err != nil { t.Fatal(err) }
	handler := policies.Handler(http.NotFoundHandler(), "master", keys, runtime.NewServer(runtime.Config{}))
	call := func(path, body string) string { req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)); req.Header.Set("Authorization", "Bearer master"); w := httptest.NewRecorder(); handler.ServeHTTP(w, req); if w.Code != http.StatusOK { t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String()) }; return w.Body.String() }
	if body := call("/policy/templates/test", `{"guardrail_definitions":[{"guardrail_name":"fixture","litellm_params":{"blocked_words":"secret\npassword"}}],"text":"Please keep this secret"}`); !strings.Contains(body, `"overall_action":"blocked"`) { t.Fatalf("template test did not block: %s", body) }
	if body := call("/policy/templates/suggest", `{"description":"PII protection"}`); !strings.Contains(body, "suggestions") { t.Fatalf("template suggestions missing: %s", body) }
	if body := call("/policy/templates/enrich/stream", `{"template_id":"competitor-mention-detection","parameters":{"brand_name":"Acme"},"competitors":["Rival One"]}`); !strings.Contains(body, "data: [DONE]") { t.Fatalf("template stream missing completion: %s", body) }
}
