package runtime

import (
	"encoding/json"
	"testing"
)

func TestOpenAIChatResponseKeepsProviderExtensions(t *testing.T) {
	body := normalizeOpenAIChat([]byte(`{"choices":[{"index":8,"provider_choice":42,
		"message":{"role":"assistant","content":"ok","refusal":null,"provider_value":"custom",
		"provider_specific_fields":{"existing":true}},"finish_reason":"stop"}]}`))
	var result struct {
		Choices []struct {
			Index int
			Provider map[string]any `json:"provider_specific_fields"`
			Message struct {
				Provider map[string]any `json:"provider_specific_fields"`
			}
		}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	choice := result.Choices[0]
	_, refusal := choice.Message.Provider["refusal"]
	if choice.Index != 0 || choice.Provider["provider_choice"] != float64(42) || !refusal ||
		choice.Message.Provider["provider_value"] != "custom" || choice.Message.Provider["existing"] != true {
		t.Fatalf("unexpected provider-specific fields: %s", body)
	}
}
