package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func isDocumentIntelligence(candidate Candidate) bool {
	return candidate.Provider == "azure_ai" &&
		(strings.Contains(candidate.ModelID, "doc-intelligence") || strings.Contains(candidate.ModelID, "documentintelligence"))
}

var azureOCRPages = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

func azureDocumentOCRPages(raw any) (string, error) {
	switch pages := raw.(type) {
	case nil:
		return "", nil
	case string:
		pages = strings.Join(strings.Fields(pages), "")
		if azureOCRPages.MatchString(pages) {
			return pages, nil
		}
	case []any:
		if len(pages) == 0 {
			return "", nil
		}
		if _, numeric := pages[0].(float64); numeric {
			indices := make([]int, 0, len(pages))
			for _, raw := range pages {
				index, ok := raw.(float64)
				if !ok || index < 0 || index >= float64(math.MaxInt32) || index != math.Trunc(index) {
					return "", errors.New("invalid Azure OCR page index")
				}
				indices = append(indices, int(index)+1)
			}
			slices.Sort(indices)
			indices = slices.Compact(indices)
			parts := make([]string, len(indices))
			for i, index := range indices {
				parts[i] = strconv.Itoa(index)
			}
			return strings.Join(parts, ","), nil
		}
		parts := make([]string, 0, len(pages))
		for _, raw := range pages {
			page, ok := raw.(string)
			if !ok {
				return "", errors.New("invalid Azure OCR page range")
			}
			parts = append(parts, strings.TrimSpace(page))
		}
		if joined := strings.Join(parts, ","); azureOCRPages.MatchString(joined) {
			return joined, nil
		}
	}
	return "", errors.New("invalid Azure OCR pages")
}

func azureDocumentOCRTarget(candidate Candidate, rawPages any) (string, error) {
	model := candidate.ModelID[strings.LastIndex(candidate.ModelID, "/")+1:]
	if !validResourceSegment(model) {
		return "", errors.New("invalid Azure Document Intelligence model")
	}
	pages, err := azureDocumentOCRPages(rawPages)
	if err != nil {
		return "", err
	}
	query := url.Values{"api-version": {"2024-11-30"}}
	if pages != "" {
		query.Set("pages", pages)
	}
	return strings.TrimRight(candidate.APIBase, "/") + "/documentintelligence/documentModels/" +
		url.PathEscape(model) + ":analyze?" + query.Encode(), nil
}

func azureDocumentOCRRequest(payload map[string]any) map[string]any {
	document, _ := payload["document"].(map[string]any)
	kind, _ := document["type"].(string)
	source, _ := document[kind].(string)
	if strings.HasPrefix(source, "data:") {
		if meta, encoded, ok := strings.Cut(source, ","); ok && strings.HasSuffix(meta, ";base64") {
			return map[string]any{"base64Source": encoded}
		}
	}
	return map[string]any{"urlSource": source}
}

func (s *Server) callAzureDocumentOCR(request *http.Request) (*http.Response, error) {
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusAccepted {
		return response, err
	}
	response.Body.Close()
	location, err := url.Parse(response.Header.Get("Operation-Location"))
	if err != nil || location.User != nil || location.Fragment != "" || !strings.EqualFold(location.Scheme, request.URL.Scheme) ||
		!strings.EqualFold(location.Host, request.URL.Host) {
		return nil, errors.New("Azure OCR returned an invalid operation URL")
	}
	ctx, cancel := context.WithTimeout(request.Context(), 120*time.Second)
	defer cancel()
	for {
		poll, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
		if err != nil {
			return nil, err
		}
		poll.Header.Set("Ocp-Apim-Subscription-Key", request.Header.Get("Ocp-Apim-Subscription-Key"))
		next, err := client.Do(poll)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(next.Body, s.config.MaxBodyBytes+1))
		next.Body.Close()
		if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
			return nil, errors.New("could not read Azure OCR operation")
		}
		next.Body = io.NopCloser(bytes.NewReader(body))
		if next.StatusCode/100 != 2 {
			return next, nil
		}
		var state struct{ Status string `json:"status"` }
		if json.Unmarshal(body, &state) != nil {
			return nil, errors.New("invalid Azure OCR operation response")
		}
		switch state.Status {
		case "succeeded":
			return next, nil
		case "running", "notStarted":
		default:
			return nil, errors.New("Azure OCR analysis failed")
		}
		delay, err := strconv.Atoi(next.Header.Get("Retry-After"))
		if err != nil || delay < 0 {
			delay = 1
		}
		timer := time.NewTimer(time.Duration(min(delay, 120)) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func azureDocumentOCRResponse(body []byte, alias string) []byte {
	var response struct {
		Result struct {
			Pages []struct {
				Number int     `json:"pageNumber"`
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
				Unit   string  `json:"unit"`
				Lines  []struct{ Content string `json:"content"` } `json:"lines"`
			} `json:"pages"`
		} `json:"analyzeResult"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	dpi, err := strconv.Atoi(os.Getenv("AZURE_DOCUMENT_INTELLIGENCE_DEFAULT_DPI"))
	if err != nil || dpi <= 0 {
		dpi = 96
	}
	pages := make([]map[string]any, 0, len(response.Result.Pages))
	for _, page := range response.Result.Pages {
		lines := make([]string, len(page.Lines))
		for i, line := range page.Lines {
			lines[i] = line.Content
		}
		scale := 1.0
		if page.Unit == "inch" {
			scale = float64(dpi)
		}
		pages = append(pages, map[string]any{
			"index": max(0, page.Number-1), "markdown": strings.Join(lines, "\n"),
			"dimensions": map[string]int{"width": int(page.Width * scale), "height": int(page.Height * scale), "dpi": dpi},
		})
	}
	result, _ := json.Marshal(map[string]any{"pages": pages, "usage_info": map[string]int{"pages_processed": len(pages)}})
	return mistralOCRResponse(result, alias)
}
