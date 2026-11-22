package proxyconfig

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type modelRegistry struct {
	server *runtime.Server
	base   Config
	token  string
	store  stateStore
	mu     sync.Mutex
	models []deployment
	credentials map[string]providerCredential
	router routerPatch
	searchTools []managedSearchTool
	passThrough []passThroughConfig
	mcpServers map[string]mcpServerConfig
	mcpToolsets []runtime.MCPToolset
}

func WithModelManagement(server *runtime.Server, config Config, token, statePath string) (http.Handler, error) {
	return withModelManagement(server, config, token, chooseStateStore(statePath, nil, "models"))
}

func WithPostgresModelManagement(server *runtime.Server, config Config, token string, db *sql.DB) (http.Handler, error) {
	return withModelManagement(server, config, token, postgresModels(db, token))
}

func withModelManagement(server *runtime.Server, config Config, token string, store stateStore) (http.Handler, error) {
	registry := &modelRegistry{server: server, base: config, token: token, store: store}
	registry.base.models = slices.Clone(config.models)
	if store != nil {
		if token == "" {
			return nil, errors.New("model state requires a proxy master key")
		}
		var state modelState
		if err := store.load(&state); err != nil {
			return nil, errors.New("could not load dynamic model state")
		}
		registry.models, registry.credentials, registry.router = state.Models, state.Credentials, state.Router
		registry.searchTools = state.SearchTools
		registry.passThrough = state.PassThrough
		registry.mcpServers = state.MCPServers
		registry.mcpToolsets = state.MCPToolsets
	}
	config, err := registry.compileConfiguration(registry.models, registry.credentials, registry.router, registry.searchTools)
	if err != nil {
		return nil, err
	}
	server.ReplaceConfiguredRouting(config.Routes, config.SearchTools, config.searchFallbacks())
	if err := registry.loadPassThrough(); err != nil {
		return nil, err
	}
	if len(registry.mcpServers) > 0 {
		servers, err := registry.compileMCP(registry.mcpServers)
		if err != nil {
			return nil, err
		}
		server.ReplaceMCPServers(servers)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /model/info", registry.info)
	mux.HandleFunc("GET /v1/model/info", registry.info)
	mux.HandleFunc("POST /model/new", registry.add)
	mux.HandleFunc("POST /model/delete", registry.remove)
	mux.HandleFunc("POST /model/update", registry.update)
	mux.HandleFunc("PATCH /model/{model_id}/update", registry.update)
	registry.registerCredentialRoutes(mux)
	registry.registerSearchToolRoutes(mux)
	registry.registerPassThroughRoutes(mux)
	registry.registerMCPRoutes(mux)
	registry.registerMCPToolsetRoutes(mux)
	server.ReplaceMCPToolsets(registry.mcpToolsets)
	mux.HandleFunc("GET /router/settings", registry.routerSettings)
	mux.HandleFunc("GET /router/fields", registry.routerSettings)
	mux.HandleFunc("POST /config/update", registry.updateRouter)
	mux.Handle("/", server)
	return mux, nil
}

func (registry *modelRegistry) compile(models []deployment) ([]runtime.Route, error) {
	return registry.compileState(models, registry.credentials)
}

func (registry *modelRegistry) compileState(models []deployment, credentials map[string]providerCredential) ([]runtime.Route, error) {
	return registry.compileRouter(models, credentials, registry.router)
}

func (registry *modelRegistry) compileRouter(models []deployment, credentials map[string]providerCredential, router routerPatch) ([]runtime.Route, error) {
	config, err := registry.compileConfiguration(models, credentials, router, registry.searchTools)
	return config.Routes, err
}

func (registry *modelRegistry) compileConfiguration(models []deployment, credentials map[string]providerCredential, router routerPatch, searchTools []managedSearchTool) (Config, error) {
	all := append(slices.Clone(registry.base.models), models...)
	seen := make(map[string]bool, len(all))
	for _, model := range all {
		if model.Info.ID == "" || seen[model.Info.ID] {
			return Config{}, errors.New("model IDs must be unique and nonempty")
		}
		seen[model.Info.ID] = true
	}
	config := router.apply(registry.base)
	config.credentials = combinedCredentials(config.credentials, credentials)
	var err error
	config.SearchTools, err = combineSearchTools(config.SearchTools, searchTools)
	if err != nil {
		return Config{}, err
	}
	config.Routes, err = config.buildRoutes(all)
	return config, err
}

func (registry *modelRegistry) allowed(w http.ResponseWriter, r *http.Request) bool {
	if registry.token == "" || subtle.ConstantTimeCompare([]byte(registry.token), []byte(runtime.ProxyCredential(r))) != 1 {
		modelError(w, http.StatusUnauthorized, "Proxy master key required")
		return false
	}
	return true
}

func (registry *modelRegistry) info(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	models := append(slices.Clone(registry.base.models), registry.models...)
	registry.mu.Unlock()
	id := r.URL.Query().Get("litellm_model_id")
	result := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if id == "" || id == model.Info.ID {
			result = append(result, publicModel(model))
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"data": result})
}

func (registry *modelRegistry) add(w http.ResponseWriter, r *http.Request) {
	registry.writeModel(w, r, false)
}

func (registry *modelRegistry) update(w http.ResponseWriter, r *http.Request) {
	registry.writeModel(w, r, true)
}

