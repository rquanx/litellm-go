package proxyconfig

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/lib/pq"
)

func spendSessionID(value string) string {
	if len(value) > 256 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return ""
	}
	return value
}

func loadSpendSessionCounts(ctx context.Context, tx *sql.Tx, scope spendLogScope, data []spendRow) error {
	ids := make([]string, 0, len(data))
	for i := range data {
		data[i].SessionCount = 1
		if data[i].SessionID != nil {
			ids = append(ids, *data[i].SessionID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	where, args := scope.apply(" WHERE session_id <> '' AND session_id = ANY($1::text[])", []any{pq.Array(ids)})
	rows, err := tx.QueryContext(ctx, "SELECT session_id, count(*) FROM minato_proxy_spend"+where+" GROUP BY session_id", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make(map[string]int, len(ids))
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range data {
		if id := data[i].SessionID; id != nil {
			data[i].SessionCount = max(1, counts[*id])
		}
	}
	return nil
}

