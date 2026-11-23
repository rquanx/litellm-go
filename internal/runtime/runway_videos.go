package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *Server) selectRunwayVideo(alias, deployment string, scope []string) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for name, route := range s.configuredRoutes {
		if !modelAllowed(scope, name) || (alias != "" && name != alias) {
			continue
		}
		for _, candidate := range route.Candidates {
			if candidate.Provider != "runwayml" || candidate.Endpoint != "videos" || candidate.DeploymentID == "" ||
				(deployment != "" && deployment != candidate.DeploymentID) || !candidateAllowed(scope, candidate) {
				continue
			}
			if alias != "" || deployment != "" {
				return candidate, true
			}
			if selected.DeploymentID != "" && selected.DeploymentID != candidate.DeploymentID {
				return Candidate{}, false
			}
			selected = candidate
		}
	}
	return selected, selected.DeploymentID != ""
}

func runwayCreationPayload(candidate Candidate, input requestPayload) map[string]any {
	fields := clone(input.fields)
	delete(fields, "model")
	delete(fields, "user")
	delete(fields, "extra_headers")
	if extra, _ := fields["extra_body"].(map[string]any); extra != nil {
		for key, value := range extra {
			fields[key] = value
		}
	}
	delete(fields, "extra_body")
	if prompt, ok := fields["prompt"]; ok {
		fields["promptText"] = prompt
	}
	delete(fields, "prompt")
	if image, ok := fields["input_reference"]; ok {
		fields["promptImage"] = image
	}
	delete(fields, "input_reference")
	for _, part := range input.form {
		if part.name == "input_reference" && len(part.body) > 0 {
			media := part.header.Get("Content-Type")
			if strings.HasPrefix(media, "image/") {
				fields["promptImage"] = "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(part.body)
			}
		}
	}
	if size, _ := fields["size"].(string); strings.Contains(size, "x") {
		fields["ratio"] = strings.ReplaceAll(size, "x", ":")
	}
	delete(fields, "size")
	switch seconds := fields["seconds"].(type) {
	case json.Number:
		if duration, err := seconds.Float64(); err == nil && duration >= 0 && duration < math.MaxInt32 {
			fields["duration"] = int(duration)
		}
	case string:
		if duration, err := strconv.ParseFloat(seconds, 64); err == nil && duration >= 0 && duration < math.MaxInt32 {
			fields["duration"] = int(duration)
		}
	case float64:
		if seconds >= 0 && seconds < math.MaxInt32 {
			fields["duration"] = int(seconds)
		}
	}
	delete(fields, "seconds")
	fields["model"] = candidate.ModelID
	return fields
}

