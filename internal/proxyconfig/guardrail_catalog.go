package proxyconfig

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
)

//go:embed guardrail-catalog.json.gz
var guardrailCatalogSnapshot []byte

type guardrailCatalog struct {
	Settings json.RawMessage `json:"settings"`
	Providers json.RawMessage `json:"providers"`
	Airlines json.RawMessage `json:"airlines"`
	Categories map[string]json.RawMessage `json:"categories"`
}

var loadGuardrailCatalog = sync.OnceValues(func() (guardrailCatalog, error) {
	var catalog guardrailCatalog
	reader, err := gzip.NewReader(bytes.NewReader(guardrailCatalogSnapshot))
	if err != nil {
		return catalog, err
	}
	defer reader.Close()
	err = json.NewDecoder(reader).Decode(&catalog)
	return catalog, err
})

func (registry *modelRegistry) registerGuardrailCatalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /guardrails/ui/add_guardrail_settings", registry.serveGuardrailCatalog)
	mux.HandleFunc("GET /guardrails/ui/provider_specific_params", registry.serveGuardrailCatalog)
	mux.HandleFunc("GET /guardrails/ui/major_airlines", registry.serveGuardrailCatalog)
	mux.HandleFunc("GET /guardrails/ui/category_yaml/{category_name}", registry.serveGuardrailCatalog)
}

func (registry *modelRegistry) serveGuardrailCatalog(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
		return
	}
	catalog, err := loadGuardrailCatalog()
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Guardrail catalog is unavailable")
		return
	}
	switch r.URL.Path {
	case "/guardrails/ui/add_guardrail_settings":
		modelJSON(w, http.StatusOK, catalog.Settings)
	case "/guardrails/ui/provider_specific_params":
		modelJSON(w, http.StatusOK, catalog.Providers)
	case "/guardrails/ui/major_airlines":
		modelJSON(w, http.StatusOK, catalog.Airlines)
	default:
		name := r.PathValue("category_name")
		body, ok := catalog.Categories[name]
		if !ok {
			modelJSON(w, http.StatusNotFound, map[string]string{"detail": "Category file not found: "+name+" (tried .yaml and .json)"})
			return
		}
		modelJSON(w, http.StatusOK, body)
	}
}
