package runtime

type Route struct {
	Alias      string      `json:"alias"`
	Version    string      `json:"version"`
	Candidates []Candidate `json:"candidates"`
	ContextFallback []Candidate `json:"-"`
	PolicyFallback []Candidate `json:"-"`
}

type Candidate struct {
	SourceID         string            `json:"sourceId"`
	ModelID          string            `json:"modelId"`
	APIBase          string            `json:"apiBase"`
	APIKey           string            `json:"apiKey"`
	CredentialRef    string            `json:"credentialRef"`
	Endpoint         string            `json:"endpoint"`
	APIVersion       string            `json:"apiVersion,omitempty"`
	Provider         string            `json:"provider,omitempty"`
	DeploymentID     string            `json:"deploymentId,omitempty"`
	ParamMappings    map[string]string `json:"paramMappings,omitempty"`
	RelativeAPIBase  bool              `json:"relativeApiBase,omitempty"`
	TextOnly         bool              `json:"textOnly,omitempty"`
	RoutingGroup     string            `json:"routingGroup,omitempty"`
	Weight           *float64          `json:"weight,omitempty"`
	RPM              *int              `json:"rpm,omitempty"`
	TPM              *int              `json:"tpm,omitempty"`
	Bedrock          *BedrockConfig    `json:"bedrock,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
}

type responseError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
		Code    any    `json:"code,omitempty"`
	} `json:"error"`
}
