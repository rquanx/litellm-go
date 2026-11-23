package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
)

func (s *Server) searchMilvus(w http.ResponseWriter, r *http.Request, candidate Candidate, id string, fields map[string]any, scope []string) {
	model, _ := fields["embedding_model"].(string)
	query, ok := geminiVectorQuery(fields["query"])
	if !ok || model == "" || !validResourceSegment(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Milvus requires a collection, an embedding model and a query")
		return
	}
	embeddings, ok := s.ragEmbeddings(w, r, scope, model, []string{query}, nil)
	if !ok {
		return
	}

	payload := map[string]any{"collectionName": id, "data": embeddings, "annsField": "book_intro_vector"}
	for _, field := range []string{"filters", "max_num_results", "ranking_options", "rewrite_query", "filter", "limit", "outputFields", "searchParams", "annsField"} {
		if value := fields[field]; value != nil {
			payload[field] = value
		}
	}
	for public, native := range map[string]string{"milvus_db_name": "dbName", "milvus_partition_names": "partitionNames"} {
		if value := fields[public]; value != nil {
			payload[native] = value
		}
	}
	textField, _ := fields["milvus_text_field"].(string)
	body, _ := json.Marshal(payload)
	s.forwardResource(w, r, candidate, "", "/v2/vectordb/entities/search", "application/json", body, func(raw []byte) ([]byte, error) {
		var result struct {
			Code int              `json:"code"`
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Code != 0 {
			return nil, errors.New("Invalid Milvus search response")
		}
		items := make([]map[string]any, 0, len(result.Data))
		for _, row := range result.Data {
			score, _ := row["distance"].(float64)
			content, _ := row[textField].(string)
			attrs := make(map[string]any)
			for key, value := range row {
				if !slices.Contains([]string{"id", "content", "distance", textField}, key) {
					attrs[key] = value
				}
			}
			items = append(items, map[string]any{"score": score, "file_id": nil, "filename": nil,
				"content": []map[string]string{{"type": "text", "text": content}}, "attributes": attrs})
		}
		return json.Marshal(map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": items})
	})
}
