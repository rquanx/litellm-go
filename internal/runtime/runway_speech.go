package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func (s *Server) runwaySpeech(parent context.Context, candidate Candidate, input requestPayload) (int, []byte, http.Header, bool, error) {
	text, ok := input.fields["input"].(string)
	if !ok {
		return 0, nil, nil, false, invalidMediaRequest{errors.New("Runway speech requires text input")}
	}
	var voice any = input.fields["voice"]
	switch value := voice.(type) {
	case string:
		id := map[string]string{"alloy": "Maya", "echo": "James", "fable": "Bernard", "onyx": "Vincent", "nova": "Serene", "shimmer": "Ella"}[value]
		if id == "" {
			id = value
		}
		voice = map[string]string{"type": "runway-preset", "presetId": id}
	case map[string]any:
	default:
		voice = map[string]string{"type": "runway-preset", "presetId": "Bernard"}
	}
	body, _ := json.Marshal(map[string]any{"model": candidate.ModelID, "promptText": text, "voice": voice})
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, nil, false, ctx.Err()
	}
	defer func() { <-s.sem }()

	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1") + "/v1"
	origin, err := url.Parse(base)
	if err != nil {
		return 0, nil, nil, false, err
	}
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	version := candidate.APIVersion
	if version == "" {
		version = "2024-11-06"
	}
	request := func(method, address string, body []byte) (int, []byte, http.Header, error) {
		req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
		if err != nil {
			return 0, nil, nil, err
		}
		for name, value := range candidate.Headers {
			req.Header.Set(name, value)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Runway-Version", version)
		req.Header.Set("Content-Type", "application/json")
		return s.sendGeminiFileRequest(&client, req)
	}

	status, raw, headers, err := request(http.MethodPost, base+"/text_to_speech", body)
	if err != nil || status/100 != 2 {
		return status, raw, headers, status == http.StatusTooManyRequests, err
	}
	var task struct {
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Output []string `json:"output"`
	}
	if json.Unmarshal(raw, &task) != nil || !validResourceSegment(task.ID) {
		return 0, nil, nil, false, errors.New("Runway returned an invalid speech task")
	}
	id := task.ID
	address := base + "/tasks/" + url.PathEscape(id)
	for {
		task = struct {
			ID     string   `json:"id"`
			Status string   `json:"status"`
			Output []string `json:"output"`
		}{}
		status, raw, headers, err = request(http.MethodGet, address, nil)
		if err != nil || status/100 != 2 {
			return status, raw, headers, false, err
		}
		if json.Unmarshal(raw, &task) != nil || (task.ID != "" && task.ID != id) {
			return 0, nil, nil, false, errors.New("Invalid Runway speech task result")
		}
		switch strings.ToUpper(task.Status) {
		case "SUCCEEDED":
			if len(task.Output) == 0 {
				return 0, nil, nil, false, errors.New("Runway returned no speech output")
			}
			return s.downloadRunwaySpeech(ctx, &client, origin, task.Output[0])
		case "PENDING", "RUNNING", "THROTTLED":
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return 0, nil, nil, false, ctx.Err()
			}
		default:
			return 0, nil, nil, false, errors.New("Runway speech task did not complete successfully")
		}
	}
}

func (s *Server) downloadRunwaySpeech(ctx context.Context, client *http.Client, origin *url.URL, output string) (int, []byte, http.Header, bool, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, output, nil)
	if err != nil || r.URL.Hostname() == "" || r.URL.User != nil || r.URL.Fragment != "" {
		return 0, nil, nil, false, errors.New("Invalid Runway speech download URL")
	}
	if !(r.URL.Scheme == origin.Scheme && strings.EqualFold(r.URL.Host, origin.Host)) {
		if r.URL.Scheme != "https" || !validOCRDocumentRequest(r) {
			return 0, nil, nil, false, errors.New("Invalid Runway speech download host")
		}
		client = ocrDownloadClient
	}
	response, err := client.Do(r)
	if err != nil {
		return 0, nil, nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return 0, nil, nil, false, errors.New("Runway speech download failed")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("Invalid Runway speech download")
	}
	if response.Header.Get("Content-Type") == "" {
		response.Header.Set("Content-Type", "audio/mpeg")
	}
	return http.StatusOK, raw, response.Header, false, nil
}
