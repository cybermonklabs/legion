package admit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The matrix of the P0 start over the priority Dispatch resolves (C1 to C4 of
// dispatch://INFRA-5636, the admission rows of its Verification's c-priority, c-p0-start and
// c-bound that the rows of p0_start_test.go, p0_resolved_test.go and p0_bound_test.go leave
// open). An issue is P0 for admission when Dispatch resolves it to P0, by its own priority
// (explicit) or from an issue above it (inherited), and every start, return, count, limit, order,
// reason and log line treats the two alike, through Issue.StartsNow and record.HoldersOf alone.
// Every resolved value a row reads is taken from p0Priorities, every kind of P0 issue from
// p0Kinds and every kind of place in the line from record.LineKind, never a sample of them.

// p0Kind is one way an issue is P0 for admission.
type p0Kind struct {
	name string
	// own is the issue's own priority and resolved the one Dispatch resolves it to: an explicit
	// P0 is its own, which Dispatch resolves to itself, and an inherited one has none of its own
	// and comes from outside.
	own      record.Priority
	resolved record.Resolved
}

// p0Kinds is every kind of P0 issue.
var p0Kinds = []p0Kind{
	{name: "explicit", own: record.P0, resolved: record.Resolved{Priority: record.P0, Known: true}},
	{name: "inherited", own: record.NoPriority, resolved: resolvedFrom(record.P0)},
}

// shape writes the kind's priorities on issue.
func (k p0Kind) shape(issue *record.Issue) { issue.Priority, issue.Resolved = k.own, k.resolved }

// resolvedTo is the shape of an issue with no priority of its own that Dispatch resolves to
// priority (resolvedFrom).
func resolvedTo(priority record.Priority) func(*record.Issue) {
	return func(issue *record.Issue) { issue.Priority, issue.Resolved = record.NoPriority, resolvedFrom(priority) }
}

// matrixMember records key under root standing in stands at status, as p0Member records one,
// with no legion label, and with what shape says of its priorities.
func matrixMember(t *testing.T, pool *pgxpool.Pool, root, key, rank string, stands phase.Phase, status string, shape func(*record.Issue)) {
	t.Helper()
	parent := root
	member := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: root, Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: rank,
		LastDispatchSeq: 2}
	if shape != nil {
		shape(&member)
	}
	putIssue(t, pool, member)
}

// resolveAsOwn writes on key's record the resolved priority Dispatch gives an issue whose own
// priority it has: that own priority itself.
func resolveAsOwn(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	stored := issue(t, pool, key)
	stored.Resolved = record.Resolved{Priority: stored.Priority, Known: true}
	putIssue(t, pool, stored)
}

// echoOf is a Dispatch issue.updated of stored at seq, decoded by the daemon's own decoder
// (intake.DecodeDispatchEvent) from the bytes Dispatch writes. It shows the issue as its record
// holds it — status, parent, rank and label — with title, its own priority own and its resolved
// priority resolved (resolvedPayload); changed is its `changed` as JSON, "" for a payload with
// none at all. The decoder reads only an issue key of the project, a number after it.
func echoOf(t *testing.T, stored record.Issue, seq int64, title string, own record.Priority, resolved record.Resolved, changed string) intake.DispatchIssue {
	t.Helper()
	parent, labels, ownLevel := "null", "[]", "null"
	if stored.Parent != nil {
		parent = strconv.Quote(*stored.Parent)
	}
	if stored.HandedOver {
		labels = fmt.Sprintf("[%q]", dispatch.LegionLabel)
	}
	if l := own.Level(); l != nil {
		ownLevel = strconv.Itoa(*l)
	}
	if changed != "" {
		changed = `,"changed":` + changed
	}
	payload := fmt.Sprintf(`{"key":%q,"status":%q,"title":%q,"parent":%s,"rank":%q,"priority":%s%s,"labels":%s,"resolved_repo":"acme/widgets","blocked_by":[],"claim":null%s}`,
		stored.Key, stored.Status, title, parent, stored.Rank, ownLevel, resolvedPayload(resolved), labels, changed)
	event := fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":%d,"actor":{"kind":"user","id":"human"},"notify":true,"type":"issue.updated","created_at":"2026-09-23T11:00:00Z","payload":%s}`,
		seq, stored.Key, seq, payload)
	fact, unread, err := intake.DecodeDispatchEvent(testProject, stored.Key, []byte(event))
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode the event of %s: %v, unread %v", stored.Key, err, unread)
	}
	decoded, ok := fact.(intake.DispatchIssue)
	if !ok {
		t.Fatalf("the event of %s decoded to %#v, want a Dispatch issue: is %s an issue key of %s?", stored.Key, fact, stored.Key, testProject)
	}
	return decoded
}

// engineAt is the workflow engine of quiet's admission reading the clock at, for a case that reads
// the movement an event marks: the store moves a mark forward only (record's MarkIssueMoved), so
// the clock must stand past the mark the case seeded for a mark the event writes to show.
func engineAt(at time.Time) *workflow.Engine {
	return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGate("").Steps(), Clock: func() time.Time { return at }}, nil)
}

// lineEntryOf is key's entry in the test project's waiting line, judged as waitingLine judges it,
// and false where the line holds none.
func lineEntryOf(t *testing.T, pool *pgxpool.Pool, key string) (record.LineEntry, bool) {
	t.Helper()
	var found record.LineEntry
	var stands bool
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.Issues(ctx, tx)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		_, _, line, _, err := blockers.JudgeLine(ctx, tx, records, testProject, issues, slots, facts, nil)
		if err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
		for _, entry := range line {
			if entry.Issue.Key == key {
				found, stands = entry, true
			}
		}
	})
	return found, stands
}

// assertStandsAs reads that key stands in the waiting line as kind, in part, holding a slot or
// not as holds says: the place a row of the line's kinds is about, read before the pass that
// promotes it.
func assertStandsAs(t *testing.T, pool *pgxpool.Pool, key string, kind record.LineKind, part record.LinePart, holds bool) {
	t.Helper()
	entry, stands := lineEntryOf(t, pool, key)
	if !stands {
		t.Fatalf("%s stands nowhere in the waiting line %v, want it there as %s", key, waitingLine(t, pool, testProject, nil), kind)
	}
	if entry.Kind != kind || entry.Part != part || entry.Holds != holds {
		t.Fatalf("%s stands in the line as %s in part %s holding a slot %t, want %s in part %s holding a slot %t",
			key, entry.Kind, entry.Part, entry.Holds, kind, part, holds)
	}
}

// levelOf is priority as the state's JSON carries it: its number, or nil for none.
func levelOf(priority record.Priority) any {
	if l := priority.Level(); l != nil {
		return float64(*l)
	}
	return nil
}

// waitingView is key's `waitingLine` in the state the projection gives now.
func waitingView(t *testing.T, pool *pgxpool.Pool, key string) map[string]any {
	t.Helper()
	view, ok := issueView(t, stateBody(t, pool), key)["waitingLine"].(map[string]any)
	if !ok {
		t.Fatalf("the state gives %s no waitingLine", key)
	}
	return view
}

// loggedAs is every line of logged, a JSON log, whose message is msg.
func loggedAs(t *testing.T, logged *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logged.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("read the log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			lines = append(lines, entry)
		}
	}
	return lines
}

