package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (log *SpendLog) payloadDetail(w http.ResponseWriter, r *http.Request) {
	scope, ok := log.spendLogScope(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	visible, args := scope.apply("TRUE", []any{r.PathValue("request_id")})
	var allowed bool
	var messages, response []byte
	err := log.db.QueryRowContext(ctx, "SELECT COALESCE(("+visible+"), false)"+
		", CASE WHEN "+visible+" THEN input_messages END"+
		", CASE WHEN "+visible+" THEN response_payload END"+
		" FROM minato_proxy_spend WHERE request_id=$1", args...).Scan(&allowed, &messages, &response)
	if errors.Is(err, sql.ErrNoRows) {
		modelJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend log payload")
		return
	}
	if !allowed {
		modelError(w, http.StatusForbidden, "Not authorized to view spend log")
		return
	}
	modelJSON(w, http.StatusOK, map[string]json.RawMessage{
		"messages": messages, "response": response, "proxy_server_request": nil,
	})
}

