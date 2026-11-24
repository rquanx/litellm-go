package proxyconfig

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/rquanx/litellm-go/internal/runtime"
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

type publicModelHubState struct {
	Groups []string       `json:"model_groups"`
	Links  map[string]any `json:"useful_links"`
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
		registry.mu.Lock()
		names := registry.publicHub.Groups
		registry.mu.Unlock()
		if names == nil {
			names = registry.base.publicModelGroups
		}
		modelJSON(w, http.StatusOK, registry.server.PublicModelGroups(r.Context(), names, publicDeploymentMetadata))
	})
	info := servePublicCatalog(publicModelHubInfo)
	mux.HandleFunc("GET /public/model_hub/info", func(w http.ResponseWriter, r *http.Request) {
		registry.mu.Lock()
		links := registry.publicHub.Links
		registry.mu.Unlock()
		if links == nil {
			links = registry.base.publicModelLinks
		}
		if len(links) == 0 {
			info(w, r)
			return
		}
		reader, err := gzip.NewReader(bytes.NewReader(publicModelHubInfo))
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read the model hub metadata")
			return
		}
		defer reader.Close()
		var fields map[string]any
		if err := json.NewDecoder(reader).Decode(&fields); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read the model hub metadata")
			return
		}
		fields["useful_links"] = links
		modelJSON(w, http.StatusOK, fields)
	})
	mux.HandleFunc("POST /model_group/make_public", registry.updatePublicModelHub)
	mux.HandleFunc("POST /model_hub/update_useful_links", registry.updatePublicModelHub)
}

func (registry *modelRegistry) updatePublicModelHub(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input publicModelHubState
	updateGroups := r.URL.Path == "/model_group/make_public"
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		(updateGroups && input.Groups == nil) || (!updateGroups && input.Links == nil) {
		modelError(w, http.StatusUnprocessableEntity, "Invalid public model hub configuration")
		return
	}
	for _, link := range input.Links {
		switch link.(type) {
		case string, map[string]any:
		default:
			modelError(w, http.StatusUnprocessableEntity, "Each useful link must be a string or object")
			return
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.publicHub
	result := map[string]any{"updated_by": "default_user_id"}
	if updateGroups {
		next.Groups = input.Groups
		result["message"], result["public_model_groups"] = "Successfully updated public model groups", next.Groups
	} else {
		next.Links = input.Links
		result["message"], result["useful_links"] = "Successfully updated useful links", next.Links
	}
	if registry.store != nil {
		if err := registry.store.save(modelState{
			Models: registry.models, Credentials: registry.credentials, Router: registry.router, SearchTools: registry.searchTools,
			PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets, PublicHub: next,
		}); err != nil {
			stateWriteError(w, err)
			return
		}
	}
	registry.publicHub = next
	modelJSON(w, http.StatusOK, result)
}