// Under a running tree root of each priority, a member with no priority of its own that Dispatch
// resolves to each value, from outside, waits for its first phase at a cap two holders below P0
// fill. It starts exactly where Dispatch resolves it to P0, and every other member waits in the
// band of the value Dispatch resolved, whatever its tree root's priority. A member whose own P1
// Dispatch resolves to P1 under the P0 root waits at P1, behind no P0 issue.
func TestEveryResolvedPriorityUnderARootOfEachPriority(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	p0Holder(t, pool, "LEGION-1", "A", record.P2)
	p0Holder(t, pool, "LEGION-2", "B", record.P3)
	var members []string
	want := map[string]record.Priority{}
	for r, rootPriority := range p0Priorities {
		root := "LEGION-" + string(rune('A'+r))
		p0Tree(t, pool, root, "C"+string(rune('a'+r)), rootPriority, true)
		resolveAsOwn(t, pool, root)
		for c, resolved := range p0Priorities {
			member := root + string(rune('0'+c))
			matrixMember(t, pool, root, member, "C"+string(rune('a'+r))+string(rune('0'+c)), phase.Admitted, "todo", resolvedTo(resolved))
			members, want[member] = append(members, member), resolved
		}
	}
	const ownP1 = "LEGION-A9"
	matrixMember(t, pool, "LEGION-A", ownP1, "Ca9", phase.Admitted, "todo", func(issue *record.Issue) {
		issue.Priority, issue.Resolved = record.P1, record.Resolved{Priority: record.P1, Known: true}
	})
	members, want[ownP1] = append(members, ownP1), record.P1

	tick(t, pool, admission, "tick")

	body := stateBody(t, pool)
	for _, member := range members {
		resolved := want[member]
		starts := resolved == record.P0
		if got := holdsSlot(t, pool, member); got != starts {
			t.Errorf("%s, resolved to %s under %s, holds a slot = %t, want %t", member, priorityName(resolved), issue(t, pool, member).Tree, got, starts)
			continue
		}
		if starts {
			continue
		}
		line, ok := issueView(t, body, member)["waitingLine"].(map[string]any)
		if !ok {
			t.Errorf("%s, resolved to %s, stands in no waiting line", member, priorityName(resolved))
			continue
		}
		if got := line["priority"]; got != levelOf(resolved) {
			t.Errorf("%s, resolved to %s under %s, waits at the line's priority %v, want %v", member, priorityName(resolved),
				issue(t, pool, member).Tree, got, levelOf(resolved))
		}
	}
	for _, holder := range []string{"LEGION-1", "LEGION-2"} {
		if !holdsSlot(t, pool, holder) {
			t.Errorf("%s lost its slot: the pass takes no slot from a holder", holder)
		}
	}
}

// Every hold on a start but the cap stays for a P0 issue of each kind: a root without the legion
// label, a root not in todo, a blocked_by link to an open issue, a closed design gate, an
// ancestor's hold and a paused project each keep it out of a slot at a cap its holders below P0
// fill. Each case holds a free P0 root of the same kind beside the held one, which does start, so a
// case in which nothing started would read as a pass that never ran. A case that names a held
// reason reads the pass's held list too, which names the held issue with that reason and the issue
// that holds it.
func TestEveryHoldButTheCapStaysForAnInheritedP0Issue(t *testing.T) {
	const held, free = "LEGION-50", "LEGION-60"
	for _, k := range p0Kinds {
		t.Run(k.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				seed func(t *testing.T, pool *pgxpool.Pool)
				// reason is the word the held list names the held issue with, and by the issue
				// that holds it; "" for a case the list is not read in.
				reason record.HeldReason
				by     string
			}{
				{name: "a root without the legion label", seed: func(t *testing.T, pool *pgxpool.Pool) {
					p0Queued(t, pool, held, "C", record.NoPriority, func(issue *record.Issue) { k.shape(issue); issue.HandedOver = false })
				}},
				{name: "a root not in todo", seed: func(t *testing.T, pool *pgxpool.Pool) {
					p0Queued(t, pool, held, "C", record.NoPriority, func(issue *record.Issue) { k.shape(issue); issue.Status = "backlog" })
				}},
				{name: "a blocked_by link to an open issue", seed: func(t *testing.T, pool *pgxpool.Pool) {
					p0Queued(t, pool, held, "C", record.NoPriority, k.shape)
					seedMirror(t, pool, held, "todo", "LEGION-70")
					seedMirror(t, pool, "LEGION-70", "in_progress")
				}},
				{name: "a closed design gate", seed: func(t *testing.T, pool *pgxpool.Pool) {
					// The tree's root holds the row it took at its admission, below P0, so it and
					// LEGION-1 fill the cap.
					p0Tree(t, pool, "LEGION-40", "Bz", record.P2, false)
					matrixMember(t, pool, "LEGION-40", held, "C", phase.Admitted, "todo", k.shape)
				}},
				{name: "an ancestor's hold", reason: record.HeldAncestry, by: "LEGION-1", seed: func(t *testing.T, pool *pgxpool.Pool) {
					// Its Dispatch parent LEGION-41 stands on the backlog, out of Legion's work,
					// under LEGION-1, which is at work, and the copy of Dispatch's issues shows
					// that chain: the ancestry hold (record.HeldReasonOf).
					putIssue(t, pool, chainRecord("LEGION-41", "LEGION-1", phase.Admitted, "backlog"))
					p0Queued(t, pool, held, "C", record.NoPriority, func(issue *record.Issue) { k.shape(issue); issue.Parent = new("LEGION-41") })
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					pool := migratedPool(t)
					admission := quiet(t, 2)
					p0Holder(t, pool, "LEGION-1", "A", record.P1)
					if tc.name != "a closed design gate" {
						p0Holder(t, pool, "LEGION-2", "B", record.P1)
					}
					tc.seed(t, pool)
					p0Queued(t, pool, free, "D", record.NoPriority, k.shape)

					tick(t, pool, admission, "tick")

					if holdsSlot(t, pool, held) {
						t.Fatalf("%s, an %s P0, holds a slot: %s holds it as it holds every other issue", held, k.name, tc.name)
					}
					if !holdsSlot(t, pool, free) {
						t.Fatalf("%s, an %s P0 with no other hold, holds no slot: the pass started no P0 issue, so this case proves nothing", free, k.name)
					}
					if tc.reason != "" {
						assertNotStarted(t, pool, held)
						_, rows := passJudgement(t, pool, admission)
						assertHeldBy(t, rows, held, tc.reason, tc.by)
					}
				})
			}
			t.Run("a paused project", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				p0Queued(t, pool, held, "C", record.NoPriority, k.shape)
				p0Queued(t, pool, free, "D", record.NoPriority, k.shape)
				pause := pauseTestProject(t, pool, fixedNow)

				tick(t, pool, admission, "paused")
				assertRowHolders(t, pool, "LEGION-1")

				endTestPause(t, pool, pause, fixedNow)
				tick(t, pool, admission, "resumed")
				assertRowHolders(t, pool, "LEGION-1", held, free)
			})
		})
	}
}

