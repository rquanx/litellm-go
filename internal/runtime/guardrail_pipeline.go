package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"
)

type GuardrailPipeline struct {
	Mode  string                  `yaml:"mode" json:"mode"`
	Steps []GuardrailPipelineStep `yaml:"steps" json:"steps"`
}

type GuardrailPipelineStep struct {
	Guardrail string `yaml:"guardrail" json:"guardrail"`
	OnPass    string `yaml:"on_pass" json:"on_pass"`
	OnFail    string `yaml:"on_fail" json:"on_fail"`
	OnError   string `yaml:"on_error" json:"on_error"`
	PassData  bool   `yaml:"pass_data" json:"pass_data"`
	Message   string `yaml:"modify_response_message" json:"modify_response_message"`
}

func (p GuardrailPipeline) Validate() error {
	if (p.Mode != "pre_call" && p.Mode != "post_call") || len(p.Steps) == 0 {
		return errors.New("guardrail pipeline needs a mode and steps")
	}
	for _, step := range p.Steps {
		if step.Guardrail == "" {
			return errors.New("guardrail pipeline step needs a guardrail")
		}
		for _, action := range []string{step.OnPass, step.OnFail, step.OnError} {
			switch action {
			case "", "allow", "block", "next", "modify_response":
			default:
				return errors.New("invalid guardrail pipeline action")
			}
		}
	}
	return nil
}

type PolicyPipeline struct {
	Name string
	ID   string
	Spec GuardrailPipeline
}

type pipelineStepResult struct {
	Guardrail string         `json:"guardrail_name"`
	Outcome   string         `json:"outcome"`
	Action    string         `json:"action_taken"`
	Data      map[string]any `json:"modified_data"`
	Error     *string        `json:"error_detail"`
	Duration  float64        `json:"duration_seconds"`
}

type pipelineResult struct {
	Action  string               `json:"terminal_action"`
	Steps   []pipelineStepResult `json:"step_results"`
	Data    map[string]any       `json:"modified_data"`
	Error   *string              `json:"error_message"`
	Message *string              `json:"modify_response_message"`
}

func (s *Server) runPipeline(r *http.Request, p GuardrailPipeline, guards []GenericGuardrail, model string, input map[string]any) pipelineResult {
	result := pipelineResult{Action: "allow", Steps: make([]pipelineStepResult, 0, len(p.Steps))}
	working := input
	stage := "request"
	if p.Mode == "post_call" {
		stage = "response"
	}
	for _, step := range p.Steps {
		start := time.Now()
		entry := pipelineStepResult{Guardrail: step.Guardrail, Outcome: "pass", Action: step.OnPass}
		index := slices.IndexFunc(guards, func(g GenericGuardrail) bool { return g.Name == step.Guardrail })
		var failure *guardrailFailure
		var candidate map[string]any
		if index < 0 {
			failure = &guardrailFailure{http.StatusBadGateway, "guardrail_error", "Guardrail '" + step.Guardrail + "' not found"}
		} else {
			raw, _ := json.Marshal(working)
			if json.Unmarshal(raw, &candidate) != nil {
				failure = &guardrailFailure{http.StatusBadRequest, "guardrail_error", "Could not copy pipeline data"}
			} else {
				failure = s.evaluateGenericGuardrail(r, guards[index], model, stage, candidate)
				if failure == nil {
					if updated, _ := json.Marshal(candidate); string(raw) != string(updated) {
						entry.Data = candidate
						if step.PassData {
							working = candidate
							result.Data = working
						}
					}
				}
			}
		}
		entry.Duration = time.Since(start).Seconds()
		if failure != nil {
			entry.Error = &failure.message
			entry.Outcome, entry.Action = "fail", step.OnFail
			if failure.kind != "content_policy_violation" {
				entry.Outcome = "error"
				if step.OnError != "" {
					entry.Action = step.OnError
				}
			}
			if entry.Action == "" {
				entry.Action = "block"
			}
		}
		if entry.Action == "" {
			entry.Action = "allow"
		}
		result.Steps = append(result.Steps, entry)
		switch entry.Action {
		case "next":
			continue
		case "block":
			result.Action, result.Error, result.Data = "block", entry.Error, nil
		case "modify_response":
			message := step.Message
			if message == "" && entry.Error != nil {
				message = *entry.Error
			}
			if message == "" {
				message = "Response modified by pipeline"
			}
			result.Action, result.Message, result.Data = "modify_response", &message, nil
		}
		return result
	}
	return result
}

func (s *Server) testGuardrailPipeline(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Pipeline GuardrailPipeline `json:"pipeline"`
		Messages []any             `json:"test_messages"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Messages == nil || request.Pipeline.Validate() != nil {
		writeError(w, 400, "guardrail_pipeline_error", "Expected a valid pipeline and test messages")
		return
	}
	guards := s.visibleGuardrails(r)
	input := map[string]any{"messages": request.Messages, "model": "test"}
	writeJSON(w, http.StatusOK, s.runPipeline(r, request.Pipeline, guards, "test", input))
}

func (s *Server) enforcePipeline(w http.ResponseWriter, r *http.Request, p PolicyPipeline, guards []GenericGuardrail, model, endpoint string, stream bool, input map[string]any) bool {
	started := time.Now()
	result := s.runPipeline(r, p.Spec, guards, model, input)
	var failure *guardrailFailure
	if result.Action == "block" || result.Action == "modify_response" {
		failure = &guardrailFailure{kind: "content_policy_violation"}
	}
	s.recordGuardrailRun("", p.ID, model, started, failure)
	switch result.Action {
	case "block":
		writeError(w, 400, "guardrail_pipeline_error", fmt.Sprintf("Content blocked by guardrail pipeline %q", p.Name))
		return false
	case "modify_response":
		writePipelineIntervention(w, endpoint, model, *result.Message, stream)
		return false
	}
	maps.Copy(input, result.Data)
	return true
}

func writePipelineIntervention(w http.ResponseWriter, endpoint, model, message string, stream bool) {
	now := time.Now().Unix()
	if endpoint == "responses" {
		writeJSON(w, 200, map[string]any{"id": fmt.Sprintf("resp_%d", time.Now().UnixNano()), "object": "response",
			"created_at": now, "model": model, "status": "completed", "output": []any{map[string]any{"content": []any{map[string]string{"type": "text", "text": message}}}},
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}})
		return
	}
	response := map[string]any{"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion", "model": model, "created": now}
	choice := map[string]any{"index": 0, "finish_reason": "content_filter"}
	response["choices"] = []any{choice}
	if stream {
		response["object"] = "chat.completion.chunk"
		choice["delta"] = map[string]string{"role": "assistant", "content": message}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(response)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
		return
	}
	choice["message"] = map[string]string{"role": "assistant", "content": message}
	response["usage"] = map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	writeJSON(w, 200, response)
}
