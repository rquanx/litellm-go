package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

type AzureSafetyOptions struct {
	Severity           int
	SeverityByCategory map[string]int
	Categories         []string
	Blocklists         []string
	HaltOnBlocklistHit bool
	OutputType         string
}

func (s *Server) evaluateAzureSafety(r *http.Request, guard GenericGuardrail, stage string, input map[string]any) *guardrailFailure {
	var texts []string
	if stage == "request" {
		messages, _ := input["messages"].([]any)
		var latest []string
		for i := len(messages) - 1; i >= 0; i-- {
			message, _ := messages[i].(map[string]any)
			if message["role"] != "user" {
				break
			}
			latest = append(latest, azureMessageText(message["content"]))
		}
		for i, j := 0, len(latest)-1; i < j; i, j = i+1, j-1 {
			latest[i], latest[j] = latest[j], latest[i]
		}
		texts = []string{strings.TrimSpace(strings.Join(latest, "\n"))}
	} else {
		choices, _ := input["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			texts = append(texts, azureMessageText(message["content"]))
		}
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		for _, chunk := range azureSafetyChunks(text, 10000) {
			if failure := s.checkAzureSafetyChunk(r, guard, chunk); failure != nil {
				return failure
			}
		}
	}
	return nil
}

func azureMessageText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	var texts []string
	items, _ := content.([]any)
	for _, raw := range items {
		part, _ := raw.(map[string]any)
		if text, ok := part["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func (s *Server) checkAzureSafetyChunk(r *http.Request, guard GenericGuardrail, text string) *guardrailFailure {
	options := guard.Azure
	shield := guard.Provider == "azure/prompt_shield"
	request := map[string]any{"userPrompt": text, "documents": []string{}}
	if !shield {
		request = map[string]any{"text": text, "categories": options.Categories, "blocklistNames": options.Blocklists,
			"haltOnBlocklistHit": options.HaltOnBlocklistHit, "outputType": options.OutputType}
	}
	body, _ := json.Marshal(request)
	raw, failure := s.postGuardrail(r, guard, body, "Ocp-Apim-Subscription-Key", guard.APIKey)
	if failure != nil || raw == nil {
		return failure
	}
	var verdict struct {
		Prompt *struct {
			Attack *bool `json:"attackDetected"`
		} `json:"userPromptAnalysis"`
		Categories []struct {
			Name     string `json:"category"`
			Severity int    `json:"severity"`
		} `json:"categoriesAnalysis"`
	}
	invalid := &guardrailFailure{502, "guardrail_error", "Invalid Azure Content Safety response"}
	if json.Unmarshal(raw, &verdict) != nil {
		return invalid
	}
	if shield {
		if verdict.Prompt == nil || verdict.Prompt.Attack == nil {
			return invalid
		}
		if *verdict.Prompt.Attack {
			return &guardrailFailure{400, "content_policy_violation", "Violated Azure Prompt Shield guardrail policy"}
		}
		return nil
	}
	if verdict.Categories == nil {
		return invalid
	}
	for _, category := range verdict.Categories {
		threshold, configured := options.SeverityByCategory[category.Name]
		if !configured || (options.Severity > 0 && options.Severity < threshold) {
			threshold, configured = options.Severity, options.Severity > 0
		}
		if !configured && options.SeverityByCategory == nil {
			threshold, configured = 2, true
		}
		if configured && category.Severity >= threshold {
			return &guardrailFailure{400, "content_policy_violation",
				fmt.Sprintf("Azure Content Safety Guardrail: %s crossed severity %d, Got severity: %d", category.Name, threshold, category.Severity)}
		}
	}
	return nil
}

func azureSafetyChunks(text string, limit int) []string {
	runes := []rune(text)
	var chunks []string
	start, end := 0, 0
	for end < len(runes) {
		space := unicode.IsSpace(runes[end])
		next := end + 1
		for next < len(runes) && unicode.IsSpace(runes[next]) == space {
			next++
		}
		if next-start > limit && end > start {
			chunks = append(chunks, string(runes[start:end]))
			start = end
		}
		for next-start > limit {
			chunks = append(chunks, string(runes[start:start+limit]))
			start += limit
		}
		end = next
	}
	if start < len(runes) {
		chunks = append(chunks, string(runes[start:]))
	}
	return chunks
}
