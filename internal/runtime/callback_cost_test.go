package runtime

import (
	"math"
	"net/http/httptest"
	"testing"
)

func TestCallbackPricesOnlyUncachedInputAtFullPrice(t *testing.T) {
	input, output, cache := 0.01, 0.03, 0.002
	recorder := &callbackResponse{ResponseWriter: httptest.NewRecorder(), price: [2]*float64{&input, &output}, cachePrice: &cache}
	recorder.recordJSON([]byte(`{"response":{"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":15},"output_tokens":3}}}`))
	recorder.finish()
	if recorder.event.Spend == nil || math.Abs(*recorder.event.Spend-0.17) > 1e-12 || recorder.event.Total != 23 {
		t.Fatalf("unexpected callback spend and usage: %+v", recorder.event)
	}
}

func TestCallbackPricesNativeCacheCreation(t *testing.T) {
	input, output, cache, creation := 0.01, 0.03, 0.002, 0.02
	recorder := &callbackResponse{ResponseWriter: httptest.NewRecorder(), price: [2]*float64{&input, &output},
		cachePrice: &cache, cacheCreationPrice: &creation}
	recorder.recordJSON([]byte(`{"usage":{"input_tokens":5,"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"output_tokens":3}}`))
	recorder.finish()
	if recorder.event.Spend == nil || math.Abs(*recorder.event.Spend-0.26) > 1e-12 || recorder.event.Total != 23 {
		t.Fatalf("unexpected callback spend and usage: %+v", recorder.event)
	}
}
