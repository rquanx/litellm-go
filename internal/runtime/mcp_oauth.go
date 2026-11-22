package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type MCPOAuth struct {
	TokenURL, ClientID, ClientSecret string
	Scopes                           []string
	mu                               sync.Mutex
	token                            *oauth2.Token
}

func (auth *MCPOAuth) bearer(ctx context.Context, client *http.Client) (string, error) {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.token.Valid() {
		return auth.token.AccessToken, nil
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	config := clientcredentials.Config{TokenURL: auth.TokenURL, ClientID: auth.ClientID, ClientSecret: auth.ClientSecret,
		Scopes: auth.Scopes, AuthStyle: oauth2.AuthStyleInParams}
	token, err := config.Token(context.WithValue(bounded, oauth2.HTTPClient, client))
	if err != nil || !token.Valid() {
		return "", errors.New("MCP OAuth token request failed")
	}
	if token.Expiry.IsZero() {
		token.Expiry = time.Now().Add(time.Hour)
	}
	auth.token = token
	return token.AccessToken, nil
}

func (auth *MCPOAuth) reset(token string) {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.token != nil && auth.token.AccessToken == token {
		auth.token = nil
	}
}

func (auth *MCPOAuth) fingerprint() [32]byte {
	if auth == nil {
		return [32]byte{}
	}
	data, _ := json.Marshal([]any{auth.TokenURL, auth.ClientID, auth.ClientSecret, auth.Scopes})
	return sha256.Sum256(data)
}
