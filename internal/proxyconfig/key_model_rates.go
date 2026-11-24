package proxyconfig

import (
	"math"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type scopedModelRates struct {
	identity string
	metadata map[string]any
}

func keyModelRateLimits(key virtualKey, owner team, workspace project, org organization, identity string) func(string, runtime.OwnerRateLimits) []runtime.OwnerRateLimits {
	scopes := []scopedModelRates{
		{"team:" + owner.ID, owner.Metadata},
		{"project:" + workspace.ID, workspace.Metadata},
		{"organization:" + org.ID, org.Metadata},
	}
	return func(model string, defaults runtime.OwnerRateLimits) []runtime.OwnerRateLimits {
		keyScope := runtime.OwnerRateLimits{Identity: "key-model:" + identity + ":" + model, RPM: defaults.RPM, TPM: defaults.TPM}
		keyScope.RPM = resolveKeyModelRate(key, owner, model, "model_rpm_limit", defaults.RPM, func(b keyModelBudget) *int { return b.RPM })
		keyScope.TPM = resolveKeyModelRate(key, owner, model, "model_tpm_limit", defaults.TPM, func(b keyModelBudget) *int { return b.TPM })
		var limits []runtime.OwnerRateLimits
		if keyScope.RPM != nil || keyScope.TPM != nil {
			limits = append(limits, keyScope)
		}
		for _, scope := range scopes {
			limit := runtime.OwnerRateLimits{
				Identity: "model:" + scope.identity + ":" + model,
				RPM: metadataModelRate(scope.metadata, "model_rpm_limit", model),
				TPM: metadataModelRate(scope.metadata, "model_tpm_limit", model),
			}
			if limit.RPM != nil || limit.TPM != nil {
				limits = append(limits, limit)
			}
		}
		return limits
	}
}

func resolveKeyModelRate(key virtualKey, owner team, model, field string, fallback *int, fromBudget func(keyModelBudget) *int) *int {
	config, _ := key.Metadata[field].(map[string]any)
	if len(config) != 0 {
		return metadataModelRate(key.Metadata, field, model)
	}
	for _, budget := range key.ModelBudgets {
		if fromBudget(budget) != nil {
			return fromBudget(key.ModelBudgets[model])
		}
	}
	if config, ok := owner.Metadata[field].(map[string]any); ok && config != nil {
		return metadataModelRate(owner.Metadata, field, model)
	}
	return fallback
}

func metadataModelRate(metadata map[string]any, field, model string) *int {
	config, _ := metadata[field].(map[string]any)
	value, ok := config[model].(float64)
	if !ok || value < 0 || value > math.MaxInt32 || value != math.Trunc(value) {
		return nil
	}
	limit := int(value)
	return &limit
}
