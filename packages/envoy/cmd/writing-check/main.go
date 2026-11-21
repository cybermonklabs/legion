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
//	writing-check readers --in <cases.jsonl> --log-after <event id> --log-through <event id> --out <cases.jsonl> [--state <file>]
//	writing-check rule-cost --in <cases.jsonl> --seed N --out <dir> [--writes 500] [--min-per-surface 30] [--plant <rule id>]
//	writing-check split --in <grouped.jsonl> --cut-a <event id> --cut-b <event id> --out <dir>
//	writing-check plant --in <manifest.jsonl> --corpus <cases.jsonl> --seed N --out <dir> [--per-shape 40] [--nolist 40] [--controls]
//	writing-check label --in <cases.jsonl> --definition <file> --out <labels.jsonl> --endpoint <url> --key-file <file> [flags]
//	writing-check validate-labeller --labels <labels.jsonl> --planted <manifest.jsonl> --seed N --out <report.md>
//	writing-check recall --labels <labels.jsonl> [--planted <manifest.jsonl>] [--heldout-seed N --heldout-share 0.5] --out <dir>
//	writing-check digest [--root <module root>]
//	writing-check score --labels <labels.jsonl> [--planted <manifest.jsonl>] --stratum real|planted --out <dir> [--final-test --set-digest <sha> --seed N --exposure-log <file>] <reader flags>
//	writing-check reproduce --outputs <outputs.jsonl> --labels <labels.jsonl> [--planted <manifest.jsonl>] --figures <figures.json> [--bootstrap-seed N]
//	writing-check regress --cases <cases.jsonl> --repeat-bound <share> <reader flags>
//	writing-check reading --in <cases.jsonl> --after <event id> --through <event id> [--exclude <manifest>]... --out <dir> <reader flags>
//	writing-check validate --description <scanner.yaml> --exposure-log <file> [--manifests <dir>]
//	writing-check dataset fetch --end <RFC 3339 time> --owner <login> --out <snapshot.jsonl.gz> [--labelled <sets tarball>]
//	writing-check dataset classify --snapshot <snapshot.jsonl.gz> --out <verdicts.jsonl> [--model <id>]
//	writing-check dataset build (--snapshot <file> --verdicts <file> --seed N [--confirmed <file> --confirmed-seed N] | --from <bundle>) [--to-development <file>] --out <dir>
//	writing-check dataset pack --snapshot <file> --verdicts <file> --seed N [--confirmed <file> --confirmed-seed N] --out <bundle.tar.gz>
//	writing-check dataset sessions --manifest <export manifest.json> --cases <build dir> --out <dir>
//	writing-check dataset owner-texts (--snapshot <file> | --from <bundle>) --out <dir>
//	writing-check dataset followups --from <bundle> --cases <build dir> --out <dir>
//	writing-check dataset named (--doc <issue>/<document>@<version>[,<version>...] | --ask <issue>/<ask id>)... --owner <login> --out <dir>
//	writing-check dataset lists --cases <build dir> --verdicts <file> --plain-labels <file> --out <dir>
//	writing-check calibrate --name <name> (--run <dir>[,<dir>...] | --rows <file>) [--labels-v1 <file>] [--labels-v2 <file>] [--split <file>] [--read-before-split <file>] [--half fit|measure|all] [--pairs <file>] [--verdicts <file>] [--measured <file>]
//	writing-check harness probe|score|grader-test|compare|phrases-labels|asked-words|local|load [flags]
//
// probe-model measures the latency of the model reader's calls, fetch reads the prose agents
// wrote from the live Dispatch event log as cases, and group joins the cases into the groups a
// set split keeps together. readers gives each case the person who read its write, by the
// server's reader rule over the records the event log rebuilds. rule-cost measures the share of
// writes each structure rule finds, split cuts the grouped cases into the three sets, plant
// plants faults and controls in copies of real writes, label labels writes with a model
// labeller, and validate-labeller checks the labeller against the planted cases. recall
// measures the code stage's recall, digest hashes the bundle, score runs the detector on
// labelled writes and computes the figures, reproduce recomputes them from the stored outputs,
// and validate checks a description file. dataset builds the dataset of INFRA-3231's R1 from the
// event log: a snapshot, the verdicts of the selection prompt, and the cases; its named step
// builds cases from document versions and asks read from the live Dispatch. harness scores the
// changes of INFRA-3231's R3 on that dataset (package harness), and calibrate checks the
// harness's answered score against what the owner did after an agent's explanation, calling no
// model. README.md says what each one does.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
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
  readers            give each case the person who read its write, replayed from the event log
  rule-cost          measure the share of writes each structure rule finds, and pick the rules to enable
  split              cut grouped cases into the development, calibration and final-pool sets
  plant              plant faults and matched controls in copies of real writes
  label              label each write's sentences with a model labeller
  validate-labeller  check the labeller's labels against the planted cases
  recall             measure the code stage's recall over the reference-vacuous sentences
  digest             print the bundle's digest and its files
  score              run the detector on labelled writes and compute the figures
  reproduce          recompute the figures from stored outputs, calling no model
  regress            run the reader on the regression cases and bound its disagreement
  reading            take the outcome reading over a window of agent writes
  validate           check a description file (scanner.yaml) against the method's rules
  dataset            build the dataset of texts the owner could not read: fetch, classify, build, pack, sessions, owner-texts, followups, named, lists
  calibrate          check the answered score against what the owner did after an explanation
  harness            score the changes of INFRA-3231 on the dataset: probe, score, grader-test, compare, phrases-labels, asked-words, local, load

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
	case "readers":
		return runReaders(ctx, args[1:], stdout, stderr)
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
	case "recall":
		return runRecall(args[1:], stdout, stderr)
	case "digest":
		return runDigest(args[1:], stdout, stderr)
	case "score":
		return runScore(ctx, args[1:], stdout, stderr)
	case "reproduce":
		return runReproduce(args[1:], stdout, stderr)
	case "regress":
		return runRegress(ctx, args[1:], stdout, stderr)
	case "reading":
		return runReading(ctx, args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "dataset":
		return runDataset(ctx, args[1:], stdout, stderr)
	case "calibrate":
		return runCalibrate(args[1:], stdout, stderr)
	case "harness":
		return harness.Run(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "writing-check: unknown subcommand %q\n\n%s", args[0], usage)
	return 2
}
