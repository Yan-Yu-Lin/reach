package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server-generated bearer secrets (client_secret, setup_token, agent_token,
// service tokens) are 124+ bits of CSPRNG output. They do not need a
// password-grade KDF: a SHA-256 digest compared in constant time is enough and
// costs microseconds instead of 64 MiB + ~150 ms of Argon2id per request.
//
// Argon2id stays for human-chosen secrets (admin password, god code, invite
// codes) and for legacy rows written before this change. Every Argon2
// verification goes through argonGate so concurrent requests cannot allocate
// unbounded 64 MiB buffers.

const tokenHashPrefix = "sha256:"

var errArgonBusy = errors.New("secret verification is busy; retry later")

// HashToken returns the storage form of a server-generated token.
func HashToken(token string) string { return tokenHashPrefix + TokenHash(token) }

func isLegacyArgonHash(stored string) bool { return strings.HasPrefix(stored, "$argon2id$") }

// argonGate bounds concurrent Argon2id verifications process-wide.
type argonGate struct {
	slots   chan struct{}
	timeout time.Duration
	runs    atomic.Int64 // number of Argon2id computations performed (tests/metrics)
}

func newArgonGate(n int, timeout time.Duration) *argonGate {
	if n <= 0 {
		n = 1
	}
	return &argonGate{slots: make(chan struct{}, n), timeout: timeout}
}

// defaultArgonGate allows two concurrent Argon2id runs (2 x 64 MiB). jason has
// two vCPUs and Argon2 parameters use parallelism 2.
var defaultArgonGate = newArgonGate(2, 3*time.Second)

func (g *argonGate) acquire(ctx context.Context) error {
	t := time.NewTimer(g.timeout)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errArgonBusy
	}
}

func (g *argonGate) release() { <-g.slots }

// VerifySecret runs an Argon2id verification under the gate.
func (g *argonGate) VerifySecret(ctx context.Context, secret, encoded string) (bool, error) {
	if secret == "" || encoded == "" {
		return false, nil
	}
	if err := g.acquire(ctx); err != nil {
		return false, err
	}
	defer g.release()
	g.runs.Add(1)
	return VerifySecret(secret, encoded), nil
}

// VerifyStoredToken checks a server-generated token against a stored hash in
// either the current SHA-256 form or the legacy Argon2id form. When a legacy
// hash verifies, upgraded holds the replacement hash the caller should persist
// (lazy migration). err is only non-nil when the Argon2 gate is saturated.
func (g *argonGate) VerifyStoredToken(ctx context.Context, token, stored string) (ok bool, upgraded string, err error) {
	if token == "" || stored == "" {
		return false, "", nil
	}
	if strings.HasPrefix(stored, tokenHashPrefix) {
		want := []byte(stored)
		got := []byte(HashToken(token))
		return subtle.ConstantTimeCompare(want, got) == 1, "", nil
	}
	if !isLegacyArgonHash(stored) {
		return false, "", nil
	}
	ok, err = g.VerifySecret(ctx, token, stored)
	if err != nil || !ok {
		return false, "", err
	}
	return true, HashToken(token), nil
}

var (
	dummyArgonOnce sync.Once
	dummyArgon     string
)

// dummyArgonHash is verified against when a login names an unknown account so
// the response time does not reveal which usernames exist. Computed lazily so
// CLI commands do not pay for it.
func dummyArgonHash() string {
	dummyArgonOnce.Do(func() {
		dummyArgon, _ = HashSecret("reach-dummy-password-for-timing-equalization")
	})
	return dummyArgon
}
