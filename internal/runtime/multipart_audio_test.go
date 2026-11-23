package runtime

import "testing"

func TestMultipartTranscriptionNormalizesOVHAndScaleway(t *testing.T) {
	for _, fixture := range []struct{ provider, media, input, want string }{
		{"ovhcloud", "application/json", `{"transcript":"fixture","seconds":3}`, `{"text":"fixture","usage":null}`},
		{"scaleway", "text/plain", "fixture", `{"text":"fixture","usage":null}`},
		{"scaleway", "application/json", `{"text":"fixture","language":"en","segments":[]}`, `{"language":"en","segments":[],"text":"fixture","usage":null}`},
	} {
		got, err := multipartAudioResponse([]byte(fixture.input), fixture.media, fixture.provider)
		if err != nil || string(got) != fixture.want {
			t.Errorf("%s response = %q, err = %v", fixture.provider, got, err)
		}
	}
}