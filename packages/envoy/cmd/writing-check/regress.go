package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// regressCase is one line of the regression cases: a body written for the test and the verdict
// each listed sentence of it must get. internal/dispatch/writingcheck/eval/regress-cases.jsonl
// holds the reader's development cases in this shape.
type regressCase struct {
	ID     string           `json:"id"`
	Body   string           `json:"body"`
	Expect []regressVerdict `json:"expect"`
}

// regressField is the field name a case's body is read as.
const regressField = "body"

type regressVerdict struct {
	Sentence string `json:"sentence"`
	Vacuous  bool   `json:"vacuous"`
}

// regressMiss is one sentence whose verdict differed from the expected one.
type regressMiss struct {
	Case     string `json:"case"`
	Sentence string `json:"sentence"`
	Expected bool   `json:"expected_vacuous"`
	Got      string `json:"got"`
}

// regressClass is one class's disagreement: the expected sentences of the class and how many got
// another verdict.
type regressClass struct {
	Class    string  `json:"class"`
	N        int     `json:"n"`
	Disagree int     `json:"disagree"`
	Share    float64 `json:"share"`
	Pass     bool    `json:"pass"`
}

// regressFault is a case the reader did not read whole: its call failed (a timeout, a provider
// error, a malformed reply, a model mismatch), or it read candidates only to the cap. It holds the
// reader's outcome and the expected sentences of the case.
type regressFault struct {
	Case      string `json:"case"`
	Outcome   string `json:"outcome"`
	Sentences int    `json:"sentences"`
}

// regressReport is what regress finds. A fault is no disagreement: the classes count the
// sentences of the cases the reader read, and a run with any fault does not pass.
type regressReport struct {
	Cases   int            `json:"cases"`
	Bound   float64        `json:"repeat_bound"`
	Classes []regressClass `json:"classes"`
	Misses  []regressMiss  `json:"misses"`
	Faults  []regressFault `json:"faults"`
	Pass    bool           `json:"pass"`
}

func runRegress(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check regress", flag.ContinueOnError)
	flags.SetOutput(stderr)
	casesPath := flags.String("cases", "", "the development cases with their expected verdicts, JSONL (required)")
	bound := flags.Float64("repeat-bound", -1, "the repeatability bound: the most share of each class that may disagree (required)")
	out := flags.String("out", "", "a JSON file for the report")
	rf := addReaderFlags(flags, true)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *casesPath == "" || *bound < 0 || *bound > 1 || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check regress: --cases, --repeat-bound (0 to 1) and the reader flags are required, and nothing else")
		return 2
	}
	model, err := rf.model()
	if err != nil {
		fmt.Fprintf(stderr, "writing-check regress: %v\n", err)
		return 2
	}
	if err := harness.NoOutputIsAnInput(harness.Files("--out", *out), append(harness.Files("--cases", *casesPath), rf.files()...)); err != nil {
		fmt.Fprintf(stderr, "writing-check regress: %v\n", err)
		return 2
	}
	cases, err := readRegressCases(*casesPath)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check regress: %v\n", err)
		return 2
	}
	results := make([]regressResult, len(cases))
	eachWrite(ctx, len(cases), rf.concurrency, func() *observedReader { return rf.newReader(model) },
		func(o *observedReader, i int) { results[i] = regressOne(ctx, o, cases[i]) })
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "writing-check regress: interrupted")
		return 1
	}
	report := regressReport{Cases: len(cases), Bound: *bound, Pass: true, Faults: []regressFault{}}
	var n, disagree [2]int
	for i, r := range results {
		if r.fault != "" {
			report.Faults = append(report.Faults, regressFault{Case: cases[i].ID, Outcome: r.fault, Sentences: r.counts[0] + r.counts[1]})
			continue
		}
		for c := range 2 {
			n[c] += r.counts[c]
		}
		for _, m := range r.misses {
			disagree[classIndex(m.Expected)]++
		}
		report.Misses = append(report.Misses, r.misses...)
	}
	for c, name := range []string{verdictVacuous, verdictNotVacuous} {
		cl := regressClass{Class: name, N: n[c], Disagree: disagree[c], Share: share(disagree[c], n[c])}
		cl.Pass = cl.Share <= *bound
		report.Pass = report.Pass && cl.Pass
		report.Classes = append(report.Classes, cl)
	}
	report.Pass = report.Pass && len(report.Faults) == 0
	if *out != "" {
		if err := writeJSON(*out, report); err != nil {
			fmt.Fprintf(stderr, "writing-check regress: %v\n", err)
			return 1
		}
	}
	fmt.Fprint(stdout, renderRegress(report))
	if len(report.Faults) > 0 {
		fmt.Fprintf(stderr, "writing-check regress: %d of %d cases were not read whole (a failed call, or the candidate cap), so the run is not complete; run it again\n", len(report.Faults), len(cases))
		return 1
	}
	if !report.Pass {
		return 1
	}
	return 0
}

