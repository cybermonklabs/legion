package specgraph

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// growthStep is how many times larger the full input of a row of this file is than its small
// input, and linearSlack how many times more than growthStep the CPU time of the work may grow
// between them: work whose steps grow with the input grows about growthStep times (10 to 22 times
// as measured, with what grows with the log of the input), under linearSlack*growthStep = 48, and
// work whose steps grow with the square of the input about growthStep² = 256 times, less the part
// of the work that stays linear. A step of 8, as pmdoc's growth tests take, let a quadratic lookup
// over these rows grow 21 times at sizes that -race runs in time, under 8's bound of 24. The bound
// is a ratio of two measures of one machine, so a constant factor that slows each step, such as the
// race detector or a loaded machine, cancels; a fixed limit of wall time does not, and failed under
// -race.
const (
	growthStep  = 16
	linearSlack = 3
)

// growsLinearly fails t unless the CPU time of the work at full grows from its time at
// full/growthStep by at most linearSlack*growthStep times. prepare builds the input of one call at
// a size, untimed, and returns the call. Each call's time is the CPU time of the thread that it runs
// on (cputest.OnThread), so another test that runs beside it and the garbage collector's workers
// are left out; the small size is timed three times and the fastest kept, and the full size again,
// up to three times, only while its fastest is over the bound. Each call runs on a goroutine of its
// own, and its time is taken with testwait.Receive, whose one bound is the test binary's deadline
// less its margin: a slow call on a loaded machine passes, and work whose steps grow with the square
// of the input that has not ended by then fails by this row's name, not at the package's timeout.
func growsLinearly(t *testing.T, what string, full int, prepare func(size int) func()) {
	t.Helper()
	small := full / growthStep
	timed := func(size int) time.Duration {
		call := prepare(size)
		took := make(chan time.Duration, 1)
		go func() { took <- cputest.OnThread(call) }()
		return testwait.Receive(t, fmt.Sprintf("%s at %d", what, size), took)
	}
	before := min(timed(small), timed(small), timed(small))
	after := timed(full)
	for i := 1; i < 3 && after > linearSlack*growthStep*before; i++ {
		after = min(after, timed(full))
	}
	verdict := fmt.Sprintf("%s took %s of CPU at %d and %s at %d, %.1f times for %d times the input", what, before, small, after, full, float64(after)/float64(before), growthStep)
	if after > linearSlack*growthStep*before {
		t.Errorf("%s; work whose steps grow with the input grows at most %d times", verdict, linearSlack*growthStep)
		return
	}
	t.Log(verdict)
}

// longID is the id i of a set of ids that share their first 512 bytes, so that two of them read as
// equal or not only past those bytes: a lookup that compares each of n ids then reads n times 512
// bytes. Its last word holds letters beside its digits, so that it states no position (IsID).
func longID(start string, i int) string {
	return start + strings.Repeat("x", 512) + fmt.Sprintf("-k%05dk", i)
}

