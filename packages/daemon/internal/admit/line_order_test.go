package admit

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// lineIssue is one record of the waiting line's fixture, with the fields the line reads and
// nothing else: the rest of a record says nothing about where an issue stands in it. An issue
// whose tree is not its own key stands under that tree's root on Dispatch's chain (putCopyTx).
func lineIssue(key, tree, status string, stands phase.Phase, rank string) record.Issue {
	return record.Issue{Key: key, Tree: tree, Project: testProject, Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// seedChains records issues with their rows of the copy (putIssue) and reads the chains of the
// test project as one pass reads them (record.ReadChains), every issue named: what a test of
// record.Waiting over facts of its own hands the line as LineFacts.Chains.
func seedChains(t *testing.T, pool *pgxpool.Pool, issues ...record.Issue) *record.Chains {
	t.Helper()
	keys := make([]string, len(issues))
	for i, issue := range issues {
		putIssue(t, pool, issue)
		keys[i] = issue.Key
	}
	var chains *record.Chains
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if chains, err = record.ReadChains(context.Background(), tx, record.NewStore(), testProject, keys); err != nil {
			t.Fatalf("read the chains: %v", err)
		}
	})
	return chains
}

// A4. The waiting line's order, read from record.Waiting over one pass's facts.
//
// Its first part is the issues whose work came back, oldest first: an approved root whose own
// first phase can start, placed at the moment its gate opened, and an issue that owes its worker
// a start, placed at that row's creation. Its second part is the issues whose first phase has
// never started, by the Dispatch rank of their tree's root: a root that never ran and ranks
// above a live tree's root goes before that tree's child, and that child goes before a root
// ranked below its tree's root. A person's todo on a running root that holds no slot does not
// put it in the line as a root that never ran.
func TestTheWaitingLineRunsReturnsFirstThenTheBoardsRank(t *testing.T) {
	opened := fixedNow.Add(-2 * time.Hour)
	approved := 1
	issues := []record.Issue{
		// A live tree ranked in the middle of the board, its gate open, its root's own first
		// phase still to come and one released child waiting for its first.
		lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
		// A child of that tree whose work came back: its implementer's start waits for a slot.
		lineIssue("LEGION-12", "LEGION-10", "in_progress", phase.Implementing, "O"),
		// A root that never ran, ranked above the live tree's root.
		lineIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A"),
		// A root that never ran, ranked below it.
		lineIssue("LEGION-30", "LEGION-30", "todo", phase.Admitted, "Z"),
		// A root a person moved back to todo while its own tree runs and it waits in accepting:
		// it holds no slot, and it is no root that never ran.
		lineIssue("LEGION-50", "LEGION-50", "todo", phase.Accepting, "B"),
	}
	facts := record.LineFacts{
		Chains: seedChains(t, migratedPool(t), issues...),
		Gates: map[string]record.TreeGate{
			"LEGION-10": {Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: "LEGION-10", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}}},
			"LEGION-50": {Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: "LEGION-50", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 8, At: opened}, EverOpened: true}}},
		},
		Owed: map[string]record.OwedStart{
			"LEGION-12": {Issue: "LEGION-12", ID: 42, CreatedAt: fixedNow.Add(-time.Hour)},
		},
	}

	var got []string
	var kinds []record.LineKind
	line, _ := record.Waiting(issues, nil, facts)
	for _, entry := range line {
		got = append(got, entry.Issue.Key)
		kinds = append(kinds, entry.Kind)
	}
	want := []string{"LEGION-10", "LEGION-12", "LEGION-20", "LEGION-11", "LEGION-30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the approved root's own first phase and the returning child first, then the board's rank", got, want)
	}
	wantKinds := []record.LineKind{record.LineFirstPhase, record.LineReturning, record.LineQueuedRoot, record.LineFirstPhase, record.LineQueuedRoot}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("the line's kinds = %v, want %v", kinds, wantKinds)
	}

	// The gate opened before the child's start came back, so the root goes first; an approval
	// that lands after it puts the root behind the child, on the one time axis the two share.
	gate := facts.Gates["LEGION-10"].Rows[record.DesignStepSpec]
	gate.ApprovedMark.At = fixedNow
	facts.Gates["LEGION-10"] = record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: gate}}
	got = nil
	reopened, _ := record.Waiting(issues, nil, facts)
	for _, entry := range reopened {
		got = append(got, entry.Issue.Key)
	}
	if want := []string{"LEGION-12", "LEGION-10", "LEGION-20", "LEGION-11", "LEGION-30"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line with the gate opened last = %v, want %v", got, want)
	}
}

