package main

// lists writes the case lists that `harness score --case-list` reads, from the build and the
// labels of the question check and the plain check, and calls no model. README.md, "lists", says
// what each list holds.

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
)

// The lists that lists writes, one case id on each line, in id order.
const (
	dsListQuestion = "question.txt"
	dsListAnswer   = "answer.txt"
	dsListPlain    = "plain.txt"
)

// dsPlainUnderstood is the plain check's final label of a plain text that the owner read and
// understood: he acted on it, or disputed its substance and not its wording.
const dsPlainUnderstood = "understood"

// dsListCase is the part of a case that lists reads.
type dsListCase struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	Part  string `json:"part"`
}

// dsQuestionVerdict is one line of the question check's verdicts: whether the owner's question
// is about the agent's words. about_words is absent for a case the check left unlabelled.
type dsQuestionVerdict struct {
	ID         string `json:"id"`
	AboutWords *bool  `json:"about_words"`
	Part       string `json:"part"`
}

// dsPlainLabel is one line of the plain check's labels: what the owner did after a plain text
// (label), and the final label after his questions and complaints were split (final).
type dsPlainLabel struct {
	CaseID string `json:"case_id"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Final  string `json:"final"`
	Part   string `json:"part"`
}

// dsLists is what lists reads: the build's cases by id, and the labels of their development part.
type dsLists struct {
	path  string
	cases map[string]dsListCase
	// about are the development question cases whose verdict says about_words true, and
	// verdicts the development verdicts by about_words: true, false or unlabelled.
	about    map[string]bool
	verdicts map[string]int
	// understood are the development plain cases whose final label is dsPlainUnderstood, and
	// finals and labels the development labels by final label and by label.
	understood     map[string]bool
	finals, labels map[string]int
}

// labelled is the case of a line that labels case id of group, and names part ("" for none). It
// refuses a case the build does not hold, a case of another group or of another part, and a case
// seen already holds.
func (l *dsLists) labelled(id, group, part string, seen map[string]bool) (dsListCase, error) {
	c, ok := l.cases[id]
	switch {
	case !ok:
		return c, fmt.Errorf("case %q is not in %s: the labels are of another build", id, l.path)
	case c.Group != group:
		return c, fmt.Errorf("case %s is of group %s, not %s", id, c.Group, group)
	case part != "" && part != c.Part:
		return c, fmt.Errorf("the line puts case %s in the %s part, and the build in the %s part", id, part, c.Part)
	case seen[id]:
		return c, fmt.Errorf("case %s is labelled twice", id)
	}
	seen[id] = true
	return c, nil
}

// readDSLists reads the build's cases at casesPath, the question check's verdicts and the plain
// check's labels.
func readDSLists(casesPath, verdictsPath, labelsPath string) (*dsLists, error) {
	l := &dsLists{path: dsCasesFile(casesPath), cases: map[string]dsListCase{}, about: map[string]bool{}, verdicts: map[string]int{},
		understood: map[string]bool{}, finals: map[string]int{}, labels: map[string]int{}}
	err := eachGzipLine(l.path, func(line []byte) error {
		var c dsListCase
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		l.cases[c.ID] = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	err = eachLine(verdictsPath, func(line []byte, _ int) error {
		var v dsQuestionVerdict
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		c, err := l.labelled(v.ID, dsGroupQuestion, v.Part, seen)
		switch {
		case err != nil || c.Part != dsDevelopment:
			return err
		case v.AboutWords == nil:
			l.verdicts["unlabelled"]++
		case *v.AboutWords:
			l.about[v.ID] = true
			l.verdicts["true"]++
		default:
			l.verdicts["false"]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen = map[string]bool{}
	err = eachLine(labelsPath, func(line []byte, _ int) error {
		var p dsPlainLabel
		if err := json.Unmarshal(line, &p); err != nil {
			return err
		}
		id := cmp.Or(p.CaseID, p.ID)
		if p.Final == "" {
			return fmt.Errorf("the label of case %q holds no final label: give the plain check's final labels", id)
		}
		c, err := l.labelled(id, dsGroupPlain, p.Part, seen)
		if err != nil || c.Part != dsDevelopment {
			return err
		}
		l.finals[p.Final]++
		l.labels[p.Label]++
		if p.Final == dsPlainUnderstood {
			l.understood[id] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// dsCountsText is counts as "<value> <n>" by value, comma-separated.
func dsCountsText(counts map[string]int) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		out = append(out, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(out, ", ")
}

func runDatasetLists(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset lists", flag.ContinueOnError)
	flags.SetOutput(stderr)
	casesPath := flags.String("cases", "", "the cases build wrote: the build directory or its cases.jsonl.gz (required)")
	verdictsPath := flags.String("verdicts", "", "the question check's verdicts: JSONL, each line a case's id, about_words and part (required)")
	labelsPath := flags.String("plain-labels", "", "the plain check's labels: JSONL, each line a case's case_id (or id), label, final label and part (required)")
	out := flags.String("out", "", "the directory for question.txt, answer.txt and plain.txt (required; keep it under $TMPDIR)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *casesPath == "" || *verdictsPath == "" || *labelsPath == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check dataset lists: --cases, --verdicts, --plain-labels and --out are required, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset lists: %v\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset lists: %v\n", err)
		return 1
	}
	l, err := readDSLists(*casesPath, *verdictsPath, *labelsPath)
	if err != nil {
		return fail(err)
	}
	lists := map[string][]string{}
	cases := map[string]int{}
	for _, c := range l.cases {
		if c.Part != dsDevelopment {
			continue
		}
		cases[c.Group]++
		switch {
		case c.Group == dsGroupQuestion && l.about[c.ID]:
			lists[dsListQuestion] = append(lists[dsListQuestion], c.ID)
		case c.Group == dsGroupAnswer:
			lists[dsListAnswer] = append(lists[dsListAnswer], c.ID)
		case c.Group == dsGroupPlain && l.understood[c.ID]:
			lists[dsListPlain] = append(lists[dsListPlain], c.ID)
		}
	}
	sum := func(counts map[string]int) int {
		n := 0
		for _, c := range counts {
			n += c
		}
		return n
	}
	// A development case that no line labels is counted as such.
	if n := cases[dsGroupQuestion] - sum(l.verdicts); n > 0 {
		l.verdicts["no verdict"] = n
	}
	if n := cases[dsGroupPlain] - sum(l.finals); n > 0 {
		l.finals["no label"] = n
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	of := map[string]string{
		dsListQuestion: fmt.Sprintf("the development question cases whose question is about the agent's words: %d of %d (about_words: %s)",
			len(lists[dsListQuestion]), cases[dsGroupQuestion], dsCountsText(l.verdicts)),
		dsListAnswer: fmt.Sprintf("the development answer cases: %d", len(lists[dsListAnswer])),
		dsListPlain: fmt.Sprintf("the development plain cases the owner understood (final label %s): %d of %d (final: %s; label: %s)",
			dsPlainUnderstood, len(lists[dsListPlain]), cases[dsGroupPlain], dsCountsText(l.finals), dsCountsText(l.labels)),
	}
	for _, name := range []string{dsListQuestion, dsListAnswer, dsListPlain} {
		ids := lists[name]
		slices.Sort(ids)
		var data []byte
		if len(ids) > 0 {
			data = []byte(strings.Join(ids, "\n") + "\n")
		}
		if err := os.WriteFile(filepath.Join(*out, name), data, 0o600); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s: %s\n%s sha256: %s\n", name, of[name], name, sha256Hex(data))
	}
	return 0
}
