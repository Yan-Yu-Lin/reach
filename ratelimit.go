package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type rateLimiter struct {
	mu          sync.Mutex
	window      time.Duration
	limit       int
	buckets     map[string]*rateBucket
	lastCleanup time.Time
	db          *sql.DB
	bucketName  string
}

type rateBucket struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: map[string]*rateBucket{}, lastCleanup: time.Now()}
}

func newPersistentRateLimiter(db *sql.DB, bucket string, limit int, window time.Duration) *rateLimiter {
	r := newRateLimiter(limit, window)
	r.db = db
	r.bucketName = bucket
	return r
}

func (r *rateLimiter) allow(key string) bool {
	if r.db != nil && r.bucketName != "" {
		return r.allowPersistent(key)
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.lastCleanup) > r.window {
		for k, b := range r.buckets {
			if now.After(b.reset) {
				delete(r.buckets, k)
			}
		}
		r.lastCleanup = now
	}
	b := r.buckets[key]
	if b == nil || now.After(b.reset) {
		r.buckets[key] = &rateBucket{count: 1, reset: now.Add(r.window)}
		return true
	}
	if b.count >= r.limit {
		return false
	}
	b.count++
	return true
}

func clientIP(req *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		remoteHost = req.RemoteAddr
	}
	// Only trust proxy headers from the local nginx proxy. Direct callers cannot
	// spoof their way around limits by setting X-Forwarded-For themselves.
	if ip := net.ParseIP(remoteHost); ip != nil && ip.IsLoopback() {
		if real := strings.TrimSpace(req.Header.Get("X-Real-IP")); real != "" {
			return real
		}
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if part := strings.TrimSpace(parts[i]); part != "" {
					return part
				}
			}
		}
	}
	return remoteHost
}

func (r *rateLimiter) allowPersistent(key string) bool {
	now := time.Now().UTC()
	reset := now.Add(r.window)
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if now.Sub(r.lastCleanup) > r.window {
		_, _ = r.db.ExecContext(ctx, `DELETE FROM rate_limits WHERE reset_at<=?`, now.Format(time.RFC3339Nano))
		r.lastCleanup = now
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	var count int
	var resetAt string
	err = tx.QueryRowContext(ctx, `SELECT count,reset_at FROM rate_limits WHERE bucket=? AND key=?`, r.bucketName, key).Scan(&count, &resetAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rate_limits (bucket,key,count,reset_at,updated_at) VALUES (?,?,?,?,?)`, r.bucketName, key, 1, reset.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			return false
		}
		return tx.Commit() == nil
	}
	if err != nil {
		return false
	}
	if t, err := time.Parse(time.RFC3339Nano, resetAt); err != nil || now.After(t) {
		if _, err := tx.ExecContext(ctx, `UPDATE rate_limits SET count=1,reset_at=?,updated_at=? WHERE bucket=? AND key=?`, reset.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), r.bucketName, key); err != nil {
			return false
		}
		return tx.Commit() == nil
	}
	if count >= r.limit {
		return false
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rate_limits SET count=count+1,updated_at=? WHERE bucket=? AND key=?`, now.Format(time.RFC3339Nano), r.bucketName, key); err != nil {
		return false
	}
	return tx.Commit() == nil
}
