package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type ragResult struct {
	header http.Header
	body   bytes.Buffer
	status int
	limit  int64
}

func (result *ragResult) Header() http.Header { return result.header }

func (result *ragResult) WriteHeader(status int) {
	if result.status == 0 {
		result.status = status
	}
}

func (result *ragResult) Write(data []byte) (int, error) {
	result.WriteHeader(http.StatusOK)
	if int64(result.body.Len())+int64(len(data)) > result.limit {
		result.status = http.StatusBadGateway
		return 0, errors.New("RAG intermediate response exceeds the configured body limit")
	}
	return result.body.Write(data)
}

func (s *Server) ragStage(w http.ResponseWriter, invoke func(http.ResponseWriter)) (*ragResult, bool) {
	result := &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
	invoke(result)
	if result.status == 0 || (result.status == http.StatusBadGateway && result.body.Len() == 0) {
		writeError(w, http.StatusBadGateway, "rag_stage_failed", "RAG stage did not return a response")
		return nil, false
	}
	if result.status/100 != 2 {
		writeRawResponse(w, result.status, result.header, result.body.Bytes())
		return nil, false
	}
	return result, true
}

func ragSubrequest(parent *http.Request, method, target string, body []byte, stream bool) *http.Request {
	path, _ := url.Parse(target)
	request := parent.Clone(parent.Context())
	request.Method, request.URL = method, path
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/json")
	if !stream {
		request.Header.Del("Accept")
	}
	return request
}

func ragQueryText(message any) string {
	fields, _ := message.(map[string]any)
	if text, ok := fields["content"].(string); ok {
		return text
	}
	parts, _ := fields["content"].([]any)
	for _, value := range parts {
		part, _ := value.(map[string]any)
		if text, _ := part["text"].(string); part["type"] == "text" && text != "" {
			return text
		}
	}
	return ""
}

func ragDocuments(chunks []any) []string {
	documents := make([]string, 0, len(chunks))
	for _, value := range chunks {
		chunk, _ := value.(map[string]any)
		parts, _ := chunk["content"].([]any)
		var text []string
		for _, item := range parts {
			part, _ := item.(map[string]any)
			if value, _ := part["text"].(string); value != "" {
				text = append(text, value)
			}
		}
		if len(text) == 0 {
			if value, _ := chunk["text"].(string); value != "" {
				text = []string{value}
			}
		}
		documents = append(documents, strings.Join(text, "\n\n"))
	}
	return documents
}

func (s *Server) ragQuery(w http.ResponseWriter, r *http.Request, scope []string) {
	var fields map[string]any
	if err := decodeBody(r, &fields, s.config.MaxBodyBytes); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Expected a RAG query request")
		return
	}
	model, _ := fields["model"].(string)
	messages, _ := fields["messages"].([]any)
	retrieval, _ := fields["retrieval_config"].(map[string]any)
	id, _ := retrieval["vector_store_id"].(string)
	if model == "" || len(messages) == 0 || (!validResourceSegment(id) && !(retrieval["custom_llm_provider"] == "vertex_ai" && vertexRAGStorePath(id))) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A model, messages and vector_store_id are required")
		return
	}
	if !modelAllowed(scope, model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	query := ragQueryText(messages[len(messages)-1])
	if strings.TrimSpace(query) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "The final RAG message must contain text")
		return
	}
	searchFields := map[string]any{"query": query, "max_num_results": 10}
	for from, to := range map[string]string{"top_k": "max_num_results", "filters": "filters", "model": "model", "custom_llm_provider": "custom_llm_provider"} {
		if value := retrieval[from]; value != nil {
			searchFields[to] = value
		}
	}
	searchBody, _ := json.Marshal(searchFields)
	search, ok := s.ragStage(w, func(intermediate http.ResponseWriter) {
		s.vectorStoreResource(intermediate, ragSubrequest(r, http.MethodPost, vectorStorePath+"/"+url.PathEscape(id)+"/search", searchBody, false), scope)
	})
	if !ok {
		return
	}
	var results map[string]any
	if json.Unmarshal(search.body.Bytes(), &results) != nil || results == nil {
		writeError(w, http.StatusBadGateway, "rag_stage_failed", "Invalid vector search results")
		return
	}
	chunks, _ := results["data"].([]any)
	documents := ragDocuments(chunks)
	var reranked map[string]any
	rerank, _ := fields["rerank"].(map[string]any)
	if inputBool(rerank, "enabled") && len(documents) > 0 {
		top := rerank["top_n"]
		if top == nil {
			top = 5
		}
		body, _ := json.Marshal(map[string]any{"model": rerank["model"], "query": query, "documents": documents, "top_n": top})
		stage, ok := s.ragStage(w, func(intermediate http.ResponseWriter) {
			s.infer(intermediate, ragSubrequest(r, http.MethodPost, "/v1/rerank", body, false), "rerank", "", scope)
		})
		if !ok {
			return
		}
		if json.Unmarshal(stage.body.Bytes(), &reranked) != nil {
			writeError(w, http.StatusBadGateway, "rag_stage_failed", "Invalid rerank results")
			return
		}
		order, _ := reranked["results"].([]any)
		ranked := make([]string, 0, len(order))
		for _, value := range order {
			entry, _ := value.(map[string]any)
			index, ok := entry["index"].(float64)
			if ok && index >= 0 && index < float64(len(documents)) && index == float64(int(index)) {
				ranked = append(ranked, documents[int(index)])
			}
		}
		documents = ranked
	}
	contextText := "Context:\n\n"
	if len(documents) != 0 {
		contextText += strings.Join(documents, "\n\n") + "\n\n"
	}
	history := make([]any, 0, len(messages)+1)
	history = append(history, messages[:len(messages)-1]...)
	fields["messages"] = append(history, map[string]string{"role": "user", "content": contextText}, messages[len(messages)-1])
	delete(fields, "retrieval_config")
	delete(fields, "rerank")
	payload, _ := json.Marshal(fields)
	stream := inputBool(fields, "stream") || r.Header.Get("Accept") == "text/event-stream"
	generate := ragSubrequest(r, http.MethodPost, "/v1/chat/completions", payload, stream)
	if stream {
		s.infer(w, generate, "chat-completions", model, scope)
		return
	}
	response, ok := s.ragStage(w, func(intermediate http.ResponseWriter) {
		s.infer(intermediate, generate, "chat-completions", model, scope)
	})
	if !ok {
		return
	}
	var final map[string]any
	if json.Unmarshal(response.body.Bytes(), &final) != nil || final == nil {
		writeError(w, http.StatusBadGateway, "rag_stage_failed", "Invalid RAG completion response")
		return
	}
	choices, _ := final["choices"].([]any)
	for _, entry := range choices {
		choice, _ := entry.(map[string]any)
		if message, ok := choice["message"].(map[string]any); ok {
			extra, _ := message["provider_specific_fields"].(map[string]any)
			if extra == nil {
				extra = make(map[string]any)
			}
			extra["search_results"] = results
			if reranked != nil {
				extra["rerank_results"] = reranked
			}
			message["provider_specific_fields"] = extra
		}
	}
	encoded, _ := json.Marshal(final)
	writeRawResponse(w, response.status, response.header, encoded)
}
