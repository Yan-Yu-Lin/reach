package main

import (
	"context"
	"sync"
)

// streamLimiter caps concurrent SSE streams globally, per client IP and per
// credential (daybreak P2-12). For "single" credentials (the mac-agent
// service token) a new stream replaces the previous one: the old stream's
// context is cancelled, so a reconnecting Mac never piles up streams.
type streamLimiter struct {
	mu                  sync.Mutex
	total               int
	perIP, perCred      map[string]int
	max, maxIP, maxCred int
	singles             map[string]*singleStream
}

type singleStream struct{ cancel context.CancelFunc }

func newStreamLimiter(max, maxIP, maxCred int) *streamLimiter {
	return &streamLimiter{max: max, maxIP: maxIP, maxCred: maxCred, perIP: map[string]int{}, perCred: map[string]int{}, singles: map[string]*singleStream{}}
}

// acquire registers a stream and returns a context derived from parent
// that is cancelled if a newer single stream replaces this one, plus a
// release func. ok is false when a cap is reached.
func (l *streamLimiter) acquire(parent context.Context, ip, cred string, single bool) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	if l == nil {
		return ctx, cancel, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if single && cred != "" {
		if prev := l.singles[cred]; prev != nil {
			prev.cancel()
		}
	}
	credFull := cred != "" && !single && l.perCred[cred] >= l.maxCred
	if l.total >= l.max || l.perIP[ip] >= l.maxIP || credFull {
		cancel()
		return nil, nil, false
	}
	l.total++
	l.perIP[ip]++
	if cred != "" {
		l.perCred[cred]++
	}
	me := &singleStream{cancel: cancel}
	if single && cred != "" {
		l.singles[cred] = me
	}
	return ctx, func() {
		cancel()
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		if l.perIP[ip]--; l.perIP[ip] <= 0 {
			delete(l.perIP, ip)
		}
		if cred != "" {
			if l.perCred[cred]--; l.perCred[cred] <= 0 {
				delete(l.perCred, cred)
			}
			if l.singles[cred] == me {
				delete(l.singles, cred)
			}
		}
	}, true
}
