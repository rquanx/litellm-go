package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestKeySpendResetPreservesHistoryAndLateCharges(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys := organizationFixture(t)
	team := keys.state.Teams["owned-team"]
	team.Members = []teamMembership{{UserID: "admin", Role: "admin"}, {UserID: "member", Role: "user"}}
	keys.state.Teams[team.ID] = team
	ctx := context.Background()
	log, err := OpenSpendLog(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	token := newKeyToken()
	digest := keyDigest(token)
	cap := 4.0
	keys.state.Keys[digest] = virtualKey{TeamID: team.ID, MaxBudget: &cap}
	started := float64(time.Now().Add(-time.Minute).Unix())
	record := func(amount float64) {
		log.Record(runtime.InferenceEvent{ID: newEntityID(), KeyDigest: digest, Model: "fixture",
			Status: "success", StartTime: started, EndTime: float64(time.Now().Unix()), Spend: &amount})
	}
	record(5)
	handler := keys.Handler(http.NotFoundHandler())
	path := "/key/" + digest + "/reset_spend"
	organizationRequest(t, handler, "POST", path, "member", `{"reset_to":2}`, 403)
	organizationRequest(t, handler, "POST", path, "admin", `{"reset_to":5}`, 400)
	reset := organizationRequest(t, handler, "POST", path, "admin", `{"reset_to":2}`, 200)
	var result struct {
		Previous float64 `json:"previous_spend"`
		Spend    float64 `json:"spend"`
	}
	if err := json.Unmarshal([]byte(reset), &result); err != nil || result.Previous != 5 || result.Spend != 2 {
		t.Fatalf("unexpected reset response: %s", reset)
	}
	check, err := log.CheckKeyBudget(ctx, token)
	if err != nil || check.ErrorMessage != "" {
		t.Fatalf("adjusted balance should allow inference: %+v, %v", check, err)
	}
	record(2)
	check, err = log.CheckKeyBudget(ctx, token)
	if err != nil || check.ErrorMessage == "" {
		t.Fatalf("a late charge was not included in budget checks: %+v, %v", check, err)
	}
	newToken := newKeyToken()
	organizationRequest(t, handler, "POST", "/key/"+digest+"/regenerate", "fixture-master", `{"new_key":"`+newToken+`"}`, 200)
	check, err = log.CheckKeyBudget(ctx, newToken)
	if err != nil || check.ErrorMessage == "" {
		t.Fatalf("rotation changed the key balance: %+v, %v", check, err)
	}
	recorded, err := log.KeySpend(ctx, digest, time.Time{})
	if err != nil || recorded != 7 {
		t.Fatalf("historical spend was changed: %g, %v", recorded, err)
	}
}
