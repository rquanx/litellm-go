package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContainerOperationsEnforceOwnership(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Error("upstream credential missing")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/containers":
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["name"] != "fixture" || input["model"] != nil {
				t.Errorf("invalid container request: %+v", input)
			}
			io.WriteString(w, `{"id":"cntr_fixture","object":"container"}`)
		case "GET /v1/containers":
			io.WriteString(w, `{"data":[{"id":"cntr_other"},{"id":"cntr_fixture"}],"first_id":"cntr_other","last_id":"cntr_fixture"}`)
		case "GET /v1/containers/cntr_fixture", "DELETE /v1/containers/cntr_fixture":
			io.WriteString(w, `{"id":"cntr_fixture"}`)
		case "POST /v1/containers/cntr_fixture/files":
			if err := r.ParseMultipartForm(1024); err != nil || r.FormValue("file_id") != "file_fixture" {
				t.Error("container file body was not forwarded")
			}
			io.WriteString(w, `{"id":"cfile_fixture","container_id":"cntr_fixture"}`)
		case "GET /v1/containers/cntr_fixture/files/cfile_fixture/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, "\x00fixture\xff")
		default:
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true, Routes: []Route{{
		Alias: "fixture", Candidates: []Candidate{{Provider: "openai", APIBase: upstream.URL, APIKey: "provider-key", DeploymentID: "fixture"}},
	}}, AuthorizeKey: func(key string) ([]string, bool) { return []string{"fixture"}, key == "a" || key == "b" || key == "same-project" },
		ResourceOwnerScopes: func(key string) []string {
			if key == "a" || key == "same-project" {
				return []string{"project:fixture"}
			}
			return nil
		}})
	send := func(method, path, key, body, contentType string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", contentType)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, result.Code, status, result.Body.String())
		}
		return result.Body.String()
	}
	container := encodeResourceID("containers", "fixture", "cntr_fixture")
	path := "/v1/containers/" + container
	if result := send("POST", "/containers", "a", `{"name":"fixture","model":"fixture"}`, "application/json", 200); !strings.Contains(result, container) {
		t.Fatalf("creation returned %s", result)
	}
	send("GET", path, "b", "", "", 404)
	send("GET", path, "same-project", "", "", 200)
	send("GET", "/v1/containers?after="+container, "b", "", "", 404)
	if result := send("GET", "/v1/containers", "a", "", "", 200); strings.Contains(result, encodeResourceID("containers", "fixture", "cntr_other")) ||
		strings.Count(result, container) != 3 {
		t.Fatalf("list returned %s", result)
	}
	file := encodeResourceID("container_files", "fixture", "cfile_fixture")
	sourceFile := encodeResourceID("files", "fixture", "file_fixture")
	if result := send("POST", path+"/files", "a", "--fixture\r\nContent-Disposition: form-data; name=\"file_id\"\r\n\r\n"+sourceFile+"\r\n--fixture--\r\n",
		"multipart/form-data; boundary=fixture", 200); !strings.Contains(result, file) || !strings.Contains(result, container) {
		t.Fatalf("file upload returned %s", result)
	}
	if result := send("GET", path+"/files/"+file+"/content", "a", "", "", 200); result != "\x00fixture\xff" {
		t.Fatalf("binary file content changed: %q", result)
	}
	send("DELETE", path, "master", "", "", 200)
	send("GET", path, "a", "", "", 404)
}
