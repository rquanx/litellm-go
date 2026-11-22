package runtime

import "testing"

func TestAzureTargets(t *testing.T) {
	for _, test := range []struct{ base, endpoint, want string }{
		{"https://fixture.invalid", "chat-completions", "https://fixture.invalid/openai/deployments/demo/chat/completions?api-version=2024-10-21"},
		{"https://fixture.invalid/openai", "responses", "https://fixture.invalid/openai/responses?api-version=2024-10-21"},
		{"https://fixture.invalid/openai/v1", "embeddings", "https://fixture.invalid/openai/v1/embeddings"},
	} {
		if target := azureTarget(test.base, "demo", test.endpoint, "2024-10-21"); target != test.want {
			t.Errorf("got %s, want %s", target, test.want)
		}
	}
}