// The rules that refuse the checks and statements of the files (refusals) take each problem of
// Validate once, so their time grows with the tree and not with its square: by the node that a
// problem names, by the strongly connected part of a loop, and, for a problem of no check and no
// statement, which refuses each of them, once. Each row is a tree of n problems or more over nodes
// whose ids share their first 512 bytes, at n of 1,000 and of 16,000 (growsLinearly; under -race a
// call at the full size takes some 15 to 40 s of CPU): a problem of each of n checks; a problem of
// each of n terms beside n checks; and a loop of each of n/2 pairs of statements, each the parent
// of the other. At the full size the rules must refuse each check or statement of the row, the
// first by the row's rule.
func TestTheRefusalsOfALargeTreeTakeTimeThatGrowsWithTheTree(t *testing.T) {
	t.Parallel()
	const full = 16000
	checks := func(n int, claim string) []Check {
		all := make([]Check, n)
		for i := range all {
			all[i] = checkStateCheck
			all[i].ID, all[i].Claim = longID("a-check", i), claim
		}
		return all
	}
	terms := func(n int) []Term {
		var all []Term
		for i := range n {
			id := fmt.Sprintf("term-k%05dk", i)
			all = append(all, Term{ID: id, Name: id, Definition: "-"})
		}
		return all
	}
	pairs := func(n int) []Statement {
		var all []Statement
		for i := range n / 2 {
			a, b := longID("a-loop", 2*i), longID("a-loop", 2*i+1)
			all = append(all, Statement{ID: a, Kind: KindBehavior, Parent: b, Text: checkStateLeaf.Text},
				Statement{ID: b, Kind: KindBehavior, Parent: a, Text: checkStateLeaf.Text})
		}
		return all
	}
	for _, row := range []struct {
		name  string
		tree  func(n int) Tree
		nodes func(n int) int // the checks and statements that the rules must refuse
		rule  string          // the rule that refuses the first check or statement of the row's own
		first string
	}{
		{"a problem of each check", func(n int) Tree {
			return Tree{Statements: []Statement{checkStateTop, checkStateLeaf}, Checks: checks(n, "-")}
		}, func(n int) int { return n }, "no-claim", longID("a-check", 0)},
		{"a problem of each term beside as many checks", func(n int) Tree {
			return Tree{Statements: []Statement{checkStateTop, checkStateLeaf}, Checks: checks(n, checkStateCheck.Claim), Terms: terms(n)}
		}, func(n int) int { return n + 2 }, "bad-term", longID("a-check", 0)},
		{"a loop of each pair of statements", func(n int) Tree { return Tree{Statements: pairs(n)} },
			func(n int) int { return n }, "loop", longID("a-loop", 0)},
	} {
		t.Run(row.name, func(t *testing.T) {
			var refused map[string]string
			growsLinearly(t, "the refusals", full, func(n int) func() {
				tree := row.tree(n)
				return func() { refused = refusals(tree) }
			})
			if len(refused) < row.nodes(full) || refused[row.first] != row.rule {
				t.Errorf("the rules refuse %d checks and statements, %s by %q; want %d or more, that one by %q", len(refused), quoted(row.first), refused[row.first], row.nodes(full), row.rule)
			}
		})
	}
}

// The derivation reads the tree of each commit of a FixtureHistory by its id, so its time grows
// with the history and not with its square: the case of TestCheckState with n commits after c1,
// at n of 1,000 and of 16,000 (growsLinearly), each with c1's tree and ties and each id sharing its
// first 512 bytes with the others, at each a pass of C and a failing example planted on it that
// caught its fault, so that the judge asks the history about each commit, and the subject the last;
// at the full size the derivation must read C true from the last.
func TestTheDerivationOverALongHistoryTakesTimeThatGrowsWithTheHistory(t *testing.T) {
	t.Parallel()
	const full = 16000
	var states []CheckState
	var err error
	var last string
	growsLinearly(t, "the derivation over a long history", full, func(n int) func() {
		s := newCheckCase(t)
		// The commits are added here and not by checkCase.commit, which finds the commit before each
		// one in the list, so that the case itself is built in time that grows with the history.
		c1 := s.history.Commits[0]
		from := c1.ID
		for i := range n {
			id := longID("c", i)
			s.history.Commits = append(s.history.Commits, FixtureCommit{ID: id, Parent: from, Files: c1.Files})
			s.Ties[id] = s.Ties[c1.ID]
			at := checkStateRun.Add(time.Duration(10+2*i) * time.Minute)
			s.Results = append(s.Results, checkRun(id, OutcomePass, at), caughtFault(id, at.Add(time.Minute)))
			from = id
		}
		s.Subject.Commit = from
		in := s.input()
		return func() { _, states, err = deriveChecks(in); last = from }
	})
	want := CheckState{ID: checkStateCheck.ID, State: CheckTrue, Reason: reasonPassed, From: longID("c", full-1)}
	if err != nil || len(states) != 1 || states[0] != want || last != want.From {
		t.Errorf("the derivation over %d commits gives %d states (%v), want the one state true from the last commit", full, len(states), err)
	}
}