func (registry *modelRegistry) writeModel(w http.ResponseWriter, r *http.Request, update bool) {
	if !registry.allowed(w, r) {
		return
	}
	var model deployment
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &model); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported model configuration")
		return
	}
	if model.Info.ID == "" && !update {
		var random [16]byte
		rand.Read(random[:])
		model.Info.ID = hex.EncodeToString(random[:])
	}
	if pathID := r.PathValue("model_id"); pathID != "" {
		if model.Info.ID != "" && model.Info.ID != pathID {
			modelError(w, http.StatusBadRequest, "Conflicting model IDs")
			return
		}
		model.Info.ID = pathID
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := slices.Clone(registry.models)
	if update {
		index := modelIndex(next, model.Info.ID)
		if index < 0 {
			modelError(w, http.StatusBadRequest, "Dynamic model ID not found")
			return
		}
		existing := next[index]
		if model.Alias == "" {
			model.Alias = existing.Alias
		}
		if model.Params.Model == "" {
			model.Params.Model = existing.Params.Model
		}
		if model.Params.APIBase == "" {
			model.Params.APIBase = existing.Params.APIBase
		}
		if model.Params.APIKey == "" {
			model.Params.APIKey = existing.Params.APIKey
		}
		if model.Params.CredentialName == "" {
			model.Params.CredentialName = existing.Params.CredentialName
		}
		if model.Params.APIVersion == "" {
			model.Params.APIVersion = existing.Params.APIVersion
		}
		if model.Params.AzureADToken == "" {
			model.Params.AzureADToken = existing.Params.AzureADToken
		}
		if model.Params.Weight == nil {
			model.Params.Weight = existing.Params.Weight
		}
		if model.Params.InputPrice == nil {
			model.Params.InputPrice = existing.Params.InputPrice
		}
		if model.Params.OutputPrice == nil {
			model.Params.OutputPrice = existing.Params.OutputPrice
		}
		if model.Params.RPM == nil {
			model.Params.RPM = existing.Params.RPM
		}
		if model.Params.TPM == nil {
			model.Params.TPM = existing.Params.TPM
		}
		if model.Params.AWSRegion == "" {
			model.Params.AWSRegion = existing.Params.AWSRegion
		}
		if model.Params.AWSAccessKey == "" {
			model.Params.AWSAccessKey = existing.Params.AWSAccessKey
		}
		if model.Params.AWSSecretKey == "" {
			model.Params.AWSSecretKey = existing.Params.AWSSecretKey
		}
		if model.Params.AWSSessionToken == "" {
			model.Params.AWSSessionToken = existing.Params.AWSSessionToken
		}
		if model.Params.AWSProfile == "" {
			model.Params.AWSProfile = existing.Params.AWSProfile
		}
		if model.Params.VertexProject == "" {
			model.Params.VertexProject = existing.Params.VertexProject
		}
		if model.Params.VertexLocation == "" {
			model.Params.VertexLocation = existing.Params.VertexLocation
		}
		if model.Info.Endpoints == nil {
			model.Info.Endpoints = existing.Info.Endpoints
		}
		if model.Info.Mode == "" {
			model.Info.Mode = existing.Info.Mode
		}
		next[index] = model
	} else {
		next = append(next, model)
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, publicModel(model))
	}
}

func (registry *modelRegistry) remove(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid model ID")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	index := modelIndex(registry.models, input.ID)
	if index < 0 {
		modelError(w, http.StatusBadRequest, "Dynamic model ID not found")
		return
	}
	next := slices.Delete(slices.Clone(registry.models), index, index+1)
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]string{"message": "Model: " + input.ID + " deleted successfully"})
	}
}

func (registry *modelRegistry) apply(w http.ResponseWriter, next []deployment) bool {
	return registry.applyState(w, next, registry.credentials)
}

func (registry *modelRegistry) applyState(w http.ResponseWriter, models []deployment, credentials map[string]providerCredential) bool {
	return registry.publish(w, models, credentials, registry.router)
}

func (registry *modelRegistry) publish(w http.ResponseWriter, models []deployment, credentials map[string]providerCredential, router routerPatch) bool {
	return registry.publishConfiguration(w, models, credentials, router, registry.searchTools)
}

func (registry *modelRegistry) publishConfiguration(w http.ResponseWriter, models []deployment, credentials map[string]providerCredential, router routerPatch, searchTools []managedSearchTool) bool {
	config, err := registry.compileConfiguration(models, credentials, router, searchTools)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if registry.store != nil {
		if err := registry.store.save(modelState{Models: models, Credentials: credentials, Router: router, SearchTools: searchTools,
			PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets}); err != nil {
			stateWriteError(w, err)
			return false
		}
	}
	registry.models, registry.credentials, registry.router = models, credentials, router
	registry.searchTools = searchTools
	registry.server.ReplaceConfiguredRouting(config.Routes, config.SearchTools, config.searchFallbacks())
	return true
}

func stateWriteError(w http.ResponseWriter, err error) {
	if errors.Is(err, errConcurrentState) {
		modelError(w, http.StatusConflict, "Proxy state was changed by another process")
		return
	}
	modelError(w, http.StatusInternalServerError, "Could not save proxy state")
}

func saveJSONState(path string, payload any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".minato-models-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(payload)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return os.Rename(file.Name(), path)
}

func decodeModelJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON value")
	}
	return nil
}

func modelIndex(models []deployment, id string) int {
	return slices.IndexFunc(models, func(model deployment) bool { return model.Info.ID == id })
}

func publicModel(model deployment) map[string]any {
	return map[string]any{
		"model_name": model.Alias, "model_info": model.Info,
		"litellm_params": map[string]string{"model": model.Params.Model},
	}
}

func modelError(w http.ResponseWriter, status int, message string) {
	modelJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

func modelJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
