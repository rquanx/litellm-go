package proxyconfig

import (
	"os"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func firstWatsonXEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func (entry deployment) watsonXRerankConfig() (*runtime.WatsonXRerankConfig, error) {
	project, err := resolveEnv(entry.Params.WatsonXProject)
	if err != nil {
		return nil, err
	}
	if project == "" {
		project = firstWatsonXEnv("WATSONX_PROJECT_ID", "WX_PROJECT_ID", "PROJECT_ID")
	}
	space, err := resolveEnv(entry.Params.WatsonXSpace)
	if err != nil {
		return nil, err
	}
	if space == "" {
		space = firstWatsonXEnv("WATSONX_DEPLOYMENT_SPACE_ID", "WATSONX_SPACE_ID", "WX_SPACE_ID", "SPACE_ID")
	}
	return &runtime.WatsonXRerankConfig{
		Project: project, Space: space, Token: os.Getenv("WATSONX_TOKEN"),
		ZenKey: os.Getenv("WATSONX_ZENAPIKEY"), IAMURL: os.Getenv("WATSONX_IAM_URL"),
	}, nil
}
