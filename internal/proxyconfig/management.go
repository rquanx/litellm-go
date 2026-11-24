package proxyconfig

import (
	"bytes"
	"context"
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
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type modelRegistry struct {
	handler     http.Handler
	keys        *KeyRegistry
	server      *runtime.Server
	base        Config
	token       string
	store       stateStore
	mu          sync.Mutex
	models      []deployment
	credentials map[string]providerCredential
	router      routerPatch
	searchTools []managedSearchTool
	passThrough []passThroughConfig
	mcpServers  map[string]mcpServerConfig
	temporaryMCP map[string]temporaryMCPServer
	mcpToolsets []runtime.MCPToolset
	toolPolicies map[string]toolPolicy
	mcpSemanticFilter map[string]any
	allowedIPs []string
	publicHub   publicModelHubState
}

func WithModelManagement(server *runtime.Server, config Config, token, statePath string, registries ...*KeyRegistry) (http.Handler, error) {
	var keys *KeyRegistry
	if len(registries) != 0 {
		keys = registries[0]
	}
	return withModelManagement(server, config, token, chooseStateStore(statePath, nil, "models"), keys)
}

func WithPostgresModelManagement(ctx context.Context, server *runtime.Server, config Config, token string, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	registry, err := withModelManagement(server, config, token, postgresModels(db, token), keys)
	if err != nil {
		return nil, err
	}
	go refreshPostgresRegistry(ctx, "managed configuration", registry.refreshManagedState)
	return registry, nil
}

func withModelManagement(server *runtime.Server, config Config, token string, store stateStore, keys *KeyRegistry) (*modelRegistry, error) {
	registry := &modelRegistry{server: server, base: config, token: token, store: store, keys: keys}
	registry.allowedIPs = slices.Clone(config.AllowedIPs)
	server.ReplaceAllowedIPs(registry.allowedIPs)
	registry.base.models = slices.Clone(config.models)
	if store != nil {
		if token == "" {
			return nil, errors.New("model state requires a proxy master key")
		}
		var state modelState
		if err := store.load(&state); err != nil {
			return nil, errors.New("could not load dynamic model state")
		}
		registry.setManagedState(state)
	}
	config, err := registry.compileConfiguration(registry.models, registry.credentials, registry.router, registry.searchTools)
	if err != nil {
		return nil, err
	}
	server.ReplaceConfiguredRouting(config.Routes, config.SearchTools, config.searchFallbacks())
	keys.setModelAccessGroups(registry.base.models, registry.models)
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
	mux.HandleFunc("GET /v2/model/info", registry.infoV2)
	mux.HandleFunc("POST /model/new", registry.add)
	mux.HandleFunc("POST /health/test_connection", registry.testModelConnection)
	mux.HandleFunc("POST /model/delete", registry.remove)
	mux.HandleFunc("POST /model/update", registry.update)
	mux.HandleFunc("PATCH /model/{model_id}/update", registry.update)
	registry.registerCredentialRoutes(mux)
	registry.registerModelAccessGroups(mux)
	registry.registerSearchToolRoutes(mux)
	registry.registerPassThroughRoutes(mux)
	registry.registerMCPRoutes(mux)
	registry.registerGuardrailCatalogRoutes(mux)
	registry.registerMCPToolsetRoutes(mux)
	registry.registerToolPolicyRoutes(mux)
	registry.registerAllowedIPRoutes(mux)
	mux.HandleFunc("GET /adaptive_router/state", func(w http.ResponseWriter, r *http.Request) {
		if registry.keys.requireAdminRead(w, r) {
			modelError(w, http.StatusNotFound, "No adaptive_router is configured on this proxy.")
		}
	})
	registry.registerMCPSemanticFilterRoutes(mux)
	registerPublicCatalogs(mux)
	mux.HandleFunc("GET /.well-known/litellm-ui-config", publicUIConfig)
	mux.HandleFunc("GET /litellm/.well-known/litellm-ui-config", publicUIConfig)
	registry.registerPublicModelHub(mux)
	mux.HandleFunc("POST /cost/estimate", registry.estimateCost)
	mux.HandleFunc("POST /spend/calculate", registry.calculateSpend)
	mux.HandleFunc("GET /model/cost_map/source", registry.costMapSource)
	mux.HandleFunc("GET /utils/supported_openai_params", registry.supportedOpenAIParams)
	modelSettings := servePublicCatalog(providerModelSettings)
	mux.HandleFunc("GET /model/settings", func(w http.ResponseWriter, r *http.Request) {
		if registry.allowModelMetadata(w, r) {
			modelSettings(w, r)
		}
	})
	server.ReplaceMCPToolsets(registry.mcpToolsets)
	registry.syncRuntimeToolPolicies()
	mux.HandleFunc("GET /router/settings", registry.routerSettings)
	mux.HandleFunc("GET /router/fields", registry.routerSettings)
	mux.HandleFunc("POST /config/update", registry.updateRouter)
	mux.HandleFunc("POST /fallback", registry.manageFallback)
	mux.HandleFunc("GET /fallback/{model}", registry.manageFallback)
	mux.HandleFunc("DELETE /fallback/{model}", registry.manageFallback)
	mux.Handle("/", server)
	registry.handler = mux
	return registry, nil
}

func (registry *modelRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	registry.handler.ServeHTTP(w, r)
}

func (registry *modelRegistry) setManagedState(state modelState) {
	registry.allowedIPs = slices.Clone(state.AllowedIPs)
	if len(registry.allowedIPs) == 0 {
		registry.allowedIPs = slices.Clone(registry.base.AllowedIPs)
	}
	registry.server.ReplaceAllowedIPs(registry.allowedIPs)
	registry.models, registry.credentials, registry.router = state.Models, state.Credentials, state.Router
	registry.searchTools, registry.passThrough = state.SearchTools, state.PassThrough
	registry.mcpServers, registry.mcpToolsets, registry.publicHub = state.MCPServers, state.MCPToolsets, state.PublicHub
	registry.toolPolicies = state.ToolPolicies
	registry.mcpSemanticFilter = state.MCPSemanticFilter
}

func (registry *modelRegistry) refreshManagedState(ctx context.Context) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	store := registry.store.(*postgresState)
	next := *store
	var state modelState
	changed, err := next.loadAfter(ctx, &state, store.revision)
	if err != nil || !changed {
		return err
	}
	cfg, err := registry.compileConfiguration(state.Models, state.Credentials, state.Router, state.SearchTools)
	if err != nil {
		return err
	}
	passThrough, err := registry.compilePassThrough(state.PassThrough)
	if err != nil {
		return err
	}
	servers, err := registry.compileMCP(state.MCPServers)
	if err != nil {
		return err
	}
	registry.server.ReplaceManagedConfiguration(cfg.Routes, cfg.SearchTools, cfg.searchFallbacks(), passThrough, servers, state.MCPToolsets)
	registry.setManagedState(state)
	registry.keys.setModelAccessGroups(registry.base.models, registry.models)
	store.revision = next.revision
	return nil
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
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var model deployment
	if err != nil || bytes.Equal(bytes.TrimSpace(body), []byte("null")) || decodeModelJSON(bytes.NewReader(body), &model) != nil {
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
	now := time.Now().UTC()
	if update {
		index := modelIndex(next, model.Info.ID)
		if index < 0 {
			modelError(w, http.StatusBadRequest, "Dynamic model ID not found")
			return
		}
		// Decode into an independent copy so omitted fields retain their value
		// and a rejected update cannot mutate the current deployment.
		existing, err := json.Marshal(next[index])
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not copy current deployment")
			return
		}
		var updated deployment
		if json.Unmarshal(existing, &updated) != nil {
			modelError(w, http.StatusInternalServerError, "Could not copy current deployment")
			return
		}
		if json.Unmarshal(body, &updated) != nil {
			modelError(w, http.StatusBadRequest, "Invalid model update")
			return
		}
		updated.Info.ID = model.Info.ID
		updated.Info.CreatedAt = next[index].Info.CreatedAt
		updated.Info.CreatedBy = next[index].Info.CreatedBy
		updated.Info.UpdatedAt = &now
		model = updated
		next[index] = model
	} else {
		model.Info.CreatedAt = &now
		model.Info.UpdatedAt = &now
		model.Info.CreatedBy = "default_user_id"
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
		if err := registry.store.save(modelState{AllowedIPs: registry.allowedIPs, Models: models, Credentials: credentials, Router: router, SearchTools: searchTools,
			PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets, ToolPolicies: registry.toolPolicies, MCPSemanticFilter: registry.mcpSemanticFilter, PublicHub: registry.publicHub}); err != nil {
			stateWriteError(w, err)
			return false
		}
	}
	registry.models, registry.credentials, registry.router = models, credentials, router
	registry.searchTools = searchTools
	registry.server.ReplaceConfiguredRouting(config.Routes, config.SearchTools, config.searchFallbacks())
	registry.keys.setModelAccessGroups(registry.base.models, models)
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
