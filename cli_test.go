package main

import (
	"context"
	"errors"
	"testing"

	"reach/internal/wscarrier"
)

// Astra R2-13: reachd ws-server must enforce the carrier path gate too.
func TestReachdWSServerRequiresPath(t *testing.T) {
	t.Setenv("REACH_WS_PATH_PREFIX", "")
	if err := runCLI(context.Background(), []string{"ws-server", "--listen", "127.0.0.1:0"}); !errors.Is(err, wscarrier.ErrNoPathPrefix) {
		t.Fatalf("err = %v", err)
	}
}
