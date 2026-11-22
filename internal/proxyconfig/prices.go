package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

//go:embed model-token-prices.json
var priceSnapshot []byte

//go:embed model-cache-prices.json
var cachePriceSnapshot []byte

var modelCachePrices = func() map[string]float64 {
	var prices map[string]float64
	if err := json.Unmarshal(cachePriceSnapshot, &prices); err != nil {
		panic("invalid embedded LiteLLM cache prices")
	}
	return prices
}()

func cacheTokenPrice(model string, override *float64) (*float64, error) {
	price, exists := modelCachePrices[model]
	if !exists && strings.HasPrefix(model, "openai/") {
		price, exists = modelCachePrices[strings.TrimPrefix(model, "openai/")]
	}
	if override != nil {
		price, exists = *override, true
	}
	if !exists {
		return nil, nil
	}
	if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return nil, errors.New("cache token price must be finite and non-negative")
	}
	return &price, nil
}

var modelTokenPrices = func() map[string][2]*float64 {
	var prices map[string][2]*float64
	if err := json.Unmarshal(priceSnapshot, &prices); err != nil {
		panic("invalid embedded LiteLLM token prices")
	}
	return prices
}()

func tokenPrice(model string, input, output *float64) (float64, [2]*float64, error) {
	prices, ok := modelTokenPrices[model]
	if !ok && strings.HasPrefix(model, "openai/") {
		prices = modelTokenPrices[strings.TrimPrefix(model, "openai/")]
	}
	var total float64
	for index, override := range []*float64{input, output} {
		price := prices[index]
		if override != nil {
			price = override
		}
		prices[index] = price
		if price == nil {
			total += 5
			continue
		}
		if *price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0) {
			return 0, prices, errors.New("token prices must be finite and non-negative")
		}
		total += *price
	}
	if math.IsInf(total, 0) {
		return 0, prices, errors.New("combined token price is too large")
	}
	return total, prices, nil
}
