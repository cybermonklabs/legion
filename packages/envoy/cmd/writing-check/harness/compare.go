package harness

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// JudgementAgreement is how often two runs gave one judgement the same grade on the cases both
// graded: the same verdict; for a list judgement, counts within listSlack of each other; for
// answer_start, an answer found in both within answerStartSlack words, or in neither.
type JudgementAgreement struct {
	Judgement Judgement `json:"judgement"`
	Agreement Share     `json:"agreement"`
	// AnyAgreement is a list judgement's agreement on whether the text has any finding, and
	// MeanCountDifference the mean absolute difference of the two counts.
	AnyAgreement        *Share   `json:"any_agreement,omitempty"`
	MeanCountDifference *float64 `json:"mean_count_difference,omitempty"`
}

// listSlack is how far apart two counts of a list judgement may be and still agree.
const listSlack = 1

// RunSummary is one compared run: its directory, its grader's input modes and its model use.
type RunSummary struct {
	Dir          string     `json:"dir"`
	GraderInputs string     `json:"grader_inputs"`
	Models       []ModelUse `json:"models"`
	WallSeconds  float64    `json:"wall_seconds"`
	CallSeconds  float64    `json:"call_seconds"`
}

// Repeatability is the agreement of two runs of the same cases, by judgement: of two runs of one
// grader, its repeatability; of two graders, their agreement. scores.md gives it beside each
// score (score --repeatability).
type Repeatability struct {
	A          RunSummary           `json:"a"`
	B          RunSummary           `json:"b"`
	Cases      int                  `json:"cases"`
	Judgements []JudgementAgreement `json:"judgements"`
}

// of is the agreement of judgement j, nil when the runs share no grade of it.
func (r *Repeatability) of(j Judgement) *JudgementAgreement {
	if r == nil {
		return nil
	}
	for i := range r.Judgements {
		if r.Judgements[i].Judgement == j {
			return &r.Judgements[i]
		}
	}
	return nil
}

func readRepeatability(path string) (*Repeatability, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Repeatability
	if err := json.Unmarshal(data, &r); err != nil || len(r.Judgements) == 0 {
		return nil, fmt.Errorf("%s is not a repeatability.json of harness compare (%v)", path, err)
	}
	return &r, nil
}

// agrees is whether two grades of judgement j agree, and whether both are grades to compare (no
// error, no grade by construction).
func agrees(j Judgement, a, b *Grade) (agree, comparable bool) {
	if a == nil || b == nil || a.Error != "" || b.Error != "" || a.ByConstruction || b.ByConstruction {
		return false, false
	}
	spec := judgementSpecs[j]
	switch {
	case spec.list != "":
		return math.Abs(float64(a.Count-b.Count)) <= listSlack, true
	case spec.verdict != "":
		if a.Verdict == nil || b.Verdict == nil {
			return false, false
		}
		return *a.Verdict == *b.Verdict, true
	}
	if a.Found == nil || b.Found == nil || a.WordsBefore == nil || b.WordsBefore == nil {
		return false, false
	}
	foundA, foundB := *a.Found && *a.WordsBefore >= 0, *b.Found && *b.WordsBefore >= 0
	if !foundA || !foundB {
		return foundA == foundB, true
	}
	return math.Abs(float64(*a.WordsBefore-*b.WordsBefore)) <= answerStartSlack, true
}

