package proxyconfig

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type realtimePriceRecord struct {
	Input *float64 `json:"input_cost_per_audio_token"`
	Output *float64 `json:"output_cost_per_audio_token"`
	CachedInput *float64 `json:"cache_read_input_audio_token_cost"`
}

var realtimePriceCatalog = sync.OnceValue(func() map[string]realtimePriceRecord {
	reader, err := gzip.NewReader(bytes.NewReader(publicPrices))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var records map[string]realtimePriceRecord
	if json.NewDecoder(reader).Decode(&records) != nil {
		return nil
	}
	realtime := make(map[string]realtimePriceRecord)
	for model, price := range records {
		if strings.Contains(model, "realtime") && (price.Input != nil || price.Output != nil) {
			realtime[model] = price
		}
	}
	return realtime
})

func realtimeAudioPrices(model string, input, output, cached *float64) (runtime.RealtimeAudioPrices, error) {
	var catalog realtimePriceRecord
	if strings.Contains(model, "realtime") {
		catalog = realtimePriceCatalog()[model]
		if catalog.Input == nil && catalog.Output == nil {
			catalog = realtimePriceCatalog()[strings.TrimPrefix(model, "openai/")]
		}
	}
	prices := []*float64{catalog.Input, catalog.Output, catalog.CachedInput}
	for index, override := range []*float64{input, output, cached} {
		if override != nil {
			prices[index] = override
		}
		if price := prices[index]; price != nil && (*price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0)) {
			return runtime.RealtimeAudioPrices{}, errors.New("audio token prices must be finite and non-negative")
		}
	}
	return runtime.RealtimeAudioPrices{Input: prices[0], Output: prices[1], CachedInput: prices[2]}, nil
}
