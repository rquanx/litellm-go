package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
)

//go:embed native-openapi.json.gz
var nativeOpenAPISnapshot []byte

func withNativeOpenAPI(next http.Handler, provider, management bool) http.Handler {
	type nativeDocuments struct {
		openAPI []byte
		routes  []byte
	}
	documents := sync.OnceValues(func() (nativeDocuments, error) {
		reader, err := gzip.NewReader(bytes.NewReader(nativeOpenAPISnapshot))
		if err != nil {
			return nativeDocuments{}, err
		}
		defer reader.Close()
		type operationGroup map[string]map[string]json.RawMessage
		var snapshot struct {
			Version string `json:"openapi"`
			Info json.RawMessage `json:"info"`
			Components json.RawMessage `json:"components"`
			Groups map[string]operationGroup `json:"groups"`
			Catalog map[string][]json.RawMessage `json:"catalog"`
		}
		if err := json.NewDecoder(reader).Decode(&snapshot); err != nil {
			return nativeDocuments{}, err
		}
		paths := make(operationGroup)
		routes := []json.RawMessage{}
		for _, group := range []string{"core", "provider", "management"} {
			if (group == "provider" && !provider) || (group == "management" && !management) {
				continue
			}
			routes = append(routes, snapshot.Catalog[group]...)
			for path, operations := range snapshot.Groups[group] {
				if paths[path] == nil {
					paths[path] = make(map[string]json.RawMessage)
				}
				for method, operation := range operations {
					paths[path][method] = operation
				}
			}
		}
		openAPI, err := json.Marshal(map[string]any{"openapi": snapshot.Version, "info": snapshot.Info,
			"components": snapshot.Components, "paths": paths})
		if err != nil {
			return nativeDocuments{}, err
		}
		catalog, err := json.Marshal(map[string][]json.RawMessage{"routes": routes})
		return nativeDocuments{openAPI: openAPI, routes: catalog}, err
	})
	mux := http.NewServeMux()
	describe := func(w http.ResponseWriter, r *http.Request) {
		document, err := documents()
		if err != nil {
			http.Error(w, "Could not read the OpenAPI document", http.StatusInternalServerError)
			return
		}
		body := document.openAPI
		if r.URL.Path == "/routes" {
			body = document.routes
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
	mux.HandleFunc("GET /openapi.json", describe)
	mux.HandleFunc("GET /routes", describe)
	mux.Handle("/", next)
	return mux
}
