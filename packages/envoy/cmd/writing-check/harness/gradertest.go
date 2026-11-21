package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
)

// The two kinds of a planted output: one a correct grader marks as failing its judgement, and
// one it marks as passing.
const (
	PlantedWrong = "wrong"
	PlantedRight = "right"
)

// answerStartSlack is how many words answer_start's words before the answer may be off by from
// a planted output's and still be marked correct.
const answerStartSlack = 5

// Planted is one planted output of the grader test: a line of the planted file, written by hand
// from a development case.
type Planted struct {
	ID        string    `json:"id"`
	Judgement Judgement `json:"judgement"`
	CaseID    string    `json:"case_id"`
	// Planted is PlantedWrong or PlantedRight.
	Planted string `json:"planted"`
	// Output is what the judgement grades: the fresh reader's answer (answered,
	// first_paragraph), the new text (lost, added, phrases, undefined, answer_start, harder) or
	// the definition (define).
	Output string `json:"output"`
	// Question replaces the case's question (define: his question with the words he selected).
	Question string `json:"question,omitempty"`
	// Original replaces the case's text as the original text (lost, added, harder), and as the
	// text the fresh reader read (answered).
	Original string `json:"original,omitempty"`
	// Marker is what the planted fault of a wrong list output is: a correct grader lists an
	// item whose quote or note holds it, case aside.
	Marker string `json:"marker,omitempty"`
	// WordsBefore is the words before the answer in an answer_start output.
	WordsBefore *int `json:"words_before,omitempty"`
}

// expectation is what a correct grader answers for p.
func (p Planted) expectation() string {
	wrong := p.Planted == PlantedWrong
	switch p.Judgement {
	case JAnswered, JFirstParagraph, JDefine:
		if wrong {
			return "does not answer"
		}
		return "answers"
	case JHarder:
		if wrong {
			return "harder to read"
		}
		return "not harder to read"
	case JAnswerStart:
		return fmt.Sprintf("answer after %d words (±%d)", *p.WordsBefore, answerStartSlack)
	}
	if wrong {
		return fmt.Sprintf("lists %q", p.Marker)
	}
	return "lists nothing"
}

// check refuses a planted line the test cannot mark.
func (p Planted) check() error {
	spec, ok := judgementSpecs[p.Judgement]
	switch {
	case !ok:
		return fmt.Errorf("planted %s: no judgement %q", p.ID, p.Judgement)
	case p.Planted != PlantedWrong && p.Planted != PlantedRight:
		return fmt.Errorf("planted %s: planted is %q, not %s or %s", p.ID, p.Planted, PlantedWrong, PlantedRight)
	case strings.TrimSpace(p.Output) == "":
		return fmt.Errorf("planted %s: no output", p.ID)
	case p.Judgement == JAnswerStart && p.WordsBefore == nil:
		return fmt.Errorf("planted %s: an answer_start output names its words_before", p.ID)
	case spec.list != "" && p.Planted == PlantedWrong && strings.TrimSpace(p.Marker) == "":
		return fmt.Errorf("planted %s: a wrong %s output names its marker", p.ID, p.Judgement)
	}
	return nil
}

// correct is whether grade marks p as a correct grader does.
func (p Planted) correct(g Grade) bool {
	if g.Error != "" {
		return false
	}
	wrong := p.Planted == PlantedWrong
	switch p.Judgement {
	case JAnswered, JFirstParagraph, JDefine:
		return g.Verdict != nil && *g.Verdict == !wrong
	case JHarder:
		return g.Verdict != nil && *g.Verdict == wrong
	case JAnswerStart:
		if g.Found == nil || !*g.Found || g.WordsBefore == nil || *g.WordsBefore < 0 {
			return false
		}
		d := *g.WordsBefore - *p.WordsBefore
		return d >= -answerStartSlack && d <= answerStartSlack
	}
	if !wrong {
		return g.Count == 0
	}
	marker := strings.ToLower(p.Marker)
	return slices.ContainsFunc(g.Items, func(it Item) bool {
		return strings.Contains(strings.ToLower(it.Quote), marker) || strings.Contains(strings.ToLower(it.Note), marker)
	})
}

// input is the judgement's input for p on its case.
func (p Planted) input(ds *Dataset, c *Case, session claude.SessionText) GradeInput {
	in := GradeInput{Judgement: p.Judgement, Reply: c.Group == GroupAnswer, Question: c.PersonQuestion(), Record: ds.RecordPart(c), Session: session}
	if p.Question != "" {
		in.Question = p.Question
	}
	original := c.Agent.Text
	if p.Original != "" {
		original = p.Original
	}
	switch p.Judgement {
	case JAnswered:
		in.Answer, in.Text = p.Output, original
	case JFirstParagraph:
		in.Answer = p.Output
	case JDefine:
		in.Definition = p.Output
	case JLost, JAdded, JHarder:
		in.Original, in.Text = original, p.Output
	default:
		in.Text = p.Output
	}
	return in
}

