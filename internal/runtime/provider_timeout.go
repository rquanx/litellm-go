package runtime

import (
	"math"
	"time"
)

func timeoutOverride(value any) time.Duration {
	seconds, _ := value.(float64)
	if seconds <= 0 || seconds > 86400 || math.IsNaN(seconds) {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func providerRequestTimeout(fields map[string]any, candidate Candidate, stream bool, routerStream time.Duration) time.Duration {
	if stream {
		for _, timeout := range []time.Duration{timeoutOverride(fields["stream_timeout"]), candidate.StreamTimeout, routerStream} {
			if timeout > 0 {
				return timeout
			}
		}
	}
	for _, timeout := range []time.Duration{timeoutOverride(fields["timeout"]), timeoutOverride(fields["request_timeout"]), candidate.Timeout} {
		if timeout > 0 {
			return timeout
		}
	}
	return 0
}
