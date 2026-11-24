package runtime

type RealtimeAudioPrices struct {
	Input *float64
	Output *float64
	CachedInput *float64
}

type realtimeTokenDetails struct {
	Audio int `json:"audio_tokens"`
	Cached int `json:"cached_tokens"`
	CachedDetails struct {
		Audio int `json:"audio_tokens"`
	} `json:"cached_tokens_details"`
}

func realtimeResponseCost(event InferenceEvent, input, output realtimeTokenDetails, candidate Candidate) *float64 {
	inputAudio := min(event.Prompt, max(0, input.Audio))
	outputAudio := min(event.Completion, max(0, output.Audio))
	if inputAudio == 0 && outputAudio == 0 {
		return event.Spend
	}
	prices := candidate.RealtimeAudio
	cachedAudio := min(inputAudio, max(0, input.CachedDetails.Audio))
	cachedText := min(event.Prompt-inputAudio, max(0, input.Cached-cachedAudio))
	cachedAudioPrice, cachedTextPrice := prices.CachedInput, candidate.CacheReadPrice
	if cachedAudioPrice == nil {
		cachedAudioPrice = prices.Input
	}
	if cachedTextPrice == nil {
		cachedTextPrice = candidate.UsagePrice[0]
	}
	var cost float64
	for _, component := range []struct {
		tokens int
		price *float64
	}{
		{inputAudio-cachedAudio, prices.Input},
		{cachedAudio, cachedAudioPrice},
		{event.Prompt-inputAudio-cachedText, candidate.UsagePrice[0]},
		{cachedText, cachedTextPrice},
		{outputAudio, prices.Output},
		{event.Completion-outputAudio, candidate.UsagePrice[1]},
	} {
		if component.tokens > 0 {
			if component.price == nil {
				return nil
			}
			cost += float64(component.tokens)*(*component.price)
		}
	}
	return &cost
}
