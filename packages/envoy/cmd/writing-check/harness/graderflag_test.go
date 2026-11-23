package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// A grading call that fails gives no grade, which marks nothing of the grader: the report counts
// those outputs apart from the ones it graded, and the test exits 1, since it is not complete.
func TestGraderTestCountsAFailedCallApartAndFails(t *testing.T) {
	build, sessions := writeFixture(t)
	refused := modeltest.Rule{Contains: markDefinition, Reply: modeltest.Reply{Status: http.StatusBadRequest, Text: "the stand-in refuses to grade the definition"}}
	stand := modeltest.New(t, append([]modeltest.Rule{refused}, allJudgementRules(t)...)...)
	code, stdout, stderr := runHarness("grader-test", "--planted", writePlanted(t), "--cases", build, "--sessions", sessions, "--out", t.TempDir(),
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t))
	if want := fmt.Sprintf("2 of %d planted outputs have no grade", 2*len(Judgements)); code != 1 || !strings.Contains(stderr, want) {
		t.Errorf("grader-test with the definition's calls refused = %d, want 1 saying %q:\n%s", code, want, stderr)
	}
	if !strings.Contains(stdout, "| define | 0 of 0 | 0 of 0 | 2 | 0 / 0 |\n") {
		t.Errorf("the report does not count the two outputs with no grade apart:\n%s", stdout)
	}
}

// The judgements graderRules leaves out, which grader-test gives: change 1 gives none of them.
const (
	markLost  = "Find each fact of the original text that the new text lost."
	markAdded = "Find each fact of the new text that its inputs do not hold."
)

// allJudgementRules answer every judgement, the three that change 1 never gives as well.
func allJudgementRules(t *testing.T) []modeltest.Rule {
	return append(graderRules(t),
		modeltest.Rule{Contains: markLost, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"lost": []any{}})}},
		modeltest.Rule{Contains: markAdded, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"added": []any{}})}},
		modeltest.Rule{Contains: markDefinition, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{
			"1_asked": "The meaning of the words.", "2_given": "The meaning.", "3_restates_words": false, "4_points_elsewhere": false,
			"5_other_words": false, "6_contradicts_record": false, "7_reason": "It answers.", "8_answers": true})}},
	)
}

