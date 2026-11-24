package proxyconfig

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestCostSettingsApplyAndPersist(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TEMP TABLE minato_proxy_state (LIKE public.minato_proxy_state INCLUDING ALL)"); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	policy := &CostSettings{store: postgresModels(db, "master")}
	policy.store.name = "cost-settings"
	policy.active.Store(&policy.base)
	handler := policy.Handler(http.NotFoundHandler(), keys)
	update := func(path, body string, code int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer master")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != code {
			t.Fatalf("got %d, want %d: %s", response.Code, code, response.Body)
		}
	}
	update("/config/cost_discount_config", `{"openai":0.2}`, 200)
	update("/config/cost_margin_config", `{"openai":{"percentage":0.1,"fixed_amount":1},"global":0.2}`, 200)
	update("/config/cost_discount_config", `{"openai":1.1}`, 400)
	base := 10.0
	got := policy.AdjustEvent(runtime.InferenceEvent{Status: "success", Provider: "openai", Spend: &base})
	if math.Abs(*got.Spend-9.8) > 1e-9 {
		t.Fatalf("unexpected adjusted spend: %v", *got.Spend)
	}
	replica := &CostSettings{store: postgresModels(db, "master")}
	replica.store.name = "cost-settings"
	if err := replica.refresh(); err != nil {
		t.Fatal(err)
	}
	if total, _ := replica.Price("gemini", 10); total != 12 {
		t.Fatalf("global margin did not persist: %g", total)
	}
}

