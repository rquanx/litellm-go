package runtime

import (
	"math"
	"net/http/httptest"
	"strings"
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

func TestAnthropicStreamPreservesInputAccountingAcrossDeltas(t *testing.T) {
	recorder := &callbackResponse{ResponseWriter: httptest.NewRecorder(), event: InferenceEvent{Stream: true}}
	recorder.recordJSON([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"output_tokens":1}}}`))
	recorder.recordJSON([]byte(`{"type":"message_delta","usage":{"output_tokens":3}}`))
	if recorder.event.Prompt != 20 || recorder.event.Completion != 3 || recorder.event.Total != 23 ||
		recorder.cachedTokens != 10 || recorder.cacheCreationTokens != 5 {
		t.Fatalf("unexpected stream accounting: %+v", recorder.event)
	}
}

func TestCallbackAccountsForNativeGeminiUsage(t *testing.T) {
	recorder := &callbackResponse{ResponseWriter: httptest.NewRecorder()}
	recorder.recordJSON([]byte(`{"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":17}}`))
	if recorder.event.Prompt != 12 || recorder.event.Completion != 5 || recorder.event.Total != 17 || recorder.cachedTokens != 7 {
		t.Fatalf("unexpected Gemini accounting: %+v", recorder.event)
	}
}

func TestSpendStreamCaptureDoesNotChangeCallbackPayload(t *testing.T) {
	recorder := &callbackResponse{ResponseWriter: httptest.NewRecorder(),
		event: newInferenceEvent("chat-completions", "fixture", true), capture: true, captureSpendStream: true}
	recorder.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"fixture\"}}]}\n\ndata: [DONE]\n\n"))
	recorder.finish()
	if got := string(recorder.event.SpendStreamResponse); got != `[{"choices":[{"delta":{"content":"fixture"}}]}]` {
		t.Fatalf("unexpected spend stream capture: %s", got)
	}
	if recorder.event.Response != nil {
		t.Fatalf("unexpected change to callback response: %s", recorder.event.Response)
	}

	oversized := &callbackResponse{ResponseWriter: httptest.NewRecorder(),
		event: newInferenceEvent("chat-completions", "fixture", true), captureSpendStream: true}
	oversized.Write([]byte("data: {\"content\":\"" + strings.Repeat("a", maxCallbackMessageBytes) + "\"}\n\n"))
	oversized.Write([]byte("data: {\"content\":\"last\"}\n\n"))
	oversized.finish()
	if oversized.event.SpendStreamResponse != nil {
		t.Fatal("oversized stream was partially captured")
	}
}
