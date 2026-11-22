package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const geminiStorePrefix = "fileSearchStores/"

func (s *Server) geminiVectorStore(w http.ResponseWriter, r *http.Request, candidate Candidate, family, path string, fields map[string]any) {
	const storeFamily = "vector_stores"
	var target, query string
	var payload map[string]any
	switch {
	case family == storeFamily && path == vectorStorePath && r.Method == http.MethodPost:
		base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1beta")
		target = base + "/v1beta/fileSearchStores"
		payload = make(map[string]any)
		if name, _ := fields["name"].(string); name != "" {
			payload["displayName"] = name
		}
	case family == "vector_stores_search":
		store := strings.TrimSuffix(strings.TrimPrefix(path, vectorStorePath+"/"), "/search")
		store, _ = strings.CutPrefix(store, geminiStorePrefix)
		if !validResourceSegment(store) {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Gemini vector store ID")
			return
		}
		var ok bool
		query, ok = geminiVectorQuery(fields["query"])
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "A vector store query is required")
			return
		}
		tool := map[string]any{"file_search_store_names": []string{geminiStorePrefix + store}}
		if filter := geminiVectorFilter(fields["filters"]); filter != "" {
			tool["metadata_filter"] = filter
		}
		target = geminiTarget(candidate, "gemini-generate")
		payload = map[string]any{
			"contents": []any{map[string]any{"parts": []any{map[string]string{"text": query}}}},
			"tools":    []any{map[string]any{"file_search": tool}},
		}
		if maxResults, _ := fields["max_num_results"].(float64); maxResults > 0 {
			payload["generationConfig"] = map[string]int{"candidateCount": 1}
		}
	default:
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Gemini vector stores support creation and search")
		return
	}
	data, _ := json.Marshal(payload)
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(data))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini vector store URL")
		return
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	upstream.Header.Set("Content-Type", "application/json")
	if err := applyGeminiCredential(upstream, candidate, key); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	for name, value := range candidate.Headers {
		upstream.Header.Set(name, value)
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach the Gemini vector store provider")
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read the Gemini vector store response")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeRawResponse(w, response.StatusCode, response.Header, body)
		return
	}
	if family == storeFamily {
		result := geminiVectorStoreCreated(body, candidate.DeploymentID)
		if result != nil {
			writeJSON(w, response.StatusCode, result)
			return
		}
	} else if result := geminiVectorSearchResults(body, query); result != nil {
		writeJSON(w, response.StatusCode, result)
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini vector store response")
}

func geminiVectorQuery(value any) (string, bool) {
	switch query := value.(type) {
	case string:
		return query, strings.TrimSpace(query) != ""
	case []any:
		parts := make([]string, 0, len(query))
		for _, part := range query {
			text, ok := part.(string)
			if !ok {
				return "", false
			}
			parts = append(parts, text)
		}
		joined := strings.Join(parts, " ")
		return joined, strings.TrimSpace(joined) != ""
	default:
		return "", false
	}
}

func geminiVectorFilter(value any) string {
	switch filter := value.(type) {
	case string:
		return filter
	case map[string]any:
		keys := make([]string, 0, len(filter))
		for key := range filter {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		expressions := make([]string, 0, len(keys))
		for _, key := range keys {
			switch entry := filter[key].(type) {
			case string:
				expressions = append(expressions, key+" = "+strconv.Quote(entry))
			case float64, bool:
				expressions = append(expressions, fmt.Sprintf("%s = %v", key, entry))
			}
		}
		return strings.Join(expressions, " AND ")
	default:
		return ""
	}
}

func geminiVectorStoreCreated(data []byte, deployment string) map[string]any {
	var response struct {
		Resource string    `json:"name"`
		Name     string    `json:"displayName"`
		Created  time.Time `json:"createTime"`
	}
	if json.Unmarshal(data, &response) != nil {
		return nil
	}
	id, ok := strings.CutPrefix(response.Resource, geminiStorePrefix)
	if !ok || !validResourceSegment(id) {
		return nil
	}
	var created int64
	if !response.Created.IsZero() {
		created = response.Created.Unix()
	}
	return map[string]any{
		"object": "vector_store", "id": encodeResourceID("vector_stores", deployment, id),
		"name": response.Name, "created_at": created, "bytes": 0, "status": "completed",
		"file_counts":   map[string]int{"in_progress": 0, "completed": 0, "failed": 0, "cancelled": 0, "total": 0},
		"expires_after": nil, "expires_at": nil, "last_active_at": nil, "metadata": nil,
	}
}

func geminiVectorSearchResults(data []byte, query string) map[string]any {
	var response struct {
		Candidates []struct {
			Grounding struct {
				Chunks []struct {
					Context *struct{ Text, URI, Title string } `json:"retrievedContext"`
				} `json:"groundingChunks"`
				Supports []struct {
					Segment    struct{ Text string } `json:"segment"`
					Indices    []int                 `json:"groundingChunkIndices"`
					Confidence []float64             `json:"confidenceScores"`
				} `json:"groundingSupports"`
			} `json:"groundingMetadata"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &response) != nil {
		return nil
	}
	matches := make([]map[string]any, 0)
	seen := make(map[string]bool)
	add := func(text string, file, filename, score any, attributes map[string]any) {
		matches = append(matches, map[string]any{"file_id": file, "filename": filename, "score": score, "attributes": attributes,
			"content": []map[string]string{{"type": "text", "text": text}}})
		seen[text] = true
	}
	for _, candidate := range response.Candidates {
		for _, chunk := range candidate.Grounding.Chunks {
			if context := chunk.Context; context != nil {
				var file, name any
				if context.URI != "" {
					file = context.URI
				}
				if context.Title != "" {
					name = context.Title
				}
				add(context.Text, file, name, nil, map[string]any{"uri": context.URI, "title": context.Title})
			}
		}
		for _, support := range candidate.Grounding.Supports {
			text := support.Segment.Text
			if text == "" || seen[text] {
				continue
			}
			var score any
			if len(support.Confidence) > 0 {
				score = support.Confidence[0]
			}
			indices := support.Indices
			if indices == nil {
				indices = []int{}
			}
			add(text, nil, nil, score, map[string]any{"grounding_chunk_indices": indices})
		}
	}
	return map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": matches, "has_more": false, "next_page": nil}
}