func (s *Server) runwayVideoResource(w http.ResponseWriter, r *http.Request, candidate Candidate, target string, input requestPayload) {
	base := strings.TrimRight(candidate.APIBase, "/")
	create := target == videoPath && r.Method == http.MethodPost
	download := strings.HasSuffix(target, "/content")
	cancel := r.Method == http.MethodDelete
	var payload map[string]any
	var address, id string
	method := r.Method
	if create {
		address = base + "/image_to_video"
		payload = runwayCreationPayload(candidate, input)
	} else {
		path := strings.TrimSuffix(strings.TrimPrefix(target, videoPath+"/"), "/content")
		var err error
		id, err = url.PathUnescape(path)
		if err != nil || target == videoPath || !validResourceSegment(id) || strings.HasPrefix(target, videoPath+"/characters/") ||
			(method != http.MethodGet && !cancel) {
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Runway supports creating, polling, cancelling and downloading videos")
			return
		}
		if !s.authorized(r) {
			key := "runway_video:" + id
			owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{key})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Runway video ownership")
				return
			}
			if !slices.Contains(s.containerOwnerScopes(r, false), owners[key]) {
				writeError(w, http.StatusNotFound, "video_not_found", "Unknown Runway video")
				return
			}
		}
		address = base + "/tasks/" + url.PathEscape(id)
		if cancel {
			address += "/cancel"
			method = http.MethodPost
			payload = map[string]any{}
		}
	}
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	upstream, err := http.NewRequestWithContext(r.Context(), method, address, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Runway video URL")
		return
	}
	for name, value := range candidate.Headers {
		upstream.Header.Set(name, value)
	}
	version := candidate.APIVersion
	if version == "" {
		version = "2024-11-06"
	}
	upstream.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	upstream.Header.Set("X-Runway-Version", version)
	upstream.Header.Set("Content-Type", "application/json")
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, raw, headers, err := s.sendGeminiFileRequest(&client, upstream)
	if err != nil || status/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach Runway")
		return
	}
	if status/100 != 2 {
		writeRawResponse(w, status, headers, raw)
		return
	}
	var result map[string]any
	if len(raw) == 0 && cancel {
		result = map[string]any{"id": id}
	} else if json.Unmarshal(raw, &result) != nil || result == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Runway task response")
		return
	}
	if value, _ := result["id"].(string); value != "" {
		if id != "" && value != id {
			writeError(w, http.StatusBadGateway, "upstream_error", "Runway returned a mismatched task ID")
			return
		}
		id = value
	}
	if !validResourceSegment(id) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Runway returned no valid task ID")
		return
	}
	output, _ := result["output"].(string)
	if urls, _ := result["output"].([]any); len(urls) > 0 {
		output, _ = urls[0].(string)
	}
	if download {
		s.runwayVideoContent(w, r, &client, upstream.URL, output)
		return
	}
	taskStatus, _ := result["status"].(string)
	mapped := map[string]string{"RUNNING": "in_progress", "SUCCEEDED": "completed", "FAILED": "failed", "CANCELLED": "failed"}[strings.ToUpper(taskStatus)]
	if mapped == "" {
		mapped = "queued"
	}
	if cancel {
		mapped = "cancelled"
	}
	response := map[string]any{"id": encodeResourceID("videos", candidate.DeploymentID, id), "object": "video", "status": mapped,
		"created_at": runwayTimestamp(result["createdAt"])}
	if !cancel {
		if output != "" {
			response["output_url"] = output
		}
		if completed, ok := result["completedAt"]; ok {
			response["completed_at"] = runwayTimestamp(completed)
		}
		if progress := result["progress"]; progress != nil && !create {
			response["progress"] = progress
		}
		if result["failureCode"] != nil || result["failure"] != nil {
			code, _ := result["failureCode"].(string)
			message, _ := result["failure"].(string)
			if code == "" {
				code = "unknown"
			}
			if message == "" {
				message = "Video generation failed"
			}
			response["error"] = map[string]string{"code": code, "message": message}
		}
	}
	if create {
		if !s.authorized(r) {
			owners := s.containerOwnerScopes(r, false)
			if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, "runway_video:"+id, owners[0]); err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Runway video ownership")
				return
			}
		}
		response["model"] = candidate.ModelID
		if ratio, _ := payload["ratio"].(string); strings.Contains(ratio, ":") {
			response["size"] = strings.ReplaceAll(ratio, ":", "x")
		}
		usage := make(map[string]any)
		if duration, ok := payload["duration"].(int); ok {
			response["seconds"] = strconv.Itoa(duration)
			usage["duration_seconds"] = duration
		}
		response["usage"] = usage
	}
	writeJSON(w, http.StatusOK, response)
}

func runwayTimestamp(value any) int64 {
	text, _ := value.(string)
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed.Unix()
	}
	return 0
}

func (s *Server) runwayVideoContent(w http.ResponseWriter, r *http.Request, client *http.Client, origin *url.URL, output string) {
	if output == "" {
		writeError(w, http.StatusConflict, "video_not_ready", "Runway has no video output for this task")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, output, nil)
	if err != nil || request.URL.User != nil || request.URL.Fragment != "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Runway video download URL")
		return
	}
	if !(request.URL.Scheme == origin.Scheme && strings.EqualFold(request.URL.Host, origin.Host)) {
		if request.URL.Scheme != "https" || !validOCRDocumentRequest(request) {
			writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Runway video download host")
			return
		}
		client = ocrDownloadClient
	}
	response, err := client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not download Runway video")
		return
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Runway video download failed")
		return
	}
	media := response.Header.Get("Content-Type")
	if media == "" {
		media = "video/mp4"
	}
	w.Header().Set("Content-Type", media)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, response.Body)
}
