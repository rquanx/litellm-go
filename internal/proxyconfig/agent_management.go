package proxyconfig

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"sync"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type managedAgents struct {
	Entries []agentConfig `json:"agents"`
	Public  []string      `json:"public_agent_groups"`
}

func WithAgentManagement(next http.Handler, server *runtime.Server, config Config, database *sql.DB, statePath string) (http.Handler, error) {
	var store stateStore
	if database != nil {
		pg := postgresModels(database, config.MasterKey)
		pg.name = "agents"
		store = pg
	} else if statePath != "" {
		store = fileState(statePath + ".agents.json")
	}
	state := managedAgents{Public: slices.Clone(config.publicAgents)}
	if store != nil {
		if err := store.load(&state); err != nil {
			return nil, errors.New("could not load managed agent state")
		}
	}
	compile := func(state managedAgents) ([]runtime.Agent, error) {
		return prepareAgents(append(slices.Clone(config.staticAgents), state.Entries...), state.Public)
	}
	agents, err := compile(state)
	if err != nil {
		return nil, err
	}
	server.ReplaceAgents(agents)
	mux := http.NewServeMux()
	var lock sync.Mutex
	change := func(w http.ResponseWriter, r *http.Request) {
		if config.MasterKey == "" || runtime.ProxyCredential(r) != config.MasterKey {
			modelError(w, http.StatusUnauthorized, "A proxy master key is required")
			return
		}
		lock.Lock()
		defer lock.Unlock()
		proposed := managedAgents{Entries: slices.Clone(state.Entries), Public: slices.Clone(state.Public)}
		id := r.PathValue("agent_id")
		index := slices.IndexFunc(proposed.Entries, func(entry agentConfig) bool { return entry.ID == id })
		var changed agentConfig
		public := r.URL.Path == "/v1/agents/make_public" || r.PathValue("agent_id") != "" && r.Method == http.MethodPost
		if public {
			var payload struct {
				IDs []string `json:"agent_ids"`
			}
			if id != "" {
				payload.IDs = []string{id}
			} else if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &payload) != nil {
				modelError(w, http.StatusBadRequest, "Expected agent IDs")
				return
			}
			if id == "" {
				proposed.Public = nil
			}
			for _, value := range payload.IDs {
				match := slices.IndexFunc(agents, func(agent runtime.Agent) bool { return agent.ID == value || agent.Name == value })
				if match < 0 {
					modelError(w, http.StatusNotFound, "Agent not found")
					return
				}
				canonical := agents[match].ID
				if !slices.Contains(proposed.Public, canonical) {
					proposed.Public = append(proposed.Public, canonical)
				}
			}
		} else {
			switch r.Method {
			case http.MethodPost:
				changed.ID = newEntityID()
			case http.MethodPatch, http.MethodPut, http.MethodDelete:
				if index < 0 {
					modelError(w, http.StatusNotFound, "Managed agent not found")
					return
				}
				changed = proposed.Entries[index]
			}
			if r.Method == http.MethodDelete {
				proposed.Entries = slices.Delete(proposed.Entries, index, index+1)
				proposed.Public = slices.DeleteFunc(proposed.Public, func(value string) bool { return value == id || value == changed.Name })
			} else {
				next := agentConfig{}
				if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &next) != nil {
					modelError(w, http.StatusBadRequest, "Invalid agent configuration")
					return
				}
				next.ID = changed.ID
				if next.Name == "" {
					next.Name = changed.Name
				}
				if next.Card == nil {
					next.Card = changed.Card
				}
				if next.Headers == nil {
					next.Headers = changed.Headers
				}
				if next.Parameters == nil {
					next.Parameters = changed.Parameters
				}
				if next.ExtraHeaders == nil {
					next.ExtraHeaders = changed.ExtraHeaders
				}
				changed = next
				if index < 0 {
					proposed.Entries = append(proposed.Entries, changed)
				} else {
					proposed.Entries[index] = changed
				}
			}
		}
		compiled, err := compile(proposed)
		if err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if store != nil {
			if err := store.save(proposed); err != nil {
				stateWriteError(w, err)
				return
			}
		}
		state, agents = proposed, compiled
		server.ReplaceAgents(compiled)
		switch {
		case public:
			modelJSON(w, http.StatusOK, map[string]any{"message": "Agent visibility updated", "public_agent_groups": proposed.Public})
		case r.Method == http.MethodDelete:
			modelJSON(w, http.StatusOK, map[string]string{"message": "Agent deleted successfully"})
		default:
			modelJSON(w, http.StatusOK, map[string]any{"agent_id": changed.ID, "agent_name": changed.Name})
		}
	}
	mux.HandleFunc("POST /v1/agents", change)
	mux.HandleFunc("POST /v1/agents/make_public", change)
	mux.HandleFunc("POST /v1/agents/{agent_id}/make_public", change)
	mux.HandleFunc("PATCH /v1/agents/{agent_id}", change)
	mux.HandleFunc("PUT /v1/agents/{agent_id}", change)
	mux.HandleFunc("DELETE /v1/agents/{agent_id}", change)
	mux.Handle("/", next)
	return mux, nil
}
