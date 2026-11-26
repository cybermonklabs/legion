package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/going/eval"
)

// newJudge is the judge on the chat deployment named deployment, at the endpoint and on the
// keyless Azure sign-in of chat, through limits, reading facts written for the moment written.
func newJudge(chat chatSettings, deployment string, limits *limiter, written time.Time) (*judge, error) {
	token, err := chatToken(chat)
	if err != nil {
		return nil, fmt.Errorf("the judge's %w", err)
	}
	model := agentloop.Model{Endpoint: agentloop.Endpoint{Kind: agentloop.OpenAIChat, BaseURL: chat.Endpoint, Token: token}, ID: deployment}
	if chat.APIVersion != "" {
		model.Endpoint.Query = url.Values{"api-version": {chat.APIVersion}}
	}
	return &judge{model: model, limits: limits, written: written}, nil
}

// renderedLabel is one calibration answer of labels.json as the judge reads it: its task, the
// task's words and facts, and the answer rendered against the seed as it stands before any task
// (chips.RenderAnswer), with the label the two labelling agents agreed.
type renderedLabel struct {
	Task     string   `json:"task"`
	Words    string   `json:"words"`
	Facts    []string `json:"facts"`
	Rendered string   `json:"rendered"`
	label    string
}

// renderedLabels is each calibration answer of labels.json as the judge reads it, in its order.
func renderedLabels(tasks []eval.Task, seed *eval.Seed) ([]renderedLabel, error) {
	labels, err := eval.LoadLabels(eval.Files, tasks)
	if err != nil {
		return nil, err
	}
	rendered := make([]renderedLabel, len(labels.Answers))
	for i, answer := range labels.Answers {
		task := tasks[slices.IndexFunc(tasks, func(t eval.Task) bool { return t.ID == answer.Task })]
		rendered[i] = renderedLabel{
			Task: answer.Task, Words: task.Words, Facts: task.Grade.Facts, Rendered: chips.RenderAnswer(answer.Answer, seed), label: answer.Label,
		}
	}
	return rendered, nil
}

// printRenderedLabels writes each calibration answer of labels.json as the judge reads it
// (renderedLabels), one JSON object a line: {task, words, facts, rendered}.
func printRenderedLabels(w io.Writer, tasks []eval.Task, seed *eval.Seed) error {
	answers, err := renderedLabels(tasks, seed)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	for _, answer := range answers {
		if err := encoder.Encode(answer); err != nil {
			return err
		}
	}
	return nil
}

// writeCalibration writes the judge section of a report: the judge's agreement with the agreed
// labels of labels.json against JudgeAgreementFloor, and each answer it read otherwise. It
// answers what the section misses of R13, "" for nothing.
func writeCalibration(w io.Writer, deployment string, c calibrationResult) string {
	fmt.Fprintln(w, "## The judge")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The judge reads each answer with its links rendered as the bar's chips (going/chips RenderAnswer).")
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
// makes at its start. It puts each answer of labels.json to the judge, rendered against the seed,
// on the chat settings run reads, and writes the judge section of a report to stdout; it exits 0
// when the judge's agreement meets JudgeAgreementFloor and 1 when it does not or the calibration
// fails. With --print-rendered-labels it calls no model: it writes each calibration answer as the
// judge reads it (printRenderedLabels) and exits 0.
func calibrateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("going-eval calibrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	deployment := flags.String("judge-deployment", "", "the chat deployment the judge reads answers with (default $DISPATCH_AZURE_CHAT_DEPLOYMENT)")
	printRendered := flags.Bool("print-rendered-labels", false, "print each calibration answer of labels.json as the judge reads it, rendered against the seed, one JSON object a line, and call no model")
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
	if *printRendered {
		if err := printRenderedLabels(stdout, tasks, seed); err != nil {
			fmt.Fprintf(stderr, "going-eval calibrate: %v\n", err)
			return 1
		}
		return 0
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
	if problem := writeCalibration(stdout, *deployment, calibrate(ctx, j, tasks, seed)); problem != "" {
		fmt.Fprintf(stdout, "RESULT: the calibration misses: %s.\n", problem)
		return 1
	}
	fmt.Fprintln(stdout, "RESULT: the judge meets R13's agreement floor.")
	return 0
}
