package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func vertexDiscoveryBase(candidate Candidate) string {
	if candidate.Vertex.DiscoveryBase != "" {
		return strings.TrimRight(candidate.Vertex.DiscoveryBase, "/")
	}
	return "https://discoveryengine.googleapis.com"
}

func (s *Server) searchVertexDiscovery(w http.ResponseWriter, r *http.Request, candidate Candidate, id string, fields map[string]any) {
	query, valid := geminiVectorQuery(fields["query"])
	collection, _ := fields["vertex_collection_id"].(string)
	if collection == "" {
		collection = "default_collection"
	}
	if !valid || !vertexRAGComponent.MatchString(id) || !vertexRAGComponent.MatchString(collection) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Vertex AI Search requires a datastore ID, a valid collection ID and a query")
		return
	}

	target := vertexDiscoveryBase(candidate) + "/v1/" + vertexRAGPrefix(candidate) +
		"/collections/" + url.PathEscape(collection) + "/dataStores/" + url.PathEscape(id) +
		"/servingConfigs/default_config:search"
	payload, _ := json.Marshal(map[string]any{"query": query, "pageSize": 10})
	raw, ok := s.vertexDocumentRequest(w, r, candidate, http.MethodPost, target, "application/json", payload)
	if !ok {
		return
	}

	var result struct {
		Results []struct {
			ID       string `json:"id"`
			Document struct {
				Derived struct {
					Title        string `json:"title"`
					Link         string `json:"link"`
					DisplayLink  string `json:"displayLink"`
					FormattedURL string `json:"formattedUrl"`
					Snippets     []struct {
						Snippet     *string `json:"snippet"`
						HTMLSnippet string  `json:"htmlSnippet"`
					} `json:"snippets"`
				} `json:"derivedStructData"`
			} `json:"document"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &result) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex AI Search result")
		return
	}

	data := make([]map[string]any, 0, len(result.Results))
	for i, entry := range result.Results {
		document := entry.Document.Derived
		textParts := make([]string, 0, len(document.Snippets))
		for _, snippet := range document.Snippets {
			text := snippet.HTMLSnippet
			if snippet.Snippet != nil {
				text = *snippet.Snippet
			}
			textParts = append(textParts, text)
		}
		text := strings.Join(textParts, " ")
		if text == "" {
			text = document.Title
		}
		id, name := document.Link, document.Title
		if id == "" {
			id = entry.ID
		}
		if name == "" {
			name = "Unknown Document"
		}
		attributes := map[string]string{"document_id": entry.ID}
		for key, value := range map[string]string{
			"link": document.Link, "title": document.Title,
			"displayLink": document.DisplayLink, "formattedUrl": document.FormattedURL,
		} {
			if value != "" {
				attributes[key] = value
			}
		}
		data = append(data, map[string]any{
			"file_id": id, "filename": name, "score": 1.0 / float64(i+1), "attributes": attributes,
			"content": []map[string]string{{"type": "text", "text": text}},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": data})
}
