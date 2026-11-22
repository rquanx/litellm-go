package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

type realtimeTicket struct {
	Secret      string `json:"secret"`
	Alias       string `json:"alias"`
	Deployment  string `json:"deployment"`
	Fingerprint string `json:"fingerprint"`
	Expires     int64  `json:"expires"`
}

func realtimeHTTPOperation(path string) string {
	for _, prefix := range []string{"/v1/realtime/", "/realtime/", "/openai/v1/realtime/"} {
		if operation, ok := strings.CutPrefix(path, prefix); ok && (operation == "client_secrets" || operation == "calls") {
			return operation
		}
	}
	return ""
}

func realtimeEndpointFingerprint(candidate Candidate) string {
	digest := sha256.Sum256([]byte(candidate.Provider + "\x00" + candidate.APIBase + "\x00" + candidate.ModelID + "\x00" + candidate.APIVersion))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (s *Server) realtimeClientSecret(w http.ResponseWriter, request *http.Request, scope []string) {
	if s.config.Token == "" {
		writeError(w, http.StatusServiceUnavailable, "master_key_required", "WebRTC requires a proxy master key")
		return
	}
	var input struct {
		Model        string         `json:"model"`
		Session      map[string]any `json:"session"`
		ExpiresAfter map[string]any `json:"expires_after"`
	}
	if err := decodeBody(request, &input, s.config.MaxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Realtime session configuration")
		return
	}
	alias, _ := input.Session["model"].(string)
	if alias == "" {
		alias = input.Model
	}
	if alias == "" {
		alias = "gpt-4o-realtime-preview"
	}
	if !modelAllowed(scope, alias) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	candidate, ok := s.resourceCandidate(alias, "", scope)
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", "No WebRTC deployment for this model")
		return
	}
	if input.Session == nil {
		input.Session = make(map[string]any)
	}
	if _, exists := input.Session["type"]; !exists {
		input.Session["type"] = "realtime"
	}
	input.Session["model"] = candidate.ModelID
	body := map[string]any{"session": input.Session}
	if input.ExpiresAfter != nil {
		body["expires_after"] = input.ExpiresAfter
	}
	payload, _ := json.Marshal(body)
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	status, headers, response, err := s.postRealtime(request.Context(), candidate, "client_secrets", key, "application/json", payload)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not create a Realtime client secret")
		return
	}
	if status < 200 || status >= 300 {
		writeRawResponse(w, status, headers, response)
		return
	}
	var result map[string]any
	if json.Unmarshal(response, &result) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Realtime client secret response")
		return
	}
	secret, _ := result["value"].(string)
	expires, _ := result["expires_at"].(float64)
	session := objectField(result, "session")
	nested := objectField(session, "client_secret")
	if secret == "" {
		secret, _ = nested["value"].(string)
	}
	if expires == 0 {
		expires, _ = nested["expires_at"].(float64)
	}
	if secret == "" || int64(expires) <= time.Now().Unix() {
		writeError(w, http.StatusBadGateway, "upstream_error", "Provider returned a missing or expired Realtime client secret")
		return
	}
	ticket := realtimeTicket{Secret: secret, Alias: alias, Deployment: candidate.DeploymentID,
		Fingerprint: realtimeEndpointFingerprint(candidate), Expires: int64(expires)}
	value := s.sealRealtimeTicket(ticket)
	result["value"] = value
	if nested != nil {
		nested["value"] = value
	}
	if session != nil {
		session["model"] = alias
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, result)
}

func (s *Server) realtimeCall(w http.ResponseWriter, request *http.Request) {
	token, _ := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	ticket, ok := s.openRealtimeTicket(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired Realtime client secret")
		return
	}
	candidate, ok := s.resourceCandidate(ticket.Alias, ticket.Deployment, nil)
	if !ok || realtimeEndpointFingerprint(candidate) != ticket.Fingerprint {
		writeError(w, http.StatusNotFound, "model_not_found", "The Realtime deployment is unavailable")
		return
	}
	offer, err := readBody(request, s.config.MaxBodyBytes)
	if err != nil || len(bytes.TrimSpace(offer)) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "A valid SDP offer is required")
		return
	}
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	config, _ := json.Marshal(map[string]string{"type": "realtime", "model": candidate.ModelID})
	if err := errors.Join(form.WriteField("sdp", string(offer)), form.WriteField("session", string(config)), form.Close()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not encode the SDP offer")
		return
	}
	status, headers, response, err := s.postRealtime(request.Context(), candidate, "calls", ticket.Secret, form.FormDataContentType(), data.Bytes())
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not establish the Realtime call")
		return
	}
	writeRawResponse(w, status, headers, response)
}

func (s *Server) sealRealtimeTicket(ticket realtimeTicket) string {
	key := sha256.Sum256([]byte("minato-realtime-v1\x00" + s.config.Token))
	var nonce [24]byte
	rand.Read(nonce[:])
	raw, _ := json.Marshal(ticket)
	return "rtc_" + base64.RawURLEncoding.EncodeToString(secretbox.Seal(nonce[:], raw, &nonce, &key))
}

func (s *Server) openRealtimeTicket(token string) (realtimeTicket, bool) {
	var ticket realtimeTicket
	raw, ok := strings.CutPrefix(token, "rtc_")
	if !ok || len(raw) > 16<<10 || s.config.Token == "" {
		return ticket, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(sealed) < 24+secretbox.Overhead {
		return ticket, false
	}
	key := sha256.Sum256([]byte("minato-realtime-v1\x00" + s.config.Token))
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	payload, ok := secretbox.Open(nil, sealed[24:], &nonce, &key)
	if !ok || json.Unmarshal(payload, &ticket) != nil {
		return ticket, false
	}
	return ticket, ticket.Secret != "" && ticket.Deployment != "" && time.Now().Unix() < ticket.Expires
}

func (s *Server) postRealtime(ctx context.Context, candidate Candidate, operation, key, contentType string, payload []byte) (int, http.Header, []byte, error) {
	base := strings.TrimRight(candidate.APIBase, "/")
	target := strings.TrimSuffix(base, "/v1") + "/v1/realtime/" + operation
	if candidate.Provider == "azure" && !strings.HasSuffix(base, "/openai/v1") {
		target = strings.TrimSuffix(base, "/openai") + "/openai/realtime/" + operation + "?api-version=" + url.QueryEscape(candidate.APIVersion)
	}
	outgoing, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	outgoing.Header.Set("Content-Type", contentType)
	if candidate.Provider == "azure" && operation == "calls" {
		outgoing.Header.Set("api-key", key)
	} else {
		providerKeyHeaders(outgoing.Header, candidate, key)
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, nil, ctx.Err()
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(outgoing)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if int64(len(body)) > s.config.MaxBodyBytes {
		err = errors.New("provider response exceeds size limit")
	}
	return response.StatusCode, response.Header, body, err
}
