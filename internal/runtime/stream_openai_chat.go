package runtime

import (
	"bytes"
	"encoding/json"
	"time"
)

func normalizeOpenAIChatStream(alias string) func([]byte) []byte {
	var created int64
	return func(frame []byte) []byte {
		lines := bytes.Split(frame, []byte{'\n'})
		for index, line := range lines {
			raw, found := bytes.CutPrefix(line, []byte("data:"))
			if !found {
				continue
			}
			var chunk map[string]any
			if json.Unmarshal(bytes.TrimSpace(raw), &chunk) != nil || chunk["object"] != "chat.completion.chunk" {
				continue
			}
			if created == 0 {
				created = time.Now().Unix()
			}
			chunk["created"] = created
			chunk["model"] = alias
			choices, _ := chunk["choices"].([]any)
			for _, value := range choices {
				choice, _ := value.(map[string]any)
				if choice["finish_reason"] == nil {
					delete(choice, "finish_reason")
				}
				if delta, ok := choice["delta"].(map[string]any); ok {
					for field, value := range delta {
						if value == nil {
							delete(delta, field)
						}
					}
				}
			}
			if encoded, err := json.Marshal(chunk); err == nil {
				lines[index] = append([]byte("data: "), encoded...)
			}
		}
		return bytes.Join(lines, []byte{'\n'})
	}
}
