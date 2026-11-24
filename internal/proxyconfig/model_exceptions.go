package proxyconfig

import (
	"context"
	"maps"
	"net/http"
	"slices"
)

func (log *SpendLog) modelExceptions(ctx context.Context, w http.ResponseWriter, where string, args []any) {
	rows, err := log.db.QueryContext(ctx, `WITH grouped AS (
		SELECT concat_ws('-', model, NULLIF(api_origin,'')) AS deployment, http_status, COUNT(*) AS count`+
		where+` AND status='failure' GROUP BY 1,2
	), ranked AS (
		SELECT deployment, SUM(count) AS failures FROM grouped GROUP BY deployment
		ORDER BY failures DESC, deployment LIMIT 200
	)
	SELECT grouped.deployment, http_status, count FROM grouped JOIN ranked USING (deployment)
	ORDER BY failures DESC, deployment, http_status`, args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read model errors")
		return
	}
	defer rows.Close()
	result := []map[string]any{}
	kinds := map[string]bool{}
	previous := ""
	for rows.Next() {
		var deployment string
		var status int
		var count int64
		if err := rows.Scan(&deployment, &status, &count); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read model errors")
			return
		}
		if len(result) == 0 || deployment != previous {
			result = append(result, map[string]any{"model": deployment, "total_exceptions": int64(0)})
			previous = deployment
		}
		row := result[len(result)-1]
		kind := exceptionType(status)
		kinds[kind] = true
		seen, _ := row[kind].(int64)
		row[kind] = seen + count
		row["total_exceptions"] = row["total_exceptions"].(int64) + count
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read model errors")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"data": result, "exception_types": slices.Sorted(maps.Keys(kinds))})
}

func exceptionType(status int) string {
	switch status {
	case 400, 422:
		return "BadRequestError"
	case 401:
		return "AuthenticationError"
	case 403:
		return "PermissionDeniedError"
	case 404:
		return "NotFoundError"
	case 408, 504:
		return "Timeout"
	case 429:
		return "RateLimitError"
	case 500:
		return "InternalServerError"
	case 502:
		return "APIConnectionError"
	case 503:
		return "ServiceUnavailableError"
	default:
		return "APIError"
	}
}
