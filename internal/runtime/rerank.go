package runtime

import "strings"

func rerankTarget(base, provider string) string {
	base = strings.TrimRight(base, "/")
	path := "/v1/rerank"
	if provider == "cohere" || provider == "cohere_chat" {
		path = "/v2/rerank"
	}
	if strings.HasSuffix(base, path) {
		return base
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/v2"), "/v1")
	return base + path
}
