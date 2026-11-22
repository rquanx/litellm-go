package runtime

import (
	"encoding/json"
	"strings"
)

func searchPayload(provider string, input map[string]json.RawMessage, query string, queries []string) []byte {
	params := make(map[string]json.RawMessage)
	copyParam := func(from, to string) {
		if value := input[from]; len(value) != 0 && string(value) != "null" && params[to] == nil {
			params[to] = value
		}
	}
	if len(queries) != 0 {
		query = strings.Join(queries, " ")
	}
	queryText, _ := json.Marshal(query)
	params["query"] = queryText
	var extras []string
	switch provider {
	case "perplexity":
		params["query"] = input["query"]
		extras = []string{"max_results", "search_domain_filter", "max_tokens_per_page", "country"}
	case "tavily":
		copyParam("search_domain_filter", "include_domains")
		var country string
		if json.Unmarshal(input["country"], &country) == nil && country != "" {
			params["country"], _ = json.Marshal(strings.ToLower(country))
		}
		extras = []string{"max_results", "exclude_domains", "topic", "search_depth", "include_answer", "include_raw_content",
			"include_images", "include_image_descriptions", "include_favicon", "time_range", "start_date", "end_date", "include_domains"}
	case "exa_ai":
		copyParam("max_results", "numResults")
		copyParam("search_domain_filter", "includeDomains")
		copyParam("country", "userLocation")
		extras = []string{"numResults", "includeDomains", "userLocation", "type", "category", "excludeDomains",
			"startCrawlDate", "endCrawlDate", "startPublishedDate", "endPublishedDate", "includeText", "excludeText", "contents"}
	case "linkup":
		delete(params, "query")
		params["q"] = queryText
		copyParam("max_results", "maxResults")
		copyParam("search_domain_filter", "includeDomains")
		extras = []string{"maxResults", "includeDomains", "excludeDomains", "depth", "outputType", "structuredOutputSchema",
			"includeImages", "includeInlineCitations", "fromDate", "toDate"}
	case "firecrawl":
		copyParam("max_results", "limit")
		extras = []string{"limit", "country", "sources", "categories", "tbs", "location", "timeout", "ignoreInvalidURLs", "scrapeOptions"}
	case "serper", "searchapi":
		delete(params, "query")
		copyParam("max_results", "num")
		var country string
		if json.Unmarshal(input["country"], &country) == nil && country != "" {
			params["gl"], _ = json.Marshal(strings.ToLower(country))
		}
		var domains []string
		if json.Unmarshal(input["search_domain_filter"], &domains) == nil && len(domains) != 0 {
			join := ") (site:"
			if provider == "searchapi" {
				join = ") AND (site:"
			}
			query = "(" + query + join + strings.Join(domains, " OR site:") + ")"
		}
		params["q"], _ = json.Marshal(query)
		extras = []string{"gl", "hl", "location", "num", "page", "tbs", "autocorrect"}
		if provider == "searchapi" {
			params["engine"] = json.RawMessage(`"google"`)
			var limit int
			if json.Unmarshal(params["num"], &limit) == nil && limit > 10 {
				params["num"] = json.RawMessage("10")
			}
			extras = []string{"gl", "hl", "location", "num", "page", "kgmid", "device", "uule", "google_domain",
				"lr", "cr", "nfpr", "filter", "safe", "time_period", "time_period_min", "time_period_max", "optimization_strategy"}
		}
	case "parallel_ai":
		delete(params, "query")
		params["objective"] = queryText
		policy := make(map[string]json.RawMessage)
		for source, target := range map[string]string{"search_domain_filter": "allowed_domains", "exclude_domains": "disallowed_domains"} {
			if value := input[source]; len(value) != 0 && string(value) != "null" {
				policy[target] = value
			}
		}
		if len(policy) != 0 {
			params["source_policy"], _ = json.Marshal(policy)
		}
		extras = []string{"max_results", "search_queries", "processor", "max_chars_per_result", "source_policy", "exclude_domains"}
	case "searxng":
		delete(params, "query")
		params["q"] = queryText
		params["format"] = json.RawMessage(`"json"`)
		var country string
		if json.Unmarshal(input["country"], &country) == nil && country != "" {
			language := strings.ToLower(country)
			switch language {
			case "us", "uk":
				language = "en"
			case "jp":
				language = "ja"
			}
			params["language"], _ = json.Marshal(language)
		}
		extras = []string{"categories", "engines", "language", "pageno", "time_range", "safesearch"}
	}
	for _, key := range extras {
		copyParam(key, key)
	}
	switch provider {
	case "exa_ai":
		setSearchDefault(params, "contents", `{"text":true}`)
	case "linkup":
		setSearchDefault(params, "depth", `"standard"`)
		setSearchDefault(params, "outputType", `"searchResults"`)
	case "firecrawl":
		setSearchDefault(params, "scrapeOptions", `{"formats":["markdown"],"onlyMainContent":true}`)
	}
	payload, _ := json.Marshal(params)
	return payload
}