// compareRuns is the agreement of runs a and b by judgement, over the cases both hold with no
// error, each interval over the cases' split groups.
func compareRuns(a, b map[string]CaseResult, seed uint64, draws int) (int, []JudgementAgreement) {
	var ids []string
	for id, ra := range a {
		if rb, ok := b[id]; ok && ra.Error == "" && rb.Error == "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out []JudgementAgreement
	for _, j := range Judgements {
		var groups []string
		var hits, anyHits []bool
		var diff float64
		for _, id := range ids {
			ga, gb := a[id].Grades[j], b[id].Grades[j]
			agree, ok := agrees(j, ga, gb)
			if !ok {
				continue
			}
			groups = append(groups, a[id].SplitGroup)
			hits = append(hits, agree)
			anyHits = append(anyHits, (ga.Count > 0) == (gb.Count > 0))
			diff += math.Abs(float64(ga.Count - gb.Count))
		}
		if len(groups) == 0 {
			continue
		}
		ja := JudgementAgreement{Judgement: j, Agreement: newShare(groups, hits, draws, seed)}
		if judgementSpecs[j].list != "" {
			anyShare := newShare(groups, anyHits, draws, seed)
			d := diff / float64(len(groups))
			ja.AnyAgreement, ja.MeanCountDifference = &anyShare, &d
		}
		out = append(out, ja)
	}
	return len(ids), out
}

// runSummary reads a run directory's parameters and model use.
func runSummary(dir string, results map[string]CaseResult, cfg Config) RunSummary {
	s := RunSummary{Dir: dir}
	if data, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
		var p RunParams
		if json.Unmarshal(data, &p) == nil {
			s.GraderInputs = p.GraderInputs
		}
	}
	var list []CaseResult
	for _, r := range results {
		if r.Error == "" {
			list = append(list, r)
			change, measure := r.calls()
			for _, c := range append(change, measure...) {
				s.CallSeconds += c.Seconds
			}
		}
	}
	s.Models = modelUse(list, cfg)
	if data, err := os.ReadFile(filepath.Join(dir, "scores.json")); err == nil {
		var f Figures
		if json.Unmarshal(data, &f) == nil {
			s.WallSeconds = f.Run.WallSeconds
		}
	}
	return s
}

func runCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dirA := fs.String("a", "", "the first run directory (required)")
	dirB := fs.String("b", "", "the second run directory (required)")
	out := fs.String("out", "", "the repeatability.json to write (required; keep it under $TMPDIR)")
	seed := fs.Uint64("seed", DefaultSeed, "the seed of the bootstrap")
	draws := fs.Int("draws", DefaultDraws, "the bootstrap's resamples")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dirA == "" || *dirB == "" || *out == "" || fs.NArg() > 0 || *draws < 1 {
		fmt.Fprintln(stderr, "writing-check harness compare: --a, --b and --out are required, and nothing else")
		return 2
	}
	if err := OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check harness compare: %v\n", err)
		return 2
	}
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "writing-check harness compare: %v\n", err)
		return 1
	}
	a, err := readResults(filepath.Join(*dirA, "cases.jsonl"))
	if err != nil {
		fmt.Fprintf(stderr, "writing-check harness compare: %v\n", err)
		return 1
	}
	b, err := readResults(filepath.Join(*dirB, "cases.jsonl"))
	if err != nil {
		fmt.Fprintf(stderr, "writing-check harness compare: %v\n", err)
		return 1
	}
	r := Repeatability{A: runSummary(*dirA, a, cfg), B: runSummary(*dirB, b, cfg)}
	r.Cases, r.Judgements = compareRuns(a, b, *seed, *draws)
	if r.Cases == 0 {
		fmt.Fprintln(stderr, "writing-check harness compare: the two runs share no case with a result")
		return 1
	}
	if err := writeJSON(*out, r); err != nil {
		fmt.Fprintf(stderr, "writing-check harness compare: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, r.markdown())
	return 0
}

// markdown is the agreement table and each run's model use.
func (r Repeatability) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Agreement of two runs on %d cases\n\nA: %s (grader inputs %s)\nB: %s (grader inputs %s)\n\n", r.Cases, r.A.Dir, r.A.GraderInputs, r.B.Dir, r.B.GraderInputs)
	fmt.Fprintf(&b, "Agreement: the same verdict; for a list, counts within %d; for answer_start, both find the answer within %d words, or neither.\n\n", listSlack, answerStartSlack)
	b.WriteString("| Judgement | Agreement [95% interval] | List: any finding in both or neither | List: mean count difference |\n| --- | --- | --- | --- |\n")
	for _, j := range r.Judgements {
		anyText, d := "-", "-"
		if j.AnyAgreement != nil {
			anyText = shareText(j.AnyAgreement)
		}
		if j.MeanCountDifference != nil {
			d = num(*j.MeanCountDifference)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", j.Judgement, shareText(&j.Agreement), anyText, d)
	}
	for _, run := range []RunSummary{r.A, r.B} {
		fmt.Fprintf(&b, "\n%s: wall %.0f s, calls %.0f s\n\n| Model | Use | Calls | Input tokens | Output tokens | Cost (USD) |\n| --- | --- | ---: | ---: | ---: | ---: |\n", run.Dir, run.WallSeconds, run.CallSeconds)
		for _, u := range run.Models {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %.2f |\n", u.Model, u.Use, u.Calls, u.Input, u.Output, u.CostUSD)
		}
	}
	return b.String()
}
