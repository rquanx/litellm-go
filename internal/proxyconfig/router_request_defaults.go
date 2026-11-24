package proxyconfig

type routerRequestDefaults struct {
	deploymentRequestDefaults `yaml:",inline"`
	DropParams *bool `yaml:"drop_params" json:"drop_params,omitempty"`
	AllowedOpenAIParams []string `yaml:"allowed_openai_params" json:"allowed_openai_params,omitempty"`
	AdditionalDropParams []string `yaml:"additional_drop_params" json:"additional_drop_params,omitempty"`
	Metadata map[string]any `yaml:"metadata" json:"metadata,omitempty"`
}