func setSearchDefault(params map[string]json.RawMessage, key, value string) {
	if params[key] == nil {
		params[key] = json.RawMessage(value)
	}
}

type searchResult struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Date        *string `json:"date"`
	LastUpdated *string `json:"last_updated"`
}

type searchProviderResult struct {
	searchResult
	Content       string  `json:"content"`
	Text          string  `json:"text"`
	PublishedDate *string `json:"publishedDate"`
	Name          *string `json:"name"`
	Type          string  `json:"type"`
	Markdown      string  `json:"markdown"`
	Description   string  `json:"description"`
	Link          string  `json:"link"`
	Pubdate       *string `json:"pubdate"`
	Excerpts      []string `json:"excerpts"`
}

func normalizeSearch(provider string, body []byte) ([]searchResult, bool) {
	var payload *struct {
		Results []searchProviderResult `json:"results"`
		Organic []searchProviderResult `json:"organic"`
		OrganicResults []searchProviderResult `json:"organic_results"`
		Data    json.RawMessage        `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, false
	}
	if provider == "firecrawl" {
		return normalizeFirecrawlSearch(payload.Data)
	}
	if provider == "serper" {
		payload.Results = payload.Organic
	}
	if provider == "searchapi" {
		payload.Results = payload.OrganicResults
	}
	results := make([]searchResult, 0, len(payload.Results))
	for _, item := range payload.Results {
		result := item.searchResult
		switch provider {
		case "tavily":
			result.Snippet, result.Date, result.LastUpdated = item.Content, nil, nil
		case "exa_ai":
			result.Snippet, result.Date, result.LastUpdated = item.Text, item.PublishedDate, nil
		case "serper", "searchapi":
			result.URL, result.LastUpdated = item.Link, nil
		case "parallel_ai":
			result.Snippet, result.Date, result.LastUpdated = strings.Join(item.Excerpts, " ... "), nil, nil
		case "searxng":
			result.Snippet, result.Date, result.LastUpdated = item.Content, item.PublishedDate, nil
			if result.Date == nil || *result.Date == "" {
				result.Date = item.Pubdate
			}
		case "linkup":
			if item.Type != "" && item.Type != "text" && item.Type != "image" {
				continue
			}
			result.Title = ""
			if item.Name != nil {
				result.Title = *item.Name
			} else if item.Type == "image" {
				result.Title = item.URL
			}
			result.Snippet, result.Date, result.LastUpdated = item.Content, nil, nil
		}
		results = append(results, result)
	}
	return results, true
}

func normalizeFirecrawlSearch(data json.RawMessage) ([]searchResult, bool) {
	var web []searchProviderResult
	var news []searchProviderResult
	if len(data) != 0 && json.Unmarshal(data, &web) != nil {
		var sections struct {
			Web  []searchProviderResult `json:"web"`
			News []searchProviderResult `json:"news"`
		}
		if json.Unmarshal(data, &sections) != nil {
			return nil, false
		}
		web, news = sections.Web, sections.News
	}
	results := make([]searchResult, 0, len(web)+len(news))
	for _, item := range web {
		result := searchResult{Title: item.Title, URL: item.URL, Snippet: item.Markdown}
		if result.Snippet == "" {
			result.Snippet = item.Description
		}
		results = append(results, result)
	}
	for _, item := range news {
		result := searchResult{Title: item.Title, URL: item.URL, Snippet: item.Markdown, Date: item.Date}
		if result.Snippet == "" {
			result.Snippet = item.Snippet
		}
		results = append(results, result)
	}
	return results, true
}
