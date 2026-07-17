package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	eventSubscriberBuffer = 32
	eventReplayLimit      = 500
	eventPublishTimeout   = 3 * time.Second
)

type ReachEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	MachineID string         `json:"machine_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt string         `json:"created_at"`
}

type eventBroker struct {
	store     *Store
	publishMu sync.Mutex
	subsMu    sync.Mutex
	subs      map[chan ReachEvent]struct{}
}

func newEventBroker(store *Store) *eventBroker {
	return &eventBroker{store: store, subs: map[chan ReachEvent]struct{}{}}
}

func (b *eventBroker) Subscribe(ctx context.Context) (chan ReachEvent, int64, int64, func(), error) {
	ch, oldest, boundary, _, _, cancel, err := b.SubscribeSince(ctx, 0, false, eventReplayLimit)
	return ch, oldest, boundary, cancel, err
}

func (b *eventBroker) SubscribeSince(ctx context.Context, cursor int64, hasCursor bool, limit int) (chan ReachEvent, int64, int64, []ReachEvent, string, func(), error) {
	ch := make(chan ReachEvent, eventSubscriberBuffer)
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	var oldest, boundary int64
	if b.store != nil {
		if err := b.store.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(id),0),COALESCE(MAX(id),0) FROM events`).Scan(&oldest, &boundary); err != nil {
			return nil, 0, 0, nil, "", nil, err
		}
	}
	var replay []ReachEvent
	var reason string
	if hasCursor {
		if cursor > boundary || (oldest > 0 && cursor < oldest-1) {
			reason = "cursor_out_of_range"
		} else {
			var overflow bool
			var err error
			replay, overflow, err = b.replayRange(ctx, cursor, boundary, limit)
			if err != nil {
				return nil, 0, 0, nil, "", nil, err
			}
			if overflow {
				replay = nil
				reason = "replay_overflow"
			}
		}
	}
	b.subsMu.Lock()
	b.subs[ch] = struct{}{}
	b.subsMu.Unlock()
	cancel := func() {
		b.subsMu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.subsMu.Unlock()
	}
	return ch, oldest, boundary, replay, reason, cancel, nil
}

func (b *eventBroker) Publish(ctx context.Context, typ, machineID string, data map[string]any) ReachEvent {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	created := nowUTC()
	payload, err := json.Marshal(data)
	if err != nil {
		return b.resyncSubscribers("event_encode_failed")
	}
	var id int64
	if b.store != nil {
		res, err := b.store.db.ExecContext(ctx, `INSERT INTO events (type,machine_id,payload_json,created_at) VALUES (?,?,?,?)`, typ, nullable(machineID), string(payload), created)
		if err != nil {
			return b.resyncSubscribers("event_persistence_failed")
		}
		id, err = res.LastInsertId()
		if err != nil || id <= 0 {
			return b.resyncSubscribers("event_persistence_failed")
		}
	}
	ev := ReachEvent{Type: typ, MachineID: machineID, Data: data, CreatedAt: created}
	if id > 0 {
		ev.ID = strconv.FormatInt(id, 10)
	}
	b.subsMu.Lock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			b.resyncSubscriberLocked(ch, "subscriber_overflow")
		}
	}
	b.subsMu.Unlock()
	return ev
}

func (b *eventBroker) resyncSubscribers(reason string) ReachEvent {
	ev := ReachEvent{Type: "machine.resync_required", Data: map[string]any{"reason": reason}, CreatedAt: nowUTC()}
	b.subsMu.Lock()
	for ch := range b.subs {
		b.resyncSubscriberLocked(ch, reason)
	}
	b.subsMu.Unlock()
	return ev
}

func (b *eventBroker) resyncSubscriberLocked(ch chan ReachEvent, reason string) {
	if _, ok := b.subs[ch]; !ok {
		return
	}
	delete(b.subs, ch)
	for {
		select {
		case <-ch:
			continue
		default:
		}
		break
	}
	ch <- ReachEvent{Type: "machine.resync_required", Data: map[string]any{"reason": reason}, CreatedAt: nowUTC()}
	close(ch)
}

func (b *eventBroker) ReplayRange(ctx context.Context, lastID, throughID int64, limit int) ([]ReachEvent, bool, error) {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	return b.replayRange(ctx, lastID, throughID, limit)
}

func (b *eventBroker) replayRange(ctx context.Context, lastID, throughID int64, limit int) ([]ReachEvent, bool, error) {
	if b.store == nil || throughID <= lastID {
		return nil, false, nil
	}
	if limit <= 0 || limit > eventReplayLimit {
		limit = eventReplayLimit
	}
	rows, err := b.store.db.QueryContext(ctx, `SELECT id,type,machine_id,payload_json,created_at FROM events WHERE id>? AND id<=? ORDER BY id LIMIT ?`, lastID, throughID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ReachEvent
	for rows.Next() {
		var id int64
		var typ, payload, created string
		var machineID sql.NullString
		if err := rows.Scan(&id, &typ, &machineID, &payload, &created); err != nil {
			return nil, false, err
		}
		data := map[string]any{}
		_ = json.Unmarshal([]byte(payload), &data)
		out = append(out, ReachEvent{ID: strconv.FormatInt(id, 10), Type: typ, MachineID: machineID.String, Data: data, CreatedAt: created})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (b *eventBroker) PruneBefore(ctx context.Context, before string) error {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	if b.store == nil {
		return nil
	}
	_, err := b.store.db.ExecContext(ctx, `DELETE FROM events WHERE created_at<?`, before)
	return err
}

type eventCursorValue struct {
	ID      int64
	Present bool
	Valid   bool
}

func eventCursor(r *http.Request) eventCursorValue {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("last_event_id")
	}
	if v == "" {
		return eventCursorValue{}
	}
	id, err := strconv.ParseInt(v, 10, 64)
	return eventCursorValue{ID: id, Present: true, Valid: err == nil && id >= 0}
}

func writeSSE(w io.Writer, ev ReachEvent) error {
	b, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if ev.Type == "machine.resync_required" {
		if _, err := fmt.Fprint(w, "id:\n"); err != nil {
			return err
		}
	} else if ev.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
	return err
}

func writeSSEComment(w http.ResponseWriter, msg string) error {
	_, err := fmt.Fprintf(w, ": %s %s\n\n", msg, time.Now().UTC().Format(time.RFC3339))
	return err
}
