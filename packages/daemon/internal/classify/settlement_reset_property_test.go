package classify

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// randomSettlementPairs is how many pairs of settlements of one head the property below draws
// besides its named rows: each is judged in both orders after each of fenceResets, which took 2.3
// seconds in a gVisor issue pod. The 2000 orders cover every classification: about 850 are
// newer, 50 more newer for some checks and older for others, 640 stale, 410 duplicates, 30 newer
// for their statuses alone and 15 conflicts.
const randomSettlementPairs = 1000

// A pull request remembers each settled head's fence (record.PullRequest.SettledHeads), and a
// settlement of a head whose fence was reset is weighed against, and applied to, that remembered
// fence (SettlementFor). So of two settlements of one head, the second is judged after any reset
// between them - a reopen, a new generation's clear, the branch leaving the head and coming back,
// with its pushes first or after - as it is with no reset: it is classified the same; where it is
// newer, or newer for its statuses alone, it leaves the judgement and the head's remembered fence
// that no reset leaves; where it is the newest settlement again, or of the same runs and
// generation, it shows the remembered fence again; and where it is stale it changes nothing of
// what the reset left. Wherever the fence stands at the head, each check's run is the one of
// highest id the two named, its check failing or cancelled as that run concluded, and each
// context's status is the one created last.
//
// The pairs are drawn as a listener's record of the head makes them: the earlier settlement, then
// a later one of the same record, in which checks ran again, new checks and statuses appeared and
// statuses changed state, its generation higher; or a later one of a record the bucket's TTL
// removed and the reconcile wrote again, which numbers its settlements afresh and can name fewer
// checks and statuses; or the same settlement again. An earlier settlement of the same record
// delivered after the later one is stale against it, so after every reset it changes nothing:
// newer then older is newer alone. One of a recreated record is not: it can name a check or a
// status the later one does not, or come at a higher generation, and the later one's fence then
// takes it in with no reset, as after one. With no reset the judgement is the release before this
// memory's, which read it only for a head the fence did not stand at (releaseBefore). The named
// rows are the shapes each of the three tests of a late settlement after a reset holds:
// TestALateSettlementWithAnOlderStatusOfAHeadWhoseFenceWasResetChangesNothing,
// TestALateSettlementNamingFewerChecksOfAHeadWhoseFenceWasResetChangesNothing and
// TestALateSettlementOfAHeadWhoseFenceWasResetChangesNothing.
func TestTheSecondOfTwoSettlementsOfAHeadIsJudgedAfterAResetAsWithNone(t *testing.T) {
	pairs := []settlementPair{
		{name: "an older commit status", sameRecord: true,
			earlier: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 1, Snapshot: "head-1",
				Failing: []string{}, Cancelled: []string{}, Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusFailure, 1)}},
			later: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 2, Snapshot: "head-2",
				Failing: []string{}, Cancelled: []string{}, Statuses: []record.CommitStatus{statusOf("deploy", record.CommitStatusSuccess, 2)}}},
		{name: "fewer checks", sameRecord: true,
			earlier: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 1, Snapshot: "head-1",
				Failing: []string{}, Cancelled: []string{}},
			later: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}, {Name: "lint", ID: 7}}, Generation: 2,
				Snapshot: "head-2", Failing: []string{"lint"}, Cancelled: []string{}}},
		{name: "an older run", sameRecord: true,
			earlier: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 4}}, Generation: 1, Snapshot: "head-1",
				Failing: []string{"ci"}, Cancelled: []string{}},
			later: SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 2, Snapshot: "head-2",
				Failing: []string{}, Cancelled: []string{}}},
	}
	rng := rand.New(rand.NewPCG(1575, 7))
	for i := range randomSettlementPairs {
		pairs = append(pairs, drawSettlementPair(rng, i))
	}
	failures := 0
	for _, pair := range pairs {
		for _, order := range []struct {
			name          string
			first, second SettlementCandidate
			older         bool
		}{
			{"in order", pair.earlier, pair.later, false},
			{"the earlier one after the later", pair.later, pair.earlier, pair.sameRecord},
		} {
			for _, wrong := range judgedAfterEveryReset(t, order.first, order.second, order.older) {
				if failures++; failures > 5 {
					t.Fatalf("more than five failures; the first are above")
				}
				t.Errorf("%s, %s: %s\n first  %+v\n second %+v", pair.name, order.name, wrong, order.first, order.second)
			}
		}
	}
}

