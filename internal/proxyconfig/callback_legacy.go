package proxyconfig

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func legacyCallbackPayload(event runtime.InferenceEvent) map[string]any {
	id := event.ResponseID
	if id == "" {
		id = event.ID
	}
	cost := "0.0"
	if event.Spend != nil {
		cost = callbackFloat(*event.Spend)
	}
	legacyTime := func(seconds float64) string {
		return time.UnixMicro(int64(seconds * 1e6)).UTC().Format("2006-01-02 15:04:05.999999")
	}
	var messages, response any
	_ = json.Unmarshal(event.Messages, &messages)
	_ = json.Unmarshal(event.Response, &response)
	usage := fmt.Sprintf("{'prompt_tokens': %d, 'completion_tokens': %d, 'total_tokens': %d}", event.Prompt, event.Completion, event.Total)
	return map[string]any{
		"id": id, "call_type": event.CallType, "cache_hit": callbackPythonValue(event.CacheHit),
		"startTime": legacyTime(event.StartTime), "endTime": legacyTime(event.EndTime),
		"model": event.Model, "user": event.EndUser, "modelParameters": "{}",
		"messages": callbackPythonValue(messages), "response": callbackPythonValue(response),
		"usage": usage, "metadata": "{}", "cost": cost,
	}
}

func callbackFloat(value float64) string {
	text := strconv.FormatFloat(value, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text
}

func callbackPythonValue(value any) string {
	switch value := value.(type) {
	case nil:
		return "None"
	case bool:
		if value {
			return "True"
		}
		return "False"
	case string:
		encoded := strconv.Quote(value)
		if strings.Contains(value, "'") && !strings.Contains(value, `"`) {
			return encoded
		}
		content := strings.ReplaceAll(encoded[1:len(encoded)-1], `\"`, `"`)
		return "'" + strings.ReplaceAll(content, "'", `\'`) + "'"
	case []any:
		items := make([]string, len(value))
		for index, item := range value {
			items[index] = callbackPythonValue(item)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		items := make([]string, 0, len(keys))
		for _, key := range keys {
			items = append(items, callbackPythonValue(key)+": "+callbackPythonValue(value[key]))
		}
		return "{" + strings.Join(items, ", ") + "}"
	default:
		return fmt.Sprint(value)
	}
}