// A P0 issue of each kind that waits in the line for a slot takes one at a cap its holders below
// P0 fill, whatever the kind of its place in the line. The rows are an inventory of
// record.LineKind: the test walks every kind the line has and fails for one no row stands for, so
// a kind added to the line fails it until it has its row. Each row reads its issue's place in the
// line before the pass that promotes it, so a row stands for the kind it names.
func TestEveryLineKindOfAnInheritedP0IssueTakesItsSlotAtAFullCap(t *testing.T) {
	rows := []struct {
		kind record.LineKind
		name string
		run  func(t *testing.T, k p0Kind)
	}{
		{kind: record.LineReturning, name: "returning", run: func(t *testing.T, k p0Kind) {
			const root, member = "LEGION-100", "LEGION-101"
			pool := migratedPool(t)
			admission := quiet(t, 2)
			p0Tree(t, pool, root, "A", record.P1, true)
			matrixMember(t, pool, root, member, "A1", phase.Implementing, "in_progress", k.shape)
			oweMemberStart(t, pool, root, member, phase.Implementing)
			p0Holder(t, pool, "LEGION-1", "B", record.P1)
			p0Holder(t, pool, "LEGION-2", "C", record.P3)
			assertStandsAs(t, pool, member, record.LineReturning, record.PartReturned, false)

			tick(t, pool, admission, "tick")

			assertRowHolders(t, pool, "LEGION-1", "LEGION-2", member)
			if !claimableStart(t, pool, member) {
				t.Fatalf("the starts of %s = %#v, want its implementer's task claimable on the slot it took", member, startsOf(t, pool, member))
			}
		}},
		{kind: record.LineWaitingOnIssue, name: "waiting_on_issue", run: func(t *testing.T, k p0Kind) {
			const waiter, other, blocker = "LEGION-10", "LEGION-11", "LEGION-90"
			pool := migratedPool(t)
			admission := quiet(t, 1)
			seedStarted(t, pool, waiter, phase.Implementing, k.shape)
			seedMirror(t, pool, waiter, "in_progress", blocker)
			seedStarted(t, pool, other, phase.Implementing, func(issue *record.Issue) { issue.Priority = record.P1 })
			seedMirror(t, pool, other, "in_progress")
			seedMirror(t, pool, blocker, "in_progress")
			tick(t, pool, admission, "the-wait-begins")
			assertRowHolders(t, pool, other)
			assertStandsAs(t, pool, waiter, record.LineWaitingOnIssue, record.PartReturned, false)

			dispatchEvent(t, pool, admission, "blocker-done", blocker, "done")

			assertRowHolders(t, pool, other, waiter)
		}},
		{kind: record.LineQueuedRoot, name: "queued_root", run: func(t *testing.T, k p0Kind) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			p0Holder(t, pool, "LEGION-1", "A", record.P1)
			p0Queued(t, pool, "LEGION-9", "C", record.NoPriority, k.shape)
			assertStandsAs(t, pool, "LEGION-9", record.LineQueuedRoot, record.PartStarting, false)

			tick(t, pool, admission, "tick")

			assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
			if got := issue(t, pool, "LEGION-9"); got.Status != "in_progress" {
				t.Fatalf("LEGION-9 stands at %s after its promotion, want in_progress: the slot opened its tree", got.Status)
			}
		}},
		{kind: record.LineFirstPhase, name: "first_phase of a tree's own root, in the returned part", run: func(t *testing.T, k p0Kind) {
			const root = "LEGION-100"
			pool := migratedPool(t)
			admission := quiet(t, 1)
			p0Holder(t, pool, "LEGION-1", "A", record.P1)
			// A root whose design gate has opened and whose first phase waits for a row of the cap:
			// it holds none, waits on no issue and owes no start.
			standing := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
				Title: root, Tree: root, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "B",
				HandedOver: true, LastDispatchSeq: 2}
			k.shape(&standing)
			putIssue(t, pool, standing)
			seedLifecycle(t, pool, root)
			seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1, ApprovedVersion: new(1),
				ApprovedMark: record.EventMark{Seq: 4, At: fixedNow.Add(-time.Hour)}, EverOpened: true})
			assertStandsAs(t, pool, root, record.LineFirstPhase, record.PartReturned, false)

			tick(t, pool, admission, "tick")

			assertRowHolders(t, pool, "LEGION-1", root)
			if got := issue(t, pool, root); got.Phase == phase.Admitted {
				t.Fatalf("%s stands at admitted after the pass, want its first phase started on the slot it took", root)
			}
		}},
		{kind: record.LineFirstPhase, name: "first_phase of a child, in the starting part", run: func(t *testing.T, k p0Kind) {
			const root, child = "LEGION-100", "LEGION-101"
			pool := migratedPool(t)
			admission := quiet(t, 1)
			p0Holder(t, pool, "LEGION-1", "A", record.P1)
			p0Tree(t, pool, root, "B", record.P1, true)
			matrixMember(t, pool, root, child, "B1", phase.Admitted, "todo", k.shape)
			assertStandsAs(t, pool, child, record.LineFirstPhase, record.PartStarting, false)

			tick(t, pool, admission, "tick")

			assertRowHolders(t, pool, "LEGION-1", child)
			if got := issue(t, pool, child); got.Phase != phase.Planning {
				t.Fatalf("%s stands in %s after the pass, want planning: its slot starts its first phase", child, got.Phase)
			}
		}},
		{kind: record.LineAnswered, name: "answered, holding its row", run: func(t *testing.T, k p0Kind) { answeredP0Root(t, k, true) }},
		{kind: record.LineAnswered, name: "answered, needing a row", run: func(t *testing.T, k p0Kind) { answeredP0Root(t, k, false) }},
	}
	covered := make(map[record.LineKind]bool, len(rows))
	for _, row := range rows {
		covered[row.kind] = true
	}
	for kind := record.LineReturning; kind.String() != ""; kind++ {
		if !covered[kind] {
			t.Errorf("the line kind %s has no row: no case shows a P0 issue standing in the line as %s take its slot at a full cap", kind, kind)
		}
	}
	for _, row := range rows {
		for _, k := range p0Kinds {
			t.Run(row.name+"/"+k.name, func(t *testing.T) { row.run(t, k) })
		}
	}
}

// answeredP0Root is the answered row of TestEveryLineKindOfAnInheritedP0IssueTakesItsSlotAtAFullCap:
// a root of kind k whose architect handed a question to a person, which keeps its row through the
// wait, is answered at a cap of two. Where holds is set it still holds that row, so its promotion
// takes none and resumes it beside the holder below P0; where it is not, its row is gone and two
// holders below P0 fill the cap, and its promotion takes a row outside the cap to resume it.
func answeredP0Root(t *testing.T, k p0Kind, holds bool) {
	t.Helper()
	const root = "LEGION-208"
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlottedInPhase(t, pool, testProject, root, "U", phase.Admitted, k.shape)
	seedArchitectClaim(t, pool, root, true)
	seedHandBack(t, pool, root, "ask-q", 5)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	want := []string{root, "LEGION-1"}
	if !holds {
		// The second holder below P0 that fills the cap once the root's row is gone. It holds its
		// row from before the wait, so the cap stands over-full while the root keeps its own.
		p0Holder(t, pool, "LEGION-2", "B", record.P1)
		want = append(want, "LEGION-2")
	}
	tick(t, pool, admission, "the-wait-begins")
	if !issue(t, pool, root).AwaitingPerson() {
		t.Fatalf("%s does not wait on a person after its hand-back: the row is about an answered root", root)
	}
	answerWait(t, pool, root, record.AnsweredByAsk, "ask-q")
	if !holds {
		if _, err := pool.Exec(context.Background(), "delete from slots where issue = $1", root); err != nil {
			t.Fatalf("take the row of %s away: %v", root, err)
		}
	}
	assertStandsAs(t, pool, root, record.LineAnswered, record.PartReturned, holds)

	tick(t, pool, admission, "answered")

	if got := issue(t, pool, root); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("%s after the answer stands in %s waiting on %+v, want it resumed at admitted with no wait", root, got.Phase, got.Awaiting)
	}
	assertRowHolders(t, pool, want...)
}

