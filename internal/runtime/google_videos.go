package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) googleVideoResource(w http.ResponseWriter, r *http.Request, candidate Candidate, target string, input requestPayload) {
	modelURL, err := url.Parse(strings.TrimSuffix(geminiTarget(candidate, "gemini-generate"), ":generateContent"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Veo provider address")
		return
	}
	result := map[string]any{"object": "video", "status": "processing"}
	create := target == videoPath && r.Method == http.MethodPost
	download := strings.HasSuffix(target, "/content")
	method := http.MethodGet
	var address string
	var body []byte
	if create {
		payload, err := veoVideoRequest(input, candidate.Vertex != nil)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		address = modelURL.String() + ":predictLongRunning"
		method = http.MethodPost
		body, _ = json.Marshal(payload)
		result["model"] = candidate.ModelID
		parameters, _ := payload["parameters"].(map[string]any)
		duration := parameters["durationSeconds"]
		if duration == nil {
			duration = 8
		}
		usage := map[string]any{"duration_seconds": duration}
		if res, _ := parameters["resolution"].(string); res != "" {
			usage["video_resolution"] = strings.ToLower(res)
		}
		result["usage"] = usage
	} else {
		if r.Method != http.MethodGet || target == videoPath || strings.HasPrefix(target, videoPath+"/characters/") {
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Veo supports creating, polling and downloading videos")
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(target, videoPath+"/"), "/content")
		operation, err := base64.RawURLEncoding.DecodeString(id)
		endpoint, ok := veoOperationURL(modelURL, string(operation), candidate.Vertex != nil)
		if err != nil || !ok {
			writeError(w, http.StatusNotFound, "video_not_found", "Invalid Veo operation ID")
			return
		}
		address = endpoint
		if candidate.Vertex != nil {
			method = http.MethodPost
			body, _ = json.Marshal(map[string]string{"operationName": string(operation)})
		}
	}
	upstream, err := newGoogleResourceRequest(r, candidate, address, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not prepare the Veo request")
		return
	}
	upstream.Method = method
	if body != nil {
		upstream.Header.Set("Content-Type", "application/json")
	}
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	vertexPoll := !create && candidate.Vertex != nil
	if vertexPoll {
		select {
		case s.veoReads <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		defer func() { <-s.veoReads }()
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach Veo")
		return
	}
	defer response.Body.Close()
	limit := s.config.MaxBodyBytes
	if vertexPoll {
		limit = max(limit, 64<<20)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read the Veo operation")
		return
	}
	if response.StatusCode/100 != 2 {
		writeRawResponse(w, response.StatusCode, response.Header, raw)
		return
	}
	var operation veoOperation
	if json.Unmarshal(raw, &operation) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Veo returned an invalid operation")
		return
	}
	if _, ok := veoOperationURL(modelURL, operation.Name, candidate.Vertex != nil); !ok {
		writeError(w, http.StatusBadGateway, "upstream_error", "Veo returned an invalid operation name")
		return
	}
	if download {
		serveVeoContent(w, r, &client, candidate, modelURL, operation)
		return
	}
	if operation.Error != nil && string(operation.Error) != "null" {
		result["error"] = operation.Error
		result["status"] = "failed"
	} else if operation.Done {
		result["status"] = "completed"
	}
	if !create && candidate.Vertex != nil {
		result["model"] = candidate.ModelID
		created := operation.Metadata.Created
		if created.IsZero() {
			created = time.Now()
		}
		result["created_at"] = created.Unix()
	}
	result["id"] = encodeResourceID("videos", candidate.DeploymentID, base64.RawURLEncoding.EncodeToString([]byte(operation.Name)))
	writeJSON(w, http.StatusOK, result)
}

type veoOperation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Error    json.RawMessage `json:"error"`
	Metadata struct {
		Created time.Time `json:"createTime"`
	} `json:"metadata"`
	Response struct {
		Videos []struct {
			Data string `json:"bytesBase64Encoded"`
			MIME string `json:"mimeType"`
			GCS string `json:"gcsUri"`
		} `json:"videos"`
		Gemini struct {
			Samples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

func veoOperationURL(modelURL *url.URL, name string, vertex bool) (string, bool) {
	fullModel := strings.TrimPrefix(modelURL.Path, "/")
	if vertex {
		_, fullModel, _ = strings.Cut("/"+fullModel, "/v1/")
		return modelURL.String() + ":fetchPredictOperation", validResourceSegment(strings.TrimPrefix(name, fullModel+"/operations/")) &&
			strings.HasPrefix(name, fullModel+"/operations/")
	}
	var model string
	_, model, _ = strings.Cut("/"+fullModel, "/v1beta/")
	prefix, id, ok := strings.Cut(name, "operations/")
	if !ok || !validResourceSegment(id) || (prefix != "" && prefix != model+"/") {
		return "", false
	}
	endpoint := *modelURL
	base, _, _ := strings.Cut(modelURL.Path, "/v1beta/")
	endpoint.Path = base + "/v1beta/" + prefix + "operations/" + id
	endpoint.RawPath = ""
	return endpoint.String(), true
}

func veoVideoRequest(input requestPayload, vertex bool) (map[string]any, error) {
	fields := clone(input.fields)
	var image any
	for _, part := range input.form {
		switch part.name {
		case "prompt", "size", "seconds", "durationSeconds", "resolution", "aspectRatio":
			fields[part.name] = string(part.body)
		case "input_reference", "image":
			mime := part.header.Get("Content-Type")
			if mime == "" || mime == "application/octet-stream" {
				mime = http.DetectContentType(part.body)
			}
			image = map[string]any{"bytesBase64Encoded": base64.StdEncoding.EncodeToString(part.body), "mimeType": mime}
		}
	}
	prompt, _ := fields["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("Veo requires a prompt")
	}
	if image == nil {
		image = fields["image"]
		if image == nil {
			image = fields["input_reference"]
		}
		if raw, ok := image.(string); ok {
			if vertex && strings.HasPrefix(raw, "gs://") {
				image = map[string]string{"gcsUri": raw}
			} else {
				prefix, data, valid := strings.Cut(raw, ",")
				if !valid || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
					return nil, errors.New("Veo requires an uploaded image or image data URI")
				}
				image = map[string]string{"bytesBase64Encoded": data, "mimeType": strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64")}
			}
		}
	}
	instance := map[string]any{"prompt": prompt}
	parameters := make(map[string]any)
	if nested, ok := fields["parameters"].(map[string]any); ok {
		for key, value := range nested {
			parameters[key] = value
		}
	}
	for _, field := range []string{"aspectRatio", "resolution", "personGeneration", "negativePrompt", "enhancePrompt", "generateAudio", "sampleCount", "seed", "storageUri"} {
		if value := fields[field]; value != nil {
			parameters[field] = value
		}
	}
	if size, _ := fields["size"].(string); size != "" {
		left, right, ok := strings.Cut(size, "x")
		width, first := strconv.Atoi(left)
		height, second := strconv.Atoi(right)
		if !ok || first != nil || second != nil || width < 1 || height < 1 {
			return nil, errors.New("Veo size must be width x height")
		}
		parameters["aspectRatio"] = "16:9"
		switch size {
		case "720x1280", "1080x1920":
			parameters["aspectRatio"] = "9:16"
		}
		if !vertex && parameters["resolution"] == nil {
			switch size {
			case "1280x720", "720x1280":
				parameters["resolution"] = "720p"
			case "1920x1080", "1080x1920":
				parameters["resolution"] = "1080p"
			}
		}
	}
	length := fields["seconds"]
	if value := fields["durationSeconds"]; value != nil {
		length = value
	}
	if length != nil {
		var duration int
		switch value := length.(type) {
		case float64:
			if value != float64(int(value)) {
				return nil, errors.New("Veo duration must be a whole number")
			}
			duration = int(value)
		case string:
			duration, _ = strconv.Atoi(value)
		}
		if duration < 1 {
			return nil, errors.New("Veo duration must be positive")
		}
		parameters["durationSeconds"] = duration
	}
	if image != nil {
		if vertex {
			instance["image"] = image
		} else {
			parameters["image"] = image
		}
	}
	payload := map[string]any{"instances": []map[string]any{instance}}
	if !vertex || len(parameters) > 0 {
		payload["parameters"] = parameters
	}
	return payload, nil
}

func serveVeoContent(w http.ResponseWriter, r *http.Request, client *http.Client, candidate Candidate, modelURL *url.URL, operation veoOperation) {
	if !operation.Done || (operation.Error != nil && string(operation.Error) != "null") {
		writeError(w, http.StatusConflict, "video_not_ready", "Veo video generation has not completed")
		return
	}
	if videos := operation.Response.Videos; candidate.Vertex != nil && len(videos) > 0 {
		if videos[0].Data != "" {
			media := videos[0].MIME
			if media == "" {
				media = "video/mp4"
			}
			w.Header().Set("Content-Type", media)
			_, _ = io.Copy(w, base64.NewDecoder(base64.StdEncoding, strings.NewReader(videos[0].Data)))
			return
		}
		if videos[0].GCS != "" {
			target, ok := veoGCSDownload(candidate, videos[0].GCS)
			if !ok {
				writeError(w, http.StatusBadGateway, "upstream_error", "Veo video storage is outside the configured bucket or prefix")
				return
			}
			downloadVeoContent(w, r, client, candidate, target)
			return
		}
	}
	samples := operation.Response.Gemini.Samples
	if len(samples) == 0 {
		writeError(w, http.StatusNotFound, "video_not_found", "Veo did not return a downloadable video")
		return
	}
	target, err := url.Parse(samples[0].Video.URI)
	if err != nil || target.Fragment != "" || target.User != nil || target.Scheme != modelURL.Scheme || !strings.EqualFold(target.Host, modelURL.Host) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Video download URL must use the configured Veo origin")
		return
	}
	downloadVeoContent(w, r, client, candidate, target.String())
}

func veoGCSDownload(candidate Candidate, value string) (string, bool) {
	uri, err := url.Parse(value)
	bucket, prefix, _ := strings.Cut(candidate.Vertex.Bucket, "/")
	if err != nil || uri.Scheme != "gs" || uri.Host != bucket || bucket == "" || uri.User != nil ||
		uri.RawQuery != "" || uri.Fragment != "" {
		return "", false
	}
	key := strings.TrimPrefix(uri.Path, "/")
	if prefix = strings.Trim(prefix, "/"); prefix != "" && !strings.HasPrefix(key, prefix+"/") {
		return "", false
	}
	for _, component := range strings.Split(key, "/") {
		if !validResourceSegment(component) {
			return "", false
		}
	}
	return vertexStorageBase(candidate) + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(key) + "?alt=media", true
}

func downloadVeoContent(w http.ResponseWriter, r *http.Request, client *http.Client, candidate Candidate, target string) {
	request, err := newGoogleResourceRequest(r, candidate, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not prepare the video download")
		return
	}
	if value := r.Header.Get("Range"); value != "" {
		request.Header.Set("Range", value)
	}
	response, err := client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not download the Veo video")
		return
	}
	defer response.Body.Close()
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
