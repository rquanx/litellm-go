package runtime

import (
	"encoding/json"
	"net/http"
)

func (s *Server) pgVectorResource(w http.ResponseWriter, r *http.Request, candidate Candidate, family, path string, fields map[string]any) {
	var forwarded map[string]any
	switch {
	case family == "vector_stores_search":
		if _, valid := geminiVectorQuery(fields["query"]); !valid {
			writeError(w, http.StatusBadRequest, "invalid_request", "A vector store query is required")
			return
		}
		forwarded = map[string]any{"query": fields["query"]}
		for _, key := range []string{"filters", "max_num_results", "ranking_options", "rewrite_query"} {
			forwarded[key] = fields[key]
		}
	case r.Method == http.MethodPost && path == vectorStorePath:
		forwarded = make(map[string]any, 5)
		for _, key := range []string{"name", "file_ids", "expires_after", "chunking_strategy", "metadata"} {
			forwarded[key] = fields[key]
		}
	default:
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "pg_vector supports vector store creation and search")
		return
	}
	body, _ := json.Marshal(forwarded)
	if family == "vector_stores_search" {
		// Search results carry document identifiers that belong to the connector.
		family = ""
	}
	s.forwardOpenAIResource(w, r, candidate, family, path, "application/json", body)
}