// An explicit and an inherited P0 issue are one class for every decision the P0 start makes: both
// start at a full cap, both stand in the P0 band by the line's own keys, both count as P0 holders
// and neither counts against the cap, and each fills the P0 bound for the other. The bound's rows
// leave a row of the cap free, so a candidate below P0 does start there: the P0 candidate waits for
// the bound alone. An older Dispatch's member under a P0 tree root, which Dispatch resolves nothing
// for, stands in the P0 band by its root's priority (decision 3 of the plan) and is no P0 issue for
// a start or the bound: it waits for a row of the cap, and takes it as a capped holder.
func TestAnExplicitAndAnInheritedP0IssueAreTreatedAlike(t *testing.T) {
	explicit, inherited := p0Kinds[0], p0Kinds[1]
	t.Run("start", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Queued(t, pool, "LEGION-8", "C", record.NoPriority, explicit.shape)
		p0Queued(t, pool, "LEGION-9", "D", record.NoPriority, inherited.shape)

		tick(t, pool, admission, "tick")

		assertRowHolders(t, pool, "LEGION-1", "LEGION-8", "LEGION-9")
	})
	t.Run("waiting order", func(t *testing.T) {
		for _, first := range p0Kinds {
			second := explicit
			if first == explicit {
				second = inherited
			}
			t.Run(first.name+" ranked first", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				admission.UseP0Bound(new(1))
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				p0Holder(t, pool, "LEGION-2", "B", record.P0)
				p0Queued(t, pool, "LEGION-80", "0", record.P1, nil)
				p0Queued(t, pool, "LEGION-8", "C", record.NoPriority, first.shape)
				p0Queued(t, pool, "LEGION-9", "D", record.NoPriority, second.shape)

				tick(t, pool, admission, "tick")

				if line, want := waitingLine(t, pool, testProject, nil), []string{"LEGION-8", "LEGION-9", "LEGION-80"}; !reflect.DeepEqual(line, want) {
					t.Fatalf("the waiting line is %v, want %v: the %s and the %s P0 issue by their rank, ahead of the P1 root ranked first",
						line, want, first.name, second.name)
				}
			})
		}
	})
	t.Run("holder count", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		seedSlottedInPhase(t, pool, testProject, "LEGION-2", "B", phase.Admitted, explicit.shape)
		seedSlottedInPhase(t, pool, testProject, "LEGION-3", "C", phase.Admitted, inherited.shape)
		p0Queued(t, pool, "LEGION-9", "D", record.P2, nil)

		tick(t, pool, admission, "tick")

		assertRowHolders(t, pool, "LEGION-2", "LEGION-3", "LEGION-9")
		if got := p0HoldersNow(t, pool); got != 2 {
			t.Fatalf("%d P0 holders, want 2: the explicit and the inherited P0 holder", got)
		}
		if got := stateBody(t, pool)["admission"].(map[string]any)["p0Holders"]; got != float64(2) {
			t.Fatalf("admission.p0Holders = %v, want 2", got)
		}
	})
	t.Run("the bound", func(t *testing.T) {
		for _, holding := range p0Kinds {
			waiting := explicit
			if holding == explicit {
				waiting = inherited
			}
			t.Run("an "+holding.name+" holder and an "+waiting.name+" candidate", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 2)
				admission.UseP0Bound(new(1))
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				seedSlottedInPhase(t, pool, testProject, "LEGION-2", "B", phase.Admitted, holding.shape)
				p0Queued(t, pool, "LEGION-9", "C", record.NoPriority, waiting.shape)
				p0Queued(t, pool, "LEGION-80", "D", record.P2, nil)

				tick(t, pool, admission, "tick")

				if holdsSlot(t, pool, "LEGION-9") {
					t.Fatalf("LEGION-9, an %s P0, holds a slot at a bound of 1 an %s P0 holder fills", waiting.name, holding.name)
				}
				if !holdsSlot(t, pool, "LEGION-80") {
					t.Fatal("LEGION-80, at P2, holds no slot: the cap had a row free, so this case does not show the bound alone held LEGION-9")
				}
				if line := waitingLine(t, pool, testProject, nil); len(line) == 0 || line[0] != "LEGION-9" {
					t.Fatalf("the waiting line is %v, want LEGION-9 first: a P0 issue at the bound waits first", line)
				}
			})
		}
	})
	t.Run("an older Dispatch's member under a P0 tree root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseP0Bound(new(1))
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		seedSlottedInPhase(t, pool, testProject, "LEGION-2", "A2", phase.Admitted, inherited.shape)
		p0Tree(t, pool, "LEGION-A", "B", record.P0, true)
		matrixMember(t, pool, "LEGION-A", "LEGION-A1", "B1", phase.Admitted, "todo", nil)
		p0Queued(t, pool, "LEGION-9", "C", record.NoPriority, explicit.shape)

		tick(t, pool, admission, "tick")

		assertRowHolders(t, pool, "LEGION-1", "LEGION-2")
		if line, want := waitingLine(t, pool, testProject, nil), []string{"LEGION-A1", "LEGION-9"}; !reflect.DeepEqual(line, want) {
			t.Fatalf("the waiting line is %v, want %v: both in the P0 band, by their tree roots' rank", line, want)
		}
		for _, key := range []string{"LEGION-A1", "LEGION-9"} {
			if got := waitingView(t, pool, key)["priority"]; got != float64(0) {
				t.Fatalf("%s waits at the line's priority %v, want 0", key, got)
			}
		}

		// A row of the cap comes free: the older Dispatch's member takes it as a holder the cap
		// counts, while the explicit P0 issue still waits for the bound.
		closeTree(t, pool, admission, "holder-closed", "LEGION-1", "A")
		assertRowHolders(t, pool, "LEGION-2", "LEGION-A1")
		if got := p0HoldersNow(t, pool); got != 1 {
			t.Fatalf("%d P0 holders, want 1: the member placed in the P0 band by its tree root counts against the cap", got)
		}
	})
}

// holdersNow is the test project's slot holders as every reader of the cap counts them
// (record.HoldersOf over its live records and the slots).
func holdersNow(t *testing.T, pool *pgxpool.Pool) record.SlotHolders {
	t.Helper()
	var held record.SlotHolders
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().LiveIssues(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := record.NewStore().Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		held = record.HoldersOf(issues, slots)
	})
	return held
}

// capReadings is what the promotion's cap half (capLets) and the stall measure's liveness clause
// (lineCanMove) answer for candidate, read as the pass reads them: over the project's live records,
// its own slots and the line one judgement builds (blockers.JudgeLine), under no P0 bound. The
// measure is asked with no tree at work, so its free-slot term decides wherever every holder waits
// by design.
func capReadings(t *testing.T, pool *pgxpool.Pool, admission *Admission, candidate string) (lets, canMove bool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.LiveIssues(ctx, tx, testProject)
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, records, testProject, issues, admission.engine.GatedSteps())
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		own := ownSlots(issues, slots)
		rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, records, testProject, issues, own, facts, nil)
		if err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
		found := false
		for _, entry := range line {
			if entry.Issue.Key == candidate {
				lets, found = capLets(entry, record.HoldersOf(issues, own), admission.cap, nil), true
			}
		}
		if !found {
			t.Fatalf("%s stands in no waiting line, want it waiting for a row", candidate)
		}
		canMove = admission.lineCanMove(issues, lineHoldsOf(facts, slots, line, dropped), nil, rows, nil)
	})
	return lets, canMove
}