// settlementPair is two settlements of "head" as its listener's record made them, earlier first.
// sameRecord is whether later is of the record earlier was: it then names every check and status
// earlier did, none older, at a higher generation.
type settlementPair struct {
	name           string
	earlier, later SettlementCandidate
	sameRecord     bool
}

// fenceReset is one way a head's fence is reset between two of its settlements, from a pull
// request whose fence stands at "head" with no head run open.
type fenceReset struct {
	name  string
	reset func(t *testing.T, pr record.PullRequest) record.PullRequest
	// close delivers what the reset left owed: the pushes of the moves a head run waits for. It
	// is the identity for a reset that opens no run.
	close func(t *testing.T, pr record.PullRequest) record.PullRequest
}

// otherSettled is the settlement of the head the branch moves to and back from.
var otherSettled = SettlementCandidate{Head: "other", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1,
	Snapshot: "other-1", Failing: []string{}, Cancelled: []string{}}

var fenceResets = []fenceReset{
	{name: "no reset", reset: func(_ *testing.T, pr record.PullRequest) record.PullRequest { return pr }},
	{name: "a reopen", reset: func(t *testing.T, pr record.PullRequest) record.PullRequest { return stored(t, Reopen(pr, "head")) }},
	{name: "a new generation's clear", reset: func(t *testing.T, pr record.PullRequest) record.PullRequest {
		// What Store.ClearGeneration writes of an open pull request's settlement.
		pr.CheckedHead, pr.Failing, pr.Cancelled, pr.CheckRuns, pr.Statuses = "", []string{}, []string{}, []record.AttemptRun{}, []record.CommitStatus{}
		return stored(t, pr)
	}},
	{name: "the branch left the head and came back", reset: func(t *testing.T, pr record.PullRequest) record.PullRequest {
		pr = stored(t, ApplyPush(pr, record.ClassifiedPush{SHA: "other", Before: "head"}))
		pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "other"))
		pr, _ = ApplyCheckSettlement(pr, otherSettled)
		pr = stored(t, ApplyPush(stored(t, pr), record.ClassifiedPush{SHA: "head", Before: "other"}))
		return stored(t, AdvancePullRequestHead(pr, Arrival{}, "head"))
	}},
	{name: "the branch left the head and came back before its pushes", reset: func(t *testing.T, pr record.PullRequest) record.PullRequest {
		pr = stored(t, AdvancePullRequestHead(pr, Arrival{}, "other"))
		pr, _ = ApplyCheckSettlement(pr, otherSettled)
		return stored(t, AdvancePullRequestHead(stored(t, pr), Arrival{}, "head"))
	}, close: func(t *testing.T, pr record.PullRequest) record.PullRequest {
		pr = stored(t, ApplyPush(pr, record.ClassifiedPush{SHA: "other", Before: "head"}))
		return stored(t, ApplyPush(pr, record.ClassifiedPush{SHA: "head", Before: "other"}))
	}},
}

