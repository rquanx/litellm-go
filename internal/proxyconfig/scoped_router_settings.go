package proxyconfig

import (
	"encoding/json"
	"errors"

	"go.yaml.in/yaml/v3"
)

func applyRouterSettings(raw json.RawMessage, destination *map[string]any) error {
	if len(raw) == 0 {
		return nil
	}
	var settings map[string]any
	if json.Unmarshal(raw, &settings) != nil {
		return errors.New("router_settings must be an object or null")
	}
	*destination = settings
	return nil
}

func scopedRouterSettings(key virtualKey, owner team) map[string]any {
	if len(key.RouterSettings) > 0 {
		return key.RouterSettings
	}
	return owner.RouterSettings
}

func importRouterSettings(fields map[string]json.RawMessage) (map[string]any, error) {
	raw := fields["router_settings"]
	delete(fields, "router_settings")
	var settings map[string]any
	if err := applyRouterSettings(raw, &settings); err != nil {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil || yaml.Unmarshal([]byte(encoded), &settings) != nil {
			return nil, err
		}
		// Normalize YAML numbers and nested objects to the JSON request types.
		payload, marshalErr := json.Marshal(settings)
		settings = nil
		if marshalErr != nil || json.Unmarshal(payload, &settings) != nil {
			return nil, err
		}
	}
	return settings, nil
}
