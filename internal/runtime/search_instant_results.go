package runtime

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

func normalizeDuckDuckGoSearch(body, request []byte) ([]searchResult, bool) {
	type topic struct {
		FirstURL *string `json:"FirstURL"`
		Text     *string `json:"Text"`
		Topics   []topic `json:"Topics"`
	}
	var payload *struct {
		AbstractURL   string  `json:"AbstractURL"`
		AbstractText  string  `json:"AbstractText"`
		Heading       string  `json:"Heading"`
		RelatedTopics []topic `json:"RelatedTopics"`
	}
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, false
	}
	var params struct {
		MaxResults *int `json:"_max_results"`
	}
	json.Unmarshal(request, &params)
	results := make([]searchResult, 0)
	if payload.AbstractURL != "" && payload.AbstractText != "" {
		results = append(results, searchResult{Title: payload.Heading, URL: payload.AbstractURL, Snippet: payload.AbstractText})
	}
	addTopic := func(item topic) {
		if item.FirstURL == nil || item.Text == nil || (params.MaxResults != nil && len(results) >= *params.MaxResults) {
			return
		}
		title, snippet, split := strings.Cut(*item.Text, " - ")
		if !split {
			snippet = title
			if runes := []rune(title); len(runes) > 50 {
				title = string(runes[:50]) + "..."
			}
		}
		results = append(results, searchResult{Title: title, URL: *item.FirstURL, Snippet: snippet})
	}
	for _, item := range payload.RelatedTopics {
		if item.FirstURL != nil && item.Text != nil {
			addTopic(item)
			continue
		}
		for _, nested := range item.Topics {
			addTopic(nested)
		}
	}
	return results, true
}

func normalizeBraveSearch(body, request []byte) ([]searchResult, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, false
	}
	var params struct {
		Count        *int   `json:"count"`
		ResultFilter string `json:"result_filter"`
	}
	json.Unmarshal(request, &params)
	limit := 20
	if params.Count != nil {
		limit = max(1, min(20, *params.Count))
	}
	allSections := []string{"web", "discussions", "faqs", "faq", "news", "videos"}
	var sections []string
	for _, raw := range strings.Split(params.ResultFilter, ",") {
		section := strings.ToLower(strings.TrimSpace(raw))
		if slices.Contains(allSections, section) {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		sections = allSections
	}
	results := make([]searchResult, 0, limit)
	for _, section := range sections {
		if payload[section] == nil {
			continue
		}
		var group struct {
			Results []struct {
				Title       string          `json:"title"`
				URL         string          `json:"url"`
				Description string          `json:"description"`
				Age         json.RawMessage `json:"age"`
				PageAge     json.RawMessage `json:"page_age"`
				Fetched     json.RawMessage `json:"fetched_content_timestamp"`
			} `json:"results"`
		}
		if json.Unmarshal(payload[section], &group) != nil {
			return nil, false
		}
		for _, item := range group.Results {
			if len(results) == limit {
				return results, true
			}
			date := item.PageAge
			switch string(date) {
			case "", "null", `""`, "0":
				date = item.Age
			}
			results = append(results, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Description,
				Date: braveSearchDate(date), LastUpdated: braveSearchDate(item.Fetched)})
		}
	}
	return results, true
}

func braveSearchDate(raw json.RawMessage) *string {
	var input string
	if json.Unmarshal(raw, &input) != nil {
		input = string(raw)
	}
	input = strings.TrimSpace(input)
	if seconds, err := strconv.ParseFloat(input, 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		if math.Abs(seconds) > 1e11 {
			seconds /= 1000
		}
		if seconds >= -62135596800 && seconds < 253402300800 {
			whole, fractional := math.Modf(seconds)
			date := time.Unix(int64(whole), int64(fractional*1e9)).UTC().Format(time.DateOnly)
			return &date
		}
	}
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly, "2006-1-2", "2006/1/2",
		time.RFC1123, time.RFC1123Z, "Jan 2, 2006", "January 2, 2006", "1/2/2006"} {
		if value, err := time.Parse(layout, input); err == nil {
			date := value.Format(time.DateOnly)
			return &date
		}
	}
	return nil
}
