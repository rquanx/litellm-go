package proxyconfig

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed model-identities.json.gz
var modelIdentitySnapshot []byte

var modelIdentities = sync.OnceValue(func() map[string][2]string {
	reader, err := gzip.NewReader(bytes.NewReader(modelIdentitySnapshot))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var models map[string][2]string
	if json.NewDecoder(reader).Decode(&models) != nil {
		return nil
	}
	return models
})

func deploymentIdentity(name, explicitProvider string) (string, string, bool) {
	provider, model, prefixed := strings.Cut(name, "/")
	if explicitProvider != "" {
		if prefixed && provider == explicitProvider {
			return provider, model, model != ""
		}
		return explicitProvider, name, name != ""
	}
	identity, known := modelIdentities()[name]
	if known {
		return identity[0], identity[1], true
	}
	if strings.HasPrefix(name, "ft:") {
		return "openai", name, true
	}
	return provider, model, prefixed && model != ""
}
