package main

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// withCompatibilityUpstream selects the handler before reading a request body.
// The selected service owns authentication, resource state, and the response.
func withCompatibilityUpstream(native http.Handler, address, nativePaths string) (http.Handler, error) {
	if address == "" {
		if nativePaths != "" {
			return nil, errors.New("LITELLM_GO_NATIVE_PATHS requires LITELLM_GO_COMPATIBILITY_UPSTREAM")
		}
		return native, nil
	}
	upstream, err := url.Parse(address)
	if err != nil || upstream.Hostname() == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") ||
		upstream.User != nil || upstream.RawQuery != "" || upstream.ForceQuery || upstream.Fragment != "" ||
		(upstream.Path != "" && upstream.Path != "/") || upstream.RawPath != "" {
		return nil, errors.New("LITELLM_GO_COMPATIBILITY_UPSTREAM must be an HTTP or HTTPS origin")
	}
	var exact, prefixes []string
	var upstreamExact, upstreamPrefixes []string
	for _, pattern := range strings.Split(nativePaths, ",") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		pattern, upstreamOnly := strings.CutPrefix(pattern, "!")
		base, wildcard := strings.CutSuffix(pattern, "/*")
		if base == "" || base != path.Clean(base) || !strings.HasPrefix(base, "/") || strings.ContainsAny(base, "*?#%\\") {
			return nil, errors.New("LITELLM_GO_NATIVE_PATHS must contain absolute paths or paths ending in /*, optionally prefixed with !")
		}
		if wildcard {
			prefix := base + "/"
			if upstreamOnly {
				upstreamPrefixes = append(upstreamPrefixes, prefix)
			} else {
				prefixes = append(prefixes, prefix)
			}
		} else if upstreamOnly {
			upstreamExact = append(upstreamExact, base)
		} else {
			exact = append(exact, base)
		}
	}
	if len(exact)+len(prefixes) > 0 && native == nil {
		return nil, errors.New("native paths require a native proxy handler")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchesCompatibilityPath(r.URL.Path, upstreamExact, upstreamPrefixes) {
			proxy.ServeHTTP(w, r)
			return
		}
		if matchesCompatibilityPath(r.URL.Path, exact, prefixes) {
			native.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}

func matchesCompatibilityPath(path string, exact, prefixes []string) bool {
	for _, name := range exact {
		if path == name {
			return true
		}
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
