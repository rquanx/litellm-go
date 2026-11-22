package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type WatsonXRerankConfig struct {
	Project string
	Space   string
	Token   string
	ZenKey  string
	IAMURL  string

	mu     sync.Mutex
	key    string
	cached string
	expiry time.Time
}

func (config *WatsonXRerankConfig) authorize(request *http.Request, client *http.Client, key string) error {
	switch {
	case config.Token != "":
		request.Header.Set("Authorization", "Bearer "+config.Token)
		return nil
	case config.ZenKey != "":
		request.Header.Set("Authorization", "ZenApiKey "+config.ZenKey)
		return nil
	case key == "":
		return errors.New("watsonx IAM requires an API key")
	}

	config.mu.Lock()
	defer config.mu.Unlock()
	if config.key == key && config.cached != "" && time.Now().Before(config.expiry) {
		request.Header.Set("Authorization", "Bearer "+config.cached)
		return nil
	}
	target := config.IAMURL
	if target == "" {
		target = "https://iam.cloud.ibm.com/identity/token"
	}
	form := url.Values{"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"}, "apikey": {key}}
	tokenRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("invalid watsonx IAM request")
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRequest.Header.Set("Accept", "application/json")
	response, err := client.Do(tokenRequest)
	if err != nil {
		return errors.New("watsonx IAM request failed")
	}
	defer response.Body.Close()
	var result struct {
		Token   string `json:"access_token"`
		Seconds int    `json:"expires_in"`
	}
	if response.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil || result.Token == "" {
		return errors.New("watsonx IAM did not issue a token")
	}
	config.cached = result.Token
	config.key = key
	config.expiry = time.Now().Add(time.Duration(max(0, min(result.Seconds, 86400)-10)) * time.Second)
	request.Header.Set("Authorization", "Bearer "+result.Token)
	return nil
}

func watsonXRerankRequest(payload map[string]any, candidate Candidate) (map[string]any, error) {
	documents, _ := payload["documents"].([]any)
	inputs := make([]any, 0, len(documents))
	for _, doc := range documents {
		if text, ok := doc.(string); ok {
			doc = map[string]string{"text": text}
		}
		inputs = append(inputs, doc)
	}
	request := map[string]any{"query": payload["query"], "inputs": inputs}
	options := map[string]any{}
	if value := payload["top_n"]; value != nil {
		options["top_n"] = value
	}
	if value, ok := payload["return_documents"].(bool); ok {
		options["inputs"] = value
	}
	parameters := map[string]any{}
	if len(options) > 0 {
		parameters["return_options"] = options
	}
	if value := payload["max_tokens_per_doc"]; value != nil {
		parameters["truncate_input_tokens"] = value
	}
	if len(parameters) > 0 {
		request["parameters"] = parameters
	}
	if strings.HasPrefix(candidate.ModelID, "deployment/") {
		return request, nil
	}
	request["model_id"] = candidate.ModelID
	project, _ := payload["project_id"].(string)
	space, _ := payload["space_id"].(string)
	if candidate.WatsonX != nil {
		if project == "" {
			project = candidate.WatsonX.Project
		}
		if space == "" {
			space = candidate.WatsonX.Space
		}
	}
	switch {
	case project != "":
		request["project_id"] = project
	case space != "":
		request["space_id"] = space
	default:
		return nil, errors.New("watsonx requires a project_id or space_id")
	}
	return request, nil
}

func watsonXRerankResponse(body []byte) []byte {
	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model_id"`
		Tokens  int    `json:"input_token_count"`
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
			Input any     `json:"input"`
		} `json:"results"`
	}
	if json.Unmarshal(body, &response) != nil || response.Results == nil {
		return body
	}
	results := make([]map[string]any, 0, len(response.Results))
	for _, item := range response.Results {
		result := map[string]any{"index": item.Index, "relevance_score": item.Score}
		if item.Input != nil {
			if text, ok := item.Input.(string); ok {
				item.Input = map[string]string{"text": text}
			}
			result["document"] = item.Input
		}
		results = append(results, result)
	}
	if response.ID == "" {
		response.ID = response.Model
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	encoded, _ := json.Marshal(map[string]any{"id": response.ID, "results": results, "meta": map[string]any{
		"tokens": map[string]int{"input_tokens": response.Tokens},
	}})
	return encoded
}
