package runtime

import (
	"net/http"
	"net/url"
	"strings"
)

func providerKeyHeaders(headers http.Header, candidate Candidate, key string) {
	if candidate.Provider == "azure" {
		if candidate.AzureADToken != "" {
			headers.Set("Authorization", "Bearer "+candidate.AzureADToken)
		} else if key != "" {
			headers.Set("api-key", key)
		}
	} else if key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
}

func azureTarget(base, model, endpoint, version string) string {
	base = strings.TrimRight(base, "/")
	suffix := strings.TrimPrefix(endpoints[endpoint].path, "/v1")
	if strings.HasSuffix(base, "/openai/v1") {
		return base + suffix
	}
	base = strings.TrimSuffix(base, "/openai")
	target := base + "/openai"
	if endpoint != "responses" && endpoint != "responses-compact" {
		target += "/deployments/" + url.PathEscape(model)
	}
	return target + suffix + "?api-version=" + url.QueryEscape(version)
}
