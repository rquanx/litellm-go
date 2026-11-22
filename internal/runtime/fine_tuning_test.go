package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFineTuningJobUsesSelectedDeployment(t *testing.T) {
	for _, provider := range []string{"openai", "azure"} {
		t.Run(provider, func(t *testing.T) {
			runFineTuningFixture(t, provider)
		})
	}
}

func runFineTuningFixture(t *testing.T, provider string) {
	base := "/v1/fine_tuning/jobs"
	if provider == "azure" {
		base = "/openai/fine_tuning/jobs"
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch provider {
		case "openai":
			if r.Header.Get("Authorization") != "Bearer fixture" {
				t.Error("missing OpenAI credential")
			}
		case "azure":
			if r.Header.Get("api-key") != "fixture" || r.URL.Query().Get("api-version") != "2024-10-21" {
				t.Error("missing Azure credential or version")
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "POST " + base:
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != "upstream-model" ||
				request["training_file"] != "file-train" || request["validation_file"] != "file-validate" ||
				request["custom_llm_provider"] != nil {
				t.Errorf("unexpected fine-tuning payload: %v", request)
			}
		case "GET " + base:
			if r.URL.Query().Get("after") != "ftjob-demo" {
				t.Errorf("incorrect fine-tuning cursor: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"object":"list","data":[{"id":"ftjob-demo"}]}`)
			return
		case "GET " + base + "/ftjob-demo", "POST " + base + "/ftjob-demo/cancel":
		default:
			t.Errorf("unexpected fine-tuning path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"id":"ftjob-demo","object":"fine_tuning.job","training_file":"file-train","validation_file":"file-validate","result_files":["file-result"]}`)
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true, Routes: []Route{{
		Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, Provider: provider, APIKey: "fixture",
			APIVersion: "2024-10-21", ModelID: "upstream-model", DeploymentID: "deployment"}},
	}}, AuthorizeKey: func(key string) ([]string, bool) { return []string{"other"}, key == "restricted" }})
	send := func(method, path, key, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("expected %d, got %d: %s", status, w.Code, w.Body)
		}
		return w.Body.String()
	}
	job := encodeResourceID(fineTuningFamily, "deployment", "ftjob-demo")
	train := encodeResourceID("files", "deployment", "file-train")
	validate := encodeResourceID("files", "deployment", "file-validate")
	created := send("POST", "/fine_tuning/jobs", "master", `{"model":"demo","custom_llm_provider":"`+provider+
		`","training_file":"`+train+`","validation_file":"`+validate+`"}`, 200)
	for _, id := range []string{job, train, validate, encodeResourceID("files", "deployment", "file-result")} {
		if !strings.Contains(created, id) {
			t.Fatalf("missing deployment-bound ID %s: %s", id, created)
		}
	}
	send("GET", fineTuningPath+"?after="+job, "master", "", 200)
	send("GET", fineTuningPath+"/"+job, "master", "", 200)
	send("POST", fineTuningPath+"/"+job+"/cancel", "master", "", 200)
	send("GET", fineTuningPath+"/"+job, "restricted", "", 400)
	send("POST", fineTuningPath, "master", `{"model":"demo","training_file":"`+train+`","validation_file":"`+
		encodeResourceID("files", "other", "file-invalid")+`"}`, 400)
}
