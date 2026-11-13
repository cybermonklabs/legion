// Command writing-check takes the measurements of Dispatch's writing check (INFRA-3231) under
// the method of INFRA-3189. It is a development tool: no image and no release ships it, and every
// file it writes holds Dispatch text, so its output belongs under $TMPDIR and never in a
// repository.
//
// Usage:
//
//	writing-check probe-model --kind <kind> --endpoint <url> --deployment <id> [flags]
//	writing-check fetch --after <event id> --through <event id> --out <cases.jsonl> [--resume]
//	writing-check group --in <cases.jsonl> --out <groups.jsonl>
//
// probe-model measures the latency of the model reader's calls, fetch reads the prose agents
// wrote from the live Dispatch event log as cases, and group joins the cases into the groups a
// set split keeps together. README.md says what each one does.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: writing-check <subcommand> [flags]

subcommands:
  probe-model  time the model reader's calls against one model endpoint
  fetch        read the prose agents wrote from the Dispatch event log as cases
  group        give each case the group a set split keeps together

writing-check <subcommand> -h lists its flags.
`

// run runs one subcommand. A refusal or a failure is written to stderr and answers a non-zero
// status: 2 for arguments the command cannot use, 1 for a run that failed.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "probe-model":
		return runProbe(ctx, args[1:], stdout, stderr)
	case "fetch":
		return runFetch(ctx, args[1:], stdout, stderr)
	case "group":
		return runGroup(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "writing-check: unknown subcommand %q\n\n%s", args[0], usage)
	return 2
}