// judgedAfterEveryReset settles first on a head with no settlement, then second after each of
// fenceResets, and says what is wrong with each judgement against second's with no reset. older
// is whether second is an earlier settlement of first's listener record, which must be stale.
func judgedAfterEveryReset(t *testing.T, first, second SettlementCandidate, older bool) []string {
	var wrong []string
	base, applied := ApplyCheckSettlement(record.PullRequest{HeadSHA: "head", Failing: []string{}}, first)
	if !applied {
		return []string{"the first settlement did not apply to a head with no settlement"}
	}
	base = stored(t, base)
	classified := ClassifySettlement(base, second)
	if older && classified != SettlementStale {
		wrong = append(wrong, fmt.Sprintf("the earlier settlement of the same record is %s against the later one, want stale", classified))
	}
	noReset, _ := ApplyCheckSettlement(base, second)
	noReset = stored(t, noReset)
	if before := releaseBefore(first, second); JudgementChanged(before, noReset) {
		wrong = append(wrong, fmt.Sprintf("with no reset the head is judged %s, and the release before judged it %s", fenceOf(noReset), fenceOf(before)))
	}
	if what := newestShown(noReset, first, second); what != "" {
		wrong = append(wrong, "with no reset "+what)
	}
	for _, r := range fenceResets {
		closed := func(pr record.PullRequest) record.PullRequest {
			if r.close == nil {
				return pr
			}
			return r.close(t, pr)
		}
		reset := r.reset(t, base)
		if reset.HeadSHA != "head" || (reset.CheckedHead == "head" && r.name != "no reset") || (reset.Run != nil) != (r.close != nil) {
			wrong = append(wrong, fmt.Sprintf("after %s the pull request is at %q, its fence at %q, a head run open %v: the reset is not the one named",
				r.name, reset.HeadSHA, reset.CheckedHead, reset.Run != nil))
			continue
		}
		if stood, stands := SettlementFor(reset, second); !stands || ClassifySettlement(stood, second) != classified {
			wrong = append(wrong, fmt.Sprintf("after %s the second settlement stands %v and is %s, want it %s as with no reset",
				r.name, stands, ClassifySettlement(stood, second), classified))
		}
		got, _ := ApplyCheckSettlement(reset, second)
		if classified == SettlementStale && r.close == nil && !reflect.DeepEqual(got, reset) {
			wrong = append(wrong, fmt.Sprintf("after %s the stale settlement changed the pull request: judged %s, want %s as the reset left it",
				r.name, fenceOf(got), fenceOf(reset)))
		}
		got = closed(stored(t, got))
		if r.close != nil && got.Run != nil {
			wrong = append(wrong, fmt.Sprintf("after %s the pushes left a head run open", r.name))
		}
		want, wantRemembered := noReset, rememberedOf(noReset, "head")
		if classified == SettlementStale {
			want = closed(reset)
			wantRemembered = rememberedOf(want, "head")
		}
		if JudgementChanged(want, got) || !reflect.DeepEqual(rememberedOf(got, "head"), wantRemembered) {
			wrong = append(wrong, fmt.Sprintf("after %s the %s settlement leaves the head judged %s, remembered %+v; want %s, remembered %+v",
				r.name, classified, fenceOf(got), rememberedOf(got, "head"), fenceOf(want), wantRemembered))
		}
		if what := newestShown(got, first, second); what != "" {
			wrong = append(wrong, "after "+r.name+" "+what)
		}
	}
	return wrong
}

// releaseBefore is first and then second as the release before the remembered fence judged them
// with no reset between them: first applied to an empty fence of a head the pull request never
// settled, and second to the fence it left. That release read its memory of settled heads only for
// a settlement of a head the fence did not stand at, which with no reset is first alone, and it
// remembered nothing of that head; ApplySettlement is as it was.
func releaseBefore(first, second SettlementCandidate) record.PullRequest {
	pr, _ := ApplySettlement(record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Cancelled: []string{}}, first)
	if applied, ok := ApplySettlement(pr, second); ok {
		return applied
	}
	return pr
}

// rememberedOf is pr's remembered fence of head, nil when it remembers none.
func rememberedOf(pr record.PullRequest, head string) *record.SettledHead {
	for _, settled := range pr.SettledHeads {
		if settled.Head == head {
			return &settled
		}
	}
	return nil
}

// fenceOf is pr's fence in a line.
func fenceOf(pr record.PullRequest) string {
	return fmt.Sprintf("at %q runs %+v failing %v cancelled %v generation %d snapshot %q statuses %+v",
		pr.CheckedHead, pr.CheckRuns, pr.Failing, pr.Cancelled, pr.Generation, pr.Snapshot, pr.Statuses)
}

