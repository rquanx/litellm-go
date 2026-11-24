package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestOwnerActivityAppliesDelegatedScopes(t *testing.T) {
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
	first := keys.state.Teams["owned-team"]
	first.Alias = "report-team"
	first.Members = []teamMembership{{UserID: "admin", Role: "admin"}, {UserID: "member", Role: "user"}}
	keys.state.Teams[first.ID] = first
	second := keys.state.Teams["other-team"]
	second.Members = []teamMembership{{UserID: "admin", Role: "user"}, {UserID: "member", Role: "admin"}}
	keys.state.Teams[second.ID] = second
	log, err := OpenSpendLog(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 1, 4, 20, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		team, user string
		tokens     int
	}{
		{"owned-team", "admin", 10},
		{"owned-team", "member", 20},
		{"other-team", "admin", 30},
		{"other-team", "member", 40},
	} {
		log.Record(runtime.InferenceEvent{
			ID: newEntityID(), Model: "owner-activity-fixture", Status: "success", Total: fixture.tokens,
			EndUser: "report-customer",
			KeyDigest: keyDigest("sk-" + fixture.user), SpendAttribution: &runtime.SpendAttribution{UserID: fixture.user, TeamID: fixture.team},
			StartTime: float64(when.Unix()), EndTime: float64(when.Unix() + 1),
		})
	}
	handler := log.Handler(http.NotFoundHandler())
	for _, fixture := range []struct {
		path, query string
		status, tokens int
	}{
		{"/team/daily/activity", "&team_ids=owned-team", 200, 30},
		{"/team/daily/activity", "&team_ids=owned-team,other-team", 200, 40},
		{"/team/daily/activity", "&team_ids=other-team&api_key=sk-member", 200, 0},
		{"/team/daily/activity", "&team_ids=unrelated", 404, 0},
		{"/user/daily/activity", "&user_id=admin", 200, 40},
		{"/user/daily/activity", "&user_id=member", 403, 0},
		{"/user/daily/activity/aggregated", "", 200, 40},
		{"/user/daily/activity", "&day=utc", 200, 0},
		{"/user/daily/activity", "&timezone=-480", 200, 40},
	} {
		date := "2026-01-04"
		if fixture.query == "&day=utc" || fixture.query == "&timezone=-480" {
			date = "2026-01-05"
		}
		path := fixture.path + "?model=owner-activity-fixture&start_date=" + date + "&end_date=" + date + fixture.query
		body := organizationRequest(t, handler, "GET", path, "admin", "", fixture.status)
		if fixture.status != 200 {
			continue
		}
		var response struct {
			Metadata struct {
				Tokens int `json:"total_tokens"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil || response.Metadata.Tokens != fixture.tokens {
			t.Fatalf("%s returned %s, want %d tokens", path, body, fixture.tokens)
		}
	}

	cost := 0.01234
	log.Record(runtime.InferenceEvent{
		ID: newEntityID(), Model: "owner-activity-tag-fixture", Status: "success", Spend: &cost,
		Tags: []string{"owner-activity-first", "owner-activity-second"},
		StartTime: float64(when.Unix()), EndTime: float64(when.Unix() + 1),
	})
	const tagQuery = "/global/spend/tags?start_date=2026-01-04&end_date=2026-01-04&tags=owner-activity-first"
	organizationRequest(t, handler, "GET", tagQuery, "admin", "", 401)
	body := organizationRequest(t, handler, "GET", tagQuery, "fixture-master", "", 200)
	var tags struct {
		SpendPerTag []struct {
			Name  string  `json:"name"`
			Spend float64 `json:"spend"`
			Count int     `json:"log_count"`
		} `json:"spend_per_tag"`
	}
	if err := json.Unmarshal([]byte(body), &tags); err != nil || len(tags.SpendPerTag) != 1 ||
		tags.SpendPerTag[0].Name != "owner-activity-first" || tags.SpendPerTag[0].Spend != 0.0123 || tags.SpendPerTag[0].Count != 1 {
		t.Fatalf("unexpected tag spend: %s", body)
	}
	const report = "/global/spend/report?start_date=2026-01-04&end_date=2026-01-04"
	organizationRequest(t, handler, "GET", report, "admin", "", 401)
	for _, fixture := range []struct{ query, field string }{
		{"", `"team_name":"report-team"`},
		{"&group_by=customer", `"customer":"report-customer"`},
		{"&group_by=api_key", `"model_details"`},
		{"&api_key=sk-admin", keyDigest("sk-admin")},
		{"&internal_user_id=admin", keyDigest("sk-admin")},
		{"&team_id=owned-team&customer_id=report-customer", `"teams_customers"`},
	} {
		body := organizationRequest(t, handler, "GET", report+fixture.query, "fixture-master", "", 200)
		if !strings.Contains(body, fixture.field) {
			t.Fatalf("report %s omitted %s: %s", fixture.query, fixture.field, body)
		}
	}
}
