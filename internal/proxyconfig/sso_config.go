package proxyconfig

import (
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type uiSSOConfig struct {
	oauth2.Config
	provider string
	userInfo string
	base     *url.URL
	missing  []string
}

func loadUISSOConfig() uiSSOConfig {
	var cfg uiSSOConfig
	cfg.Scopes = []string{"openid", "email", "profile"}
	fields := func(name string) string {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			cfg.missing = append(cfg.missing, name)
		}
		return value
	}
	switch {
	case os.Getenv("GOOGLE_CLIENT_ID") != "":
		cfg.provider = "google"
		cfg.ClientID = os.Getenv("GOOGLE_CLIENT_ID")
		cfg.ClientSecret = fields("GOOGLE_CLIENT_SECRET")
		cfg.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}
		cfg.userInfo = "https://openidconnect.googleapis.com/v1/userinfo"
	case os.Getenv("MICROSOFT_CLIENT_ID") != "":
		cfg.provider = "microsoft"
		cfg.ClientID = os.Getenv("MICROSOFT_CLIENT_ID")
		cfg.ClientSecret = fields("MICROSOFT_CLIENT_SECRET")
		tenant := fields("MICROSOFT_TENANT")
		issuer := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0"
		cfg.Endpoint = oauth2.Endpoint{AuthURL: issuer + "/authorize", TokenURL: issuer + "/token", AuthStyle: oauth2.AuthStyleInParams}
		cfg.userInfo = "https://graph.microsoft.com/oidc/userinfo"
	case os.Getenv("GENERIC_CLIENT_ID") != "":
		cfg.provider = "generic"
		cfg.ClientID = os.Getenv("GENERIC_CLIENT_ID")
		cfg.ClientSecret = fields("GENERIC_CLIENT_SECRET")
		cfg.Endpoint = oauth2.Endpoint{AuthURL: fields("GENERIC_AUTHORIZATION_ENDPOINT"), TokenURL: fields("GENERIC_TOKEN_ENDPOINT"), AuthStyle: oauth2.AuthStyleInHeader}
		if strings.EqualFold(os.Getenv("GENERIC_INCLUDE_CLIENT_ID"), "true") {
			cfg.Endpoint.AuthStyle = oauth2.AuthStyleInParams
		}
		cfg.userInfo = fields("GENERIC_USERINFO_ENDPOINT")
		if scope := strings.Fields(os.Getenv("GENERIC_SCOPE")); len(scope) != 0 {
			cfg.Scopes = scope
		}
	default:
		return cfg
	}
	cfg.base = ssoURL(fields("PROXY_BASE_URL"))
	if cfg.base != nil && !cfg.base.ForceQuery && cfg.base.RawQuery == "" {
		cfg.RedirectURL = cfg.base.JoinPath("sso/callback").String()
	}
	return cfg
}

func ssoURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		if ip := net.ParseIP(host); u.Scheme != "http" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil
		}
	}
	return u
}

func (cfg uiSSOConfig) ready() bool {
	return cfg.provider != "" && len(cfg.missing) == 0 && cfg.RedirectURL != "" &&
		ssoURL(cfg.Endpoint.AuthURL) != nil && ssoURL(cfg.Endpoint.TokenURL) != nil && ssoURL(cfg.userInfo) != nil
}

const uiSSOFlowTTL = 5 * time.Minute
