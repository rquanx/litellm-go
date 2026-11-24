package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func (log *SpendLog) providerSpend(w http.ResponseWriter, r *http.Request) {
	caller, valid := log.keys.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin && caller.user == "" {
		modelError(w, http.StatusForbidden, "A user-bound key is required")
		return
	}
	query := r.URL.Query()
	start, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
	end, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
	if startErr != nil || endErr != nil || end.Before(start) {
		modelJSON(w, http.StatusBadRequest, map[string]any{"detail": map[string]string{
			"error": "Please provide valid start_date and end_date",
		}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	statement := `SELECT provider, SUM(spend) FROM minato_proxy_spend
		WHERE started_at >= $1 AND started_at < $2
		AND provider <> '' AND model_id <> '' AND spend IS NOT NULL`
	args := []any{start, end.AddDate(0, 0, 1)}
	if !caller.admin {
		statement += " AND user_id = $3"
		args = append(args, caller.user)
	}
	rows, err := log.db.QueryContext(ctx, statement+" GROUP BY provider ORDER BY provider", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read provider spend")
		return
	}
	defer rows.Close()
	type providerTotal struct {
		Provider string  `json:"provider"`
		Spend    float64 `json:"spend"`
	}
	result := []providerTotal{}
	for rows.Next() {
		var total providerTotal
		if err := rows.Scan(&total.Provider, &total.Spend); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read provider spend")
			return
		}
		result = append(result, total)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read provider spend")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func (log *SpendLog) endUserSpend(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	var input struct {
		Start *time.Time `json:"startTime"`
		End   *time.Time `json:"endTime"`
		Key   *string    `json:"api_key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	readErr := decoder.Decode(&input)
	if readErr == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
		readErr = errors.New("expected one JSON value")
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		modelError(w, http.StatusBadRequest, "Invalid end-user spend request")
		return
	}
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -30)
	if input.Start != nil {
		start = *input.Start
	}
	if input.End != nil {
		end = *input.End
	}
	if end.Before(start) {
		modelError(w, http.StatusBadRequest, "endTime must be at or after startTime")
		return
	}
	if input.Key != nil && strings.HasPrefix(*input.Key, "sk-") {
		digest := keyDigest(*input.Key)
		input.Key = &digest
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, `SELECT NULLIF(end_user_id,''), COUNT(*), SUM(spend) AS total_spend
		FROM minato_proxy_spend WHERE started_at >= $1 AND started_at < $2
		AND ($3::text IS NULL OR api_key = $3)
		GROUP BY NULLIF(end_user_id,'') ORDER BY total_spend DESC NULLS LAST, 1 LIMIT 100`, start, end, input.Key)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read end-user spend")
		return
	}
	defer rows.Close()
	type endUserTotal struct {
		User  *string  `json:"end_user"`
		Count int64    `json:"total_count"`
		Spend *float64 `json:"total_spend"`
	}
	result := []endUserTotal{}
	for rows.Next() {
		var total endUserTotal
		if err := rows.Scan(&total.User, &total.Count, &total.Spend); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read end-user spend")
			return
		}
		result = append(result, total)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read end-user spend")
		return
	}
	modelJSON(w, http.StatusOK, result)
}
