package runtime

import (
	"context"
	"regexp"
	"time"
)

type Route struct {
	Alias      string      `json:"alias"`
	Version    string      `json:"version"`
	Hidden     bool        `json:"-"`
	Candidates []Candidate `json:"candidates"`
	ContextFallback []Candidate `json:"-"`
	PolicyFallback []Candidate `json:"-"`
	RequestRetries *int `json:"-"`
	RequestRetryPolicy RetryPolicy `json:"-"`
}

type Candidate struct {
	SourceID         string            `json:"sourceId"`
	ModelID          string            `json:"modelId"`
	APIBase          string            `json:"apiBase"`
	APIKey           string            `json:"apiKey"`
	CredentialRef    string            `json:"credentialRef"`
	Endpoint         string            `json:"endpoint"`
	APIVersion       string            `json:"apiVersion,omitempty"`
	AzureADToken     string            `json:"-"`
	AzureToken       func(context.Context) (string, error) `json:"-"`
	AzureIdentityKey string `json:"-"`
	Provider         string            `json:"provider,omitempty"`
	Region           string            `json:"region,omitempty"`
	DeploymentID     string            `json:"deploymentId,omitempty"`
	DropParams *bool `json:"dropParams,omitempty"`
	RequestDefaults map[string]any `json:"-"`
	AllowedOpenAIParams []string `json:"allowedOpenAIParams,omitempty"`
	AdditionalDropParams []string `json:"additionalDropParams,omitempty"`
	SupportedOpenAIParams []string `json:"-"`
	ReasoningCapabilities OpenAIReasoningCapabilities `json:"-"`
	ParamMappings    map[string]string `json:"paramMappings,omitempty"`
	RelativeAPIBase  bool              `json:"relativeApiBase,omitempty"`
	TextOnly         bool              `json:"textOnly,omitempty"`
	Tags             []string          `json:"-"`
	TagRegex         []*regexp.Regexp  `json:"-"`
	RoutingGroup     string            `json:"routingGroup,omitempty"`
	TokenPrice       float64           `json:"-"`
	UsagePrice       [2]*float64       `json:"-"`
	RealtimeAudio RealtimeAudioPrices `json:"-"`
	RealtimeProtocol string `json:"-"`
	CacheReadPrice   *float64          `json:"-"`
	CacheCreationPrice *float64        `json:"-"`
	Vertex           *VertexConfig     `json:"-"`
	WatsonX          *WatsonXRerankConfig `json:"-"`
	Weight           *float64          `json:"weight,omitempty"`
	RPM              *int              `json:"rpm,omitempty"`
	TPM              *int              `json:"tpm,omitempty"`
	DefaultKeyRPM *int `json:"defaultKeyRPM,omitempty"`
	DefaultKeyTPM *int `json:"defaultKeyTPM,omitempty"`
	Timeout time.Duration `json:"-"`
	StreamTimeout time.Duration `json:"-"`
	NumRetries *int `json:"-"`
	Bedrock          *BedrockConfig    `json:"bedrock,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	HealthMode string `json:"-"`
	HealthModel string `json:"-"`
	HealthVoice string `json:"-"`
	HealthTimeout time.Duration `json:"-"`
	HealthMaxTokens *int `json:"-"`
	HealthSupportsMaxTokens *bool `json:"-"`
	HealthReasoningEffort string `json:"-"`
	DisableBackgroundHealthCheck bool `json:"-"`
	UseInPassThrough bool `json:"-"`
}

type responseError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
		Code    any    `json:"code,omitempty"`
	} `json:"error"`
}