// newestShown says what is wrong with pr's fence where it stands at "head", against the
// settlements it was made of: each check's run must be the one of highest id they name, failing
// or cancelled exactly as that run concluded, and each context's status the one created last.
func newestShown(pr record.PullRequest, settlements ...SettlementCandidate) string {
	if pr.CheckedHead != "head" {
		return ""
	}
	newest := map[string]record.AttemptRun{}
	statuses := map[string]record.CommitStatus{}
	for _, s := range settlements {
		for _, run := range s.CheckRuns {
			switch {
			case slices.Contains(s.Failing, run.Name):
				run.Conclusion = record.RunFailed
			case slices.Contains(s.Cancelled, run.Name):
				run.Conclusion = record.RunCancelled
			default:
				run.Conclusion = record.RunSucceeded
			}
			if known, found := newest[run.Name]; !found || run.ID > known.ID {
				newest[run.Name] = run
			}
		}
		for _, status := range s.Statuses {
			if known, found := statuses[status.Context]; !found || status.CreatedAt.After(known.CreatedAt) {
				statuses[status.Context] = status
			}
		}
	}
	if len(pr.CheckRuns) != len(newest) {
		return fmt.Sprintf("the fence holds runs %+v, want the newest of each check of %+v", pr.CheckRuns, newest)
	}
	for _, run := range pr.CheckRuns {
		want := newest[run.Name]
		if run.ID != want.ID || run.Conclusion != want.Conclusion ||
			slices.Contains(pr.Failing, run.Name) != (want.Conclusion == record.RunFailed) ||
			slices.Contains(pr.Cancelled, run.Name) != (want.Conclusion == record.RunCancelled) {
			return fmt.Sprintf("the fence holds %+v, failing %v and cancelled %v, want the newest run of %s, %+v", run, pr.Failing, pr.Cancelled, run.Name, want)
		}
	}
	if len(pr.Statuses) != len(statuses) {
		return fmt.Sprintf("the fence holds statuses %+v, want the newest of each context of %+v", pr.Statuses, statuses)
	}
	for _, status := range pr.Statuses {
		if want := statuses[status.Context]; !status.CreatedAt.Equal(want.CreatedAt) || status.State != want.State {
			return fmt.Sprintf("the fence holds %+v, want the newest status of %s, %+v", status, status.Context, want)
		}
	}
	return ""
}

// The checks, contexts and conclusions a drawn settlement names. aggregate is a check named with
// no run - an aggregate job whose needs never ran, so GitHub started nothing - which a settlement
// lists failing or cancelled or not at all.
var (
	drawnChecks      = []string{"ci", "e2e", "lint"}
	drawnContexts    = []string{"deploy", "license"}
	drawnConclusions = []record.RunConclusion{record.RunSucceeded, record.RunFailed, record.RunCancelled}
	drawnStates      = []record.CommitStatusState{record.CommitStatusSuccess, record.CommitStatusFailure, record.CommitStatusError}
)

// drawnSettlement is a settlement of "head" as the listener's record holds it: each check's run
// and what it concluded, the list aggregate is in, and each context's status.
type drawnSettlement struct {
	runs       map[string]record.AttemptRun
	aggregate  record.RunConclusion
	statuses   map[string]record.CommitStatus
	generation int64
	snapshot   string
}

// candidate is d as the listener sends it.
func (d drawnSettlement) candidate() SettlementCandidate {
	c := SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{}, Generation: d.generation, Snapshot: d.snapshot,
		Failing: []string{}, Cancelled: []string{}}
	listed := func(name string, conclusion record.RunConclusion) {
		switch conclusion {
		case record.RunFailed:
			c.Failing = append(c.Failing, name)
		case record.RunCancelled:
			c.Cancelled = append(c.Cancelled, name)
		}
	}
	for _, name := range drawnChecks {
		if run, found := d.runs[name]; found {
			c.CheckRuns = append(c.CheckRuns, record.AttemptRun{Name: name, ID: run.ID})
			listed(name, run.Conclusion)
		}
	}
	listed("aggregate", d.aggregate)
	for _, context := range drawnContexts {
		if status, found := d.statuses[context]; found {
			c.Statuses = append(c.Statuses, status)
		}
	}
	return c
}

