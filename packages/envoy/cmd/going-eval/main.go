// Command going-eval is the eval of the command bar's agent (R13 of dispatch://INFRA-1588/spec),
// over the tasks and the seeded Dispatch of going/eval. It has four subcommands:
//
//	go run ./cmd/going-eval seed --database-url <url> [--envoy-fixture-out <file>]
//	go run ./cmd/going-eval run [--database-url <url>] [--server-bin <file>] [--tasks <ids or kinds>] [--tries <n>]
//	    [--parallel <n>] [--judge-deployment <name>] [--answers-out <file>] [--work-dir <dir>] [--keep-databases] [--source <text>]
//	go run ./cmd/going-eval calibrate [--judge-deployment <name>] [--print-rendered-labels]
//	go run ./cmd/going-eval replay --body <file> [--tries <n>] [--parallel <n>] [--variants <names>] [--bisect <part>]
//	    [--system <file>] [--replace <old>=><new>]...
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
// seed's sessions, both on the seed's clock: the server's agent tells each run's model the seed's
// now as the time now (DISPATCH_TEST_AGENT_NOW, under the server's test hooks), and the fake
// Envoy answers each session last seen when the seed says, so the seed's times, written for its
// day, read as of that day, as the judge reads them. It acts as a person through the routes: it
// starts a run with the task's words, page, mode and level, answers each plan the run shows as
// the task's grade says, and reads the run to its end. A change task is graded by the store it
// leaves (eval.GradeEnd), an answer task by a judge model reading the answer against the task's
// facts, and every message and comment of the person's by c13's rule (eval.CheckPersonWrites).
// The judge reads an answer as the person reads it in the bar, with each link rendered as the
// bar's chip draws it from the store the try left (chips.RenderAnswer); the check of the links it
// holds reads it as written. A try the judge cannot read is unjudged, neither a pass nor a fail,
// and is put to the judge again once at the run's end. The tries of one task run before the next
// task starts, --parallel of them at once.
//
// The chat model is the one the server's own settings name, read from run's environment:
// DISPATCH_AZURE_CHAT_ENDPOINT, DISPATCH_AZURE_CHAT_DEPLOYMENT, DISPATCH_AZURE_CHAT_API_VERSION,
// DISPATCH_AZURE_CHAT_SCOPE and the keyless Azure sign-in (DISPATCH_AZURE_TENANT_ID,
// DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE); no API key is read. The server
// and its agent call it through an https proxy of the eval's on loopback, whose limiter holds
// every chat call of the run, the judge's too, under 500,000 tokens a minute and counts what each
// spent. Each server paces its own calls to its share of that limit, the limit over --parallel.
// The proxy writes chat-calls.log into the work dir, a line for each call, and keeps the request
// body of each call the deployment's content filter refused as refused/<n>.json beside it, which
// that call's line names (refused_body=): the server logs no request body.
// The judge is --judge-deployment, on the same sign-in (by default the chat deployment).
// The decision model is the one DISPATCH_DECISION_MODEL, DISPATCH_DECISION_MODEL_ENDPOINT and
// DISPATCH_DECISION_MODEL_TOKEN_FILE name in run's environment, which each server is given as they
// are (decisionSettings): its agent asks it what each turn's words ask, and the no-plan guard
// skips the check of a turn it reads as Ask. With none set, no turn is read and every one checked.
// Before the tries, run puts each calibration answer of going/eval's labels.json to the judge,
// rendered against the seed as it stands before any task; calibrate does that alone and writes
// the same judge section, to stand beside a run's report. calibrate --print-rendered-labels calls
// no model: it prints each calibration answer as the judge reads it, one JSON object a line,
// {task, words, facts, rendered}.
//
// replay sends one kept body to the chat deployment, on the same settings and sign-in, --tries
// times for each variant of it, the body with one named part taken out - the system message or
// half its lines, the tool list, the page and its read, one message's text - and prints how many
// calls of each the content filter refused, with its label, and how many were answered;
// --bisect narrows the refusal to the lines, sentences and words of one part, and --system and
// --replace change the body's words before any variant, to measure a rewording (replayCommand).
// It lives here, not in a command of its own, since it reads the settings run reads, signs in as
// the judge does, and replays the bodies run's proxy keeps.
//
// The report goes to stdout: each task's tries and why each failed one failed, pass^1 and pass^3
// of each kind and of all tasks with the floors of going/eval and whether each is met, the tries
// the judge could not read with its error, the judge's agreement with the labels, what the tries
// did of their planted instructions, the person's messages and comments the rule does not let
// stand, the most chat tokens of any minute, for each level the p99 and the max of one model
// call's output tokens, its reasoning among them, and of its reasoning tokens, over every model
// call of its tries as their reply frames say them, each run stopped at a model call's output
// tokens (OUTPUT_TOKEN_BUDGET) with the tokens of that call, and each run stopped at its working
// time (TIME_BUDGET) with how long the agent's pacer held its model calls, the checks the no-plan
// guard made of replies in prose, with the median and the p99 of the time they added to the turns
// of each kind, the turns whose check it skipped and those it decided on with no line from the
// decision model, and how many the run made, and the tasks whose tries read the reference graph
// (GET /api/v1/references and an issue's and a document's references), with how many tries of
// each did, to run again after a change to those routes. No floor is given over a kind that holds
// an unjudged try, and the report's RESULT then says the run is incomplete. Each failing or
// unjudged try keeps its database, which its line names. --answers-out writes each try as one
// JSON line, with its answer, its level, its run's working time and the pacer's hold of it, the
// turns its run ended, the output and reasoning tokens of each of its model calls and the turn,
// time and tokens of each check of the no-plan guard, or why it skipped one, with what the
// decision model read the turn's words as, the tools its runs called and how many of
// those calls read the reference graph, and a try the judge read again at the run's end once more.
// run exits 0 when the report meets every floor, 1 when it misses one, is incomplete or fails, and
// 2 on a usage error.
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
	"       going-eval calibrate [--judge-deployment <name>] [--print-rendered-labels]\n" +
	replayUsage

// run answers the exit status of the subcommand args name: 0 done, 1 refused or failed, 2 a usage
// error. Every refusal goes to stderr.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "run" {
		return runCommand(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "calibrate" {
		return calibrateCommand(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "replay" {
		return replayCommand(ctx, args[1:], stdout, stderr)
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
