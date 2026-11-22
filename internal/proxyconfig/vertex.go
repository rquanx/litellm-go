package proxyconfig

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
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
	if entry.Params.APIKey == "" {
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second})
		credentials, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
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
