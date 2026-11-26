package specgraph

import (
	"testing"
	"time"
)

// historyStub is a History that answers paths, as known, for any two commits, and counts its asks.
type historyStub struct {
	paths []string
	asks  int
}

func (h *historyStub) Changed(_, _ string) ([]string, bool, error) {
	h.asks++
	return h.paths, true, nil
}

// R8: a result at one commit counts at another only when the check's tie is known at both and no
// path that changed between them reaches either. The derivation of a check's state
// (checkJudge.carries) and CI's citations (CitationInput.changeCause) both decide it, so each row
// asks both and wants one decision from them, with the cause that citations prints. Each reads
// only what the decision needs: the derivation asks History for the changed paths only when both
// ties are known, and citations asks for the tie at the base only when the tie at the commit is
// known and no changed path reaches it. A changed path that git names alone for a tree, as the
// gitlink of a submodule that moved, reaches a tie that holds a path under it; its control is a
// path whose name only begins that of a directory of the tie.
func TestBothCallersOfTheCarryRuleDecideAlike(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name                    string
		changed                 []string
		head, base              Tie
		carries                 bool
		cause                   string
		historyAsked, baseAsked bool
	}{
		{name: "equal trees", head: known("a/*"), base: known("a/*"), carries: true, historyAsked: true, baseAsked: true},
		{name: "a path that no tie covers", changed: []string{"docs/x.md"}, head: known("a/*"), base: known("a/*"),
			carries: true, historyAsked: true, baseAsked: true},
		{name: "a path that the tie at the head covers", changed: []string{"a/a.go"}, head: known("a/*"), base: known("a/*"),
			cause: "a/a.go changed", historyAsked: true},
		{name: "a path that only the tie at the base covers", changed: []string{"old/data.json"}, head: known("a/*"),
			base: known("a/*", "old/**"), cause: "old/data.json changed (the tie at the base commit holds it)",
			historyAsked: true, baseAsked: true},
		{name: "the tie at the head is unknown", head: Tie{Why: "go list failed"}, base: known("a/*"),
			cause: "its tie is unknown (go list failed)"},
		{name: "the tie at the base is unknown", head: known("a/*"), base: Tie{Why: "go list failed at the base"},
			cause: "its tie at the base commit is unknown (go list failed at the base)", baseAsked: true},
		{name: "a changed path that the tie at the head holds a path under", changed: []string{"sub"},
			head: known("a/*", "sub/*"), base: known("a/*", "sub/*"), carries: true, cause: "sub changed (the tie holds sub/*, a path under it)",
			historyAsked: true},
		{name: "a changed path that only the tie at the base holds a path under", changed: []string{"sub"},
			head: known("a/*"), base: known("a/*", "sub/x/answer.json"), carries: true,
			cause: "sub changed (the tie at the base commit holds sub/x/answer.json, a path under it)", historyAsked: true, baseAsked: true},
		{name: "control: a changed path whose name begins that of a directory of the tie", changed: []string{"su"},
			head: known("a/*", "sub/*"), base: known("a/*", "sub/*"), carries: true, historyAsked: true, baseAsked: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			const check = "check-a"
			history := &historyStub{paths: row.changed}
			judge, err := newCheckJudge(Input{Subject: Point{Commit: "head", Branch: "b1"}, Now: time.Unix(1, 0), History: history,
				Ties: CommitTies{"head": {check: row.head}, "base": {check: row.base}}, JobRuns: []JobRun{},
				Uncited: map[string]map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			carries := judge.carries(check, "base")
			baseAsked := false
			cause := CitationInput{Changed: row.changed, Tie: func(Check) Tie { return row.head },
				BaseTie: func(Check) Tie {
					baseAsked = true
					return row.base
				}}.changeCause(Check{ID: check})
			if carries != row.carries || cause != row.cause {
				t.Errorf("the derivation: carries %t; citations: %q; want carries %t and %q", carries, cause, row.carries, row.cause)
			}
			if historyAsked := history.asks > 0; historyAsked != row.historyAsked || baseAsked != row.baseAsked {
				t.Errorf("the derivation asked History: %t; citations asked for the tie at the base: %t; want %t and %t",
					historyAsked, baseAsked, row.historyAsked, row.baseAsked)
			}
		})
	}
}
