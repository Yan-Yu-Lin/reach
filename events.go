package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type ReachEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	MachineID string         `json:"machine_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt string         `json:"created_at"`
}

type eventBroker struct {
	store *Store
	mu    sync.Mutex
	subs  map[chan ReachEvent]struct{}
}

func newEventBroker(store *Store) *eventBroker {
	return &eventBroker{store: store, subs: map[chan ReachEvent]struct{}{}}
}

func (b *eventBroker) Subscribe() (chan ReachEvent, func()) {
	ch := make(chan ReachEvent, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

func (b *eventBroker) Publish(ctx context.Context, typ, machineID string, data map[string]any) ReachEvent {
	created := nowUTC()
	payload, _ := json.Marshal(data)
	var id int64
	if b.store != nil {
		res, err := b.store.db.ExecContext(ctx, `INSERT INTO events (type,machine_id,payload_json,created_at) VALUES (?,?,?,?)`, typ, nullable(machineID), string(payload), created)
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	if id == 0 {
		id = time.Now().UnixNano()
	}
	ev := ReachEvent{ID: fmt.Sprintf("%d", id), Type: typ, MachineID: machineID, Data: data, CreatedAt: created}
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
	return ev
}

func (b *eventBroker) ReplaySince(ctx context.Context, lastID int64, limit int) ([]ReachEvent, error) {
	if b.store == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := b.store.db.QueryContext(ctx, `SELECT id,type,machine_id,payload_json,created_at FROM events WHERE id>? ORDER BY id LIMIT ?`, lastID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReachEvent
	for rows.Next() {
		var id int64
		var typ, payload, created string
		var machineID sql.NullString
		if err := rows.Scan(&id, &typ, &machineID, &payload, &created); err != nil {
			return nil, err
		}
		data := map[string]any{}
		_ = json.Unmarshal([]byte(payload), &data)
		out = append(out, ReachEvent{ID: fmt.Sprintf("%d", id), Type: typ, MachineID: machineID.String, Data: data, CreatedAt: created})
	}
	return out, rows.Err()
}

func lastEventID(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("last_event_id")
	}
	id, _ := strconv.ParseInt(v, 10, 64)
	return id
}

func writeSSE(w http.ResponseWriter, ev ReachEvent) error {
	b, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if ev.MachineID != "" {
		_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
	return err
}

func writeSSEComment(w http.ResponseWriter, msg string) error {
	_, err := fmt.Fprintf(w, ": %s %s\n\n", msg, time.Now().UTC().Format(time.RFC3339))
	return err
}
