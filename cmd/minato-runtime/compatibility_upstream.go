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
	for _, pattern := range strings.Split(nativePaths, ",") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		base, wildcard := strings.CutSuffix(pattern, "/*")
		if base == "" || base != path.Clean(base) || !strings.HasPrefix(base, "/") || strings.ContainsAny(base, "*?#%\\") {
			return nil, errors.New("LITELLM_GO_NATIVE_PATHS must contain absolute paths or paths ending in /*")
		}
		if wildcard {
			prefixes = append(prefixes, base+"/")
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
		for _, name := range exact {
			if r.URL.Path == name {
				native.ServeHTTP(w, r)
				return
			}
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				native.ServeHTTP(w, r)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
