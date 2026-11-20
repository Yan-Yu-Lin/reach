package main

import (
	"context"
	"sync"
)

// streamLimiter caps concurrent SSE streams globally, per client IP and per
// credential (daybreak P2-12). For "single" credentials (the mac-agent
// service token) a new stream replaces the previous one: the old stream's
// context is cancelled, so a reconnecting Mac never piles up streams.
//
// Replacement reserves capacity first (astra R2-12, daybreak round 3
// P3-A): the caps are checked as if the old stream were already gone, and
// only if the new one fits is the old one's slot taken over and the old
// stream cancelled. A rejected replacement leaves the old stream running.
type streamLimiter struct {
	mu                  sync.Mutex
	total               int
	perIP, perCred      map[string]int
	max, maxIP, maxCred int
	singles             map[string]*singleStream
}

// singleStream owns one counted slot. released guards against counting
// the slot down twice (once when it is taken over, once by its own
// release func).
type singleStream struct {
	cancel   context.CancelFunc
	ip       string
	released bool
}

func newStreamLimiter(max, maxIP, maxCred int) *streamLimiter {
	return &streamLimiter{max: max, maxIP: maxIP, maxCred: maxCred, perIP: map[string]int{}, perCred: map[string]int{}, singles: map[string]*singleStream{}}
}

func (l *streamLimiter) dec(ip, cred string) {
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
	if cred != "" {
		if l.perCred[cred]--; l.perCred[cred] <= 0 {
			delete(l.perCred, cred)
		}
	}
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
	var prev *singleStream
	if single && cred != "" {
		prev = l.singles[cred]
	}
	// Capacity as it would be once prev's slot is handed over.
	total, ipCount := l.total, l.perIP[ip]
	if prev != nil {
		total--
		if prev.ip == ip {
			ipCount--
		}
	}
	credFull := cred != "" && !single && l.perCred[cred] >= l.maxCred
	if total >= l.max || ipCount >= l.maxIP || credFull {
		cancel()
		return nil, nil, false
	}
	if prev != nil {
		prev.released = true
		l.dec(prev.ip, cred)
		prev.cancel()
	}
	l.total++
	l.perIP[ip]++
	if cred != "" {
		l.perCred[cred]++
	}
	me := &singleStream{cancel: cancel, ip: ip}
	if single && cred != "" {
		l.singles[cred] = me
	}
	return ctx, func() {
		cancel()
		l.mu.Lock()
		defer l.mu.Unlock()
		if me.released {
			return
		}
		me.released = true
		l.dec(ip, cred)
		if cred != "" && l.singles[cred] == me {
			delete(l.singles, cred)
		}
	}, true
}
