package proxyconfig

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type callbackSettings struct {
	Guardrails []genericGuardrailSpec `yaml:"guardrails"`
	SSOUserDefaults *SSOUserDefaults `yaml:"default_internal_user_params"`
	AzureTokenRefresh bool                 `yaml:"enable_azure_ad_token_refresh"`
	PublicAgentGroups []string             `yaml:"public_agent_groups"`
	Cache             bool                 `yaml:"cache"`
	CacheParams       inferenceCacheParams `yaml:"cache_params"`
	Callbacks         []string             `yaml:"callbacks"`
	Success           []string             `yaml:"success_callback"`
	Failure           []string             `yaml:"failure_callback"`
	UseV1             bool                 `yaml:"generic_api_use_v1"`
	Format            string               `yaml:"generic_logger_log_format"`
	MaxRetries        int                  `yaml:"generic_logger_max_retries"`
	RetryDelay        *float64             `yaml:"generic_logger_retry_delay"`
	Endpoint          string               `yaml:"generic_logger_endpoint"`
	Headers           map[string]string    `yaml:"generic_logger_headers"`
	DisableMessages   *bool                `yaml:"turn_off_message_logging"`
	PublicModelGroups []string             `yaml:"public_model_groups"`
	PublicModelLinks  map[string]any       `yaml:"public_model_groups_links"`
	PublicMCPServers  []string             `yaml:"public_mcp_servers"`
	Responses         struct {
		Background struct {
			Polling  any      `yaml:"polling_via_cache"`
			Native   []string `yaml:"native_background_mode"`
			TTL      int      `yaml:"ttl"`
			RedisURL string   `yaml:"redis_url"`
		} `yaml:"background_mode"`
	} `yaml:"responses"`
}

type GenericCallbacks []*GenericCallback

type GenericCallback struct {
	name     string
	endpoint string
	format   string
	headers  map[string]string
	legacy   bool
	success  bool
	failure  bool
	retries  int
	delay    time.Duration
}

type callbackPreset struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers"`
	Format   string            `json:"log_format"`
	Events   []string          `json:"event_types"`
}

//go:embed callback_presets.json
var callbackPresetData []byte

var callbackEnvironment = regexp.MustCompile(`\{\{environment_variables\.([A-Z_]+)\}\}`)

func expandCallbackEnvironment(value string) string {
	return callbackEnvironment.ReplaceAllStringFunc(value, func(reference string) string {
		return os.Getenv(callbackEnvironment.FindStringSubmatch(reference)[1])
	})
}

func prepareCallback(settings callbackSettings) (GenericCallbacks, bool, error) {
	names := slices.Concat(settings.Callbacks, settings.Success, settings.Failure)
	if len(names) == 0 {
		return nil, false, nil
	}
	var callbackPresets map[string]callbackPreset
	if err := json.Unmarshal(callbackPresetData, &callbackPresets); err != nil {
		return nil, false, errors.New("invalid embedded callback presets")
	}
	callbacks := make(GenericCallbacks, 0, len(names))
	used := make(map[string]bool)
	for _, name := range names {
		if used[name] {
			continue
		}
		used[name] = true
		preset, known := callbackPresets[name]
		if name != "generic_api" && !known {
			return nil, false, errors.New("unsupported LiteLLM callback")
		}
		callback, err := prepareGenericCallback(name, settings, preset)
		if err != nil {
			return nil, false, err
		}
		callbacks = append(callbacks, callback)
	}
	return callbacks, settings.DisableMessages != nil && !*settings.DisableMessages, nil
}

func prepareGenericCallback(name string, settings callbackSettings, preset callbackPreset) (*GenericCallback, error) {
	configured := settings.Endpoint
	if name != "generic_api" {
		configured = expandCallbackEnvironment(preset.Endpoint)
	}
	endpoint, err := resolveEnv(configured)
	if err != nil {
		return nil, err
	}
	if endpoint == "" && name == "generic_api" {
		endpoint = os.Getenv("GENERIC_LOGGER_ENDPOINT")
	}
	address, err := url.Parse(endpoint)
	if err != nil || address.Hostname() == "" || address.User != nil || address.Fragment != "" ||
		(address.Scheme != "http" && address.Scheme != "https") {
		return nil, errors.New("generic_api callback requires an HTTP or HTTPS endpoint")
	}
	headers := make(map[string]string)
	for header, value := range preset.Headers {
		headers[header] = expandCallbackEnvironment(value)
	}
	if raw := strings.TrimSpace(os.Getenv("GENERIC_LOGGER_HEADERS")); raw != "" {
		if strings.HasPrefix(raw, "{") {
			if json.Unmarshal([]byte(raw), &headers) != nil {
				return nil, errors.New("invalid GENERIC_LOGGER_HEADERS")
			}
		} else {
			for item := range strings.SplitSeq(raw, ",") {
				name, value, found := strings.Cut(item, "=")
				if !found || strings.TrimSpace(name) == "" {
					return nil, errors.New("invalid GENERIC_LOGGER_HEADERS")
				}
				headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
			}
		}
	}
	for name, value := range settings.Headers {
		resolved, err := resolveEnv(value)
		if err != nil {
			return nil, err
		}
		headers[name] = resolved
	}
	if settings.MaxRetries < 0 || settings.MaxRetries > 8 {
		return nil, errors.New("invalid generic_api max retries")
	}
	delay := time.Second
	if value := settings.RetryDelay; value != nil {
		if *value < 0 || *value > 60 || math.IsNaN(*value) {
			return nil, errors.New("invalid generic_api retry delay")
		}
		delay = time.Duration(*value * float64(time.Second))
	}
	format := preset.Format
	if settings.Format != "" && name == "generic_api" {
		format = settings.Format
	}
	if format == "" {
		format = "json_array"
	}
	if format != "json_array" && format != "ndjson" && format != "single" {
		return nil, errors.New("invalid generic_api log format")
	}
	all := slices.Contains(settings.Callbacks, name)
	success, failure := all || slices.Contains(settings.Success, name), all || slices.Contains(settings.Failure, name)
	if len(preset.Events) > 0 {
		success = success && slices.Contains(preset.Events, "llm_api_success")
		failure = failure && slices.Contains(preset.Events, "llm_api_failure")
	}
	return &GenericCallback{name: name, endpoint: endpoint, format: format, headers: headers,
		legacy: settings.UseV1, retries: settings.MaxRetries, delay: delay, success: success, failure: failure}, nil
}

