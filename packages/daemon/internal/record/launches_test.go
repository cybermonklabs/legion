package record

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// agentClaim is one claim a launch test records: its token, issue, tree and role, the generation
// its row holds, and whether the row records a process (a locator) at that generation.
type agentClaim struct {
	token      claim.Token
	issue      string
	tree       string
	role       claim.Role
	generation uint64
	live       bool
	state      supervise.ClaimState
}

// putAgentClaim writes c's row as the claim write leaves it, with a locator where c is live.
func putAgentClaim(t *testing.T, tx pgx.Tx, c agentClaim) {
	t.Helper()
	var locator []byte
	if c.live {
		encoded, err := json.Marshal(runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: c.token,
			Incarnation: fmt.Sprintf("31001:%d", c.generation), Tmux: &runtime.TmuxLocator{Window: "@3", Pane: "%41"}})
		must(t, err)
		locator = encoded
	}
	state := c.state
	if state == "" {
		state = supervise.StateIdle
	}
	if _, err := tx.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		locator, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'LEGION', $2, $3, $4, $5, '', '', $6, $7, 0, 0, 0, 0)`,
		string(c.token), c.tree, c.issue, string(c.role), int64(c.generation), locator, string(state)); err != nil {
		t.Fatalf("record the claim %s: %v", c.token, err)
	}
}

// putLaunch writes the launch row the claim write leaves when it records the process of agent at
// generation (store's upsertClaim).
func putLaunch(t *testing.T, tx pgx.Tx, agent claim.Token, generation uint64) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `insert into claim_launches (claim, generation) values ($1, $2)`, string(agent), int64(generation)); err != nil {
		t.Fatalf("record the launch %s at %d: %v", agent, generation, err)
	}
}

// launchRead is the read numbered read that the daemon received at at, of a launch whose harness
// is set to Claude Opus 5 and in which each of counts answered.
func launchRead(read uint64, at time.Time, counts ...shimwire.ModelCount) LaunchRead {
	if counts == nil {
		counts = []shimwire.ModelCount{}
	}
	return LaunchRead{Totals: shimwire.Totals{Read: read, SetModel: "anthropic/claude-opus-5", Counts: counts}, At: at}
}

// opus is a count of calls Claude Opus 5 answered.
func opus(calls uint64) shimwire.ModelCount {
	return shimwire.ModelCount{Model: "anthropic/claude-opus-5", Calls: calls}
}

// readText is a read as a failure prints it: its number, its counts and its moment, or <none>.
func readText(read *LaunchRead) string {
	if read == nil {
		return "<none>"
	}
	return fmt.Sprintf("{read %d counts %v at %s}", read.Totals.Read, read.Totals.Counts, read.At.UTC().Format(time.RFC3339Nano))
}

// launchesText is each launch of launches as a failure prints it, in their order.
func launchesText(launches []Launch) string {
	parts := make([]string, len(launches))
	for i, launch := range launches {
		parts[i] = fmt.Sprintf("%d newest %s final %s", launch.Generation, readText(launch.Newest), readText(launch.Final))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// sameRead is whether got is want: the same totals, received at the same instant.
func sameRead(got *LaunchRead, want *LaunchRead) bool {
	if got == nil || want == nil {
		return got == want
	}
	return reflect.DeepEqual(got.Totals, want.Totals) && got.At.Equal(want.At)
}

// launchesOf is each launch of agent the store holds, from generation 0, read in its own
// transaction.
func launchesOf(t *testing.T, tx pgx.Tx, agent claim.Token) []Launch {
	t.Helper()
	launches, err := NewStore().AgentLaunches(t.Context(), tx, agent, 0)
	must(t, err)
	return launches.Launches
}

// assertLaunches fails unless launches are want, launch by launch: each generation, with its newest
// read and its final totals.
func assertLaunches(t *testing.T, what string, launches []Launch, want ...Launch) {
	t.Helper()
	same := len(launches) == len(want)
	for i := 0; same && i < len(want); i++ {
		same = launches[i].Generation == want[i].Generation && sameRead(launches[i].Newest, want[i].Newest) &&
			sameRead(launches[i].Final, want[i].Final)
	}
	if !same {
		t.Fatalf("%s: the launches are %s, want %s", what, launchesText(launches), launchesText(want))
	}
}

// Two facts can read one launch in either order, so a read that commits later can be the older
// one. The shim raises a launch's read number at each read, and the store keeps a read only where
// its number is above the one it holds. An older read and a second read of the same number leave
// the newer one, and a read with a higher number replaces it. A read of a launch with no row, one
// that started under the release before, records that launch with its read.
func TestKeepReadKeepsTheHigherReadNumber(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const agent claim.Token = "legion-legion-legion-209-implementer"
	at := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)
	second := launchRead(2, at, opus(4))
	// The older read was received later: the number decides, not the moment.
	first := launchRead(1, at.Add(time.Minute), opus(3))
	againSecond := launchRead(2, at.Add(2*time.Minute), opus(5))
	fifth := launchRead(5, at.Add(3*time.Minute), opus(9))
	inTx(t, st, func(tx pgx.Tx) {
		putAgentClaim(t, tx, agentClaim{token: agent, issue: "LEGION-209", tree: "LEGION-208", role: claim.RoleImplementer, generation: 3, live: true})
		putLaunch(t, tx, agent, 3)
		keep := func(generation uint64, read LaunchRead) {
			t.Helper()
			must(t, store.KeepRead(t.Context(), tx, LaunchKey{Claim: agent, Generation: generation}, read))
		}
		keep(3, second)
		assertLaunches(t, "after the first read", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &second})
		keep(3, first)
		assertLaunches(t, "after an older read", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &second})
		keep(3, againSecond)
		assertLaunches(t, "after a second read of the same number", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &second})
		keep(3, fifth)
		assertLaunches(t, "after a newer read", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &fifth})
		keep(2, first)
		assertLaunches(t, "after a read of a launch with no row", launchesOf(t, tx, agent),
			Launch{Generation: 2, Newest: &first}, Launch{Generation: 3, Newest: &fifth})
	})
}

// The shim sends its final totals once, when its harness exits, and the daemon may hand the same
// frame to the store twice. The store keeps the first final it gets and leaves the newest read
// alone. A final of a launch with no row, one that started under the release before, records that
// launch with its final, and a read of it after that leaves the final alone too.
func TestKeepFinalKeepsTheFirstFinal(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const agent claim.Token = "legion-legion-legion-209-tester"
	at := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)
	newest := launchRead(3, at, opus(2))
	final := launchRead(4, at.Add(time.Minute), opus(6))
	later := launchRead(5, at.Add(2*time.Minute), opus(7))
	inTx(t, st, func(tx pgx.Tx) {
		putAgentClaim(t, tx, agentClaim{token: agent, issue: "LEGION-209", tree: "LEGION-208", role: claim.RoleTester, generation: 4})
		putLaunch(t, tx, agent, 3)
		must(t, store.KeepRead(t.Context(), tx, LaunchKey{Claim: agent, Generation: 3}, newest))
		keepFinal := func(generation uint64, read LaunchRead) {
			t.Helper()
			must(t, store.KeepFinal(t.Context(), tx, LaunchKey{Claim: agent, Generation: generation}, read))
		}
		keepFinal(3, final)
		assertLaunches(t, "after the final", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &newest, Final: &final})
		keepFinal(3, later)
		assertLaunches(t, "after a second final", launchesOf(t, tx, agent), Launch{Generation: 3, Newest: &newest, Final: &final})
		keepFinal(4, final)
		assertLaunches(t, "after a final of a launch with no row", launchesOf(t, tx, agent),
			Launch{Generation: 3, Newest: &newest, Final: &final}, Launch{Generation: 4, Final: &final})
		must(t, store.KeepRead(t.Context(), tx, LaunchKey{Claim: agent, Generation: 4}, later))
		assertLaunches(t, "after a read of that launch", launchesOf(t, tx, agent),
			Launch{Generation: 3, Newest: &newest, Final: &final}, Launch{Generation: 4, Newest: &later, Final: &final})
	})
}

// A step's end reads each launch of its agent from the generation its start read. AgentLaunches
// answers the claim's generation now, whether the claim records a process now, and each launch
// row of that claim from since on, in generation order, with its newest read and its final. A
// launch of another claim is none of them, and a claim the store does not hold has none.
func TestAgentLaunchesReadsEachLaunchSinceAGeneration(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const agent, other claim.Token = "legion-legion-legion-209-reviewer", "legion-legion-legion-210-reviewer"
	at := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	ended := launchRead(8, at, opus(11))
	newest := launchRead(2, at.Add(time.Hour), opus(1))
	inTx(t, st, func(tx pgx.Tx) {
		putAgentClaim(t, tx, agentClaim{token: agent, issue: "LEGION-209", tree: "LEGION-208", role: claim.RoleReviewer, generation: 5, live: true})
		putAgentClaim(t, tx, agentClaim{token: other, issue: "LEGION-210", tree: "LEGION-208", role: claim.RoleReviewer, generation: 3, live: true})
		for _, generation := range []uint64{2, 3, 5} {
			putLaunch(t, tx, agent, generation)
		}
		putLaunch(t, tx, other, 3)
		must(t, store.KeepFinal(t.Context(), tx, LaunchKey{Claim: agent, Generation: 3}, ended))
		must(t, store.KeepRead(t.Context(), tx, LaunchKey{Claim: agent, Generation: 5}, newest))
		must(t, store.KeepRead(t.Context(), tx, LaunchKey{Claim: other, Generation: 3}, newest))
		for _, tc := range []struct {
			since uint64
			want  []Launch
		}{
			{0, []Launch{{Generation: 2}, {Generation: 3, Final: &ended}, {Generation: 5, Newest: &newest}}},
			{3, []Launch{{Generation: 3, Final: &ended}, {Generation: 5, Newest: &newest}}},
			{4, []Launch{{Generation: 5, Newest: &newest}}},
			{6, nil},
		} {
			got, err := store.AgentLaunches(t.Context(), tx, agent, tc.since)
			must(t, err)
			if got.Generation != 5 || !got.Live {
				t.Fatalf("AgentLaunches since %d = generation %d, live %t; want the claim's generation 5, live", tc.since, got.Generation, got.Live)
			}
			assertLaunches(t, fmt.Sprintf("since %d", tc.since), got.Launches, tc.want...)
		}
		if _, err := tx.Exec(t.Context(), `update claims set locator = null, state = 'suspended' where token = $1`, string(agent)); err != nil {
			t.Fatalf("let the claim's process go: %v", err)
		}
		got, err := store.AgentLaunches(t.Context(), tx, agent, 5)
		must(t, err)
		if got.Generation != 5 || got.Live {
			t.Fatalf("AgentLaunches of a claim with no process = generation %d, live %t; want generation 5, not live", got.Generation, got.Live)
		}
		assertLaunches(t, "with no process", got.Launches, Launch{Generation: 5, Newest: &newest})
		unknown, err := store.AgentLaunches(t.Context(), tx, "legion-legion-legion-404-reviewer", 0)
		must(t, err)
		if unknown.Generation != 0 || unknown.Live || len(unknown.Launches) != 0 {
			t.Fatalf("AgentLaunches of a claim the store does not hold = generation %d, live %t, %s; want nothing",
				unknown.Generation, unknown.Live, launchesText(unknown.Launches))
		}
	})
}

// A step's start keeps what it read of its agent (StartTotals). A launch after the start counts
// from zero, and FromZero is the first generation that does. A live launch is the start's own,
// so the next generation is the first. A claim with no process whose generation has a launch row
// ran that launch before the step, so the next generation is the first again. A claim with no
// process and no row at its generation has not launched it, so that generation is the first.
func TestAgentLaunchesGiveTheStartTotalsAndTheirFirstLaunchFromZero(t *testing.T) {
	at := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	read := launchRead(3, at, opus(2))
	for _, tc := range []struct {
		name     string
		launches AgentLaunches
		used     *LaunchRead
		want     StartTotals
	}{
		{"a live launch with a read", AgentLaunches{Generation: 5, Live: true, Launches: []Launch{{Generation: 2}, {Generation: 5}}},
			&read, StartTotals{Generation: 5, Live: true, Read: &read, FromZero: 6}},
		{"a live launch with no usable read", AgentLaunches{Generation: 5, Live: true, Launches: []Launch{{Generation: 5}}},
			nil, StartTotals{Generation: 5, Live: true, FromZero: 6}},
		{"a launch that ended before the step", AgentLaunches{Generation: 5, Launches: []Launch{{Generation: 2}, {Generation: 5, Final: &read}}},
			nil, StartTotals{Generation: 5, FromZero: 6}},
		{"a generation not launched yet", AgentLaunches{Generation: 5, Launches: []Launch{{Generation: 2}, {Generation: 3}}},
			nil, StartTotals{Generation: 5, FromZero: 5}},
		{"a claim never launched", AgentLaunches{}, nil, StartTotals{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.launches.StartTotals(tc.used)
			if got.Generation != tc.want.Generation || got.Live != tc.want.Live || got.FromZero != tc.want.FromZero || !sameRead(got.Read, tc.want.Read) {
				t.Fatalf("StartTotals = {generation %d live %t read %s fromZero %d}, want {generation %d live %t read %s fromZero %d}",
					got.Generation, got.Live, readText(got.Read), got.FromZero,
					tc.want.Generation, tc.want.Live, readText(tc.want.Read), tc.want.FromZero)
			}
		})
	}
}

// A step's start keeps what it read of its agent on its row (KeepStartTotals), and the end reads
// it back with the start (StepStarts, EndStepStart). The next start of that place is another run,
// so it clears what the run before kept: its end must not count from an earlier run's read. So
// does the boot's open of a run no start counted, which read nothing. Keeping start totals for a
// place no start raised is refused.
func TestANewStartClearsTheStartTotalsOfTheRunBefore(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const tester = "legion-legion-legion-1-tester"
	testing3 := StepPlace{Task: "default", Kind: "step", Place: 3}
	at := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)
	read := launchRead(4, at, opus(3))
	kept := StartTotals{Generation: 2, Live: true, Read: &read, FromZero: 3}
	startTotalsOf := func(tx pgx.Tx) *StartTotals {
		t.Helper()
		start, err := store.StepStarts(t.Context(), tx, "LEGION-1", testing3)
		must(t, err)
		return start.StartTotals
	}
	sameStart := func(what string, got *StartTotals, want *StartTotals) {
		t.Helper()
		if (got == nil) != (want == nil) || got != nil && (got.Generation != want.Generation || got.Live != want.Live ||
			got.FromZero != want.FromZero || !sameRead(got.Read, want.Read)) {
			t.Fatalf("%s: the start totals are %+v, want %+v", what, got, want)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		raise(t, tx, "LEGION-1", testing3, tester, at)
		sameStart("a start that kept none", startTotalsOf(tx), nil)
		must(t, store.KeepStartTotals(t.Context(), tx, "LEGION-1", testing3, kept))
		sameStart("the start's own read", startTotalsOf(tx), &kept)
		ended, closed, err := store.EndStepStart(t.Context(), tx, "LEGION-1", testing3)
		if err != nil || !closed {
			t.Fatalf("EndStepStart = %t, %v; want the run ended now", closed, err)
		}
		sameStart("the end's read of its start", ended.StartTotals, &kept)
		raise(t, tx, "LEGION-1", testing3, tester, at.Add(time.Hour))
		sameStart("the next start", startTotalsOf(tx), nil)

		must(t, store.KeepStartTotals(t.Context(), tx, "LEGION-1", testing3, kept))
		if _, err := tx.Exec(t.Context(), `update step_starts set run = 'uncounted' where issue = 'LEGION-1'`); err != nil {
			t.Fatalf("mark the place uncounted, as the roll forward of a rollback does: %v", err)
		}
		opened, err := store.OpenUncountedStep(t.Context(), tx, "LEGION-1", testing3, func(starts int64) StepRecord {
			return StepRecord{ID: fmt.Sprint(starts), Task: "default", Step: 3, Kind: "step", Role: "tester", Agent: tester, Generation: 1}
		})
		if err != nil || !opened {
			t.Fatalf("OpenUncountedStep = %t, %v; want the uncounted run opened", opened, err)
		}
		sameStart("the boot's open of an uncounted run", startTotalsOf(tx), nil)

		err = store.KeepStartTotals(t.Context(), tx, "LEGION-1", StepPlace{Task: "default", Kind: "step", Place: 4}, kept)
		if err == nil || !strings.Contains(err.Error(), "no start") {
			t.Fatalf("KeepStartTotals of a place no start raised = %v, want a refusal naming no start", err)
		}
	})
}

// The agents a fact of an issue can start or end a step of are the issue's own claims and the
// architect of its tree, which signs a step off and stands in its acceptance, whichever claim of
// them is not retired. A claim of another issue of the tree, a retired claim and an architect of
// another tree are none of them. An issue the record does not hold has its own claims alone.
func TestStepAgentClaimsAreTheIssuesClaimsAndItsTreesArchitect(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		child := issueFixture("LEGION-209")
		child.Tree = "LEGION-208"
		must(t, store.PutIssue(t.Context(), tx, child))
		must(t, store.PutIssue(t.Context(), tx, issueFixture("LEGION-208")))
		for _, c := range []agentClaim{
			{token: "legion-legion-legion-209-implementer", issue: "LEGION-209", tree: "LEGION-208", role: claim.RoleImplementer, live: true},
			{token: "legion-legion-legion-209-planner", issue: "LEGION-209", tree: "LEGION-208", role: claim.RolePlanner, state: supervise.StateSuspended},
			{token: "legion-legion-legion-209-tester", issue: "LEGION-209", tree: "LEGION-208", role: claim.RoleTester, state: supervise.StateRetired},
			{token: "legion-legion-legion-208-architect", issue: "LEGION-208", tree: "LEGION-208", role: claim.RoleArchitect, live: true},
			{token: "legion-legion-legion-208-implementer", issue: "LEGION-208", tree: "LEGION-208", role: claim.RoleImplementer, live: true},
			{token: "legion-legion-legion-210-implementer", issue: "LEGION-210", tree: "LEGION-208", role: claim.RoleImplementer, live: true},
			{token: "legion-legion-legion-300-architect", issue: "LEGION-300", tree: "LEGION-300", role: claim.RoleArchitect, live: true},
			{token: "legion-legion-legion-404-implementer", issue: "LEGION-404", tree: "LEGION-404", role: claim.RoleImplementer, live: true},
		} {
			putAgentClaim(t, tx, c)
		}
		for issue, want := range map[string][]claim.Token{
			"LEGION-209": {"legion-legion-legion-208-architect", "legion-legion-legion-209-implementer", "legion-legion-legion-209-planner"},
			"LEGION-208": {"legion-legion-legion-208-architect", "legion-legion-legion-208-implementer"},
			"LEGION-404": {"legion-legion-legion-404-implementer"},
			"LEGION-405": nil,
		} {
			got, err := store.StepAgentClaims(t.Context(), tx, issue)
			must(t, err)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("StepAgentClaims(%s) = %v, want %v", issue, got, want)
			}
		}
	})
}
