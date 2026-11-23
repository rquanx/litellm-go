package proxyconfig

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"
)

const browserSSOTable = `minato_browser_sso`

type postgresBrowserSSO struct {
	db  *sql.DB
	key [32]byte
}

func openPostgresBrowserSSO(db *sql.DB, master string) (*postgresBrowserSSO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+browserSSOTable+` (
		state_digest TEXT PRIMARY KEY, binding TEXT NOT NULL, verifier TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS minato_browser_sso_expiry ON `+browserSSOTable+`(expires_at)`); err != nil {
		return nil, err
	}
	return &postgresBrowserSSO{db: db, key: sha256.Sum256([]byte("minato-browser-sso\x00" + master))}, nil
}

func (store *postgresBrowserSSO) save(parent context.Context, binding, state string, flow uiSSOFlow) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('minato:browser-sso',0))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+browserSSOTable+` WHERE expires_at<=CURRENT_TIMESTAMP`); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+browserSSOTable).Scan(&pending); err != nil {
		return err
	}
	if pending >= 4096 {
		return errors.New("too many pending SSO attempts")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+browserSSOTable+` (state_digest,binding,verifier,expires_at) VALUES($1,$2,$3,$4)`,
		keyDigest(state), binding, sealState([]byte(flow.verifier), &store.key), flow.expires); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *postgresBrowserSSO) consume(parent context.Context, binding, state string) (uiSSOFlow, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var flow uiSSOFlow
	var secret string
	err := store.db.QueryRowContext(ctx, `DELETE FROM `+browserSSOTable+`
		WHERE state_digest=$1 AND binding=$2 AND expires_at>CURRENT_TIMESTAMP RETURNING verifier,expires_at`,
		keyDigest(state), binding).Scan(&secret, &flow.expires)
	if err != nil {
		return flow, err
	}
	verifier, err := openState(secret, &store.key)
	flow.verifier = string(verifier)
	return flow, err
}