// drawSettlementPair draws the pair numbered i: two settlements of one listener record, two of a
// record recreated between them, or one settlement twice.
func drawSettlementPair(rng *rand.Rand, i int) settlementPair {
	earlier := drawnSettlement{runs: map[string]record.AttemptRun{}, statuses: map[string]record.CommitStatus{},
		aggregate: drawnConclusions[rng.IntN(3)], generation: 1 + rng.Int64N(3)}
	earlier.snapshot = fmt.Sprintf("earlier-%d", earlier.generation)
	for _, name := range drawnChecks {
		if rng.IntN(3) > 0 {
			earlier.runs[name] = record.AttemptRun{ID: 1 + rng.Int64N(5), Conclusion: drawnConclusions[rng.IntN(3)]}
		}
	}
	for _, context := range drawnContexts {
		if rng.IntN(2) == 0 {
			earlier.statuses[context] = statusOf(context, drawnStates[rng.IntN(3)], 1+rng.IntN(4))
		}
	}
	switch kind := rng.IntN(5); {
	case kind == 4:
		return settlementPair{name: fmt.Sprintf("pair %d, one settlement twice", i), earlier: earlier.candidate(), later: earlier.candidate()}
	case kind == 3:
		return settlementPair{name: fmt.Sprintf("pair %d, a recreated record", i), earlier: earlier.candidate(),
			later: laterSettlement(rng, earlier, true).candidate()}
	default:
		return settlementPair{name: fmt.Sprintf("pair %d, one record", i), earlier: earlier.candidate(),
			later: laterSettlement(rng, earlier, false).candidate(), sameRecord: true}
	}
}

// laterSettlement is a settlement the listener's record sends after earlier: some checks ran
// again, to a run of higher id and a conclusion of its own, new checks and statuses appeared, and
// statuses changed state, each created later. A record the bucket's TTL removed and the reconcile
// wrote again (recreated) numbers its settlements afresh, at any generation, and names only some
// of the checks and statuses earlier did, each no older, and the aggregate as it now reads; one
// record names every check and status earlier did, at a higher generation, and the aggregate in
// one of the two lists if earlier did.
func laterSettlement(rng *rand.Rand, earlier drawnSettlement, recreated bool) drawnSettlement {
	later := drawnSettlement{runs: map[string]record.AttemptRun{}, statuses: map[string]record.CommitStatus{},
		aggregate: drawnConclusions[rng.IntN(3)], generation: earlier.generation + 1 + rng.Int64N(2)}
	later.snapshot = fmt.Sprintf("later-%d", later.generation)
	if recreated {
		later.generation = 1 + rng.Int64N(earlier.generation+2)
		later.snapshot = fmt.Sprintf("recreated-%d", later.generation)
	} else if earlier.aggregate != record.RunSucceeded {
		later.aggregate = drawnConclusions[1+rng.IntN(2)]
	}
	for _, name := range drawnChecks {
		run, had := earlier.runs[name]
		switch {
		case had && (!recreated || rng.IntN(2) == 0):
			if rng.IntN(2) == 0 {
				run = record.AttemptRun{ID: run.ID + 1 + rng.Int64N(3), Conclusion: drawnConclusions[rng.IntN(3)]}
			}
			later.runs[name] = run
		case !had && rng.IntN(3) == 0:
			later.runs[name] = record.AttemptRun{ID: 1 + rng.Int64N(8), Conclusion: drawnConclusions[rng.IntN(3)]}
		}
	}
	for _, context := range drawnContexts {
		status, had := earlier.statuses[context]
		switch {
		case had && (!recreated || rng.IntN(2) == 0):
			if rng.IntN(2) == 0 {
				status = record.CommitStatus{Context: context, State: drawnStates[rng.IntN(3)],
					CreatedAt: status.CreatedAt.Add(time.Duration(1+rng.IntN(3)) * time.Minute)}
			}
			later.statuses[context] = status
		case !had && rng.IntN(3) == 0:
			later.statuses[context] = statusOf(context, drawnStates[rng.IntN(3)], 1+rng.IntN(6))
		}
	}
	return later
}