// A4's blocker clause: an issue whose first phase an unfinished Dispatch issue holds where it
// stands is in no line, however long it has waited, and the line the caller reads says so only
// because the caller judged the mirror for it (record.LineFacts.Held, blockers.HeldIssues).
func TestTheWaitingLineLeavesOutAnIssueABlockerHolds(t *testing.T) {
	issues := []record.Issue{
		lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
	}
	approved := 1
	facts := record.LineFacts{
		Chains: seedChains(t, migratedPool(t), issues...),
		Gates: map[string]record.TreeGate{
			"LEGION-10": {Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: "LEGION-10", Step: record.DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}}},
		},
		Held: map[string][]string{"LEGION-11": {"LEGION-900"}},
	}
	var got []string
	line, _ := record.Waiting(issues, nil, facts)
	for _, entry := range line {
		got = append(got, entry.Issue.Key)
	}
	if want := []string{"LEGION-10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the held child waits on its blocker, not on a slot", got, want)
	}
}

// R19 of dispatch://INFRA-3320 (k29): the line's two parts. Work that came back comes first,
// oldest first; then first starts by the Dispatch rank of each issue's top issue — the highest
// issue on its chain in Legion's work — and, within one top issue, by its own rank. The top issue
// is read from Dispatch's chain as the copy holds it and never from the stored tree: LEGION-82
// stores LEGION-81 as its tree, whose rank stands first on the board, and stands under it on the
// chain, but LEGION-81 holds no gate of its own and LEGION-80 above it is the top issue, so
// LEGION-82 takes LEGION-80's place in the board's order.
func TestTheLineRanksFirstStartsByTheTopIssue(t *testing.T) {
	opened := func(at time.Time) record.TreeGate {
		approved := 1
		return record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Step: record.DesignStepSpec,
			LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: at}, EverOpened: true}}}
	}
	under := func(issue record.Issue, parent string) record.Issue {
		issue.Parent = record.ParentOf(parent)
		return issue
	}
	// A top issue ranked B whose own first phase came back two hours ago, an issue at work under
	// it, two issues under that one waiting for their first phase, and one that owes its worker a
	// start an hour ago. LEGION-82's stored tree is LEGION-81, ranked 0.
	top := lineIssue("LEGION-80", "LEGION-80", "in_progress", phase.Admitted, "B")
	middle := under(lineIssue("LEGION-81", "LEGION-80", "in_progress", phase.Implementing, "0"), "LEGION-80")
	late := under(lineIssue("LEGION-82", "LEGION-81", "todo", phase.Admitted, "Z"), "LEGION-81")
	early := under(lineIssue("LEGION-83", "LEGION-80", "todo", phase.Admitted, "D"), "LEGION-81")
	owing := under(lineIssue("LEGION-84", "LEGION-80", "in_progress", phase.Implementing, "E"), "LEGION-80")
	// A top issue ranked A whose gate opened three hours ago, with one issue under it waiting for
	// its first phase, ranked Y; and a root that never ran, ranked C.
	first := lineIssue("LEGION-90", "LEGION-90", "in_progress", phase.Admitted, "A")
	firstChild := under(lineIssue("LEGION-91", "LEGION-90", "todo", phase.Admitted, "Y"), "LEGION-90")
	queued := lineIssue("LEGION-95", "LEGION-95", "todo", phase.Admitted, "C")
	issues := []record.Issue{top, middle, late, early, owing, first, firstChild, queued}
	facts := record.LineFacts{
		Chains: seedChains(t, migratedPool(t), issues...),
		Gates: map[string]record.TreeGate{"LEGION-80": opened(fixedNow.Add(-2 * time.Hour)),
			"LEGION-90": opened(fixedNow.Add(-3 * time.Hour))},
		Owed: map[string]record.OwedStart{"LEGION-84": {Issue: "LEGION-84", ID: 7, CreatedAt: fixedNow.Add(-time.Hour)}},
	}

	line, _ := record.Waiting(issues, nil, facts)
	var got []string
	for _, entry := range line {
		got = append(got, entry.Issue.Key)
	}
	want := []string{"LEGION-90", "LEGION-80", "LEGION-84", "LEGION-91", "LEGION-83", "LEGION-82", "LEGION-95"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the work that came back oldest first, then each first start by its top issue's rank and its own", got, want)
	}
	tops := map[string][2]string{"LEGION-91": {"LEGION-90", "A"}, "LEGION-83": {"LEGION-80", "B"},
		"LEGION-82": {"LEGION-80", "B"}, "LEGION-95": {"LEGION-95", "C"}}
	for _, entry := range line {
		wantTop, starting := tops[entry.Issue.Key]
		if !starting {
			if entry.Part != record.PartReturned {
				t.Errorf("%s stands in part %v, want the returned part", entry.Issue.Key, entry.Part)
			}
			continue
		}
		if entry.Part != record.PartStarting || entry.RootKey != wantTop[0] || entry.RootRank != wantTop[1] {
			t.Errorf("%s stands in part %v under top issue %q at rank %q, want the starting part under %s at %s",
				entry.Issue.Key, entry.Part, entry.RootKey, entry.RootRank, wantTop[0], wantTop[1])
		}
	}
}
