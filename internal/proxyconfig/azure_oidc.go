package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func azureOIDCSource(reference string) (func(context.Context) (string, error), error) {
	kind, audience, ok := strings.Cut(strings.TrimPrefix(reference, "oidc/"), "/")
	if !ok || audience == "" {
		return nil, errors.New("Azure OIDC requires a provider and audience")
	}
	switch kind {
	case "env":
		return azureOIDCEnv(audience), nil
	case "circleci":
		return azureOIDCEnv("CIRCLE_OIDC_TOKEN"), nil
	case "circleci_v2":
		return azureOIDCEnv("CIRCLE_OIDC_TOKEN_V2"), nil
	case "azure":
		if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") == "" {
			var entry deployment
			entry.Params.AzureScope = audience
			entry.Params.AzureTokenRefresh = true
			source, _, err := entry.azureIdentity(true, "")
			if err != nil {
				return nil, err
			}
			return source, nil
		}
		fallthrough
	case "env_path", "file":
		return func(context.Context) (string, error) {
			path := audience
			switch kind {
			case "file":
				var err error
				path, err = azureOIDCFilePath(path)
				if err != nil {
					return "", err
				}
			case "env_path":
				path = os.Getenv(audience)
			case "azure":
				path = os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
			}
			file, err := os.Open(path)
			if err != nil {
				return "", errors.New("could not open Azure OIDC assertion file")
			}
			defer file.Close()
			data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
			token := strings.TrimSpace(string(data))
			if err != nil || len(data) > 64<<10 || token == "" {
				return "", errors.New("could not read Azure OIDC assertion file")
			}
			return token, nil
		}, nil
	case "google", "github":
		return func(ctx context.Context) (string, error) {
			return fetchAzureOIDCAssertion(ctx, kind, audience)
		}, nil
	default:
		return nil, errors.New("unsupported Azure OIDC provider")
	}
}

func azureOIDCEnv(name string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", errors.New("Azure OIDC assertion environment variable is empty")
		}
		return value, nil
	}
}

func azureOIDCFilePath(requested string) (string, error) {
	if filepath.IsAbs(requested) {
		path, err := filepath.EvalSymlinks(requested)
		if err == nil {
			roots := []string{"/var/run/secrets", "/run/secrets"}
			if override := os.Getenv("LITELLM_OIDC_ALLOWED_CREDENTIAL_DIRS"); override != "" {
				roots = strings.Split(override, ",")
			}
			for _, root := range roots {
				root = strings.TrimSpace(root)
				if !filepath.IsAbs(root) {
					continue
				}
				resolved, err := filepath.EvalSymlinks(root)
				if err != nil {
					continue
				}
				relative, err := filepath.Rel(resolved, path)
				if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
					return path, nil
				}
			}
		}
	}
	return "", errors.New("Azure OIDC assertion file must be inside an allowed credential directory")
}

func fetchAzureOIDCAssertion(ctx context.Context, kind, audience string) (string, error) {
	endpoint := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"
	headers := make(http.Header)
	headers.Set("Metadata-Flavor", "Google")
	if kind == "github" {
		endpoint = os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
		token := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
		if token == "" || !strings.HasPrefix(endpoint, "https://") {
			return "", errors.New("GitHub OIDC request credentials are missing")
		}
		headers.Del("Metadata-Flavor")
		headers.Set("Authorization", "Bearer "+token)
		headers.Set("Accept", "application/json; api-version=2.0")
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" {
		return "", errors.New("invalid Azure OIDC assertion endpoint")
	}
	query := target.Query()
	query.Set("audience", audience)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", errors.New("could not prepare Azure OIDC assertion request")
	}
	request.Header = headers
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("could not fetch Azure OIDC assertion")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || response.StatusCode != http.StatusOK {
		return "", errors.New("Azure OIDC assertion request failed")
	}
	token := strings.TrimSpace(string(data))
	if kind == "github" {
		var result struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(data, &result) != nil {
			return "", errors.New("invalid GitHub OIDC response")
		}
		token = result.Value
	}
	if token == "" {
		return "", errors.New("OIDC assertion response is empty")
	}
	return token, nil
}
