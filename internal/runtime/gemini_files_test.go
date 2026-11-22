package runtime

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGeminiFileUploadAndResourceOperations(t *testing.T) {
	const info = `{"name":"files/demo-file","uri":"https://generativelanguage.googleapis.com/v1beta/files/demo-file","displayName":"fixture.txt","sizeBytes":"12","createTime":"2026-01-02T00:00:00.001Z","state":"ACTIVE"}`
	var uploads atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/upload/session" && r.Header.Get("x-goog-api-key") != "fixture" {
			t.Error("Gemini credential missing")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /upload/v1beta/files":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), `"display_name":"fixture.txt"`) || r.Header.Get("X-Goog-Upload-Protocol") != "resumable" ||
				r.Header.Get("X-Goog-Upload-Header-Content-Length") != "12" {
				t.Errorf("wrong Gemini upload start request: %s %v", data, r.Header)
			}
			target := upstream.URL + "/upload/session"
			if uploads.Load() > 0 {
				target = "http://unrelated.invalid/upload/session"
			}
			w.Header().Set("X-Goog-Upload-URL", target)
			fmt.Fprint(w, "{}")
		case "POST /upload/session":
			uploads.Add(1)
			data, _ := io.ReadAll(r.Body)
			if string(data) != "fixture-body" || r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" ||
				r.Header.Get("x-goog-api-key") != "" || r.Header.Get("Authorization") != "" {
				t.Errorf("wrong Gemini finalize request: %s %v", data, r.Header)
			}
			fmt.Fprint(w, `{"file":`+info+`}`)
		case "GET /v1beta/files/demo-file":
			fmt.Fprint(w, info)
		case "DELETE /v1beta/files/demo-file":
			fmt.Fprint(w, "{}")
		default:
			t.Errorf("unexpected Gemini file route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL + "/v1beta", APIKey: "fixture",
			Provider: "gemini", Endpoint: "gemini-generate", DeploymentID: "gemini-deployment"}}}},
	})
	send := func(request *http.Request, status int) string {
		t.Helper()
		request.Header.Set("Authorization", "Bearer master")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s returned %d: %s", request.Method, request.URL.Path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	upload := func(status int) string {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("model", "demo"); err != nil {
			t.Fatal(err)
		}
		if err := form.WriteField("custom_llm_provider", "gemini"); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "fixture.txt")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(file, "fixture-body")
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/files", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		return send(r, status)
	}
	id := encodeResourceID("files", "gemini-deployment", "demo-file")
	if data := upload(200); !strings.Contains(data, id) || !strings.Contains(data, `"status":"uploaded"`) {
		t.Fatal(data)
	}
	upload(502)
	path := "/v1/files/" + id
	if data := send(httptest.NewRequest("GET", path, nil), 200); !strings.Contains(data, `"status":"processed"`) {
		t.Fatal(data)
	}
	if data := send(httptest.NewRequest("DELETE", path, nil), 200); !strings.Contains(data, `"deleted":true`) {
		t.Fatal(data)
	}
}
