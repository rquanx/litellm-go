package runtime

type Route struct {
	Alias      string      `json:"alias"`
	Version    string      `json:"version"`
	Candidates []Candidate `json:"candidates"`
}

type Candidate struct {
	SourceID         string            `json:"sourceId"`
	ModelID          string            `json:"modelId"`
	APIBase          string            `json:"apiBase"`
	APIKey           string            `json:"apiKey"`
	CredentialRef    string            `json:"credentialRef"`
	Endpoint         string            `json:"endpoint"`
	Headers          map[string]string `json:"headers,omitempty"`
}

type responseError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
		Code    any    `json:"code,omitempty"`
	} `json:"error"`
}