// The four readers of the cap in the admission pass — the promotion (its cap half, capLets, and
// what it does), the free-slot reading (slotFree), and the stall measure's free-slot term
// (lineCanMove) — and the state's `admission.p0Holders` count the holders record.HoldersOf counts,
// for one holder of each kind at a cap of one: an explicit P0 holder, an inherited one, one below
// P0, one lowered below P0 and one raised to P0 by an update of its resolved priority alone. The
// holder waits on a person, which is a wait by design, so lineCanMove's slot term passes over it
// and its free-slot term decides. slotFree says whether a slot stands free beyond what the line
// takes, so it reads the holder alone, before the candidate below P0 joins the line; the three
// others read that candidate, and the pass then promotes it or not. No reader takes the holder's
// slot.
func TestTheFourReadersOfTheCapCountTheSameHolders(t *testing.T) {
	const holder, candidate = "LEGION-1", "LEGION-9"
	for _, tc := range []struct {
		name  string
		shape func(*record.Issue)
		// change is the Dispatch event that moves the holder's priority before the readings, nil
		// for none.
		change func(t *testing.T, stored record.Issue) intake.DispatchIssue
		p0     bool
	}{
		{name: "an explicit P0 holder", shape: p0Kinds[0].shape, p0: true},
		{name: "an inherited P0 holder", shape: p0Kinds[1].shape, p0: true},
		{name: "a holder below P0", shape: func(issue *record.Issue) {
			issue.Priority, issue.Resolved = record.P1, record.Resolved{Priority: record.P1, Known: true}
		}},
		{name: "a holder lowered below P0", shape: p0Kinds[0].shape, change: func(t *testing.T, stored record.Issue) intake.DispatchIssue {
			return echoOf(t, stored, 2, stored.Title, record.P2, record.Resolved{Priority: record.P2, Known: true}, `["priority","resolved_priority"]`)
		}},
		{name: "a holder raised to P0 by an update of its resolved priority", shape: resolvedTo(record.P2),
			change: func(t *testing.T, stored record.Issue) intake.DispatchIssue {
				return echoOf(t, stored, 2, stored.Title, record.NoPriority, resolvedFrom(record.P0), `["resolved_priority"]`)
			}, p0: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			waits := quietRootRecord(holder, "A", phase.AwaitingPerson)
			tc.shape(&waits)
			putIssue(t, pool, waits)
			seedLifecycle(t, pool, holder)
			takeSlot(t, pool, holder)
			if tc.change != nil {
				apply(t, pool, admission, "change", tc.change(t, issue(t, pool, holder)), admission.engine)
			}
			held := holdersNow(t, pool)
			if held.Total() != 1 || (held.P0 == 1) != tc.p0 {
				t.Fatalf("record.HoldersOf counts %+v, want the one holder counted as P0 = %t", held, tc.p0)
			}
			room := held.Capped < admission.cap

			if got := slotFreeNow(t, pool, admission); got != room {
				t.Errorf("slotFree = %t, want %t: record.HoldersOf counts %+v under a cap of 1", got, room, held)
			}
			p0Queued(t, pool, candidate, "C", record.P2, nil)
			lets, canMove := capReadings(t, pool, admission, candidate)
			if lets != room {
				t.Errorf("capLets for %s = %t, want %t: record.HoldersOf counts %+v", candidate, lets, room, held)
			}
			if canMove != room {
				t.Errorf("lineCanMove = %t, want %t: record.HoldersOf counts %+v and the holder waits by design", canMove, room, held)
			}
			if got := stateBody(t, pool)["admission"].(map[string]any)["p0Holders"]; got != float64(held.P0) {
				t.Errorf("admission.p0Holders = %v, want %d", got, held.P0)
			}

			tick(t, pool, admission, "tick")

			if got := holdsSlot(t, pool, candidate); got != room {
				t.Errorf("the promotion gave %s a slot = %t, want %t: record.HoldersOf counts %+v", candidate, got, room, held)
			}
			if !holdsSlot(t, pool, holder) {
				t.Fatalf("%s lost its slot: no reader of the cap takes a slot from a holder", holder)
			}
		})
	}
}

// An inherited P0 issue's start writes the line every P0 start writes, naming the issue, the P0
// holders, the other holders and the cap; at the P0 bound its refusal writes the line every
// refusal writes, with the bound.
func TestAnInheritedStartAndRefusalAreLogged(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		pool := migratedPool(t)
		var logged bytes.Buffer
		admission := newAdmission(t, 2, slog.New(slog.NewJSONHandler(&logged, nil)))
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Holder(t, pool, "LEGION-2", "B", record.P2)
		inheritedHolder(t, pool, "LEGION-3", "C", resolvedFrom(record.P0))
		inheritedQueued(t, pool, "LEGION-9", "D", resolvedFrom(record.P0))

		tick(t, pool, admission, "tick")

		starts := loggedAs(t, &logged, p0StartMessage)
		if len(starts) != 1 {
			t.Fatalf("the pass logged %d lines %q, want one for LEGION-9; the log:\n%s", len(starts), p0StartMessage, logged.String())
		}
		if got := starts[0]; got["issue"] != "LEGION-9" || got["p0_holders"] != float64(2) || got["other_holders"] != float64(2) || got["cap"] != float64(2) {
			t.Fatalf("the inherited P0 start's line = %v, want issue LEGION-9, p0_holders 2 (LEGION-3 and LEGION-9), other_holders 2 and cap 2", got)
		}
	})
	t.Run("refusal", func(t *testing.T) {
		pool := migratedPool(t)
		var logged bytes.Buffer
		admission := newAdmission(t, 1, slog.New(slog.NewJSONHandler(&logged, nil)))
		admission.UseP0Bound(new(1))
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		inheritedHolder(t, pool, "LEGION-2", "B", resolvedFrom(record.P0))
		inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P0))

		tick(t, pool, admission, "tick")

		refusals := loggedAs(t, &logged, p0BoundRefusalMessage)
		if len(refusals) != 1 {
			t.Fatalf("the pass logged %d lines %q, want one for LEGION-9; the log:\n%s", len(refusals), p0BoundRefusalMessage, logged.String())
		}
		if got := refusals[0]; got["issue"] != "LEGION-9" || got["p0_bound"] != float64(1) || got["p0_holders"] != float64(1) ||
			got["other_holders"] != float64(1) || got["cap"] != float64(1) {
			t.Fatalf("the inherited P0 refusal's line = %v, want issue LEGION-9, p0_bound 1, p0_holders 1, other_holders 1 and cap 1", got)
		}
	})
}

