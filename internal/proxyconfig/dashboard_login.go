package proxyconfig

import (
	"crypto/sha256"
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rquanx/litellm-go/internal/runtime"
)

const dashboardSessionIssuer = "litellm-go-dashboard"

func WithDashboardLogin(next http.Handler, master string, registries ...*KeyRegistry) http.Handler {
	username, exists := os.LookupEnv("UI_USERNAME")
	if !exists {
		username = "admin"
	}
	password, exists := os.LookupEnv("UI_PASSWORD")
	if !exists {
		password = master
	}
	if master == "" || password == "" || strings.EqualFold(os.Getenv("DISABLE_ADMIN_UI"), "true") {
		return next
	}
	login := dashboardLogin{master: master, username: sha256.Sum256([]byte(username)), password: sha256.Sum256([]byte(password))}
	if len(registries) > 0 {
		login.keys = registries[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fallback/login", dashboardPasswordForm)
	mux.HandleFunc("POST /login", login.signIn)
	mux.HandleFunc("POST /v2/login", login.signIn)
	mux.Handle("/", next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := runtime.ProxyCredential(r)
		if encoded, found := strings.CutPrefix(token, "litellm-ui."); found && len(encoded) < 2048 {
			parsed, err := jwt.Parse(encoded, func(*jwt.Token) (any, error) {
				return []byte(master), nil
			}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(dashboardSessionIssuer), jwt.WithExpirationRequired())
			if err == nil && parsed.Valid {
				r = runtime.WithVerifiedProxyCredential(r, master)
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type dashboardLogin struct {
	master string
	username, password [32]byte
	keys *KeyRegistry
}

var dashboardLoginForm = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LiteLLM Go sign in</title>
<style>
:root{color-scheme:light dark;font:16px system-ui}body{display:grid;min-height:100svh;place-items:center;margin:0}
main{width:min(340px,calc(100vw - 56px))}form{display:grid;gap:16px}label{display:grid;gap:6px}
input,button{font:inherit;padding:10px 12px;border:1px solid currentColor;border-radius:6px}button{cursor:pointer}
</style>
<main>
<h1>Sign in to LiteLLM Go</h1>
<form action="{{.}}" method="post">
<label>Username<input name="username" autocomplete="username" required></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button>
</form>
</main>
</html>`))

func dashboardPasswordForm(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	target := dashboardPath().JoinPath("login")
	_ = dashboardLoginForm.Execute(w, target.String())
}

func (login dashboardLogin) signIn(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	asJSON := r.URL.Path == "/v2/login"
	if asJSON {
		if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 16<<10), &data); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid login request")
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid login request")
			return
		}
		data.Username, data.Password = r.PostForm.Get("username"), r.PostForm.Get("password")
	}
	username, password := sha256.Sum256([]byte(data.Username)), sha256.Sum256([]byte(data.Password))
	if subtle.ConstantTimeCompare(login.username[:], username[:]) & subtle.ConstantTimeCompare(login.password[:], password[:]) != 1 {
		if login.keys == nil {
			modelError(w, http.StatusUnauthorized, "Invalid username or password")
			return
		}
		signed, expires, ok := login.issuePasswordSession(w, r, data.Username, data.Password)
		if ok {
			writeDashboardSession(w, r, signed, expires, asJSON)
		}
		return
	}
	now := time.Now().UTC()
	expires := now.Add(24*time.Hour)
	claims := jwt.MapClaims{"iss": dashboardSessionIssuer, "sub": "default_user_id",
		"iat": now.Unix(), "exp": expires.Unix(), "jti": newEntityID()}
	inner, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(login.master))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not issue login credential")
		return
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "default_user_id", "user_role": "proxy_admin", "key": "litellm-ui."+inner,
		"login_method": "username_password", "premium_user": false, "auth_header_name": "Authorization",
		"server_root_path": os.Getenv("SERVER_ROOT_PATH"), "iat": now.Unix(), "exp": expires.Unix(),
	}).SignedString([]byte(login.master))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not issue login session")
		return
	}
	writeDashboardSession(w, r, signed, expires, asJSON)
}

func writeDashboardSession(w http.ResponseWriter, r *http.Request, signed string, expires time.Time, asJSON bool) {
	base := dashboardPath()
	redirect := base.JoinPath("ui")
	redirect.Path += "/"
	redirect.RawQuery = "login=success"
	cookiePath := base.Path
	if cookiePath == "" {
		cookiePath = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{Name:"token", Value:signed, Path:cookiePath,
		Expires:expires, Secure:base.Scheme=="https" || r.TLS!=nil, SameSite:http.SameSiteLaxMode})
	if asJSON {
		modelJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect.String(), "token": signed})
		return
	}
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

func dashboardPath() *url.URL {
	if base := ssoURL(os.Getenv("PROXY_BASE_URL")); base != nil && base.RawQuery == "" {
		return base
	}
	return &url.URL{Path:"/"+strings.Trim(os.Getenv("SERVER_ROOT_PATH"), "/")}
}
