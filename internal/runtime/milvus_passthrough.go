package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type MilvusIndexLookup func(r *http.Request, index, permission string) (alias, collection, database string, found, allowed bool, err error)

func (s *Server) serveMilvusPassThrough(w http.ResponseWriter, r *http.Request, suffix string) bool {
	if suffix == "" {
		return false
	}
	permission := ""
	switch r.Method {
	case http.MethodPost:
		switch suffix {
		case "v2/vectordb/entities/search", "v2/vectordb/entities/get", "v2/vectordb/entities/query":
			permission = "read"
		case "v2/vectordb/entities/upsert", "v2/vectordb/entities/insert":
			permission = "write"
		}
	case http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	var scope []string
	if !s.authorized(r) {
		var valid bool
		if s.config.AuthorizeKey != nil {
			scope, valid = s.config.AuthorizeKey(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
		if permission == "" {
			writeError(w, http.StatusForbidden, "index_not_allowed", "This Milvus operation requires the proxy master key")
			return true
		}
	}
	if s.config.MilvusIndexLookup == nil {
		writeError(w, http.StatusServiceUnavailable, "resource_error", "Milvus index registry is unavailable")
		return true
	}
	body, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		return true
	}
	var fields map[string]json.RawMessage
	var index string
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["collectionName"], &index) != nil || !validResourceSegment(index) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A registered collectionName is required")
		return true
	}
	alias, collection, database, found, allowed, err := s.config.MilvusIndexLookup(r, index, permission)
	switch {
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not resolve the Milvus index")
		return true
	case !found:
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Milvus index")
		return true
	case !allowed:
		writeError(w, http.StatusForbidden, "index_not_allowed", "The key does not allow this index operation")
		return true
	}
	candidate, ok := s.selectResourceProvider(alias, "", "milvus", scope, true)
	if !ok || !validResourceSegment(collection) {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Milvus deployment or collection")
		return true
	}
	target, err := url.Parse(candidate.APIBase)
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Milvus URL")
		return true
	}
	fields["collectionName"], _ = json.Marshal(collection)
	delete(fields, "dbName")
	if database != "" {
		fields["dbName"], _ = json.Marshal(database)
	}
	body, _ = json.Marshal(fields)
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	prefix, _, _ := strings.Cut(r.URL.Path[1:], "/")
	s.forwardPassThrough(w, r, PassThroughRoute{Path: "/" + prefix, Target: *target, MergeQuery: true, Headers: http.Header{
		"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"},
	}})
	return true
}