// A holder that Dispatch raises to P0 by an update of its resolved priority alone (J3 of
// dispatch://INFRA-5793: an issue above it moved) leaves the cap's count at the pass of that
// update, which gives the slot it leaves to the line below P0, and keeps its own slot.
func TestAHolderRaisedToP0ByAResolvedUpdateLeavesTheCapsCount(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	inheritedHolder(t, pool, "LEGION-1", "A", resolvedFrom(record.P2))
	p0Queued(t, pool, "LEGION-9", "C", record.P3, nil)
	tick(t, pool, admission, "tick")
	assertRowHolders(t, pool, "LEGION-1")

	stored := issue(t, pool, "LEGION-1")
	apply(t, pool, admission, "raised", echoOf(t, stored, 2, stored.Title, record.NoPriority, resolvedFrom(record.P0), `["resolved_priority"]`), admission.engine)

	assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	if got := p0HoldersNow(t, pool); got != 1 {
		t.Fatalf("%d P0 holders after the update, want 1: LEGION-1 at its inherited P0", got)
	}
}

// A newer observation with no `resolved_priority` sets the record's resolved priority unknown over
// a known one, whichever path brought it, and a present null sets a known none (decision 1 of the
// plan): an event, a boot-listing row and the held summary a newer event refreshes. So after a
// rollback to a Dispatch release that resolves no priority, its listing leaves only an issue whose
// own priority is P0 starting outside the cap.
func TestAMissingResolvedPriorityReplacesAKnownOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent record.Resolved
		// listed is the listing row's resolved priority, nil where the row carries none.
		listed *dispatch.ResolvedPriority
		// changed is the event's `changed`: an older Dispatch writes none.
		changed string
	}{
		{name: "missing"},
		{name: "a present null", sent: record.Resolved{Known: true}, listed: &dispatch.ResolvedPriority{}, changed: `["resolved_priority","priority_from"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(t *testing.T, pool *pgxpool.Pool, path string) {
				t.Helper()
				if got := issue(t, pool, "LEGION-9").Resolved; got != tc.sent {
					t.Fatalf("after %s LEGION-9 holds the resolved priority %s, want %s", path, resolvedName(got), resolvedName(tc.sent))
				}
				if holdsSlot(t, pool, "LEGION-9") {
					t.Fatalf("LEGION-9 holds a slot at a full cap after %s replaced its inherited P0", path)
				}
			}
			t.Run("an issue event", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P0))
				stored := issue(t, pool, "LEGION-9")
				apply(t, pool, admission, "event", echoOf(t, stored, 2, stored.Title, record.NoPriority, tc.sent, tc.changed), admission.engine)
				check(t, pool, "the event")
			})
			t.Run("a boot-listing row", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P0))
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
					HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: tc.listed}}, 2, 2, true)
				check(t, pool, "the listing")
			})
			t.Run("a held summary", func(t *testing.T) {
				admission := New(record.NewStore(), nil, 1, testProject, nil)
				admission.holdDeferred(map[string]intake.DispatchIssue{"LEGION-9": {Key: "LEGION-9", Status: "todo", Seq: 5, Resolved: resolvedFrom(record.P0)}}, 10)
				admission.refreshHeldSummary(intake.DispatchIssue{Key: "LEGION-9", Status: "todo", Seq: 7, Resolved: tc.sent})
				if got := stillHeld(admission)["LEGION-9"].Resolved; got != tc.sent {
					t.Fatalf("the held summary after a newer event holds %s, want %s", resolvedName(got), resolvedName(tc.sent))
				}
			})
		})
	}
	t.Run("an older Dispatch's listing after a rollback", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		inheritedQueued(t, pool, "LEGION-8", "C", resolvedFrom(record.P0))
		p0Queued(t, pool, "LEGION-9", "D", record.NoPriority, func(issue *record.Issue) { p0Kinds[0].shape(issue); issue.LastDispatchSeq = 1 })

		reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
			{Key: "LEGION-8", Title: "LEGION-8", Status: "todo", Rank: "C", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets"},
			{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "D", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Priority: level(record.P0)},
		}, 2, 2, true)

		for _, key := range []string{"LEGION-8", "LEGION-9"} {
			if got := issue(t, pool, key).Resolved; got.Known {
				t.Fatalf("%s after the older listing holds the resolved priority %s, want it unknown", key, resolvedName(got))
			}
		}
		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	})
}

// A boot listing's todo echo of a running tree's member, at the record's own sequence, keeps the
// status and the Dispatch status the record holds, as it always did, and still takes the
// priorities Dispatch now gives the member, so the first boot after Dispatch resolves priorities
// reads a member's newly resolved P0 with no edit of its own. The pass of that listing counts the
// member as a P0 holder, which gives the cap's one slot to the root below P0 that waits.
func TestAnEqualSequenceTodoEchoOfARunningMemberRefreshesItsPriorityAndKeepsItsStatus(t *testing.T) {
	const root, member = "LEGION-A", "LEGION-A1"
	for _, tc := range []struct {
		name     string
		before   record.Priority
		own      *int
		resolved dispatch.ResolvedPriority
		wantOwn  record.Priority
		want     record.Resolved
	}{
		{name: "a newly inherited P0", resolved: dispatch.ResolvedPriority{Priority: level(record.P0), From: outside},
			want: resolvedFrom(record.P0)},
		{name: "a newly own P0", before: record.P2, own: level(record.P0), resolved: dispatch.ResolvedPriority{Priority: level(record.P0)},
			wantOwn: record.P0, want: record.Resolved{Priority: record.P0, Known: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			p0Tree(t, pool, root, "A", record.P1, true)
			matrixMember(t, pool, root, member, "A1", phase.Implementing, "in_progress", func(issue *record.Issue) {
				issue.Priority, issue.LastDispatchSeq = tc.before, 5
			})
			takeSlot(t, pool, member)
			p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
			tick(t, pool, admission, "tick")
			assertRowHolders(t, pool, member)
			before := issue(t, pool, member)

			parent := root
			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: member, Title: member, Status: "todo", Parent: &parent,
				Rank: "A1", LastSeq: 5, ResolvedRepo: "acme/widgets", Priority: tc.own, Resolved: &tc.resolved}}, 5, 5, true)

			got := issue(t, pool, member)
			if got.Status != before.Status || got.DispatchStatus != before.DispatchStatus || got.Phase != before.Phase || got.LastDispatchSeq != before.LastDispatchSeq {
				t.Fatalf("%s after the echo stands at %s, Dispatch status %s, in %s at sequence %d; want %s, %s, %s and %d as before it",
					member, got.Status, got.DispatchStatus, got.Phase, got.LastDispatchSeq, before.Status, before.DispatchStatus, before.Phase, before.LastDispatchSeq)
			}
			if got.Priority != tc.wantOwn || got.Resolved != tc.want {
				t.Fatalf("%s after the echo holds own %s and resolved %s, want own %s and resolved %s", member,
					priorityName(got.Priority), resolvedName(got.Resolved), priorityName(tc.wantOwn), resolvedName(tc.want))
			}
			assertRowHolders(t, pool, member, "LEGION-9")
		})
	}
}

// An update of a member's resolved priority alone — each `changed` the decoder reads as one
// (intake's resolvedOnly) — is no work of the member's own (C1 of dispatch://INFRA-5636): a held
// child of a tree already told to the controller as stopped keeps its movement mark, the tree its
// quiet time and the root its stall mark, and the next pass tells no second stall. A title edit,
// an own-priority edit and an update whose `changed` is missing, empty or names a field beside the
// resolved priority each move all three: the child's mark and the tree's measure to the moment of
// the update, and the root's stall mark to that moment once the tree is quiet past the bound again.
// Each update is applied by an engine whose clock stands past every mark, so a mark it wrote would
// show.
func TestAResolvedOnlyUpdateMovesNoTreeClock(t *testing.T) {
	const root, child = "LEGION-600", "LEGION-601"
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	seedQuietRoot(t, pool, root, "A", phase.Implementing)
	held := record.Issue{Key: child, Tree: root, Parent: new(root), Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: child, Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated},
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A1", LastDispatchSeq: 1, Resolved: resolvedFrom(record.P2)}
	putIssue(t, pool, held)
	if record.KeepsSlot(held) {
		takeSlot(t, pool, child)
	}
	records := claimStore(t, pool)
	seedClaim(t, records, root, root, claim.RoleArchitect, supervise.StateIdle)
	seedClaim(t, records, root, child, claim.RoleImplementer, supervise.StateIdle)
	settled := settleMovements(t, pool, root, child)
	now := settled.Add(7 * time.Hour)
	admission.now = func() time.Time { return now }
	tick(t, pool, admission, "stall-1")
	if got := stalledRoots(t, pool); !reflect.DeepEqual(got, []string{root}) {
		t.Fatalf("the controller was told of %v, want %s: the case is about a tree already told as stopped", got, root)
	}
	publishStalledNotices(t, pool)

	// assertClocks reads that the child last moved at moved, the tree's measure stands there too,
	// and the root's stall mark is told.
	assertClocks := func(t *testing.T, what string, moved, told time.Time) {
		t.Helper()
		if got := issue(t, pool, child).MovedAt; !got.Equal(moved) {
			t.Fatalf("after %s %s last moved at %s, want %s", what, child, got.Format(time.RFC3339Nano), moved.Format(time.RFC3339Nano))
		}
		var trees map[string]record.TreeQuiet
		inTx(t, pool, func(tx pgx.Tx) {
			ctx, keys := context.Background(), []string{root, child}
			issues, err := record.NewStore().IssuesByKey(ctx, tx, keys)
			if err != nil {
				t.Fatalf("read the issues of %s: %v", root, err)
			}
			chains, err := record.ReadChains(ctx, tx, record.NewStore(), testProject, keys)
			if err != nil {
				t.Fatalf("read the chains of %s: %v", root, err)
			}
			if trees, _, err = admission.quietTrees(ctx, tx, issues, chains, admission.now()); err != nil {
				t.Fatalf("measure the trees: %v", err)
			}
		})
		if got := trees[root].MovedAt; !got.Equal(moved) {
			t.Fatalf("after %s the tree %s last moved at %s, want %s", what, root, got.Format(time.RFC3339Nano), moved.Format(time.RFC3339Nano))
		}
		if got := issue(t, pool, root).TreeStalledToldAt; got == nil || !got.Equal(told) {
			t.Fatalf("after %s %s carries the stall mark %v, want %s", what, root, got, told.Format(time.RFC3339Nano))
		}
	}
	assertClocks(t, "the first notice", settled, settled)

	seq := int64(1)
	for i, update := range []struct {
		changed  string
		resolved record.Resolved
	}{
		{changed: `["resolved_priority"]`, resolved: resolvedFrom(record.P0)},
		{changed: `["priority_from"]`, resolved: record.Resolved{Priority: record.P0, From: "OTHER-6", Known: true}},
		{changed: `["resolved_priority","priority_from"]`, resolved: record.Resolved{Priority: record.P1, From: "OTHER-7", Known: true}},
	} {
		seq++
		stored := issue(t, pool, child)
		at := now.Add(time.Duration(i+1) * time.Hour)
		apply(t, pool, admission, "resolved-only-"+strconv.Itoa(i), echoOf(t, stored, seq, stored.Title, record.NoPriority, update.resolved, update.changed), engineAt(at))
		if got := issue(t, pool, child).Resolved; got != update.resolved {
			t.Fatalf("%s after the update whose changed is %s holds %s, want %s: the update must reach the record", child, update.changed,
				resolvedName(got), resolvedName(update.resolved))
		}
		assertClocks(t, "the update whose changed is "+update.changed, settled, settled)
	}
	now = now.Add(4 * time.Hour)
	tick(t, pool, admission, "stall-2")
	if got := stalledRoots(t, pool); got != nil {
		t.Fatalf("the pass after the updates of the resolved priority told the controller of %v, want nothing: the stall was already told", got)
	}

	for i, edit := range []struct {
		name, title, changed string
		own                  record.Priority
		resolved             record.Resolved
	}{
		{name: "a title edit", title: "renamed", changed: `["title"]`, resolved: record.Resolved{Priority: record.P1, From: "OTHER-7", Known: true}},
		{name: "an own-priority edit", title: "renamed", changed: `["priority","resolved_priority","priority_from"]`, own: record.P1,
			resolved: record.Resolved{Priority: record.P1, Known: true}},
		{name: "an update with no changed list", title: "renamed", own: record.P1, resolved: record.Resolved{Priority: record.P2, Known: true}},
		{name: "an update whose changed list is empty", title: "renamed", changed: `[]`, own: record.P1, resolved: record.Resolved{Priority: record.P3, Known: true}},
		{name: "an update of the resolved priority and the title", title: "renamed again", changed: `["resolved_priority","title"]`,
			own: record.P1, resolved: record.Resolved{Priority: record.P1, Known: true}},
	} {
		seq++
		stored := issue(t, pool, child)
		at := now.Add(time.Hour)
		apply(t, pool, admission, "edit-"+strconv.Itoa(i), echoOf(t, stored, seq, edit.title, edit.own, edit.resolved, edit.changed), engineAt(at))
		assertClocks(t, edit.name, at, *issue(t, pool, root).TreeStalledToldAt)
		now = at.Add(7 * time.Hour)
		tick(t, pool, admission, "stall-after-"+strconv.Itoa(i))
		if got := stalledRoots(t, pool); !reflect.DeepEqual(got, []string{root}) {
			t.Fatalf("the controller was told of %v after %s and a quiet past the bound, want %s again: the edit moved the tree", got, edit.name, root)
		}
		assertClocks(t, "the notice after "+edit.name, at, at)
		publishStalledNotices(t, pool)
	}
}

// An event older than a read the daemon takes as a fact (decision 2 of the plan) leaves that read's
// resolved priority, whether the event carries another or none: the view of an unrecorded issue
// (intake's settleObserved), release_children's observed child, and the issue a status write
// returns (intake.OwnStatusWrite). Each read is at sequence 5 with a P0 from outside; the event at
// sequence 4 carries a P2 from another issue, or, from an older Dispatch, no resolved priority at
// all. The pass after it still counts the issue as a P0 holder at a cap a holder below P0 fills.
func TestAnOlderEventLeavesEachReadsResolvedPriority(t *testing.T) {
	read := resolvedFrom(record.P0)
	served := &dispatch.ResolvedPriority{Priority: level(record.P0), From: outside}
	for _, older := range []struct {
		name     string
		resolved record.Resolved
		changed  string
	}{
		{name: "an older event with another resolved priority", resolved: record.Resolved{Priority: record.P2, From: "OTHER-9", Known: true},
			changed: `["resolved_priority","priority_from"]`},
		{name: "an older event with no resolved priority"},
	} {
		t.Run(older.name, func(t *testing.T) {
			keeps := func(t *testing.T, pool *pgxpool.Pool, admission *Admission, key string) {
				t.Helper()
				if got := issue(t, pool, key).Resolved; got != read {
					t.Fatalf("the read stored the resolved priority %s for %s, want %s", resolvedName(got), key, resolvedName(read))
				}
				stored := issue(t, pool, key)
				apply(t, pool, admission, "older", echoOf(t, stored, 4, stored.Title, stored.Priority, older.resolved, older.changed), admission.engine)
				if got := issue(t, pool, key).Resolved; got != read {
					t.Fatalf("%s after the event at sequence 4 holds %s, want the read's %s", key, resolvedName(got), resolvedName(read))
				}
				tick(t, pool, admission, "next")
				if !holdsSlot(t, pool, key) {
					t.Fatalf("%s holds no slot at a full cap after the older event", key)
				}
				if got := p0HoldersNow(t, pool); got != 1 {
					t.Fatalf("%d P0 holders after the older event, want 1: %s at the read's P0", got, key)
				}
			}
			t.Run("the view of an unrecorded issue", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				apply(t, pool, admission, "view", intake.ObservedIssue(dispatch.Issue{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
					HandedOver: true, LastSeq: 5, ResolvedRepo: "acme/widgets", Resolved: served}), admission.engine)
				keeps(t, pool, admission, "LEGION-9")
			})
			t.Run("release_children's observed child", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				p0Tree(t, pool, "LEGION-500", "B", record.P1, true)
				parent := "LEGION-500"
				child := intake.ObservedIssue(dispatch.Issue{Key: "LEGION-501", Title: "LEGION-501", Status: "todo", Parent: &parent, Rank: "B1",
					LastSeq: 5, ResolvedRepo: "acme/widgets", Resolved: served})
				// The daemon's own handler order: the mirror writes the child's row of the copy of
				// Dispatch's issues first, which its chain is read from (applyAsTheDaemon).
				if _, err := intake.ApplyFact(context.Background(), pool, "api", "waves/release", child,
					blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
					t.Fatalf("apply the released child: %v", err)
				}
				keeps(t, pool, admission, "LEGION-501")
			})
			t.Run("the issue a status write returns", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedHolder(t, pool, "LEGION-9", "C", resolvedFrom(record.P2))
				applyFact(t, pool, admission, "own-status-write", intake.OwnStatusWrite(dispatch.Issue{Key: "LEGION-9", Title: "LEGION-9",
					Status: "in_progress", Rank: "C", HandedOver: true, LastSeq: 5, ResolvedRepo: "acme/widgets", Resolved: served}))
				keeps(t, pool, admission, "LEGION-9")
			})
		})
	}
}

// Within the P0 band an explicit and an inherited P0 issue stand by the line's own keys alone: the
// issues whose work came back first, oldest first, and then the issues that have not started, by
// their tree root's rank and then their own, as TestTheWaitingLinePutsAP0ChildOfAP1TreeAheadOfAP1RootWhoseWorkCameBackAndAP2RootRankedFirst
// holds for explicit ones; the band itself stands ahead of a P1 root ranked first on the board.
// One line is read three times, with every P0 issue explicit, and with the two kinds mixed one way
// and the other, and it is one order each time.
func TestTheOrderWithinABandIsKeptForExplicitAndInheritedP0(t *testing.T) {
	const (
		returnedOld, returnedNew    = "LEGION-31", "LEGION-32"
		queuedFirst, queuedLast     = "LEGION-41", "LEGION-42"
		tree, childLate, childEarly = "LEGION-50", "LEGION-51", "LEGION-52"
		p1First                     = "LEGION-60"
	)
	want := []string{returnedOld, returnedNew, queuedFirst, childEarly, childLate, queuedLast, p1First}
	explicit, inherited := p0Kinds[0], p0Kinds[1]
	for _, tc := range []struct {
		name  string
		kinds map[string]p0Kind
	}{
		{name: "every P0 issue explicit", kinds: map[string]p0Kind{returnedOld: explicit, returnedNew: explicit, queuedFirst: explicit,
			childEarly: explicit, childLate: explicit, queuedLast: explicit}},
		{name: "mixed", kinds: map[string]p0Kind{returnedOld: inherited, returnedNew: explicit, queuedFirst: explicit,
			childEarly: inherited, childLate: explicit, queuedLast: inherited}},
		{name: "mixed the other way", kinds: map[string]p0Kind{returnedOld: explicit, returnedNew: inherited, queuedFirst: inherited,
			childEarly: explicit, childLate: inherited, queuedLast: explicit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// Two roots whose work came back, ranked against their age: each owes its worker a start
			// and holds no row, and the older start is written first.
			for _, key := range []string{returnedOld, returnedNew} {
				rank := map[string]string{returnedOld: "Z", returnedNew: "A"}[key]
				came := quietRootRecord(key, rank, phase.Implementing)
				tc.kinds[key].shape(&came)
				putIssue(t, pool, came)
				seedLifecycle(t, pool, key)
				oweStart(t, pool, key, key, phase.Implementing)
			}
			p0Queued(t, pool, queuedFirst, "B", record.NoPriority, tc.kinds[queuedFirst].shape)
			p0Queued(t, pool, queuedLast, "D", record.NoPriority, tc.kinds[queuedLast].shape)
			p0Tree(t, pool, tree, "C", record.P1, true)
			matrixMember(t, pool, tree, childLate, "C2", phase.Admitted, "todo", tc.kinds[childLate].shape)
			matrixMember(t, pool, tree, childEarly, "C1", phase.Admitted, "todo", tc.kinds[childEarly].shape)
			p0Queued(t, pool, p1First, "0", record.P1, nil)

			if got := waitingLine(t, pool, testProject, nil); !reflect.DeepEqual(got, want) {
				t.Fatalf("the waiting line is %v, want %v", got, want)
			}
		})
	}
}

// The line's reason for a member whose priority Dispatch resolves from an issue above it names
// that issue, `priority_from`, and never the member's top issue in Legion's work, which stands at
// another priority: in the part of the issues whose work came back, where no other clause names
// the top issue, and in the part of the issues that have not started, where the top issue is named
// only as the rank that places the member.
func TestTheReasonNamesPriorityFromForAMemberUnderAnotherRoot(t *testing.T) {
	const root, starting, returning = "LEGION-A", "LEGION-77", "LEGION-78"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	p0Tree(t, pool, root, "B", record.P1, true)
	resolveAsOwn(t, pool, root)
	matrixMember(t, pool, root, starting, "B1", phase.Admitted, "todo", resolvedTo(record.P2))
	matrixMember(t, pool, root, returning, "B2", phase.Implementing, "in_progress", resolvedTo(record.P2))
	oweMemberStart(t, pool, root, returning, phase.Implementing)

	tick(t, pool, admission, "tick")

	placed := "It waits at P2, the priority of " + outside + ", the nearest issue above it in Dispatch that has one."
	for _, tc := range []struct {
		key string
		// rankClause is the one clause that may name the top issue: the rank that places the
		// member, "" where none does.
		rankClause string
	}{
		{key: returning},
		{key: starting, rankClause: "the Dispatch rank of its top issue " + root},
	} {
		reason, _ := waitingView(t, pool, tc.key)["reason"].(string)
		if !strings.Contains(reason, placed) {
			t.Errorf("%s's reason is %q, want it to say %q", tc.key, reason, placed)
		}
		named := strings.Count(reason, root)
		if tc.rankClause != "" {
			named -= strings.Count(reason, tc.rankClause)
		}
		if named != 0 {
			t.Errorf("%s's reason is %q, which names its tree root %s beyond the rank that places it", tc.key, reason, root)
		}
	}
}
