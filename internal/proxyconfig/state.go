package proxyconfig

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/crypto/nacl/secretbox"
)

var errConcurrentState = errors.New("proxy state was changed by another process")

type stateStore interface {
	load(target any) error
	save(value any) error
}

type fileState string

func (store fileState) load(target any) error {
	file, err := os.Open(string(store))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	return decodeModelJSON(file, target)
}

func (store fileState) save(value any) error {
	return saveJSONState(string(store), value)
}

type postgresState struct {
	db       *sql.DB
	name     string
	revision int64
	key      *[32]byte
}

func OpenPostgresState(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL state configuration")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_proxy_state (
		name TEXT PRIMARY KEY,
		revision BIGINT NOT NULL,
		value JSONB NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, errors.New("could not initialize PostgreSQL proxy state")
	}
	return db, nil
}

func (store *postgresState) load(target any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := store.loadAfter(ctx, target, -1)
	return err
}

func (store *postgresState) loadAfter(ctx context.Context, target any, lastRevision int64) (bool, error) {
	var payload []byte
	var revision int64
	err := store.db.QueryRowContext(ctx, `SELECT CASE WHEN revision <> $2 THEN value END, revision FROM minato_proxy_state WHERE name = $1`, store.name, lastRevision).
		Scan(&payload, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		store.revision = 0
		return lastRevision > 0, nil
	}
	if err != nil {
		return false, err
	}
	if revision == lastRevision {
		return false, nil
	}
	if store.key != nil {
		var encrypted string
		if json.Unmarshal(payload, &encrypted) != nil {
			return false,errors.New("model state is not encrypted")
		}
		payload, err = openState(encrypted, store.key)
		if err != nil {
			return false,err
		}
	}
	if err := decodeModelJSON(bytes.NewReader(payload), target); err != nil {
		return false,err
	}
	store.revision = revision
	return true,nil
}

func (store *postgresState) save(value any) error {
	return store.saveWithAudit(value)
}

func (store *postgresState) saveWithAudit(value any, events ...*managementAuditEvent) error {
	events = slices.DeleteFunc(events, func(event *managementAuditEvent) bool { return event == nil })
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if store.key != nil {
		payload, err = json.Marshal(sealState(payload, store.key))
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec := store.db.ExecContext
	var tx *sql.Tx
	if len(events) > 0 {
		tx, err = store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		exec = tx.ExecContext
	}
	var result sql.Result
	if store.revision == 0 {
		result, err = exec(ctx, `INSERT INTO minato_proxy_state (name, revision, value)
			VALUES ($1, 1, $2::jsonb) ON CONFLICT (name) DO NOTHING`, store.name, string(payload))
	} else {
		result, err = exec(ctx, `UPDATE minato_proxy_state SET value = $2::jsonb, revision = revision + 1
			WHERE name = $1 AND revision = $3`, store.name, string(payload), store.revision)
	}
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errConcurrentState
	}
	if len(events) > 0 {
		for _, event := range events {
			if err := event.insert(ctx, tx, store.revision == 0); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	store.revision++
	return nil
}

func sealState(payload []byte, key *[32]byte) string {
	var nonce [24]byte
	rand.Read(nonce[:])
	encrypted := secretbox.Seal(nonce[:], payload, &nonce, key)
	return base64.RawURLEncoding.EncodeToString(encrypted)
}

func openState(value string, key *[32]byte) ([]byte, error) {
	encrypted, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(encrypted) < 24+secretbox.Overhead {
		return nil, errors.New("invalid encrypted model state")
	}
	var nonce [24]byte
	copy(nonce[:], encrypted[:24])
	payload, ok := secretbox.Open(nil, encrypted[24:], &nonce, key)
	if !ok {
		return nil, errors.New("could not decrypt model state")
	}
	return payload, nil
}

func postgresModels(db *sql.DB, master string) *postgresState {
	secret := os.Getenv("LITELLM_GO_STATE_KEY")
	if secret == "" {
		secret = os.Getenv("MINATO_RUNTIME_STATE_KEY")
	}
	if secret == "" {
		secret = master
	}
	key := sha256.Sum256([]byte(secret))
	return &postgresState{db: db, name: "models", key: &key}
}

func chooseStateStore(path string, db *sql.DB, name string) stateStore {
	if db != nil {
		return &postgresState{db: db, name: name}
	}
	if path != "" {
		return fileState(path)
	}
	return nil
}
