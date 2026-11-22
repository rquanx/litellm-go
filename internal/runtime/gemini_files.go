package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func (s *Server) geminiFile(w http.ResponseWriter, r *http.Request, candidate Candidate, path string, form []formPart) {
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1beta")
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	if path == "/v1/files" && r.Method == http.MethodPost {
		s.uploadGeminiFile(w, r, &client, candidate, base, form)
		return
	}
	id := strings.TrimPrefix(path, "/v1/files/")
	if path == "/v1/files" || strings.Contains(id, "/") || !validResourceSegment(id) {
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Gemini files support upload, retrieval and deletion")
		return
	}
	upstream, err := newGeminiFileRequest(r, candidate, base+"/v1beta/files/"+id, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini file URL")
		return
	}
	status, data, headers, err := s.sendGeminiFileRequest(&client, upstream)
	if !writeGeminiFileError(w, status, data, headers, err) {
		return
	}
	if r.Method == http.MethodDelete {
		native, _ := url.PathUnescape(id)
		writeJSON(w, http.StatusOK, map[string]any{"id": encodeResourceID("files", candidate.DeploymentID, native), "deleted": true, "object": "file"})
		return
	}
	writeGeminiFile(w, data, candidate, false)
}

func (s *Server) uploadGeminiFile(w http.ResponseWriter, r *http.Request, client *http.Client, candidate Candidate, base string, form []formPart) {
	var fileData []byte
	var filename, media string
	for _, part := range form {
		if part.name != "file" {
			continue
		}
		_, parameters, _ := mime.ParseMediaType(part.header.Get("Content-Disposition"))
		if filename != "" || parameters["filename"] == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "Provide exactly one named Gemini file")
			return
		}
		filename, fileData = parameters["filename"], part.body
		media = part.header.Get("Content-Type")
	}
	if filename == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "A Gemini file is required")
		return
	}
	if media == "" || media == "application/octet-stream" {
		media = http.DetectContentType(fileData)
	}
	metadata, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": filename}})
	start, err := newGeminiFileRequest(r, candidate, base+"/upload/v1beta/files", metadata)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini file upload URL")
		return
	}
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("X-Goog-Upload-Protocol", "resumable")
	start.Header.Set("X-Goog-Upload-Command", "start")
	start.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.Itoa(len(fileData)))
	start.Header.Set("X-Goog-Upload-Header-Content-Type", media)
	status, data, headers, err := s.sendGeminiFileRequest(client, start)
	if !writeGeminiFileError(w, status, data, headers, err) {
		return
	}
	target, err := url.Parse(headers.Get("X-Goog-Upload-Url"))
	if err != nil || target.User != nil || target.Fragment != "" || target.Host == "" ||
		!strings.EqualFold(start.URL.Host, target.Host) || start.URL.Scheme != target.Scheme {
		writeError(w, http.StatusBadGateway, "upstream_error", "Gemini file upload URL must use the configured provider origin")
		return
	}
	finalize, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(fileData))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini upload session")
		return
	}
	finalize.Header.Set("X-Goog-Upload-Offset", "0")
	finalize.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	status, data, headers, err = s.sendGeminiFileRequest(client, finalize)
	if !writeGeminiFileError(w, status, data, headers, err) {
		return
	}
	writeGeminiFile(w, data, candidate, true)
}

func newGeminiFileRequest(r *http.Request, candidate Candidate, target string, body []byte) (*http.Request, error) {
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if err := applyGeminiCredential(upstream, candidate, key); err != nil {
		return nil, err
	}
	for name, value := range candidate.Headers {
		upstream.Header.Set(name, value)
	}
	return upstream, nil
}

func (s *Server) sendGeminiFileRequest(client *http.Client, request *http.Request) (int, []byte, http.Header, error) {
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err == nil && int64(len(data)) > s.config.MaxBodyBytes {
		err = io.ErrShortBuffer
	}
	return response.StatusCode, data, response.Header, err
}

func writeGeminiFileError(w http.ResponseWriter, status int, data []byte, headers http.Header, err error) bool {
	if err != nil || status/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not complete the Gemini file request")
		return false
	}
	if status/100 != 2 {
		writeRawResponse(w, status, headers, data)
		return false
	}
	return true
}

func geminiFileID(id string) string {
	if strings.HasPrefix(id, "https://") || strings.HasPrefix(id, "http://") {
		uri, err := url.Parse(id)
		if err != nil {
			return ""
		}
		_, path, found := strings.Cut(uri.Path, "/files/")
		if !found {
			return ""
		}
		id = path
	}
	return strings.TrimPrefix(id, "files/")
}

func writeGeminiFile(w http.ResponseWriter, data []byte, candidate Candidate, created bool) {
	if created {
		var upload struct {
			File json.RawMessage `json:"file"`
		}
		_ = json.Unmarshal(data, &upload)
		data = upload.File
	}
	var file struct {
		Name    string          `json:"name"`
		URI     string          `json:"uri"`
		Display string          `json:"displayName"`
		Created time.Time       `json:"createTime"`
		Size    json.Number     `json:"sizeBytes"`
		State   string          `json:"state"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &file) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini file response")
		return
	}
	id := geminiFileID(file.Name)
	if id == "" {
		id = geminiFileID(file.URI)
	}
	if !validResourceSegment(id) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Gemini returned an invalid file ID")
		return
	}
	status := "uploaded"
	var details any
	if !created {
		switch file.State {
		case "ACTIVE":
			status = "processed"
		case "FAILED":
			status = "error"
			details = string(file.Error)
		}
	}
	var timestamp int64
	if !file.Created.IsZero() {
		timestamp = file.Created.Unix()
	}
	size, _ := file.Size.Int64()
	writeJSON(w, http.StatusOK, map[string]any{"id": encodeResourceID("files", candidate.DeploymentID, id),
		"bytes": size, "created_at": timestamp, "filename": file.Display, "object": "file", "purpose": "user_data",
		"status": status, "status_details": details})
}
