package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Provisioning freeze, re-verified inside deploy-prepare (daybreak round 6
// R6-P1-02).
//
// scripts/preflight-provisioning-freeze.sh runs early, while reachd is
// still serving; a client can register (and a setup-token holder can start
// a provision) in the minutes before reachd is stopped. So deploy-prepare
// checks again after reachd is stopped, holding the writers lock
// exclusively, before the fence and the snapshot, in one read transaction.
//
// It fails closed: only states known to be finished pass.
//
//	requests.status         provisioned, expired, denied
//	requests.provision_phase NULL (legacy or never provisioned), delivered,
//	                        cleaned
//	machines.status         anything but 'provisioning'
//
// Anything else -- pending/approved/provisioning, response_pending,
// cleanup_pending, claimed, os_created, or a value this binary does not
// know -- refuses the deploy.

var (
	frozenRequestStatus = map[string]bool{"provisioned": true, "expired": true, "denied": true}
	frozenPhase         = map[string]bool{phaseDelivered: true, phaseCleaned: true}
)

var errNotFrozen = errors.New("provisioning is not frozen")

func checkProvisioningFrozen(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='requests'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil // fresh database: nothing can be in flight
	}
	hasPhase, err := txColumnExists(ctx, tx, "requests", "provision_phase")
	if err != nil {
		return err
	}
	phaseExpr := "NULL"
	if hasPhase {
		phaseExpr = "provision_phase"
	}
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(status,'<null>'), `+phaseExpr+`, COUNT(*) FROM requests GROUP BY 1, 2`)
	if err != nil {
		return err
	}
	var open []string
	for rows.Next() {
		var status string
		var phase sql.NullString
		var count int
		if err := rows.Scan(&status, &phase, &count); err != nil {
			_ = rows.Close()
			return err
		}
		if !frozenRequestStatus[status] || (phase.Valid && !frozenPhase[phase.String]) {
			label := "status=" + status
			if phase.Valid {
				label += " phase=" + phase.String
			}
			open = append(open, fmt.Sprintf("%d request(s) %s", count, label))
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='machines'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM machines WHERE status='provisioning'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			open = append(open, fmt.Sprintf("%d machine(s) status=provisioning", n))
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("%w: %s; deny them or let them expire and finish, and do not add or reinstall machines until Stage 5", errNotFrozen, strings.Join(open, ", "))
	}
	return nil
}

func txColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&n)
	return n > 0, err
}
