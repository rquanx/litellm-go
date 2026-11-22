package proxyconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type callbackSettings struct {
	Cache             bool                 `yaml:"cache"`
	CacheParams       inferenceCacheParams `yaml:"cache_params"`
	Callbacks        []string          `yaml:"callbacks"`
	Success          []string          `yaml:"success_callback"`
	Failure          []string          `yaml:"failure_callback"`
	Endpoint         string            `yaml:"generic_logger_endpoint"`
	Headers          map[string]string `yaml:"generic_logger_headers"`
	DisableMessages  *bool             `yaml:"turn_off_message_logging"`
	PublicModelGroups []string         `yaml:"public_model_groups"`
	PublicModelLinks  map[string]any   `yaml:"public_model_groups_links"`
	PublicMCPServers  []string         `yaml:"public_mcp_servers"`
	Responses struct {
		Background struct {
			Polling any `yaml:"polling_via_cache"`
			Native []string `yaml:"native_background_mode"`
			TTL int `yaml:"ttl"`
			RedisURL string `yaml:"redis_url"`
		} `yaml:"background_mode"`
	} `yaml:"responses"`
}

type GenericCallback struct {
	endpoint string
	headers  map[string]string
	success  bool
	failure  bool
}

func prepareCallback(settings callbackSettings) (*GenericCallback, bool, error) {
	for _, names := range [][]string{settings.Callbacks, settings.Success, settings.Failure} {
		for _, name := range names {
			if name != "generic_api" {
				return nil, false, errors.New("unsupported LiteLLM callback")
			}
		}
	}
	if len(settings.Callbacks)+len(settings.Success)+len(settings.Failure) == 0 {
		return nil, false, nil
	}
	endpoint, err := resolveEnv(settings.Endpoint)
	if err != nil {
		return nil, false, err
	}
	if endpoint == "" {
		endpoint = os.Getenv("GENERIC_LOGGER_ENDPOINT")
	}
	address, err := url.Parse(endpoint)
	if err != nil || address.Hostname() == "" || address.User != nil || address.Fragment != "" ||
		(address.Scheme != "http" && address.Scheme != "https") {
		return nil, false, errors.New("generic_api callback requires an HTTP or HTTPS endpoint")
	}
	headers := make(map[string]string)
	if raw := os.Getenv("GENERIC_LOGGER_HEADERS"); raw != "" && (json.Unmarshal([]byte(raw), &headers) != nil || headers == nil) {
		return nil, false, errors.New("invalid GENERIC_LOGGER_HEADERS")
	}
	for name, value := range settings.Headers {
		resolved, err := resolveEnv(value)
		if err != nil {
			return nil, false, err
		}
		headers[name] = resolved
	}
	all := slices.Contains(settings.Callbacks, "generic_api")
	return &GenericCallback{endpoint: endpoint, headers: headers, success: all || slices.Contains(settings.Success, "generic_api"),
		failure: all || slices.Contains(settings.Failure, "generic_api")}, settings.DisableMessages != nil && !*settings.DisableMessages, nil
}

func (callback *GenericCallback) Start(ctx context.Context) func(runtime.InferenceEvent) {
	queue := make(chan runtime.InferenceEvent, 64)
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		batch := make([]runtime.InferenceEvent, 0, 16)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			callback.send(client, batch)
			clear(batch)
			batch = batch[:0]
		}
		for {
			select {
			case event := <-queue:
				batch = append(batch, event)
				if len(batch) == cap(batch) {
					flush()
				}
			case <-ticker.C:
				flush()
			case <-ctx.Done():
				return
			}
		}
	}()
	return func(event runtime.InferenceEvent) {
		if (event.Status == "success" && !callback.success) || (event.Status == "failure" && !callback.failure) {
			return
		}
		select {
		case queue <- event:
		default:
		}
	}
}

func (callback *GenericCallback) send(client *http.Client, batch []runtime.InferenceEvent) {
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	request, err := http.NewRequest(http.MethodPost, callback.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range callback.headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err == nil {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	}
}
