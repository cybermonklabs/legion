package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// textsFlags are the flags of score that --texts-from takes from the run it names, so a run of
// --texts-from gives none of them: what decided the texts, and which cases they are.
var textsFlags = []string{
	"change", "model", "models", "local", "loaded", "rounds", "unedited", "writer-order",
	"part", "groups", "sample", "sample-seed", "session-chars", "commit", "new-run",
}

// textsSource is one run that --texts-from names: its directory, its parameters and its case
// lines.
type textsSource struct {
	dir    string
	params RunParams
	lines  map[string]CaseResult
}

// readTextsFrom reads the runs of --texts-from dir: the run in dir, or, where dir holds no
// run.json, the run in each directory of dir that holds one, by name, as a lockstep run (score
// --models) writes them; lockstep says it is the latter. The runs must agree on all that decides
// which texts they hold: the change, the part, the groups, the sample, the cases, the session
// export and the session part's length. Each run of a lockstep names its candidate, a different
// one.
func readTextsFrom(dir string) (sources []textsSource, lockstep bool, err error) {
	read := func(d string) error {
		data, err := os.ReadFile(filepath.Join(d, "run.json"))
		if err != nil {
			return fmt.Errorf("--texts-from: %w", err)
		}
		var p RunParams
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("--texts-from %s: run.json: %w", d, err)
		}
		lines, err := readResults(filepath.Join(d, "cases.jsonl"))
		if err != nil || len(lines) == 0 {
			return fmt.Errorf("--texts-from %s holds no results (%v)", d, err)
		}
		sources = append(sources, textsSource{dir: d, params: p, lines: lines})
		return nil
	}
	switch _, err := os.Stat(filepath.Join(dir, "run.json")); {
	case err == nil:
		return sources, false, read(dir)
	case !errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("--texts-from: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, fmt.Errorf("--texts-from: %w", err)
	}
	for _, e := range entries {
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "run.json")); !e.IsDir() || err != nil {
			continue
		}
		if err := read(sub); err != nil {
			return nil, false, err
		}
	}
	if len(sources) == 0 {
		return nil, false, fmt.Errorf("--texts-from %s holds no run.json, and no directory of it holds one", dir)
	}
	same := func(p RunParams) string {
		key, _ := json.Marshal([]any{p.Change, p.Changes, p.Part, p.Groups, p.Sample, p.CasesSHA256, p.SessionsSHA, p.SessionChars})
		return string(key)
	}
	models := map[string]bool{}
	for _, s := range sources {
		switch m := s.params.Model; {
		case same(s.params) != same(sources[0].params):
			return nil, false, fmt.Errorf("--texts-from %s: the runs of %s and %s are of other changes, parts, groups, samples, cases or sessions", dir, sources[0].dir, s.dir)
		case m == "" || models[m]:
			return nil, false, fmt.Errorf("--texts-from %s: the run of %s names no candidate, or one that another of its runs names", dir, s.dir)
		}
		models[s.params.Model] = true
	}
	return sources, true, nil
}

// change is the run's change for a lane of this run: its texts, with no call. plan is the plan
// of the run's change, nil for change 1.
func (s textsSource) change(plan *changePlan) *textsFrom {
	return &textsFrom{name: s.params.Change, dir: s.dir, asks: plan != nil && plan.asksWriter(), lines: s.lines}
}

// keep sets in params what decided the run's texts and that this run does not decide: the
// changes of a combination, the writer and its prompts' digest and order, change 3's settings and
// the local Dispatch's; and the run, as TextsFrom.
func (s textsSource) keep(params *RunParams) {
	p := s.params
	params.Changes, params.WriterSHA256, params.WriterOrder = p.Changes, p.WriterSHA256, p.WriterOrder
	params.Rounds, params.Unedited, params.Local = p.Rounds, p.Unedited, p.Local
	if w := p.Models[RoleWriter]; w != "" {
		params.Models[RoleWriter] = w
	}
	params.TextsFrom = s.dir
}

// textsFrom is the change of an earlier run, whose texts score --texts-from grades again: each
// case's text as that run's line holds it, with its definition, trace, wait, change calls and
// writer's answer, and no call. asks says the change asks the writer a side question (5 or 6,
// alone or in a combination).
type textsFrom struct {
	name  string
	dir   string
	asks  bool
	lines map[string]CaseResult
}

func (t *textsFrom) Name() string { return t.name }

// Apply is the case's line in the run. A case that the run holds no line for fails, and so does
// a case whose line holds no text (its change failed). A line that keeps no writer's answer, of
// a change that asks one, for a case with a session part, says so (AnswerNotKept): the change
// may have had an answer that the judgements now do not read.
func (t *textsFrom) Apply(_ context.Context, c *Case) (Applied, error) {
	r, ok := t.lines[c.ID]
	switch {
	case !ok:
		return Applied{}, fmt.Errorf("--texts-from %s holds no line of the case", t.dir)
	case r.TextSHA256 == "":
		return Applied{}, fmt.Errorf("--texts-from %s holds no text of the case: %s", t.dir, r.Error)
	}
	a := Applied{
		Text: r.Text, Same: r.Text == c.Agent.Text, Definition: r.Definition, AsksWriter: t.asks,
		WaitSeconds: r.WaitSeconds, Calls: r.ChangeCalls, Trace: r.ChangeTrace, TextsFrom: t.dir,
	}
	if r.WriterAnswer != nil {
		a.WriterAnswer = *r.WriterAnswer
	} else {
		a.AnswerNotKept = t.asks && r.Session
	}
	return a, nil
}
