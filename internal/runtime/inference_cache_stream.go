package runtime

import (
	"bytes"
	"encoding/json"
)

type streamCacheCapture struct {
	frames                  []byte
	responses               bool
	disabled, done, sawData bool
}

func (capture *streamCacheCapture) add(output []byte) {
	if capture.disabled || len(output) == 0 {
		return
	}
	if len(capture.frames)+len(output) > maxCacheResponse {
		capture.disabled, capture.frames = true, nil
		return
	}
	for line := range bytes.SplitSeq(output, []byte{'\n'}) {
		value, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !found {
			continue
		}
		value = bytes.TrimSpace(value)
		if bytes.Equal(value, []byte("[DONE]")) {
			if !capture.responses {
				capture.done = true
			}
			continue
		}
		var event map[string]json.RawMessage
		if capture.done || json.Unmarshal(value, &event) != nil || event == nil || sseFrameFailure(line) {
			capture.disabled, capture.frames = true, nil
			return
		}
		capture.sawData = true
		if capture.responses && string(event["type"]) == `"response.completed"` {
			capture.done = true
		}
	}
	capture.frames = append(capture.frames, bytes.ReplaceAll(output, []byte("\r\n"), []byte("\n"))...)
}

func (capture *streamCacheCapture) complete() bool {
	return !capture.disabled && capture.done && capture.sawData
}
