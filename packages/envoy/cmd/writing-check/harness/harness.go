// Package harness is INFRA-3231's measurement harness (R2, R3): `writing-check harness <sub>`. It
// scores each change of R3 on the dataset of R1. For each case, the change gives the text a
// person would see; a fresh reader answers the person's question from that text alone; and the
// grader gives each judgement of R2, one prompt per judgement, against Dispatch's record as the
// dataset stored it and, where the case has one, the writer's session up to the text. Every
// score has its 95% interval over the split groups.
//
// The roles and the candidates are candidates.yaml, the prices prices.yaml, the prompts
// prompts/. Session text reaches Claude models only: package claude holds it, as a type no other
// code can read. A run that reads the held-out part writes a line to the exposure log first.
// Every output holds Dispatch text and goes under $TMPDIR, never into a repository.
package harness

import (
	"context"
	"fmt"
	"io"
)

const usage = `usage: writing-check harness <subcommand> [flags]

subcommands:
  probe        call each role model and each candidate 20 times; fail on a route_not_found or an id the provider does not serve
  score        score a change over a part of the dataset, with every score's 95% interval, the wait and the cost
  grader-test  give the grader planted wrong outputs beside right ones, and report how many it marks correctly
  compare      the agreement of two score runs of the same cases, by judgement: a grader's repeatability, or two graders' agreement
  phrases-labels  the phrases judgement against a labeller round's sentence labels: precision and recall
  asked-words  the undefined judgement of a score run against the words the owner asked about: recall
  local        start a local Dispatch (bun run dispatch:local) on a database of its own
  load         re-create each case's record in the local Dispatch

writing-check harness <subcommand> -h lists its flags.
`

// Run runs one harness subcommand: 2 for arguments it cannot use, 1 for a run that failed.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "probe":
		return runProbe(ctx, args[1:], stdout, stderr)
	case "score":
		return runScore(ctx, args[1:], stdout, stderr)
	case "grader-test":
		return runGraderTest(ctx, args[1:], stdout, stderr)
	case "compare":
		return runCompare(args[1:], stdout, stderr)
	case "phrases-labels":
		return runPhrasesLabels(ctx, args[1:], stdout, stderr)
	case "asked-words":
		return runAskedWords(args[1:], stdout, stderr)
	case "local":
		return runLocal(ctx, args[1:], stdout, stderr)
	case "load":
		return runLoad(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "writing-check harness: unknown subcommand %q\n\n%s", args[0], usage)
	return 2
}
