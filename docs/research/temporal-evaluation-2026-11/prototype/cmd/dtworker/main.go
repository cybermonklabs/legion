// Command dtworker is the second process the durabletask-go experiments need.
//
// It is a separate process on purpose. durabletask-go is a library: the engine and
// its activities run inside the application's own process, so the only way to ask
// what happens when the process holding an activity dies is to have a process that
// can be killed without taking the driver with it. Experiment 16 kills one and
// starts a replacement; experiment 17 does the same to a workflow that is waiting
// for a person.
//
// It prints `dtworker: key=value` lines for everything its driver waits on, the
// first of them always its own process id, and then serves until it is stopped.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp16"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp17"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

func main() {
	var cfg libx.WorkerConfig
	flag.StringVar(&cfg.Mode, "mode", "", "which experiment's workflow to host: exp16 or exp17")
	flag.StringVar(&cfg.DSN, "postgres-dsn", os.Getenv("LEGION_TEST_PG_DSN"), "the PostgreSQL URL the library's tables live on")
	flag.StringVar(&cfg.Schema, "schema", "", "the schema this run owns (required)")
	flag.StringVar(&cfg.InstanceID, "instance-id", "", "the workflow instance this process starts, or is expected to take over (required)")
	flag.StringVar(&cfg.WorkDir, "work-dir", "", "the scratch directory the workflow's files live in (required)")
	flag.BoolVar(&cfg.Start, "start", false, "create the workflow instance; without it this process only polls, which is where recovery happens")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "dtworker: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg libx.WorkerConfig) error {
	for name, value := range map[string]string{
		"-postgres-dsn": cfg.DSN,
		"-schema":       cfg.Schema,
		"-instance-id":  cfg.InstanceID,
		"-work-dir":     cfg.WorkDir,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	// The driver stops a worker by killing its process group; a worker that is asked
	// to stop politely gets here and lets the engine close its connections.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Mode {
	case "exp16":
		return exp16.Worker(ctx, cfg)
	case "exp17":
		return exp17.Worker(ctx, cfg)
	default:
		return fmt.Errorf("-mode must be exp16 or exp17, got %q", cfg.Mode)
	}
}
