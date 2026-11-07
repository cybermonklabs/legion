// Command experiments runs one experiment of the Temporal evaluation per invocation
// against a real Temporal server:
//
//	experiments -experiment N -address HOST:PORT -namespace eN \
//	    [-session-worker PATH] [-metrics-url URL] [-work-dir DIR]
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
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// notImplemented is the exit code of an experiment number nothing implements yet. It is
// neither PASS nor FAIL: nothing was measured, so the runner must not read it as either.
const notImplemented = 2

func main() {
	var cfg harness.Config
	flag.IntVar(&cfg.Experiment, "experiment", 0, "which experiment to run, 1..6")
	flag.StringVar(&cfg.Address, "address", "127.0.0.1:7233", "the Temporal server's gRPC address")
	flag.StringVar(&cfg.Namespace, "namespace", "default", "the Temporal namespace to run in")
	flag.StringVar(&cfg.SessionWorker, "session-worker", "", "absolute path of the built cmd/sessionworker binary (experiment 3)")
	flag.StringVar(&cfg.MetricsURL, "metrics-url", "", "the dev server's Prometheus endpoint (experiment 6)")
	flag.StringVar(&cfg.WorkDir, "work-dir", "", "a writable scratch directory the experiment may use")
	flag.StringVar(&cfg.RepoRoot, "repo-root", "", "the Legion repository's root, whose own code experiment 1 measures (default: found by walking up from the working directory)")
	// The control of experiment 3: run everything but the kill. Nothing then fails, so
	// the experiment must report that it measured nothing and exit non-zero.
	noKill := flag.Bool("no-kill", false, "experiment 3 only: skip the kill, to prove a run that kills nothing fails")
	flag.Parse()

	if cfg.Experiment < 1 || cfg.Experiment > 6 {
		fmt.Fprintf(os.Stderr, "experiments: -experiment must be 1..6, got %d\n", cfg.Experiment)
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
	default:
		fmt.Printf("experiment %d: not implemented\n", cfg.Experiment)
		os.Exit(notImplemented)
	}
}
