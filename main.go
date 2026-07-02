package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:]
	// Same binary can be symlinked/copied as reachctl. If invoked that way and no explicit
	// command is provided, show CLI help rather than starting the daemon by surprise.
	if filepath.Base(os.Args[0]) == "reachctl" && len(args) == 0 {
		usage()
		return
	}
	if err := runCLI(ctx, args); err != nil {
		log.Fatalf("reach: %v", err)
	}
}
