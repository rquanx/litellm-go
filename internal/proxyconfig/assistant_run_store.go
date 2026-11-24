package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type AssistantRunStore struct {
	db *sql.DB
	key *[32]byte
	spend *SpendLog
	cost *CostSettings
}

func (spend *SpendLog) OpenAssistantRunStore(ctx context.Context, master string, policy ...*CostSettings) (*AssistantRunStore, error) {
	_, err := spend.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_pending_assistant_runs (
		id TEXT PRIMARY KEY, payload TEXT NOT NULL, available_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS minato_pending_assistant_runs_available ON minato_pending_assistant_runs (available_at)`)
	if err != nil {
		return nil, err
	}
	store := &AssistantRunStore{db: spend.db, key: postgresModels(spend.db, master).key, spend: spend}
	if len(policy) > 0 {
		store.cost = policy[0]
	}
	return store, nil
}

func (store *AssistantRunStore) Schedule(ctx context.Context, job runtime.PendingAssistantRun) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO minato_pending_assistant_runs (id,payload,available_at,expires_at)
		VALUES ($1,$2,$3,GREATEST(NOW(),$3::timestamptz)+INTERVAL '1 day') ON CONFLICT (id) DO NOTHING`,
		job.ID, sealState(payload, store.key), job.AvailableAt)
	return err
}

func (store *AssistantRunStore) Complete(ctx context.Context, id string, event runtime.InferenceEvent) error {
	event.ID = id
	if store.cost != nil {
		event = store.cost.AdjustEvent(event)
	}
	if err := store.spend.Record(event); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx, `DELETE FROM minato_pending_assistant_runs WHERE id=$1`, id)
	return err
}

func (store *AssistantRunStore) Run(ctx context.Context, server *runtime.Server) {
	ticker := time.NewTicker(3*time.Second)
	defer ticker.Stop()
	for {
		if err := store.recoverOne(ctx, server.RecoverAssistantRun); err != nil && ctx.Err() == nil {
			log.Print("could not recover pending assistant run")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (store *AssistantRunStore) recoverOne(parent context.Context, recover func(context.Context, runtime.PendingAssistantRun) (runtime.InferenceEvent, error)) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var id, encrypted string
	err := store.db.QueryRowContext(ctx, `UPDATE minato_pending_assistant_runs SET available_at=NOW()+INTERVAL '45 seconds'
		WHERE id=(SELECT id FROM minato_pending_assistant_runs
			WHERE available_at<=NOW() AND expires_at>NOW() ORDER BY available_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id,payload`).Scan(&id, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = store.db.ExecContext(ctx, `DELETE FROM minato_pending_assistant_runs WHERE expires_at <= NOW()`)
		return err
	}
	if err != nil {
		return err
	}
	payload, err := openState(encrypted, store.key)
	if err != nil {
		return err
	}
	var job runtime.PendingAssistantRun
	if err := json.Unmarshal(payload, &job); err != nil || job.ID != id {
		return errors.New("invalid pending assistant run")
	}
	event, err := recover(ctx, job)
	if err != nil {
		return err
	}
	return store.Complete(ctx, id, event)
}

