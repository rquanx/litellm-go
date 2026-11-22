package proxyconfig

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

//go:embed public-model-hub-info.json.gz
var publicModelHubInfo []byte

//go:embed model-hub-metadata.json.gz
var modelHubMetadata []byte

var loadModelHubMetadata = sync.OnceValue(func() modelHubCatalog {
	var catalog modelHubCatalog
	reader, err := gzip.NewReader(bytes.NewReader(modelHubMetadata))
	if err == nil {
		defer reader.Close()
		_ = json.NewDecoder(reader).Decode(&catalog)
	}
	return catalog
})

type modelHubCatalog struct {
	OpenAIChatParams []string                  `json:"openai_chat_params"`
	Models           map[string]map[string]any `json:"models"`
}

func publicDeploymentMetadata(candidate runtime.Candidate) (map[string]any, []string) {
	catalog := loadModelHubMetadata()
	metadata := catalog.Models[candidate.Provider+"/"+candidate.ModelID]
	if metadata == nil && candidate.Provider == "openai" {
		metadata = catalog.Models[candidate.ModelID]
	}
	var params []string
	if candidate.Provider == "openai" && candidate.Endpoint == "chat-completions" &&
		strings.HasPrefix(candidate.ModelID, "gpt-") && !strings.HasPrefix(candidate.ModelID, "gpt-5") {
		params = append(params, catalog.OpenAIChatParams...)
		if candidate.ModelID != "gpt-4" && candidate.ModelID != "gpt-3.5-turbo-16k" {
			params = append(params, "response_format")
		}
	}
	return metadata, params
}

func (registry *modelRegistry) registerPublicModelHub(mux *http.ServeMux) {
	mux.HandleFunc("GET /public/model_hub", func(w http.ResponseWriter, r *http.Request) {
		groups := registry.server.PublicModelGroups(r.Context(), registry.base.publicModelGroups, publicDeploymentMetadata)
		modelJSON(w, http.StatusOK, groups)
	})
	info := servePublicCatalog(publicModelHubInfo)
	if len(registry.base.publicModelLinks) > 0 {
		reader, err := gzip.NewReader(bytes.NewReader(publicModelHubInfo))
		if err == nil {
			var fields map[string]any
			err = json.NewDecoder(reader).Decode(&fields)
			reader.Close()
			if err == nil {
				fields["useful_links"] = registry.base.publicModelLinks
				info = func(w http.ResponseWriter, _ *http.Request) { modelJSON(w, http.StatusOK, fields) }
			}
		}
	}
	mux.HandleFunc("GET /public/model_hub/info", info)
}
