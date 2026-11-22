package proxyconfig

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type passThroughConfig struct {
	Path          string            `yaml:"path" json:"path"`
	Target        string            `yaml:"target" json:"target"`
	Headers       map[string]string `yaml:"headers" json:"headers,omitempty"`
	DefaultQuery  map[string]string `yaml:"default_query_params" json:"default_query_params,omitempty"`
	ForwardHeaders bool             `yaml:"forward_headers" json:"forward_headers,omitempty"`
	MergeQuery    bool              `yaml:"merge_query_params" json:"merge_query_params,omitempty"`
	IncludeSubpath bool             `yaml:"include_subpath" json:"include_subpath"`
	Auth          *bool             `yaml:"auth" json:"auth,omitempty"`
	Methods       []string          `yaml:"methods" json:"methods,omitempty"`
	Cost          *float64          `yaml:"cost_per_request" json:"cost_per_request,omitempty"`
	ID            string            `yaml:"id" json:"id,omitempty"`
}

func preparePassThrough(entries []passThroughConfig) ([]runtime.PassThroughRoute, error) {
	var routes []runtime.PassThroughRoute
	seen := make(map[string]bool)
	allMethods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD"}
	for _, entry := range entries {
		name := strings.TrimRight(entry.Path, "/")
		if name == "" || name != path.Clean(name) || !strings.HasPrefix(name, "/") ||
			strings.ContainsAny(name, "%{}?#\\") || runtime.IsBuiltInProxyPath(name) {
			return nil, fmt.Errorf("invalid or reserved pass-through path %q", entry.Path)
		}
		target, err := resolveEnv(entry.Target)
		if err != nil {
			return nil, err
		}
		address, err := url.Parse(target)
		if err != nil || address.Hostname() == "" || (address.Scheme != "https" && address.Scheme != "http") ||
			address.User != nil || address.Fragment != "" {
			return nil, fmt.Errorf("invalid target for pass-through path %q", name)
		}
		headers := make(http.Header, len(entry.Headers))
		for key, value := range entry.Headers {
			resolved, err := resolveEnv(value)
			if err != nil {
				return nil, err
			}
			headers.Set(key, resolved)
		}
		query := make(url.Values, len(entry.DefaultQuery))
		for key, value := range entry.DefaultQuery {
			resolved, err := resolveEnv(value)
			if err != nil {
				return nil, err
			}
			query.Set(key, resolved)
		}
		methods := entry.Methods
		if len(methods) == 0 {
			methods = allMethods[:5]
		}
		normalized := make([]string, 0, len(methods))
		for _, method := range methods {
			method = strings.ToUpper(method)
			signature := method + " " + name
			if !slices.Contains(allMethods, method) || seen[signature] {
				return nil, fmt.Errorf("invalid or duplicate pass-through method on %q", name)
			}
			seen[signature] = true
			normalized = append(normalized, method)
		}
		if entry.Cost != nil && (*entry.Cost < 0 || math.IsNaN(*entry.Cost) || math.IsInf(*entry.Cost, 0)) {
			return nil, fmt.Errorf("invalid pass-through cost on %q", name)
		}
		routes = append(routes, runtime.PassThroughRoute{Path: name, Target: *address, Headers: headers, DefaultQuery: query,
			ForwardHeaders: entry.ForwardHeaders, MergeQuery: entry.MergeQuery, IncludeSubpath: entry.IncludeSubpath,
			Public: entry.Auth != nil && !*entry.Auth, Methods: normalized, Cost: entry.Cost})
	}
	return routes, nil
}
