package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssistantAcceptedRunIsMeteredAfterDisconnect(t *testing.T) {
	pollStarted := make(chan struct{})
	var created, polls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/threads/thread/runs":
			created.Add(1)
			io.WriteString(w, `{"id":"run","status":"queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/threads/thread/runs/run":
			if polls.Add(1) == 1 {
				close(pollStarted)
				<-r.Context().Done()
				return
			}
			io.WriteString(w, `{"id":"run","status":"completed","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	recorded := make(chan InferenceEvent, 4)
	price := 0.25
	candidate := Candidate{Provider: "openai", Endpoint: "chat-completions", RoutingGroup: "demo",
		DeploymentID: "dep", ModelID: "native", APIBase: origin.URL, UsagePrice: [2]*float64{&price, &price}}
	server := NewServer(Config{AuthorizeInference: true, MaxConcurrent: 1,
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}},
		Callback: func(event InferenceEvent) { recorded <- event }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "POST", "/v1/threads/thread/runs?model=demo", strings.NewReader(`{"assistant_id":"asst"}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-pollStarted:
	case <-time.After(2*time.Second):
		t.Fatal("run was not accepted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2*time.Second):
		t.Fatal("client request did not return")
	}
	select {
	case event := <-recorded:
		if event.Total != 5 || event.Spend == nil || *event.Spend != 1.25 || event.Model != "native" ||
			!strings.HasPrefix(event.ID, "assistant-run-") || event.Status != "success" {
			t.Fatalf("unexpected asynchronous accounting: %+v", event)
		}
	case <-time.After(2*time.Second):
		t.Fatal("run completion was not recorded")
	}
	if created.Load() != 1 || polls.Load() != 2 || len(recorded) != 0 {
		t.Fatal("reconciliation repeated the run or emitted multiple events")
	}
}

func TestAssistantInterruptedStreamPollsKnownRun(t *testing.T) {
	var polls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: thread.run.in_progress\ndata: {\"object\":\"thread.run\",\"id\":\"run\",\"status\":\"in_progress\"}\n\n")
		case http.MethodGet:
			if r.Header.Get("Accept") != "application/json" || r.URL.Path != "/v1/threads/thread/runs/run" {
				t.Errorf("unexpected polling request: %s %s", r.URL.Path, r.Header.Get("Accept"))
			}
			polls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"run","status":"completed","usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		}
	}))
	defer origin.Close()
	events := make(chan InferenceEvent, 2)
	server := NewServer(Config{AuthorizeInference: true,
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "openai", Endpoint: "chat-completions",
			RoutingGroup: "demo", DeploymentID: "dep", ModelID: "native", APIBase: origin.URL}}}},
		Callback: func(event InferenceEvent) { events <- event }})
	req := httptest.NewRequest("POST", "/v1/threads/thread/runs?model=demo",
		strings.NewReader(`{"assistant_id":"asst","stream":true}`))
	req.Header.Set("Accept", "text/event-stream")
	server.ServeHTTP(httptest.NewRecorder(), req)
	select {
	case event := <-events:
		if event.Total != 3 || !event.Stream || event.Status != "success" || polls.Load() != 1 || len(events) != 0 {
			t.Fatalf("unexpected streaming run completion: %+v", event)
		}
	case <-time.After(2*time.Second):
		t.Fatal("streaming run completion was not reconciled")
	}
}

func TestAssistantRunPollingHonorsDeploymentTimeout(t *testing.T) {
	var creates atomic.Int32
	polled := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/threads/thread_fixture/runs" {
			creates.Add(1)
			fmt.Fprint(w, `{"id":"run_fixture","status":"queued"}`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/threads/thread_fixture/runs/run_fixture" {
			polled <- struct{}{}
			w.Header().Set("OpenAI-Poll-After-Ms", "5000")
			fmt.Fprint(w, `{"id":"run_fixture","status":"in_progress"}`)
			return
		}
		t.Errorf("unexpected resource: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true,
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "openai", APIBase: origin.URL,
			Endpoint: "chat-completions", ModelID: "native", DeploymentID: "chosen", Timeout: 250 * time.Millisecond}}}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/threads/thread_fixture/runs?model=demo", strings.NewReader(`{"assistant_id":"asst_fixture"}`))
	request.Header.Set("Authorization", "Bearer master")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout || creates.Load() != 1 {
		t.Fatalf("expected a timeout and one run creation, got %d, %d: %s", response.Code, creates.Load(), response.Body)
	}
	select {
	case <-polled:
	default:
		t.Fatal("run status was never polled")
	}
}

func TestAzureAssistantsUseDeploymentAffinity(t *testing.T) {
	for _, name := range []string{"api-key", "entra"} {
		t.Run(name, func(t *testing.T) {
			const deployment = "azure-assistants"
			var polls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("api-version") != "2025-04-01-preview" || r.Header.Get("OpenAI-Beta") != "assistants=v2" {
					t.Errorf("unexpected Azure Assistants request: %s", r.URL)
				}
				if name == "api-key" && (r.Header.Get("api-key") != "fixture-key" || r.Header.Get("Authorization") != "") {
					t.Error("expected only the Azure API key")
				}
				if name == "entra" && (r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("api-key") != "") {
					t.Error("expected only the Azure Entra token")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "POST /openai/assistants":
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"model":"actual-deployment"`) {
						t.Errorf("provider did not receive the deployment name: %s", body)
					}
					fmt.Fprint(w, `{"id":"asst_fixture","object":"assistant"}`)
				case "GET /openai/assistants":
					fmt.Fprint(w, `{"object":"list","data":[{"id":"asst_fixture","object":"assistant"}]}`)
				case "DELETE /openai/assistants/asst_fixture":
					fmt.Fprint(w, `{"id":"asst_fixture","object":"assistant.deleted","deleted":true}`)
				case "POST /openai/threads":
					fmt.Fprint(w, `{"id":"thread_fixture","object":"thread"}`)
				case "GET /openai/threads/thread_fixture":
					fmt.Fprint(w, `{"id":"thread_fixture","object":"thread"}`)
				case "POST /openai/threads/thread_fixture/messages", "GET /openai/threads/thread_fixture/messages":
					fmt.Fprint(w, `{"id":"msg_fixture","object":"thread.message","thread_id":"thread_fixture"}`)
				case "POST /openai/threads/thread_fixture/runs":
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"assistant_id":"asst_fixture"`) {
						t.Errorf("provider did not receive the original assistant ID: %s", body)
					}
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, `{"id":"run_fixture","object":"thread.run","status":"queued"}`)
				case "GET /openai/threads/thread_fixture/runs/run_fixture":
					if r.Header.Get("X-Stainless-Poll-Helper") != "true" {
						t.Error("poll helper header was missing")
					}
					if polls.Add(1) == 1 {
						w.Header().Set("OpenAI-Poll-After-Ms", "1")
						fmt.Fprint(w, `{"id":"run_fixture","object":"thread.run","status":"in_progress"}`)
						return
					}
					fmt.Fprint(w, `{"id":"run_fixture","object":"thread.run","status":"completed","assistant_id":"asst_fixture","thread_id":"thread_fixture","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
				default:
					t.Errorf("unexpected resource: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer origin.Close()
			candidate := Candidate{APIBase: origin.URL + "/openai", APIKey: "fixture-key", Provider: "azure",
				ModelID: "actual-deployment", DeploymentID: deployment, RoutingGroup: "demo", APIVersion: "2025-04-01-preview"}
			if name == "entra" {
				candidate.AzureToken = func(context.Context) (string, error) { return "fixture-token", nil }
			}
			recorded := make(chan InferenceEvent, 1)
			server := NewServer(Config{Token: "master", AuthorizeInference: true,
				Callback: func(event InferenceEvent) { recorded <- event },
				Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
			assistant := encodeResourceID("assistants", deployment, "asst_fixture")
			thread := encodeResourceID("threads", deployment, "thread_fixture")
			message := encodeResourceID("thread_messages", deployment, "msg_fixture")
			threadPath := "/v1/threads/" + thread
			for _, test := range []struct{ method, path, body, expected string }{
				{"POST", "/v1/assistants", `{"model":"demo"}`, assistant},
				{"GET", "/assistants?custom_llm_provider=azure", "", assistant},
				{"POST", "/v1/threads?model=demo", "", thread},
				{"GET", threadPath, "", thread},
				{"POST", threadPath + "/messages", `{"role":"user","content":"fixture"}`, message},
				{"GET", threadPath + "/messages", "", message},
				{"POST", threadPath + "/runs", `{"assistant_id":"` + assistant + `"}`, encodeResourceID("thread_runs", deployment, "run_fixture")},
				{"DELETE", "/v1/assistants/" + assistant, "", `"deleted":true`},
			} {
				request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
				request.Header.Set("Authorization", "Bearer master")
				response := httptest.NewRecorder()
				server.ServeHTTP(response, request)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.expected) {
					t.Fatalf("%s %s returned %d %s", test.method, test.path, response.Code, response.Body)
				}
			}
			select {
			case event := <-recorded:
				if event.Total != 5 || event.Provider != "azure" || event.ModelGroup != "demo" {
					t.Fatalf("unexpected run accounting: %+v", event)
				}
			default:
				t.Fatal("missing Azure run accounting")
			}
			if polls.Load() != 2 {
				t.Fatalf("expected two run polls, got %d", polls.Load())
			}
		})
	}
}

func TestAssistantResourceAffinityAndStreaming(t *testing.T) {
	var calls int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("OpenAI-Beta") != "assistants=v2" || r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Errorf("wrong provider headers")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/assistants":
			if r.Method != "POST" || !strings.Contains(string(body), `"model":"native"`) {
				t.Errorf("wrong assistant creation: %s", body)
			}
			fmt.Fprint(w, `{"id":"asst_real","object":"assistant","model":"native"}`)
		case "/v1/threads":
			fmt.Fprint(w, `{"id":"thread_real","object":"thread"}`)
		case "/v1/threads/thread_real/runs":
			if !strings.Contains(string(body), `"assistant_id":"asst_real"`) {
				t.Errorf("wrong run body %s", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: thread.run.completed\ndata: {\"id\":\"run_real\",\"object\":\"thread.run\",\"thread_id\":\"thread_real\",\"assistant_id\":\"asst_real\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("wrong provider path %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()
	candidate := Candidate{APIBase: origin.URL, APIKey: "fixture-provider", Provider: "openai", Endpoint: "chat-completions",
		ModelID: "native", DeploymentID: "primary", RoutingGroup: "demo"}
	recorded := make(chan InferenceEvent, 1)
	server := NewServer(Config{Token: "master", AuthorizeInference: true, Callback: func(event InferenceEvent) { recorded <- event }, Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}},
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"other"}, token == "other-key" }})
	call := func(token, path, body string, want int) string {
		t.Helper()
		request := httptest.NewRequest("POST", path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s returned %d, expected %d: %s", path, response.Code, want, response.Body)
		}
		return response.Body.String()
	}
	assistant := encodeResourceID("assistants", "primary", "asst_real")
	thread := encodeResourceID("threads", "primary", "thread_real")
	if !strings.Contains(call("master", "/assistants", `{"model":"demo","name":"fixture"}`, 200), assistant) ||
		!strings.Contains(call("master", "/v1/threads?model=demo", "", 200), thread) {
		t.Fatal("expected deployment-bound IDs")
	}
	path := "/threads/" + thread + "/runs"
	body := `{"assistant_id":"`+assistant+`","stream":true}`
	call("other-key", path, body, 400)
	mismatch := `{"assistant_id":"`+encodeResourceID("assistants", "different", "asst_real")+`","stream":true}`
	call("master", path, mismatch, 400)
	result := call("master", path, body, 200)
	if !strings.Contains(result, "event: thread.run.completed") || !strings.Contains(result, assistant) ||
		!strings.Contains(result, thread) || !strings.Contains(result, encodeResourceID("thread_runs", "primary", "run_real")) ||
		!strings.Contains(result, "data: [DONE]") || calls != 3 {
		t.Fatalf("unexpected assistant stream %s across %d provider calls", result, calls)
	}
	event := <-recorded
	if event.CallType != "arun_thread" || event.Total != 5 || event.ModelGroup != "demo" || event.Model != "native" ||
		!strings.HasPrefix(event.ID, "assistant-run-") {
		t.Fatalf("assistant usage callback did not preserve attribution: %+v", event)
	}
}
