// Command experiments runs one experiment of the workflow-engine evaluation per
// invocation against a real engine:
//
//	experiments -experiment N -address HOST:PORT -namespace eN \
//	    [-session-worker PATH] [-metrics-url URL] [-work-dir DIR] [-repo-root DIR]
//	experiments -experiment N -postgres-dsn URL [-dbos-worker PATH] [-work-dir DIR]
//
// Experiments 1 to 6 measure Temporal and take its address; experiments 7 to 9
// measure DBOS, which is a library keeping its state in the application's own
// PostgreSQL, and take that database's URL instead.
//
// Each experiment prints its measured numbers as plain lines and then exactly one last
// line, `experiment N: PASS` or `experiment N: FAIL`, exiting 0 only on PASS.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp1"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp2"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp3"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp4"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp5"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp6"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp7"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp8"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp9"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// notImplemented is the exit code of an experiment number nothing implements yet. It is
// neither PASS nor FAIL: nothing was measured, so the runner must not read it as either.
const notImplemented = 2

func main() {
	var cfg harness.Config
	flag.IntVar(&cfg.Experiment, "experiment", 0, "which experiment to run, 1..9")
	flag.StringVar(&cfg.Address, "address", "127.0.0.1:7233", "the Temporal server's gRPC address (experiments 1..6)")
	flag.StringVar(&cfg.Namespace, "namespace", "default", "the Temporal namespace to run in (experiments 1..6)")
	flag.StringVar(&cfg.SessionWorker, "session-worker", "", "absolute path of the built cmd/sessionworker binary (experiment 3)")
	flag.StringVar(&cfg.MetricsURL, "metrics-url", "", "the dev server's Prometheus endpoint (experiment 6)")
	flag.StringVar(&cfg.WorkDir, "work-dir", "", "a writable scratch directory the experiment may use")
	flag.StringVar(&cfg.RepoRoot, "repo-root", "", "the Legion repository's root, whose own code experiment 1 measures (default: found by walking up from the working directory)")
	flag.StringVar(&cfg.PostgresDSN, "postgres-dsn", os.Getenv("LEGION_TEST_PG_DSN"), "the PostgreSQL the DBOS experiments measure against (experiments 7..9)")
	flag.StringVar(&cfg.DBOSWorker, "dbos-worker", "", "absolute path of the built cmd/dbosworker binary (experiments 7..9; default: the dbosworker beside this binary)")
	// The control of experiments 3 and 7: run everything but the kill. Nothing then
	// fails, so the experiment must report that it measured nothing and exit non-zero.
	noKill := flag.Bool("no-kill", false, "experiments 3 and 7 only: skip the kill, to prove a run that kills nothing fails")
	flag.Parse()

	if cfg.Experiment < 1 || cfg.Experiment > 9 {
		fmt.Fprintf(os.Stderr, "experiments: -experiment must be 1..9, got %d\n", cfg.Experiment)
		os.Exit(notImplemented)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Experiment {
	case 1:
		os.Exit(harness.Verdict(cfg.Experiment, exp1.Run(ctx, cfg)))
	case 2:
		os.Exit(harness.Verdict(cfg.Experiment, exp2.Run(ctx, cfg)))
	case 3:
		os.Exit(harness.Verdict(cfg.Experiment, exp3.Run(ctx, cfg, *noKill)))
	case 4:
		os.Exit(harness.Verdict(cfg.Experiment, exp4.Run(ctx, cfg)))
	case 5:
		os.Exit(harness.Verdict(cfg.Experiment, exp5.Run(ctx, cfg)))
	case 6:
		os.Exit(harness.Verdict(cfg.Experiment, exp6.Run(ctx, cfg)))
	case 7:
		os.Exit(harness.Verdict(cfg.Experiment, exp7.Run(ctx, cfg, *noKill)))
	case 8:
		os.Exit(harness.Verdict(cfg.Experiment, exp8.Run(ctx, cfg)))
	case 9:
		os.Exit(harness.Verdict(cfg.Experiment, exp9.Run(ctx, cfg)))
	default:
		fmt.Printf("experiment %d: not implemented\n", cfg.Experiment)
		os.Exit(notImplemented)
	}
}
