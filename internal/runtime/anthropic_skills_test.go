package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicSkillsMultipartAndDeploymentAffinity(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("beta") != "true" || r.Header.Get("X-Api-Key") != "fixture" ||
			r.Header.Get("Anthropic-Beta") != "skills-2025-10-02" {
			t.Error("Anthropic skill headers or query missing")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/skills":
			if err := r.ParseMultipartForm(4096); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("display_title") != "Example" || r.FormValue("model") != "" || len(r.MultipartForm.File["files[]"]) != 1 {
				t.Error("incorrect skill form")
			}
			fmt.Fprint(w, `{"id":"skill-demo","display_title":"Example","type":"skill"}`)
		case "GET /v1/skills":
			if r.URL.Query().Get("source") != "custom" {
				t.Error("source selector lost")
			}
			fmt.Fprint(w, `{"data":[{"id":"skill-demo","display_title":"Example","type":"skill"}],"next_page":"pg_1"}`)
		case "GET /v1/skills/skill-demo", "DELETE /v1/skills/skill-demo":
			fmt.Fprint(w, `{"id":"skill-demo","type":"skill"}`)
		default:
			t.Errorf("unexpected skill path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fixture.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master", Routes: []Route{{Alias: "demo", Candidates: []Candidate{{
		Provider: "anthropic", APIBase: fixture.URL + "/v1", APIKey: "fixture", Endpoint: "anthropic-messages", DeploymentID: "deployment",
	}}}}})
	call := func(r *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s returned %d: %s", r.Method, r.URL.Path, w.Code, w.Body)
		}
		return w
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("display_title", "Example"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("files[]", "fixture.zip")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(file, "fixture")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest("POST", anthropicSkillPath, &body)
	post.Header.Set("Content-Type", writer.FormDataContentType())
	var output struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call(post).Body.Bytes(), &output); err != nil ||
		output.ID != encodeResourceID(anthropicSkillFamily, "deployment", "skill-demo") {
		t.Fatal("deployment-affine skill ID missing")
	}
	list := call(httptest.NewRequest("GET", anthropicSkillPath+"?source=custom", nil))
	if !strings.Contains(list.Body.String(), encodeResourceID(anthropicSkillFamily+"_page", "deployment", "pg_1")) {
		t.Fatal(list.Body)
	}
	call(httptest.NewRequest("GET", anthropicSkillPath+"/"+output.ID, nil))
	call(httptest.NewRequest("DELETE", anthropicSkillPath+"/"+output.ID, nil))
}
