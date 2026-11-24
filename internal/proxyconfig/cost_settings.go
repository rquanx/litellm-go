package proxyconfig

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
	"go.yaml.in/yaml/v3"
)

type costMargin struct {
	Percentage float64 `json:"percentage" yaml:"percentage"`
	Fixed float64 `json:"fixed_amount" yaml:"fixed_amount"`
}

func (margin *costMargin) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '{' {
		type object costMargin
		return decodeModelJSON(bytes.NewReader(raw), (*object)(margin))
	}
	return json.Unmarshal(raw, &margin.Percentage)
}

func (margin *costMargin) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		type object costMargin
		return node.Decode((*object)(margin))
	}
	return node.Decode(&margin.Percentage)
}

type costSettings struct {
	Discounts map[string]float64 `json:"discounts"`
	Margins map[string]costMargin `json:"margins"`
}

func (settings costSettings) validate() error {
	valid := func(value, limit float64) bool { return value >= 0 && value <= limit }
	for provider, discount := range settings.Discounts {
		if strings.TrimSpace(provider) == "" || !valid(discount, 1) {
			return errors.New("Discounts must be between 0 and 1")
		}
	}
	for provider, margin := range settings.Margins {
		if strings.TrimSpace(provider) == "" || !valid(margin.Percentage, 10) || !valid(margin.Fixed, math.MaxFloat64) {
			return errors.New("Margins must have a percentage between 0 and 10 and a nonnegative fixed amount")
		}
	}
	return nil
}

type CostSettings struct {
	mu sync.Mutex
	store *postgresState
	revision int64
	base costSettings
	active atomic.Pointer[costSettings]
}

func (config Config) OpenCostSettings(db *sql.DB, master string) (*CostSettings, error) {
	policy := &CostSettings{base: config.CostSettings}
	policy.active.Store(&policy.base)
	if db != nil {
		policy.store = postgresModels(db, master)
		policy.store.name = "cost-settings"
		if err := policy.refresh(); err != nil {
			return nil, err
		}
		go func() {
			ticker := time.NewTicker(10*time.Second)
			defer ticker.Stop()
			for range ticker.C {
				_ = policy.refresh()
			}
		}()
	}
	return policy, nil
}

func (policy *CostSettings) refresh() error {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var current costSettings
	changed, err := policy.store.loadAfter(ctx, &current, policy.revision)
	if err == nil && changed {
		if policy.store.revision == 0 {
			current = policy.base
		}
		if err = current.validate(); err == nil {
			policy.active.Store(&current)
			policy.revision = policy.store.revision
		}
	}
	return err
}

func (policy *CostSettings) Price(provider string, base float64) (total, marginCost float64) {
	current := policy.active.Load()
	base -= base * current.Discounts[provider]
	margin, exists := current.Margins[provider]
	if !exists {
		margin = current.Margins["global"]
	}
	added := base*margin.Percentage + margin.Fixed
	if math.IsInf(base+added, 0) || math.IsNaN(base+added) {
		return base, 0
	}
	return base + added, added
}

func (policy *CostSettings) AdjustEvent(event runtime.InferenceEvent) runtime.InferenceEvent {
	if event.Spend != nil && event.Status == "success" && !event.CacheHit {
		price, _ := policy.Price(event.Provider, *event.Spend)
		event.Spend = &price
	}
	return event
}

func (policy *CostSettings) Handler(next http.Handler, keys *KeyRegistry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config/cost_discount_config", func(w http.ResponseWriter, r *http.Request) {
		policy.handle(w, r, keys, false)
	})
	mux.HandleFunc("PATCH /config/cost_discount_config", func(w http.ResponseWriter, r *http.Request) {
		policy.handle(w, r, keys, false)
	})
	mux.HandleFunc("GET /config/cost_margin_config", func(w http.ResponseWriter, r *http.Request) {
		policy.handle(w, r, keys, true)
	})
	mux.HandleFunc("PATCH /config/cost_margin_config", func(w http.ResponseWriter, r *http.Request) {
		policy.handle(w, r, keys, true)
	})
	mux.Handle("/", next)
	return mux
}

func (policy *CostSettings) handle(w http.ResponseWriter, r *http.Request, keys *KeyRegistry, margins bool) {
	values := func(config *costSettings) any {
		if margins {
			if config.Margins == nil {
				return map[string]costMargin{}
			}
			return config.Margins
		}
		if config.Discounts == nil {
			return map[string]float64{}
		}
		return config.Discounts
	}
	if r.Method == http.MethodGet {
		if keys.requireAdminRead(w, r) {
			modelJSON(w, http.StatusOK, map[string]any{"values": values(policy.active.Load())})
		}
		return
	}
	if !keys.requireMaster(w, r) {
		return
	}
	if policy.store == nil {
		modelError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	current := *policy.active.Load()
	var err error
	body := http.MaxBytesReader(w, r.Body, 128<<10)
	if margins {
		current.Margins = nil
		err = decodeModelJSON(body, &current.Margins)
	} else {
		current.Discounts = nil
		err = decodeModelJSON(body, &current.Discounts)
	}
	if err != nil || (margins && current.Margins == nil) || (!margins && current.Discounts == nil) {
		modelError(w, http.StatusUnprocessableEntity, "Supply an object of cost settings")
		return
	}
	if err := current.validate(); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy.store.revision = policy.revision
	if err := policy.store.save(current); err != nil {
		if errors.Is(err, errConcurrentState) {
			modelError(w, http.StatusConflict, "Cost settings were changed by another process")
		} else {
			modelError(w, http.StatusInternalServerError, "Could not save cost settings")
		}
		return
	}
	policy.revision = policy.store.revision
	policy.active.Store(&current)
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "values": values(&current)})
}