// readPlanted reads the planted file.
func readPlanted(path string) ([]Planted, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Planted
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var p Planted
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if err := p.check(); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, scanner.Err()
}

// PlantedResult is one planted output's grade and mark.
type PlantedResult struct {
	Planted Planted `json:"planted"`
	Expect  string  `json:"expect"`
	Grade   Grade   `json:"grade"`
	Correct bool    `json:"correct"`
}

func runGraderTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness grader-test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plantedPath := fs.String("planted", "", "the planted outputs, JSONL, written by hand from development cases (required; keep it under $TMPDIR)")
	casesDir := fs.String("cases", "", "the dataset build directory (required)")
	sessionsDir := fs.String("sessions", "", "the session directory `writing-check dataset sessions` wrote (required)")
	out := fs.String("out", "", "the directory for the test's results (required; keep it under $TMPDIR)")
	sessionChars := fs.Int("session-chars", 30000, "the bytes of a case's session, its newest, that a grading call reads")
	inputsFlag := fs.String("grader-inputs", "final", "the grader's input modes: final, full or trimmed, then any ,<judgement>=full|trimmed")
	graderFlag := fs.String("grader", "", "the grading model of this test in place of candidates.yaml's roles.grader, with the same fallbacks: a Claude model with a row in prices.yaml that plays no other role; a candidate may grade here, since no candidate wrote a planted output (default: roles.grader)")
	addedGrader := fs.String("added-grader", AddedOld, "the added grader, as score takes it: old, inputs-first, verify or inputs-first-verify")
	mf := addModelFlags(fs)
	mf.addPaceFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "writing-check harness grader-test: %s\n", msg)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check harness grader-test: %v\n", err)
		return 1
	}
	if *plantedPath == "" || *casesDir == "" || *sessionsDir == "" || *out == "" || fs.NArg() > 0 {
		return usage("--planted, --cases, --sessions and --out are required, and nothing else")
	}
	if !slices.Contains(AddedGraders, *addedGrader) {
		return usage("--added-grader is " + strings.Join(AddedGraders, ", "))
	}
	if err := OutsideRepository("--out", *out); err != nil {
		return usage(err.Error())
	}
	inputs, err := ParseGraderInputs(*inputsFlag)
	if err != nil {
		return usage(err.Error())
	}
	cfg, err := LoadConfig()
	if err != nil {
		return fail(err)
	}
	if cfg, err = cfg.WithGrader(*graderFlag, true); err != nil {
		return usage(err.Error())
	}
	graderID := cfg.Models.Roles.Grader.ID
	candidate := cfg.IsCandidate(graderID)
	planted, err := readPlanted(*plantedPath)
	if err != nil {
		return usage(err.Error())
	}
	ds, err := LoadDataset(*casesDir, "", docLimit)
	if err != nil {
		return fail(err)
	}
	var ids []string
	for _, p := range planted {
		c := ds.Case(p.CaseID)
		switch {
		case c == nil:
			return usage(fmt.Sprintf("planted %s names case %s, which the dataset does not hold", p.ID, p.CaseID))
		case c.Part != PartDevelopment:
			return usage(fmt.Sprintf("planted %s is written from case %s of the %s part: planted outputs come from development cases only", p.ID, p.CaseID, c.Part))
		}
		if !slices.Contains(ids, c.ID) {
			ids = append(ids, c.ID)
		}
	}
	for _, j := range Judgements {
		var kinds []string
		for _, p := range planted {
			if p.Judgement == j {
				kinds = append(kinds, p.Planted)
			}
		}
		if !slices.Contains(kinds, PlantedWrong) || !slices.Contains(kinds, PlantedRight) {
			return usage(fmt.Sprintf("the planted file holds no wrong output and right output both for judgement %s", j))
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	sessions, err := claude.CachedExcerpts(*sessionsDir, ids, *sessionChars, filepath.Join(*out, "session-excerpts.jsonl.gz"))
	if err != nil {
		return fail(err)
	}
	runner, err := mf.runner()
	if err != nil {
		return usage(err.Error())
	}
	grader, err := mf.grader(cfg, inputs, runner)
	if err != nil {
		return usage(err.Error())
	}
	grader.Added = *addedGrader
	started := time.Now()
	results := make([]PlantedResult, len(planted))
	var wg sync.WaitGroup
	for i, p := range planted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := ds.Case(p.CaseID)
			g := grader.Grade(ctx, p.input(ds, c, sessions[c.ID]))
			results[i] = PlantedResult{Planted: p, Expect: p.expectation(), Grade: g, Correct: p.correct(g)}
		}()
	}
	wg.Wait()
	uncached := runner.Pacer.Use()
	promptsSHA := PromptsDigest()
	report := graderReport(results, graderID, candidate, grader, promptsSHA, time.Since(started)) + paceText(mf.uncachedPerMinute, uncached)
	if err := writeJSON(filepath.Join(*out, "grader-test.json"), map[string]any{"grader": graderID, "grader_is_candidate": candidate, "grader_inputs": inputs.String(),
		"added_grader": *addedGrader, "prompts_sha256": promptsSHA,
		"uncached_input_per_minute": mf.uncachedPerMinute, "uncached_input": uncached, "results": results}); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "grader-test.md"), []byte(report), 0o600); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, report)
	errored := 0
	for _, r := range results {
		if r.Grade.Error != "" {
			errored++
			fmt.Fprintf(stderr, "writing-check harness grader-test: %s: %s\n", r.Planted.ID, r.Grade.Error)
		}
	}
	// A grade the call did not give marks nothing of the grader, so the test is not complete.
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("stopped (%w): %d of %d planted outputs have no grade, so the test is not complete; run it again", err, errored, len(results)))
	}
	if errored > 0 {
		return fail(fmt.Errorf("%d of %d planted outputs have no grade, as their calls failed, so the test is not complete; run it again", errored, len(results)))
	}
	return 0
}

