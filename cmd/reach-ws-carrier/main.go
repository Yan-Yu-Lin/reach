package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"reach/internal/wscarrier"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatalf("reach-ws-carrier: %v", err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "server" {
		if len(args) > 0 {
			args = args[1:]
		}
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		listen := fs.String("listen", "127.0.0.1:9401", "listen address")
		target := fs.String("target", "127.0.0.1:22", "target TCP address")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return wscarrier.Serve(ctx, *listen, *target, log.Default())
	}
	if args[0] == "client" {
		fs := flag.NewFlagSet("client", flag.ContinueOnError)
		rawURL := fs.String("url", "", "ws:// or wss:// URL")
		path := fs.String("path-prefix", "", "URL path prefix")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *rawURL == "" {
			return fmt.Errorf("--url is required")
		}
		return wscarrier.Client(ctx, *rawURL, *path, os.Stdin, os.Stdout)
	}
	return fmt.Errorf("usage: reach-ws-carrier [server --listen ADDR --target ADDR | client --url URL --path-prefix PATH]")
}
