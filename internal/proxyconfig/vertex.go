package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var vertexComponent = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (entry deployment) vertexConfig(base string) (*runtime.VertexConfig, string, error) {
	project, err := resolveEnv(entry.Params.VertexProject)
	if err != nil {
		return nil, "", err
	}
	if project == "" {
		project = os.Getenv("VERTEXAI_PROJECT")
	}
	if project == "" {
		project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	location, err := resolveEnv(entry.Params.VertexLocation)
	if err != nil {
		return nil, "", err
	}
	if location == "" {
		location = os.Getenv("VERTEXAI_LOCATION")
	}
	if location == "" {
		location = "us-central1"
	}
	config := &runtime.VertexConfig{Project: project, Location: location}
	config.Bucket, err = resolveEnv(entry.Params.GCSBucket)
	if err != nil {
		return nil, "", err
	}
	if config.Bucket == "" {
		config.Bucket = os.Getenv("GCS_BUCKET_NAME")
	}
	config.StorageBase, err = resolveEnv(entry.Params.GCSEndpoint)
	if err != nil {
		return nil, "", err
	}
	if config.StorageBase != "" {
		target, err := url.Parse(config.StorageBase)
		if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Hostname() == "" ||
			target.User != nil || target.RawQuery != "" || target.Fragment != "" {
			return nil, "", errors.New("gcs_endpoint must be an HTTP or HTTPS URL without credentials, query or fragment")
		}
	}
	config.DiscoveryBase, err = resolveEnv(entry.Params.VertexDiscoveryEndpoint)
	if err != nil {
		return nil, "", err
	}
	if config.DiscoveryBase != "" {
		target, err := url.Parse(config.DiscoveryBase)
		if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Hostname() == "" ||
			target.User != nil || target.ForceQuery || target.RawQuery != "" || target.Fragment != "" {
			return nil, "", errors.New("vertex_discovery_endpoint must be an HTTP or HTTPS URL without credentials, query or fragment")
		}
	}
	if entry.Params.APIKey == "" {
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second})
		credentials, err := entry.vertexCredentials(ctx)
		if err != nil {
			return nil, "", errors.New("could not load Application Default Credentials for Vertex AI")
		}
		config.TokenSource = credentials.TokenSource
		if config.Project == "" {
			config.Project = credentials.ProjectID
		}
	}
	if !vertexComponent.MatchString(config.Project) || !vertexComponent.MatchString(config.Location) {
		return nil, "", errors.New("Vertex AI requires a valid project ID and location")
	}
	if base == "" {
		base = "https://aiplatform.googleapis.com"
		if config.Location != "global" {
			base = "https://" + config.Location + "-aiplatform.googleapis.com"
		}
		if entry.Info.Mode == "rerank" || strings.Contains(entry.Params.Model, "/semantic-ranker-") {
			base = "https://discoveryengine.googleapis.com"
		}
	}
	return config, base, nil
}

func (entry deployment) vertexCredentials(ctx context.Context) (*google.Credentials, error) {
	const scope = "https://www.googleapis.com/auth/cloud-platform"
	value := entry.Params.VertexCredentials
	if value == nil {
		value = entry.Params.VertexAICredentials
	}
	if value == nil {
		for _, env := range []string{"VERTEXAI_CREDENTIALS", "VERTEX_CREDENTIALS"} {
			if setting := os.Getenv(env); setting != "" {
				value = setting
				break
			}
		}
	}
	var data []byte
	switch input := value.(type) {
	case nil:
		return google.FindDefaultCredentials(ctx, scope)
	case string:
		resolved, err := resolveEnv(input)
		if err != nil {
			return nil, err
		}
		resolved = strings.TrimSpace(resolved)
		if strings.HasPrefix(resolved, "{") {
			data = []byte(resolved)
		} else {
			data, err = os.ReadFile(resolved)
			if err != nil {
				return nil, errors.New("could not read configured Vertex credentials")
			}
		}
	case map[string]any:
		var err error
		data, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("Vertex credentials must be a JSON object or a filename")
	}
	return google.CredentialsFromJSON(ctx, data, scope)
}