func (callbacks GenericCallbacks) Names() []string {
	names := make([]string, 0, len(callbacks))
	for _, callback := range callbacks {
		names = append(names, callback.name)
	}
	return names
}

func (callbacks GenericCallbacks) Start(ctx context.Context) func(runtime.InferenceEvent) {
	send := make([]func(runtime.InferenceEvent), 0, len(callbacks))
	for _, callback := range callbacks {
		send = append(send, callback.Start(ctx))
	}
	return func(event runtime.InferenceEvent) {
		for _, callback := range send {
			callback(event)
		}
	}
}

func (callback *GenericCallback) Start(ctx context.Context) func(runtime.InferenceEvent) {
	queue := make(chan runtime.InferenceEvent, 64)
	client := &http.Client{
		Timeout:       5 * time.Second,
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
			callback.send(ctx, client, batch)
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

func (callback *GenericCallback) send(ctx context.Context, client *http.Client, batch []runtime.InferenceEvent) {
	payload := make([]map[string]any, 0, len(batch))
	for _, event := range batch {
		if callback.legacy {
			payload = append(payload, legacyCallbackPayload(event))
		} else {
			payload = append(payload, standardCallbackPayload(event))
		}
	}
	switch callback.format {
	case "single":
		for _, event := range payload {
			if body, err := json.Marshal(event); err == nil {
				callback.deliver(ctx, client, body)
			}
		}
	case "ndjson":
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		for _, event := range payload {
			if encoder.Encode(event) != nil {
				return
			}
		}
		callback.deliver(ctx, client, bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	default:
		if body, err := json.Marshal(payload); err == nil {
			callback.deliver(ctx, client, body)
		}
	}
}

func (callback *GenericCallback) deliver(ctx context.Context, client *http.Client, body []byte) {
	for attempt := 0; attempt <= callback.retries && ctx.Err() == nil; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, callback.endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		request.Header.Set("Content-Type", "application/json")
		for key, value := range callback.headers {
			request.Header.Set(key, value)
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			status := response.StatusCode
			if status != http.StatusTooManyRequests && status < 500 {
				return
			}
		}
		if attempt == callback.retries {
			return
		}
		timer := time.NewTimer(min(30*time.Second, callback.delay<<attempt))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func standardCallbackPayload(event runtime.InferenceEvent) map[string]any {
	firstOutput := event.FirstOutput
	if firstOutput == 0 {
		firstOutput = event.EndTime
	}
	var details, message any
	if event.Status == "failure" && event.HTTPStatus > 0 {
		message = http.StatusText(event.HTTPStatus)
		details = map[string]any{"error_code": strconv.Itoa(event.HTTPStatus), "llm_provider": event.Provider, "error_message": message}
	}
	var cost float64
	if event.Spend != nil {
		cost = *event.Spend
	}
	return map[string]any{
		"id": event.ID, "trace_id": event.TraceID, "litellm_call_id": event.ID,
		"call_type": event.CallType, "stream": event.Stream, "status": event.Status,
		"status_fields": map[string]string{"llm_api_status": event.Status, "guardrail_status": "not_run"},
		"model":         event.Model, "model_group": event.ModelGroup, "model_id": event.ModelID, "custom_llm_provider": event.Provider,
		"api_base": event.APISource, "startTime": event.StartTime, "endTime": event.EndTime,
		"completionStartTime": firstOutput, "response_time": max(0, event.EndTime-event.StartTime),
		"total_tokens": event.Total, "prompt_tokens": event.Prompt, "completion_tokens": event.Completion,
		"response_cost": cost, "cost_breakdown": nil, "response_cost_failure_debug_info": nil,
		"cache_hit": event.CacheHit, "cache_key": nil, "saved_cache_cost": 0,
		"cache_read_input_tokens": event.CacheReadInputTokens, "cache_creation_input_tokens": event.CacheCreationInputTokens,
		"model_map_information": map[string]any{}, "metadata": map[string]any{}, "model_parameters": map[string]any{},
		"hidden_params": map[string]any{}, "request_tags": []string{}, "end_user": event.EndUser,
		"requester_ip_address": nil, "user_agent": nil, "messages": event.Messages, "response": event.Response,
		"error_str": message, "error_information": details, "guardrail_information": nil, "standard_built_in_tools_params": nil,
	}
}

func (callbacks GenericCallbacks) Probe(ctx context.Context, service string, event runtime.InferenceEvent) bool {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, callback := range callbacks {
		if callback.name == service && callback.success {
			probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			callback.send(probeCtx, client, []runtime.InferenceEvent{event})
			return true
		}
	}
	return false
}