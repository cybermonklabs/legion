// Command experiments runs one experiment of the workflow-engine evaluation per
// invocation against a real engine:
//
//	experiments -experiment N -address HOST:PORT -namespace eN \
//	    [-session-worker PATH] [-metrics-url URL] [-work-dir DIR] [-repo-root DIR]
//	experiments -experiment N -postgres-dsn URL [-dbos-worker PATH] [-work-dir DIR]
//	experiments -experiment N [-restate-server PATH] [-restate-service PATH] \
//	    [-restate-ingress HOST:PORT] [-restate-admin HOST:PORT] [-work-dir DIR]
//
// Experiments 1 to 6 and 19 to 21 measure Temporal and take its address; experiments 7 to 9
// measure DBOS, which is a library keeping its state in the application's own
// PostgreSQL, and take that database's URL instead; experiments 10 to 12 measure
// Restate, which is a server that calls the application, and take that server's two
// loopback addresses, the restate-server binary to start, and the binary of the
// service process the server is to call.
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
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp10"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp11"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp12"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp13"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp14"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp15"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp16"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp17"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp18"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp19"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp2"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp20"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp21"
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
	flag.IntVar(&cfg.Experiment, "experiment", 0, "which experiment to run, 1..21")
	flag.StringVar(&cfg.Address, "address", "127.0.0.1:7233", "the Temporal server's gRPC address (experiments 1..6 and 19..21)")
	flag.StringVar(&cfg.Namespace, "namespace", "default", "the Temporal namespace to run in (experiments 1..6 and 19..21)")
	flag.StringVar(&cfg.SessionWorker, "session-worker", "", "absolute path of the built cmd/sessionworker binary (experiment 3)")
	flag.StringVar(&cfg.MetricsURL, "metrics-url", "", "the dev server's Prometheus endpoint (experiment 6)")
	flag.StringVar(&cfg.WorkDir, "work-dir", "", "a writable scratch directory the experiment may use")
	flag.StringVar(&cfg.RepoRoot, "repo-root", "", "the Legion repository's root, whose own code experiment 1 measures (default: found by walking up from the working directory)")
	flag.StringVar(&cfg.PostgresDSN, "postgres-dsn", os.Getenv("LEGION_TEST_PG_DSN"), "the PostgreSQL the library experiments measure against (experiments 7..9 and 13..18)")
	flag.StringVar(&cfg.DBOSWorker, "dbos-worker", "", "absolute path of the built cmd/dbosworker binary (experiments 7..9; default: the dbosworker beside this binary)")
	flag.StringVar(&cfg.RestateIngress, "restate-ingress", "127.0.0.1:8080", "the Restate server's ingress address, which must be on 127.0.0.1 (experiments 10..12)")
	flag.StringVar(&cfg.RestateAdmin, "restate-admin", "127.0.0.1:9070", "the Restate server's admin address, which must be on 127.0.0.1 (experiments 10..12)")
	flag.StringVar(&cfg.RestateServer, "restate-server", "", "absolute path of a restate-server executable, which the experiment starts itself (experiments 10..12; default: the restate-server beside this binary, and failing that a server already answering at -restate-admin)")
	flag.StringVar(&cfg.RestateService, "restate-service", "", "absolute path of the built cmd/restateservice binary, the process Restate calls (experiments 10..12; default: the restateservice beside this binary)")
	flag.StringVar(&cfg.GWWorker, "gw-worker", "", "absolute path of the built cmd/gwworker binary (experiments 13 and 14; default: the gwworker beside this binary)")
	flag.StringVar(&cfg.DTWorker, "dt-worker", "", "absolute path of the built cmd/dtworker binary (experiments 16 and 17; default: the dtworker beside this binary)")
	// The control of experiments 3, 7, 10, 13 and 16: run everything but the kill.
	// Nothing then fails, so the experiment must report that it measured nothing and
	// exit non-zero.
	noKill := flag.Bool("no-kill", false, "experiments 3, 7, 10, 13 and 16 only: skip the kill, to prove a run that kills nothing fails")
	flag.Parse()

	if cfg.Experiment < 1 || cfg.Experiment > 21 {
		fmt.Fprintf(os.Stderr, "experiments: -experiment must be 1..21, got %d\n", cfg.Experiment)
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
	case 10:
		os.Exit(harness.Verdict(cfg.Experiment, exp10.Run(ctx, cfg, *noKill)))
	case 11:
		os.Exit(harness.Verdict(cfg.Experiment, exp11.Run(ctx, cfg)))
	case 12:
		os.Exit(harness.Verdict(cfg.Experiment, exp12.Run(ctx, cfg)))
	case 13:
		os.Exit(harness.Verdict(cfg.Experiment, exp13.Run(ctx, cfg, *noKill)))
	case 14:
		os.Exit(harness.Verdict(cfg.Experiment, exp14.Run(ctx, cfg)))
	case 15:
		os.Exit(harness.Verdict(cfg.Experiment, exp15.Run(ctx, cfg)))
	case 16:
		os.Exit(harness.Verdict(cfg.Experiment, exp16.Run(ctx, cfg, *noKill)))
	case 17:
		os.Exit(harness.Verdict(cfg.Experiment, exp17.Run(ctx, cfg)))
	case 18:
		os.Exit(harness.Verdict(cfg.Experiment, exp18.Run(ctx, cfg)))
	case 19:
		os.Exit(harness.Verdict(cfg.Experiment, exp19.Run(ctx, cfg)))
	case 20:
		os.Exit(harness.Verdict(cfg.Experiment, exp20.Run(ctx, cfg)))
	case 21:
		os.Exit(harness.Verdict(cfg.Experiment, exp21.Run(ctx, cfg)))
	default:
		fmt.Printf("experiment %d: not implemented\n", cfg.Experiment)
		os.Exit(notImplemented)
	}
}
