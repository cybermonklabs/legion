// Command dbosworker is the second process the DBOS experiments need.
//
// It is a separate process on purpose. DBOS is a library: the workflow runs inside the
// application's own process, so the only way to ask what happens when the process
// holding a step dies is to have a process that can be killed without taking the
// driver with it. Experiment 7 kills one and starts a replacement under the same
// executor id; experiment 8 does the same to a workflow that is waiting for a person;
// experiment 9 uses one to record a run under the code as it stood before a change.
//
// It prints `dbosworker: key=value` lines for everything its driver waits on, the
// first of them always its own process id, and then serves until it is stopped.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/dbosx"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp7"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp8"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp9"
)

// shutdownTimeout is how long a worker that was asked to stop gives DBOS to let go of
// its database connections. A worker that is killed never reaches it.
const shutdownTimeout = 5 * time.Second

func main() {
	var cfg dbosx.WorkerConfig
	flag.StringVar(&cfg.Mode, "mode", "", "which experiment's workflow to host: exp7, exp8 or exp9")
	flag.StringVar(&cfg.DSN, "postgres-dsn", os.Getenv("LEGION_TEST_PG_DSN"), "the PostgreSQL URL the DBOS system database lives on")
	flag.StringVar(&cfg.Schema, "schema", "", "the system-database schema this run owns (required)")
	flag.StringVar(&cfg.AppName, "app-name", "", "the DBOS application name (required)")
	flag.StringVar(&cfg.AppVersion, "app-version", "", "the DBOS application version, pinned by the driver (required)")
	flag.StringVar(&cfg.ExecutorID, "executor-id", "", "the DBOS executor id this process opens under (required)")
	flag.StringVar(&cfg.WorkflowID, "workflow-id", "", "the workflow this process starts, or is expected to recover (required)")
	flag.StringVar(&cfg.WorkDir, "work-dir", "", "the scratch directory the workflow's files live in (required)")
	flag.BoolVar(&cfg.Start, "start", false, "start the workflow; without it this process only launches DBOS, which is where recovery happens")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "dbosworker: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg dbosx.WorkerConfig) error {
	for name, value := range map[string]string{
		"-postgres-dsn": cfg.DSN,
		"-schema":       cfg.Schema,
		"-app-name":     cfg.AppName,
		"-app-version":  cfg.AppVersion,
		"-executor-id":  cfg.ExecutorID,
		"-workflow-id":  cfg.WorkflowID,
		"-work-dir":     cfg.WorkDir,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	// The driver stops a worker by killing its process group; a worker that is asked
	// to stop politely gets here and lets DBOS close its connections.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Mode {
	case "exp7":
		return exp7.Worker(ctx, cfg, shutdownTimeout)
	case "exp8":
		return exp8.Worker(ctx, cfg, shutdownTimeout)
	case "exp9":
		return exp9.Worker(ctx, cfg, shutdownTimeout)
	default:
		return fmt.Errorf("-mode must be exp7, exp8 or exp9, got %q", cfg.Mode)
	}
}
