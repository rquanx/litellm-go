package runtime

import (
	"net/url"
	"strings"
)

func azureTarget(base, model, endpoint, version string) string {
	base = strings.TrimRight(base, "/")
	suffix := strings.TrimPrefix(endpoints[endpoint].path, "/v1")
	if strings.HasSuffix(base, "/openai/v1") {
		return base + suffix
	}
	base = strings.TrimSuffix(base, "/openai")
	target := base + "/openai"
	if endpoint != "responses" {
		target += "/deployments/" + url.PathEscape(model)
	}
	return target + suffix + "?api-version=" + url.QueryEscape(version)
}
