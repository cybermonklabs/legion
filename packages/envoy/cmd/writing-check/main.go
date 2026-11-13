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
//	writing-check rule-cost --in <cases.jsonl> --seed N --out <dir> [--writes 500] [--min-per-surface 30] [--plant <rule id>]
//	writing-check split --in <grouped.jsonl> --cut-a <event id> --cut-b <event id> --out <dir>
//	writing-check plant --in <manifest.jsonl> --corpus <cases.jsonl> --seed N --out <dir> [--per-shape 40] [--nolist 40] [--controls]
//	writing-check label --in <cases.jsonl> --definition <file> --out <labels.jsonl> --endpoint <url> --key-file <file> [flags]
//	writing-check validate-labeller --labels <labels.jsonl> --planted <manifest.jsonl> --seed N --out <report.md>
//
// probe-model measures the latency of the model reader's calls, fetch reads the prose agents
// wrote from the live Dispatch event log as cases, and group joins the cases into the groups a
// set split keeps together. rule-cost measures the share of writes each structure rule finds,
// split cuts the grouped cases into the three sets, plant plants faults and controls in copies
// of real writes, label labels writes with a model labeller, and validate-labeller checks the
// labeller against the planted cases. README.md says what each one does.
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
  probe-model        time the model reader's calls against one model endpoint
  fetch              read the prose agents wrote from the Dispatch event log as cases
  group              give each case the group a set split keeps together
  rule-cost          measure the share of writes each structure rule finds, and pick the rules to enable
  split              cut grouped cases into the development, calibration and final-pool sets
  plant              plant faults and matched controls in copies of real writes
  label              label each write's sentences with a model labeller
  validate-labeller  check the labeller's labels against the planted cases

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
	case "rule-cost":
		return runRuleCost(args[1:], stdout, stderr)
	case "split":
		return runSplit(args[1:], stdout, stderr)
	case "plant":
		return runPlant(args[1:], stdout, stderr)
	case "label":
		return runLabel(ctx, args[1:], stdout, stderr)
	case "validate-labeller":
		return runValidateLabeller(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "writing-check: unknown subcommand %q\n\n%s", args[0], usage)
	return 2
}
