package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// The labeller's validation bound: at least minAgreeing planted cases agree and none disagrees,
// which bounds the disagreement rate at about 2.0% at confidence validationConfidence (the exact
// one-sided Clopper-Pearson bound, 1 − 0.025^(1/183)).
const (
	minAgreeing          = 183
	validationConfidence = 0.975
)

// validationCell counts one class and case type.
type validationCell struct {
	Class     string  `json:"class"`
	Family    string  `json:"family"`
	Agree     int     `json:"agree"`
	Disagree  int     `json:"disagree"`
	Missing   int     `json:"missing"`
	UpperDisc float64 `json:"disagreement_upper_bound"`
}

// validationReport is what validate-labeller finds.
type validationReport struct {
	Seed       int64            `json:"seed"`
	Planted    int              `json:"planted"`
	Cells      []validationCell `json:"cells"`
	ByClass    []validationCell `json:"by_class"`
	ByFamily   []validationCell `json:"by_family"`
	Total      validationCell   `json:"total"`
	Models     map[string]int   `json:"served_models"`
	Mismatches []mismatch       `json:"disagreements"`
	Pass       bool             `json:"pass"`
	Reasons    []string         `json:"reasons,omitempty"`
}

// mismatch is one planted case the labeller got wrong or did not label.
type mismatch struct {
	Copy    string `json:"copy"`
	Kind    string `json:"kind"`
	Family  string `json:"family"`
	Shape   string `json:"shape"`
	Verb    string `json:"verb"`
	Verdict string `json:"verdict"`
}

func runValidateLabeller(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check validate-labeller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	labelsPath := flags.String("labels", "", "the labels file label wrote (required)")
	plantedPath := flags.String("planted", "", "plant's manifest.jsonl (required)")
	seed := flags.Int64("seed", 0, "the registered seed the planted cases were drawn with (required)")
	out := flags.String("out", "", "the report file, Markdown; a .json beside it holds the same (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *labelsPath == "" || *plantedPath == "" || *out == "" || !flagGiven(flags, "seed") || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check validate-labeller: --labels, --planted, --seed and --out are required, and nothing else")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check validate-labeller: %v\n", err)
		return 1
	}
	planted, err := readPlanted(*plantedPath)
	if err != nil {
		return fail(err)
	}
	for _, p := range planted {
		if p.Seed != *seed {
			return fail(fmt.Errorf("planted case %s was drawn with seed %d, not the registered seed %d", p.Copy, p.Seed, *seed))
		}
	}
	labels := map[writeKey]labelRecord{}
	err = eachLine(*labelsPath, func(line []byte, _ int) error {
		var r labelRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		labels[r.key()] = r
		return nil
	})
	if err != nil {
		return fail(err)
	}
	report := validateLabels(planted, labels)
	report.Seed = *seed
	md := renderValidation(report)
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return fail(err)
	}
	if err := writeJSON(strings.TrimSuffix(*out, ".md")+".json", report); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, md)
	if !report.Pass {
		return 1
	}
	return 0
}

// expectedVerdict is the verdict a planted sentence of a kind must get.
func expectedVerdict(kind string) string {
	if kind == kindFault {
		return verdictVacuous
	}
	return verdictNotVacuous
}

// plantedVerdict is the labeller's verdict on a planted case's sentence: the verdict of the one
// sentence of the copy's planted field that reads as the planted sentence, or "" when the copy
// or that sentence has no label.
func plantedVerdict(p plantedCase, labels map[writeKey]labelRecord) string {
	r, ok := labels[writeKey{p.EventID, p.SourceID, p.Copy}]
	if !ok || r.Error != "" {
		return ""
	}
	verdict, found := "", 0
	for _, s := range r.Sentences {
		if s.Field == p.Field && s.Text == p.SentenceProse {
			verdict = s.Verdict
			found++
		}
	}
	if found != 1 {
		return ""
	}
	return verdict
}

