package proxyconfig

import (
	"crypto/rand"
	"crypto/subtle"
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
	path   string
	mu     sync.Mutex
	models []deployment
}

func WithModelManagement(server *runtime.Server, config Config, token, statePath string) (http.Handler, error) {
	registry := &modelRegistry{server: server, base: config, token: token, path: statePath}
	registry.base.models = slices.Clone(config.models)
	if statePath != "" {
		if token == "" {
			return nil, errors.New("model state requires a proxy master key")
		}
		file, err := os.Open(statePath)
		if err == nil {
			readErr := decodeModelJSON(file, &registry.models)
			file.Close()
			if readErr != nil {
				return nil, errors.New("invalid model state")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	routes, err := registry.compile(registry.models)
	if err != nil {
		return nil, err
	}
	server.ReplaceConfiguredRoutes(routes)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /model/info", registry.info)
	mux.HandleFunc("GET /v1/model/info", registry.info)
	mux.HandleFunc("POST /model/new", registry.add)
	mux.HandleFunc("POST /model/delete", registry.remove)
	mux.HandleFunc("POST /model/update", registry.update)
	mux.HandleFunc("PATCH /model/{model_id}/update", registry.update)
	mux.Handle("/", server)
	return mux, nil
}

func (registry *modelRegistry) compile(models []deployment) ([]runtime.Route, error) {
	all := append(slices.Clone(registry.base.models), models...)
	seen := make(map[string]bool, len(all))
	for _, model := range all {
		if model.Info.ID == "" || seen[model.Info.ID] {
			return nil, errors.New("model IDs must be unique and nonempty")
		}
		seen[model.Info.ID] = true
	}
	return buildRoutes(all, registry.base.fallbacks, registry.base.defaultFallbacks)
}

func (registry *modelRegistry) allowed(w http.ResponseWriter, r *http.Request) bool {
	expected := "Bearer " + registry.token
	if registry.token == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(r.Header.Get("Authorization"))) != 1 {
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
		if model.Params.APIVersion == "" {
			model.Params.APIVersion = existing.Params.APIVersion
		}
		if model.Params.Weight == nil {
			model.Params.Weight = existing.Params.Weight
		}
		if model.Params.RPM == nil {
			model.Params.RPM = existing.Params.RPM
		}
		if model.Params.TPM == nil {
			model.Params.TPM = existing.Params.TPM
		}
		if model.Info.Endpoints == nil {
			model.Info.Endpoints = existing.Info.Endpoints
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
	routes, err := registry.compile(next)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if registry.path != "" && saveModels(registry.path, next) != nil {
		modelError(w, http.StatusInternalServerError, "Could not save model state")
		return false
	}
	registry.models = next
	registry.server.ReplaceConfiguredRoutes(routes)
	return true
}

func saveModels(path string, models []deployment) error {
	return saveJSONState(path, models)
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
