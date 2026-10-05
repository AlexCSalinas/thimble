package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexcsalinas/thimble/internal/api"
	"github.com/alexcsalinas/thimble/internal/sandbox"
)

// serve runs the control plane (phase 4) and the envd proxy. Point the stock
// E2B SDK at it with:
//
//	E2B_API_KEY=anything E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	snap := fs.String("snapshot", "build/snap", "template snapshot every sandbox is created from")
	apiAddr := fs.String("api", "127.0.0.1:3000", "control plane listen address")
	envdAddr := fs.String("envd", "127.0.0.1:49983", "envd proxy listen address")
	idle := fs.Uint64("idle", 96, "balloon target in MiB after restore (0 = off)")
	apiKey := fs.String("api-key", os.Getenv("THIMBLE_API_KEY"), "required X-API-Key value (empty = accept any non-empty key)")
	fs.Parse(args)

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	mg, err := sandbox.NewManager(sandbox.Config{SnapshotDir: *snap, IdleMiB: *idle, Logf: log.Printf})
	if err != nil {
		return err
	}
	defer mg.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; fmt.Fprintln(os.Stderr, "thimble: shutting down"); cancel() }()

	return api.Serve(ctx, api.New(mg, *apiKey, log.Printf), *apiAddr, *envdAddr)
}
