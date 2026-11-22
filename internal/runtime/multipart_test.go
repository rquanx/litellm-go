package runtime

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTranscriptionPreservesMultipartAcrossFallback(t *testing.T) {
	fileData := []byte{0x00, 0xff, 0x1b, 0x02}
	calls := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		actual, err := io.ReadAll(file)
		if err != nil || !bytes.Equal(actual, fileData) || header.Filename != "sample.wav" {
			t.Errorf("multipart file changed: %v, %v", header.Filename, err)
		}
		if r.FormValue("language") != "en" {
			t.Error("transcription language was lost")
		}
		model := r.FormValue("model")
		calls <- model
		if model == "primary" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "fixture transcript")
	}))
	defer upstream.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("model", "transcribe")
	_ = form.WriteField("language", "en")
	file, err := form.CreateFormFile("file", "sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(fileData)
	_ = form.Close()
	server := NewServer(Config{Routes: []Route{{Alias: "transcribe", Candidates: []Candidate{
		{ModelID: "primary", APIBase: upstream.URL, Endpoint: "transcription"},
		{ModelID: "backup", APIBase: upstream.URL, Endpoint: "transcription"},
	}}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	result := httptest.NewRecorder()
	server.ServeHTTP(result, req)
	if result.Code != http.StatusOK || result.Body.String() != "fixture transcript" || len(calls) != 2 {
		t.Fatalf("status=%d body=%q attempts=%d", result.Code, result.Body.String(), len(calls))
	}
	if first, second := <-calls, <-calls; first != "primary" || second != "backup" {
		t.Fatalf("unexpected attempt order: %s, %s", first, second)
	}
}
