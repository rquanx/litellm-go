package proxyconfig

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed public-model-cost-map.json.gz
var publicPrices []byte

//go:embed public-providers.json.gz
var publicProviders []byte

//go:embed public-provider-fields.json.gz
var publicProviderFields []byte

func registerPublicCatalogs(mux *http.ServeMux) {
	mux.HandleFunc("GET /public/litellm_model_cost_map", servePublicCatalog(publicPrices))
	mux.HandleFunc("GET /public/providers", servePublicCatalog(publicProviders))
	mux.HandleFunc("GET /public/providers/fields", servePublicCatalog(publicProviderFields))
}

func servePublicCatalog(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Vary", "Accept-Encoding")
		for _, value := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(value), ";")
			if !strings.EqualFold(coding, "gzip") {
				continue
			}
			quality := 1.0
			if raw, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
				quality, _ = strconv.ParseFloat(raw, 64)
			}
			if quality > 0 {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Write(data)
				return
			}
		}
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read the public catalog")
			return
		}
		defer reader.Close()
		_, _ = io.Copy(w, reader)
	}
}
