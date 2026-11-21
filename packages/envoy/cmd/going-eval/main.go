// Command going-eval is the eval of the command bar's agent (R13 of dispatch://INFRA-1588/spec),
// over the tasks and the seeded Dispatch of going/eval. It has three subcommands:
//
//	go run ./cmd/going-eval seed --database-url <url> [--envoy-fixture-out <file>]
//	go run ./cmd/going-eval run [--database-url <url>] [--server-bin <file>] [--tasks <ids or kinds>] [--tries <n>]
//	    [--parallel <n>] [--judge-deployment <name>] [--answers-out <file>] [--work-dir <dir>] [--keep-databases] [--source <text>]
//	go run ./cmd/going-eval calibrate [--judge-deployment <name>]
//
// seed, which the browser harness runs to serve a Dispatch on the eval's seed with the real
// models (DISPATCH_E2E_MODELS=real, packages/dispatch/e2e/harness-eval-seed.ts), migrates the
// database the URL names and writes going/eval's seed.json into it (eval.WriteSeed), or writes
// nothing where the database already holds the seed. A database that holds a project the seed
// does not is refused before anything is written to it, the migration included. With
// --envoy-fixture-out it writes the seed's agent sessions to that file as JSON, the sessions a
// fake Envoy must answer for the seeded issues' claims, routes and role holders to name live,
// unresponsive or gone sessions.
//
// run is the eval. On the Postgres server --database-url names (by default
// $LEGION_TEST_PG_DSN; never the agent sessions' database that OMP_SESSION_SQL_DSN_FILE names,
// which it refuses) it writes the seed into a template database, and for each try of each task
// it copies the template, starts a Dispatch server on the copy (cmd/dispatch, built unless
// --server-bin names a build) with its agent process and an in-process fake Envoy answering the
// seed's sessions, and acts as a person through the routes: it starts a run with the task's
// words, page, mode and level, answers each plan the run shows as the task's grade says, and
// reads the run to its end. A change task is graded by the store it leaves (eval.GradeEnd), an
// answer task by a judge model reading the answer against the task's facts, and every message
// and comment of the person's by c13's rule (eval.CheckPersonWrites). The tries of one task run
// before the next task starts, --parallel of them at once.
//
// The chat model is the one the server's own settings name, read from run's environment:
// DISPATCH_AZURE_CHAT_ENDPOINT, DISPATCH_AZURE_CHAT_DEPLOYMENT, DISPATCH_AZURE_CHAT_API_VERSION,
// DISPATCH_AZURE_CHAT_SCOPE and the keyless Azure sign-in (DISPATCH_AZURE_TENANT_ID,
// DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE); no API key is read. The server
// and its agent call it through an https proxy of the eval's on loopback, whose limiter holds
// every chat call of the run, the judge's too, under 500,000 tokens a minute and counts what each
// spent. The judge is --judge-deployment, on the same sign-in (by default the chat deployment).
// Before the tries, run puts each calibration answer of going/eval's labels.json to the judge;
// calibrate does that alone and writes the same judge section, to stand beside a run's report.
//
// The report goes to stdout: each task's tries and why each failed one failed, pass^1 and pass^3
// of each kind and of all tasks with the floors of going/eval and whether each is met, the
// judge's agreement with the labels, what the tries did of their planted instructions, the
// person's messages and comments the rule does not let stand, and the most chat tokens of any
// minute. Each failing try keeps its database, which its line names. run exits 0 when the report
// meets every floor, 1 when it misses one or fails, and 2 on a usage error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sjawhar/envoy/going/eval"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// usage is what the command prints for no subcommand or one it does not know.
const usage = "usage: going-eval seed --database-url <url> [--envoy-fixture-out <file>]\n" +
	"       going-eval run [--database-url <url>] [--server-bin <file>] [--tasks <ids or kinds>] [--tries <n>] [--parallel <n>] [--judge-deployment <name>] [--answers-out <file>] [--work-dir <dir>] [--keep-databases] [--source <text>]\n" +
	"       going-eval calibrate [--judge-deployment <name>]"

// run answers the exit status of the subcommand args name: 0 done, 1 refused or failed, 2 a usage
// error. Every refusal goes to stderr.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "run" {
		return runCommand(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "calibrate" {
		return calibrateCommand(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 0 || args[0] != "seed" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("going-eval seed", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databaseURL := flags.String("database-url", "", "the Postgres URL of the Dispatch database to seed (required)")
	fixtureOut := flags.String("envoy-fixture-out", "", "a file to write the seed's agent sessions to, as JSON, for a fake Envoy")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() > 0 || strings.TrimSpace(*databaseURL) == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := seed(ctx, *databaseURL, *fixtureOut, stdout); err != nil {
		fmt.Fprintf(stderr, "going-eval seed: %v\n", err)
		return 1
	}
	return 0
}

// seed refuses a database holding a project the seed does not, before it migrates it
// (eval.RefuseOtherProjects), migrates the database, writes the committed seed into it, and writes
// the seed's sessions to fixtureOut where it names a file.
func seed(ctx context.Context, databaseURL, fixtureOut string, stdout io.Writer) error {
	loaded, err := eval.LoadSeed(eval.Files)
	if err != nil {
		return err
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	if err := eval.RefuseOtherProjects(ctx, database, loaded); err != nil {
		return err
	}
	if err := database.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate the database: %w", err)
	}
	written, err := eval.WriteSeed(ctx, database, loaded)
	if err != nil {
		return err
	}
	if written {
		fmt.Fprintf(stdout, "going-eval seed: wrote %d projects, %d issues, %d documents and files, %d asks, %d comments and %d messages\n",
			len(loaded.Projects), len(loaded.Issues), len(loaded.Artifacts), len(loaded.Asks), len(loaded.Comments), len(loaded.Messages))
	} else {
		fmt.Fprintln(stdout, "going-eval seed: the database already holds the seed; nothing written")
	}
	if fixtureOut == "" {
		return nil
	}
	encoded, err := json.Marshal(loaded.Sessions)
	if err != nil {
		return err
	}
	if err := os.WriteFile(fixtureOut, encoded, 0o600); err != nil {
		return fmt.Errorf("write the Envoy fixture: %w", err)
	}
	return nil
}
