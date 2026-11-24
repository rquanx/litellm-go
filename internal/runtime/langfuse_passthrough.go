package runtime

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

func (s *Server) serveLangfusePassThrough(w http.ResponseWriter, r *http.Request, suffix string) bool {
	if suffix == "" || !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "PATCH"}, r.Method) {
		return false
	}
	_, token, ok := r.BasicAuth()
	authorized := ok && token != "" && token == s.config.Token
	if !authorized && ok && token != "" && s.config.AuthorizeKey != nil {
		_, authorized = s.config.AuthorizeKey(token)
	}
	if !authorized {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return true
	}
	for attempt := 0; ; attempt++ {
		if strings.ContainsAny(suffix, "\\\x00\r\n") || strings.HasPrefix(suffix, "/") {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Langfuse endpoint path")
			return true
		}
		for _, segment := range strings.Split(suffix, "/") {
			if segment == "." || segment == ".." {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Langfuse endpoint path")
				return true
			}
		}
		next, err := url.PathUnescape(suffix)
		if err != nil || next == suffix {
			break
		}
		if attempt == 7 {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Langfuse endpoint path")
			return true
		}
		suffix = next
	}
	base := os.Getenv("LANGFUSE_HOST")
	if base == "" {
		base = "https://cloud.langfuse.com"
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Langfuse host")
		return true
	}
	public, secret := os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
	if public == "" || secret == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Langfuse credentials are missing")
		return true
	}
	target.Path, target.RawPath, target.RawQuery, target.Fragment = "", "", "", ""
	route := PassThroughRoute{Path: "/langfuse", Target: *target, MergeQuery: true,
		Headers: http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+secret))}},
		proxyToken: token}
	s.forwardPassThrough(w, r, route)
	return true
}