// writePlanted writes a planted file of a wrong and a right output for every judgement, on the
// fixture's development cases.
func writePlanted(t *testing.T) string {
	t.Helper()
	words := 6
	var b strings.Builder
	for _, j := range Judgements {
		id := "question/comment:q1@11"
		if j == JFirstParagraph || j == JAnswerStart {
			id = "answer/comment:a2@20"
		}
		for _, kind := range []string{PlantedWrong, PlantedRight} {
			p := Planted{ID: string(j) + "-" + kind, Judgement: j, CaseID: id, Planted: kind, Output: "The zork train is the queue of pull requests."}
			if kind == PlantedWrong {
				p.Marker = "zork"
			}
			if j == JAnswerStart {
				p.WordsBefore = &words
			}
			line, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(append(line, '\n'))
		}
	}
	path := filepath.Join(t.TempDir(), "planted.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// `score --grader` grades the run with the model it names, in place of candidates.yaml's grader:
// every grading call goes to it, and run.json and the figures record it as the grader. Each Claude
// model of prices.yaml that is no candidate plays a role of candidates.yaml, so the run takes the
// fresh reader's model as its grader and gives the reader's part to a candidate (--reader).
func TestTheGraderFlagReplacesTheGraderOfOneRun(t *testing.T) {
	const grader, reader = "claude-sonnet-4-6", "claude-fable-5-2"
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	code, _, stderr := runHarness(scoreArgs(t, build, sessions, out, stand, "--grader", grader, "--reader", reader)...)
	if code != 0 {
		t.Fatalf("score --grader %s = %d:\n%s", grader, code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(out, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var params RunParams
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatal(err)
	}
	if params.Models[RoleGrader] != grader || params.Models[RoleReader] != reader {
		t.Errorf("run.json records the models %v, want the grader %s and the reader %s", params.Models, grader, reader)
	}
	graded := 0
	for _, r := range stand.Requests() {
		switch {
		case strings.Contains(r.Text, markReader):
			if r.Model != reader {
				t.Errorf("a reader's call went to %s, want %s", r.Model, reader)
			}
		case r.Model != grader:
			t.Errorf("a grading call went to %s, want %s", r.Model, grader)
		default:
			graded++
		}
	}
	if graded == 0 {
		t.Error("the run made no grading call")
	}
	f := readFigures(t, out)
	found := false
	for _, u := range f.Models {
		if u.Model == grader {
			found = true
			if u.Role != RoleGrader || u.CostUSD <= 0 {
				t.Errorf("the figures record %s as %+v, want the priced grader", grader, u)
			}
		}
	}
	if !found {
		t.Errorf("the figures name no use of %s: %+v", grader, f.Models)
	}
}

// `score --grader` refuses a model that may not grade: a candidate, the writer, the fresh reader,
// a fallback grader (R2) and a model prices.yaml has no price for. It calls no model.
func TestTheGraderFlagRefusesAModelThatPlaysAnotherPart(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	for _, c := range []struct{ name, id, says string }{
		{"a candidate", "claude-sonnet-5-5", "is a candidate"},
		{"the writer", cfg.Models.Roles.Writer.ID, "is the writer"},
		{"the fresh reader", cfg.Models.Roles.Reader.ID, "is the fresh reader"},
		{"a fallback grader", cfg.Models.Roles.GraderFallbacks[0].ID, "is a fallback grader"},
		{"a model with no price", "claude-unpriced-1", "has no Claude price"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "run")
			code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--grader", c.id,
				"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
			if code != 2 || !strings.Contains(stderr, c.id) || !strings.Contains(stderr, c.says) {
				t.Errorf("score --grader %s = %d, want 2 saying it %s:\n%s", c.id, code, c.says, stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("a refused run made its output directory (%v)", err)
			}
		})
	}
	if n := len(stand.Requests()); n != 0 {
		t.Errorf("the refused runs made %d model calls, want none", n)
	}
}

// `grader-test --grader` admits a candidate: no candidate wrote a planted output. The report says
// the grader is a candidate, and grader-test.json records the grader and that it is one. With no
// --grader the test grades with candidates.yaml's grader and says nothing of the kind. With no
// --added-grader the report names the added grader old.
func TestGraderTestAdmitsACandidateAsItsGrader(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	build, sessions := writeFixture(t)
	planted := writePlanted(t)
	for _, c := range []struct {
		grader, want string
		candidate    bool
	}{
		{"claude-sonnet-5-5", "claude-sonnet-5-5", true},
		{"", cfg.Models.Roles.Grader.ID, false},
	} {
		stand := modeltest.New(t, allJudgementRules(t)...)
		out := t.TempDir()
		args := []string{"grader-test", "--planted", planted, "--cases", build, "--sessions", sessions, "--out", out,
			"--gateway", stand.URL, "--gateway-key-file", keyFile(t)}
		if c.grader != "" {
			args = append(args, "--grader", c.grader)
		}
		code, stdout, stderr := runHarness(args...)
		if code != 0 {
			t.Fatalf("grader-test --grader %q = %d:\n%s", c.grader, code, stderr)
		}
		said := strings.Contains(stdout, c.want+" is a candidate of candidates.yaml.")
		if !strings.HasPrefix(stdout, "# Grader test: "+c.want+"\n") || said != c.candidate || !strings.Contains(stdout, "\nThe grader's inputs are trimmed,lost=full,added=full,undefined=full,define=full (--grader-inputs). The added grader is old (--added-grader). Prompts SHA-256 "+PromptsDigest()+".\n") {
			t.Errorf("grader-test --grader %q reports:\n%s\nwant the grader %s, said to be a candidate: %t, and the default inputs as resolved, added grader old and the prompts' digest named", c.grader, stdout, c.want, c.candidate)
		}
		data, err := os.ReadFile(filepath.Join(out, "grader-test.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Grader    string `json:"grader"`
			Candidate bool   `json:"grader_is_candidate"`
		}
		if err := json.Unmarshal(data, &record); err != nil || record.Grader != c.want || record.Candidate != c.candidate {
			t.Errorf("grader-test.json records %+v (%v), want the grader %s, a candidate: %t", record, err, c.want, c.candidate)
		}
		if len(stand.Requests()) == 0 {
			t.Fatal("the test made no grading call")
		}
		for _, r := range stand.Requests() {
			if r.Model != c.want {
				t.Errorf("a grading call went to %s, want %s", r.Model, c.want)
			}
		}
	}
}