func classIndex(vacuous bool) int {
	if vacuous {
		return 0
	}
	return 1
}

// readRegressCases reads the cases and checks that each expected sentence is one prose sentence
// of its body.
func readRegressCases(path string) ([]regressCase, error) {
	var cases []regressCase
	err := eachLine(path, func(line []byte, _ int) error {
		var c regressCase
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		if c.ID == "" || len(c.Expect) == 0 {
			return errors.New("a case needs an id and at least one expected verdict")
		}
		ps := prose.ProseSentences(regressField, c.Body)
		for _, e := range c.Expect {
			if !slices.ContainsFunc(ps, func(s prose.Sentence) bool { return s.Text == e.Sentence }) {
				return fmt.Errorf("case %s: %q is no prose sentence of its body", c.ID, e.Sentence)
			}
		}
		cases = append(cases, c)
		return nil
	})
	if err == nil && len(cases) == 0 {
		err = errors.New("the cases file holds no case")
	}
	return cases, err
}

// regressResult is what the detector did with one case: the sentences whose verdict differs from
// the expected one, the expected sentences of each class, and the reader's outcome where it did
// not read the case whole ("" where it did).
type regressResult struct {
	misses []regressMiss
	counts [2]int
	fault  string
}

// regressOne runs the detector on one case, as the server does (the code stage, then one reader
// call). A sentence is called vacuous when it is a candidate the reader called vacuous. A case the
// reader did not read whole (regressFault) has no miss, only the fault.
func regressOne(ctx context.Context, o *observedReader, c regressCase) regressResult {
	var r regressResult
	for _, e := range c.Expect {
		r.counts[classIndex(e.Vacuous)]++
	}
	cands := candidates.Find(prose.ProseSentences(regressField, c.Body), candidates.Options{})
	called := map[string]bool{}
	if len(cands) > 0 {
		verdicts, outcome := o.Read(ctx, cands)
		if outcome != reader.OutcomeRead {
			r.fault = outcome
			return r
		}
		byID := map[string]string{}
		for _, cand := range cands {
			byID[cand.ID] = cand.Sentence.Text
		}
		for _, v := range verdicts {
			if v.Vacuous {
				called[byID[v.ID]] = true
			}
		}
	}
	for _, e := range c.Expect {
		if called[e.Sentence] != e.Vacuous {
			v := verdictNotVacuous
			if called[e.Sentence] {
				v = verdictVacuous
			}
			r.misses = append(r.misses, regressMiss{Case: c.ID, Sentence: e.Sentence, Expected: e.Vacuous, Got: v})
		}
	}
	return r
}

// renderRegress is the report as text.
func renderRegress(r regressReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "regress: %d cases, repeatability bound %s per class\n\n| Class | Sentences | Disagree | Share | Within bound |\n| --- | ---: | ---: | ---: | --- |\n", r.Cases, pct(r.Bound))
	for _, c := range r.Classes {
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %t |\n", c.Class, c.N, c.Disagree, pct(c.Share), c.Pass)
	}
	for _, m := range r.Misses {
		fmt.Fprintf(&b, "- %s: %q expected vacuous=%t, got %s\n", m.Case, m.Sentence, m.Expected, m.Got)
	}
	if len(r.Faults) > 0 {
		fmt.Fprintf(&b, "\nNot read whole (a failed call, or the candidate cap): %d cases\n", len(r.Faults))
		for _, f := range r.Faults {
			fmt.Fprintf(&b, "- %s: %s, %d expected sentences unread\n", f.Case, f.Outcome, f.Sentences)
		}
	}
	if r.Pass {
		b.WriteString("\nResult: PASS\n")
	} else {
		b.WriteString("\nResult: FAIL\n")
	}
	return b.String()
}