// graderReport is the grader test's table: for each judgement, the wrong and the right outputs
// it marked correctly, of those it graded, and the outputs whose call failed and gave no grade,
// then each output with what a correct grader answers and what it answered, then what the calls
// cost. candidate says the grader is a candidate of candidates.yaml. Under them the report names
// the grader's input modes and its added grader's variant, whatever they are, defaults included,
// and promptsSHA, the prompts' digest as scores.md names it (PromptsDigest).
func graderReport(results []PlantedResult, graderID string, candidate bool, grader *Grader, promptsSHA string, elapsed time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Grader test: %s\n\n", graderID)
	if candidate {
		fmt.Fprintf(&b, "%s is a candidate of candidates.yaml. It grades here because no candidate wrote a planted output; `harness score` refuses it as the grader (R2).\n\n", graderID)
	}
	fmt.Fprintf(&b, "The grader's inputs are %s (--grader-inputs). The added grader is %s (--added-grader). Prompts SHA-256 %s.\n\n", grader.Inputs, grader.Added, promptsSHA)
	b.WriteString("| Judgement | Wrong outputs marked correctly | Right outputs marked correctly | No grade (the call failed) | Correct / graded |\n| --- | ---: | ---: | ---: | ---: |\n")
	all, allCorrect, allErrored := 0, 0, 0
	for _, j := range Judgements {
		var wrong, wrongOK, right, rightOK, errored int
		for _, r := range results {
			switch {
			case r.Planted.Judgement != j:
				continue
			case r.Grade.Error != "":
				errored++
			case r.Planted.Planted == PlantedWrong:
				wrong++
				if r.Correct {
					wrongOK++
				}
			default:
				right++
				if r.Correct {
					rightOK++
				}
			}
		}
		all += wrong + right
		allCorrect += wrongOK + rightOK
		allErrored += errored
		fmt.Fprintf(&b, "| %s | %d of %d | %d of %d | %d | %d / %d |\n", j, wrongOK, wrong, rightOK, right, errored, wrongOK+rightOK, wrong+right)
	}
	fmt.Fprintf(&b, "| all | | | %d | %d / %d |\n\n| Output | Judgement | Planted | A correct grader | The grader | Correct |\n| --- | --- | --- | --- | --- | --- |\n", allErrored, allCorrect, all)
	var total Call
	for _, r := range results {
		for _, c := range r.Grade.ownCalls() {
			total = addCall(total, c)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %t |\n", r.Planted.ID, r.Planted.Judgement, r.Planted.Planted, r.Expect, gradeText(r.Grade), r.Correct)
	}
	fmt.Fprintf(&b, "\nModel cost: %.2f USD, refused calls included; prompt tokens %d at the input price, %d written to the cache, %d read from it; wall time %s.\n",
		total.CostUSD, total.Input, total.CacheWrite, total.CacheRead, elapsed.Round(time.Second))
	return b.String()
}

// gradeText is a grade in a few words.
func gradeText(g Grade) string {
	switch {
	case g.Error != "":
		return "error"
	case g.Verdict != nil:
		return fmt.Sprintf("%t", *g.Verdict)
	case g.Found != nil:
		if g.WordsBefore != nil {
			return fmt.Sprintf("found %t, %d words before", *g.Found, *g.WordsBefore)
		}
		return fmt.Sprintf("found %t", *g.Found)
	case g.Verify != nil:
		return fmt.Sprintf("%d listed, %d kept after the check", len(g.Verify.Listed), g.Count)
	}
	return fmt.Sprintf("%d listed", g.Count)
}
