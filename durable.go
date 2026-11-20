package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// Durable action journal (daybreak round 3 P1-A, astra R2-02).
//
// A failed deploy may restore the pre-deploy DB snapshot. That is only safe
// if no operator or enrollment decision was made on the new version. The
// audit log cannot answer that: it is written after the mutation and its
// errors are ignored. The journal can:
//
//   - DB-only decisions (revoke-all, logout, service token create/revoke,
//     approve/deny, invite claim, update policy, process title) insert their
//     journal row in the SAME transaction as the mutation. If the row cannot
//     be written, the mutation rolls back and the caller gets a 5xx.
//   - Decisions with OS side effects (provision, enable, disable, retire,
//     provisioning recovery) commit an "intent" row BEFORE touching the OS
//     and update it to done/failed afterwards. The intent alone is enough to
//     block an automatic restore, so a crash between the OS step and the DB
//     record can never be rolled back silently.
//
// `reachd deploy-fence` records MAX(id) before the new binary starts; any
// row above it, in any state, blocks the automatic snapshot restore.

const (
	durableDone   = "done"
	durableIntent = "intent"
	durableFailed = "failed"
)

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// recordDurable inserts one journal row through ex (a *sql.Tx for DB-only
// decisions, the DB for OS intents) and returns its id.
func recordDurable(ctx context.Context, ex sqlExecer, kind, machineID, actor, state string) (int64, error) {
	now := nowUTC()
	res, err := ex.ExecContext(ctx, `INSERT INTO durable_actions (kind,machine_id,actor,state,created_at,updated_at) VALUES (?,?,?,?,?,?)`, kind, nullable(machineID), nullable(actor), state, now, now)
	if err != nil {
		return 0, fmt.Errorf("record durable action %s: %w", kind, err)
	}
	return res.LastInsertId()
}

// settleDurable marks an intent done or failed. Best effort: the row
// already blocks restores whatever its state, so a failure here only leaves
// a less precise label for the operator.
func (s *Store) settleDurable(ctx context.Context, id int64, state string) {
	if id == 0 {
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE durable_actions SET state=?, updated_at=? WHERE id=?`, state, nowUTC(), id); err != nil {
		log.Printf("settle durable action %d: %v", id, err)
	}
}

// durableTx runs fn and the journal row in one transaction. If fn returns
// an error nothing is committed and no row is written.
// errNoChange: fn changed nothing; durableTx commits nothing and writes no
// journal row, so no-ops cannot fill the journal or block rollbacks
// (daybreak round 4 P3-01). durableTx returns nil to the caller.
var errNoChange = errors.New("no change")

func (s *Store) durableTx(ctx context.Context, kind, machineID, actor string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	if _, err := recordDurable(ctx, tx, kind, machineID, actor, durableDone); err != nil {
		return err
	}
	return tx.Commit()
}

// inTx runs fn in one transaction.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// journalRetention: rows older than this cannot belong to any deploy
// fence still in use (a deploy finishes or is rolled back within hours).
const journalRetention = 90 * 24 * time.Hour

// pruneJournal deletes old journal rows (daybreak round 4 P3-01).
func (s *Store) pruneJournal(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM durable_actions WHERE created_at<?`, now.Add(-journalRetention).UTC().Format(time.RFC3339))
	return err
}
