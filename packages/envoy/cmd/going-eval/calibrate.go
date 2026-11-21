package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/eval"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// newJudge is the judge on the chat deployment named deployment, at the endpoint and on the
// keyless Azure sign-in of chat, through limits, reading facts written for the moment written.
func newJudge(chat chatSettings, deployment string, limits *limiter, written time.Time) (*judge, error) {
	tokens, err := embed.NewEntraTokenSource(embed.EntraConfig{
		TenantID: chat.TenantID, ClientID: chat.ClientID, TokenFile: chat.TokenFile, Scope: cmp.Or(chat.Scope, embed.DefaultEntraScope),
	})
	if err != nil {
		return nil, fmt.Errorf("the judge's Azure sign-in: %w", err)
	}
	model := agentloop.Model{Endpoint: agentloop.Endpoint{Kind: agentloop.OpenAIChat, BaseURL: chat.Endpoint, Token: tokens.Token}, ID: deployment}
	if chat.APIVersion != "" {
		model.Endpoint.Query = url.Values{"api-version": {chat.APIVersion}}
	}
	return &judge{model: model, limits: limits, written: written}, nil
}

// writeCalibration writes the judge section of a report: the judge's agreement with the agreed
// labels of labels.json against JudgeAgreementFloor, and each answer it read otherwise. It
// answers what the section misses of R13, "" for nothing.
func writeCalibration(w io.Writer, deployment string, c calibrationResult) string {
	fmt.Fprintln(w, "## The judge")
	fmt.Fprintln(w)
	if c.err != nil {
		fmt.Fprintf(w, "The calibration did not run: %v\n\n", c.err)
		return "the judge's calibration did not run"
	}
	ok := c.answers == eval.JudgeCalibrationAnswers && float64(c.agreed)/float64(c.answers) >= eval.JudgeAgreementFloor
	fmt.Fprintf(w, "The judge (%s) gives the agreed label of the two labelling agents to %s of the calibration answers of labels.json; the floor is %.0f%% of %d: %s.\n",
		deployment, rate(c.agreed, c.answers), 100*eval.JudgeAgreementFloor, eval.JudgeCalibrationAnswers, meets(ok))
	for _, d := range c.disagreed {
		fmt.Fprintf(w, "- %s\n", d)
	}
	fmt.Fprintln(w)
	if !ok {
		return fmt.Sprintf("the judge agrees with %s of the labels, under %.0f%%", rate(c.agreed, c.answers), 100*eval.JudgeAgreementFloor)
	}
	return ""
}

// calibrateCommand is `going-eval calibrate`: the judge's calibration alone, which a run also
// makes at its start. It puts each answer of labels.json to the judge, on the chat settings run
// reads, and writes the judge section of a report to stdout; it exits 0 when the judge's
// agreement meets JudgeAgreementFloor and 1 when it does not or the calibration fails.
func calibrateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("going-eval calibrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	deployment := flags.String("judge-deployment", "", "the chat deployment the judge reads answers with (default $DISPATCH_AZURE_CHAT_DEPLOYMENT)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	tasks, err := eval.LoadTasks(eval.Files)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval calibrate: %v\n", err)
		return 1
	}
	seed, err := eval.LoadSeed(eval.Files)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval calibrate: %v\n", err)
		return 1
	}
	chat, err := chatFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "going-eval calibrate: %v\n", err)
		return 1
	}
	*deployment = cmp.Or(*deployment, chat.Deployment)
	j, err := newJudge(chat, *deployment, newLimiter(tokenLimit), seed.Now)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval calibrate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "# going-eval calibrate, %s\n\n", time.Now().UTC().Format(time.RFC3339))
	if problem := writeCalibration(stdout, *deployment, calibrate(ctx, j, tasks)); problem != "" {
		fmt.Fprintf(stdout, "RESULT: the calibration misses: %s.\n", problem)
		return 1
	}
	fmt.Fprintln(stdout, "RESULT: the judge meets R13's agreement floor.")
	return 0
}
