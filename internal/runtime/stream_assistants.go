package runtime

import (
	"bytes"
	"encoding/json"
)

func assistantStreamRunState(frame []byte) ([]byte, bool) {
	data := streamErrorBody(frame)
	var event struct {
		Object string `json:"object"`
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &event) != nil || event.Object != "thread.run" {
		return nil, false
	}
	if assistantRunPending(event.Status) {
		return data, true
	}
	return nil, true
}

func wrapAssistantFrame(frame []byte, deployment string) []byte {
	lines := bytes.Split(frame, []byte{'\n'})
	for index, line := range lines {
		raw, found := bytes.CutPrefix(line, []byte("data:"))
		if !found {
			continue
		}
		var event struct { Object string }
		if json.Unmarshal(bytes.TrimSpace(raw), &event) != nil {
			continue
		}
		family := assistantResourceKind(event.Object)
		if family != "" {
			lines[index] = append([]byte("data: "), wrapOpenAIResource(bytes.TrimSpace(raw), family, deployment)...)
		}
	}
	return bytes.Join(lines, []byte{'\n'})
}