// validateLabels compares the labeller's verdict on each planted sentence with its kind, by
// class (fault, control) and case type (surface family). It passes only with at least
// minAgreeing agreeing cases in total, and no disagreeing and no unlabelled case anywhere.
func validateLabels(planted []plantedCase, labels map[writeKey]labelRecord) validationReport {
	report := validationReport{Planted: len(planted), Models: map[string]int{}}
	cells := map[[2]string]*validationCell{}
	classes := map[string]*validationCell{}
	families := map[string]*validationCell{}
	total := &validationCell{Class: "all", Family: "all"}
	for _, p := range planted {
		key := [2]string{p.Kind, p.Family}
		if cells[key] == nil {
			cells[key] = &validationCell{Class: p.Kind, Family: p.Family}
		}
		if classes[p.Kind] == nil {
			classes[p.Kind] = &validationCell{Class: p.Kind, Family: "all"}
		}
		if families[p.Family] == nil {
			families[p.Family] = &validationCell{Class: "all", Family: p.Family}
		}
		verdict := plantedVerdict(p, labels)
		for _, c := range []*validationCell{cells[key], classes[p.Kind], families[p.Family], total} {
			switch verdict {
			case "":
				c.Missing++
			case expectedVerdict(p.Kind):
				c.Agree++
			default:
				c.Disagree++
			}
		}
		if verdict != expectedVerdict(p.Kind) {
			report.Mismatches = append(report.Mismatches, mismatch{Copy: p.Copy, Kind: p.Kind, Family: p.Family, Shape: p.Shape, Verb: p.Verb, Verdict: verdict})
		}
		if r, ok := labels[writeKey{p.EventID, p.SourceID, p.Copy}]; ok {
			for _, call := range r.Calls {
				report.Models[call.Model]++
			}
		}
	}
	for _, kind := range []string{kindFault, kindControl} {
		for _, f := range familyOrder {
			if c := cells[[2]string{kind, f}]; c != nil {
				c.UpperDisc = disagreementBound(c.Disagree, c.Agree+c.Disagree)
				report.Cells = append(report.Cells, *c)
			}
		}
		if c := classes[kind]; c != nil {
			c.UpperDisc = disagreementBound(c.Disagree, c.Agree+c.Disagree)
			report.ByClass = append(report.ByClass, *c)
		}
	}
	for _, f := range familyOrder {
		if c := families[f]; c != nil {
			c.UpperDisc = disagreementBound(c.Disagree, c.Agree+c.Disagree)
			report.ByFamily = append(report.ByFamily, *c)
		}
	}
	total.UpperDisc = disagreementBound(total.Disagree, total.Agree+total.Disagree)
	report.Total = *total
	if total.Agree < minAgreeing {
		report.Reasons = append(report.Reasons, fmt.Sprintf("%d agreeing cases, fewer than %d", total.Agree, minAgreeing))
	}
	for _, c := range report.Cells {
		if c.Disagree > 0 {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%d disagreeing %s cases of %s", c.Disagree, c.Class, c.Family))
		}
		if c.Missing > 0 {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%d unlabelled %s cases of %s", c.Missing, c.Class, c.Family))
		}
	}
	report.Pass = len(report.Reasons) == 0
	return report
}

// disagreementBound is the exact one-sided upper confidence bound on a disagreement rate after x
// disagreements in n cases, at validationConfidence. It is 1 for no case and for x = n.
func disagreementBound(x, n int) float64 { return upperBound(x, n, validationConfidence) }

// renderValidation is the report as Markdown.
func renderValidation(r validationReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# validate-labeller\n\n- seed: %d\n- planted cases: %d\n- rule: at least %d agreeing cases, and no disagreeing or unlabelled case in any class and case type (%d agreeing and 0 disagreeing bound disagreement at %s at %.1f%%)\n",
		r.Seed, r.Planted, minAgreeing, minAgreeing, pct(disagreementBound(0, minAgreeing)), 100*validationConfidence)
	fmt.Fprintln(&b, "- served model ids:")
	for _, m := range sortedKeys(r.Models) {
		label := m
		if label == "" {
			label = "(no reply)"
		}
		fmt.Fprintf(&b, "  - %s: %d calls\n", label, r.Models[m])
	}
	fmt.Fprintf(&b, "\n| Class | Case type | Agree | Disagree | Unlabelled | Disagreement bound (%.1f%%) |\n| --- | --- | ---: | ---: | ---: | ---: |\n", 100*validationConfidence)
	rows := append(append(append(append([]validationCell{}, r.Cells...), r.ByClass...), r.ByFamily...), r.Total)
	for _, c := range rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %s |\n", c.Class, c.Family, c.Agree, c.Disagree, c.Missing, pct(c.UpperDisc))
	}
	if len(r.Mismatches) > 0 {
		fmt.Fprintf(&b, "\n## Disagreeing and unlabelled cases\n\n| Copy | Class | Case type | Shape | Verb | Verdict |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, m := range r.Mismatches {
			v := m.Verdict
			if v == "" {
				v = "(no label)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", m.Copy, m.Kind, m.Family, m.Shape, m.Verb, v)
		}
	}
	if r.Pass {
		fmt.Fprintf(&b, "\nResult: PASS\n")
	} else {
		fmt.Fprintf(&b, "\nResult: FAIL\n")
		for _, reason := range r.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
	}
	return b.String()
}
