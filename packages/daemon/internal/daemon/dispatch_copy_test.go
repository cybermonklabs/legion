package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The copy of Dispatch's issues holds each issue's labels and its rank from every reading that
// carries them, each at the sequence of that reading: an issue event, a listing of the project,
// the periodic turn's re-read of one issue, and Dispatch's answer to the daemon's own status
// write. Each reaches the copy through the daemon's own wiring (copyRuntime): the event and the
// answer through the runtime's own handlers, the listing through the boot's reconcile, and the
// re-read through the turn itself, the last three read over HTTP by the daemon's own Dispatch
// client from a board whose JSON names each field as Dispatch's own model does.
func TestTheCopyHoldsLabelsAndRank(t *testing.T) {
	t.Parallel()
	const key = "CAPTURE-3"
	labels, rank := []string{"design", "urgent"}, "a3"
	t.Run("an issue event", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{}, 1)

		applyCopyFact(t, w, "dispatch", "labels-and-rank", capturedDispatchEvent(t, key, 4,
			eventEdit{`"labels":[]`, `"labels":` + jsonOf(t, labels)}, eventEdit{`"rank":"UUU"`, `"rank":` + jsonOf(t, rank)}))

		assertLabelsAndRank(t, pool, key, labels, rank, 4)
	})
	t.Run("a listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{listing: []map[string]any{boardSummary(key, "backlog", 5, labels, rank)}}, 1)
		w.consumers = bootClaimConsumers(t, pool)

		if err := w.reconcile(context.Background()); err != nil {
			t.Fatalf("the boot's reconcile of the listing: %v", err)
		}

		assertLabelsAndRank(t, pool, key, labels, rank, 5)
	})
	t.Run("the turn's re-read of one issue", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{issues: map[string]map[string]any{key: boardIssue(key, "in_progress", 7, labels, rank)}}, 1)
		// A root waits on the issue, so the turn reads it (waitedOn).
		seedWaitingRoot(t, pool, "CAPTURE-1", "A")
		seedTickMirror(t, pool, "CAPTURE-1", "todo", 1, key)
		seedTickMirror(t, pool, key, "todo", 1)

		w.applyControllerTick(context.Background(), time.Now())

		assertLabelsAndRank(t, pool, key, labels, rank, 7)
	})
	t.Run("the answer to the daemon's own status write", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{issues: map[string]map[string]any{key: boardIssue(key, "backlog", 9, labels, rank)}}, 1)
		written, err := w.dispatch.SetStatus(context.Background(), key, "todo")
		if err != nil {
			t.Fatalf("write the status of %s: %v", key, err)
		}

		// The fact api's recordOwnStatusWrite applies for a write Dispatch answered at a sequence.
		event := intake.OwnStatusWrite(written)
		fact := intake.ObservedIssue(written)
		fact.Delivered = &event
		applyCopyFact(t, w, "api", "status/own-write", fact)

		assertLabelsAndRank(t, pool, key, labels, rank, 9)
	})
}

// Every kind of reading of Dispatch writes the blockers it carries at its own sequence, before the
// decisions that read them, so admission starts no issue an unfinished blocker holds. In each case
// W waits on B and B is unfinished: an issue event of W names B, while the copy shows B in
// progress; the first boot's listing, which refills the project's rows of the copy, lists W
// waiting on B and B in progress; the periodic turn re-reads B; and the release's read of W, as it
// adopts W into a live tree, shows W waiting on B. In the first three W is a labelled todo root on
// the waiting line with a slot free for it; in the fourth the tree's root waits on W, its open
// child, in awaiting_children and holds no slot (dispatch://INFRA-3320 R11), so both slots stand
// free. In each W takes a slot only if the pass of the same fact reads a copy in which W waits on
// nothing.
//
// The turn re-reads a blocker only while the copy shows it unfinished
// (TestThePeriodicTurnReReadsOnlyWhatSomethingWaitsOn), so its case holds B todo in the copy,
// where W already waits on it, and makes B a labelled todo root on the waiting line too, behind W:
// the turn reads B in progress and waiting on another issue, and B, which the copy showed waiting
// on nothing, takes no slot, which is the tick's admission pass reading B's row as that reading
// wrote it.
func TestEachReadingKindWritesItsBlockers(t *testing.T) {
	t.Parallel()
	const waiter, blocker, other = "CAPTURE-3", "CAPTURE-7", "CAPTURE-8"
	t.Run("an issue event", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{}, 1)
		seedWaitingRoot(t, pool, waiter, "A")
		seedTickMirror(t, pool, blocker, "in_progress", 1)

		applyCopyFact(t, w, "dispatch", "blocked-by", capturedDispatchEvent(t, waiter, 4,
			eventEdit{`"labels":[]`, `"blocked_by":` + jsonOf(t, []string{blocker}) + `,"labels":["legion"]`}))

		if got := copied(t, pool, waiter); !slices.Equal(got.blockers, []string{blocker}) || got.blockersSeq != 4 {
			t.Fatalf("the copy holds %s waiting on %q at %d, want %s at the event's 4", waiter, got.blockers, got.blockersSeq, blocker)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: %s waits on %s, which is in progress", slotted, waiter, blocker)
		}
	})
	t.Run("the first boot's listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{listing: []map[string]any{
			boardSummary(waiter, "todo", 4, []string{"legion"}, "A", blocker),
			boardSummary(blocker, "in_progress", 6, []string{}, "B"),
		}}, 1)
		w.consumers = bootClaimConsumers(t, pool)
		seedWaitingRoot(t, pool, waiter, "A")
		if _, err := pool.Exec(context.Background(), `insert into dispatch_issue_refills (project) values ($1)`, "CAPTURE"); err != nil {
			t.Fatalf("mark CAPTURE for the refill: %v", err)
		}

		if err := w.reconcile(context.Background()); err != nil {
			t.Fatalf("the boot's reconcile of the listing: %v", err)
		}

		if got := copied(t, pool, waiter); !slices.Equal(got.blockers, []string{blocker}) || got.blockersSeq != 4 {
			t.Fatalf("the copy holds %s waiting on %q at %d, want %s at the listed 4", waiter, got.blockers, got.blockersSeq, blocker)
		}
		if got := copied(t, pool, blocker); got.status != "in_progress" || got.statusSeq != 6 {
			t.Fatalf("the copy holds %s %q at %d, want in_progress at the listed 6", blocker, got.status, got.statusSeq)
		}
		if marks := refillMarks(t, pool); marks["CAPTURE"] == "" {
			t.Fatalf("the refill marks = %q, want CAPTURE's stamped by the boot that refilled", marks)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: %s waits on %s, which is in progress", slotted, waiter, blocker)
		}
	})
	t.Run("the turn's re-read", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{issues: map[string]map[string]any{
			blocker: boardIssue(blocker, "in_progress", 7, []string{"legion"}, "B", other),
		}}, 1)
		seedWaitingRoot(t, pool, waiter, "A")
		seedTickMirror(t, pool, waiter, "todo", 1, blocker)
		seedWaitingRoot(t, pool, blocker, "B")
		seedTickMirror(t, pool, blocker, "todo", 1)

		w.applyControllerTick(context.Background(), time.Now())

		got := copied(t, pool, blocker)
		if got.status != "in_progress" || got.statusSeq != 7 || !slices.Equal(got.blockers, []string{other}) || got.blockersSeq != 7 {
			t.Fatalf("the copy holds %s %q at %d waiting on %q at %d, want the turn's reading: in_progress waiting on %s, both at 7",
				blocker, got.status, got.statusSeq, got.blockers, got.blockersSeq, other)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: %s waits on %s, in progress, and %s on %s, of which the copy holds no row",
				slotted, waiter, blocker, blocker, other)
		}
	})
	t.Run("the release's read", func(t *testing.T) {
		const tree = "CAPTURE-1"
		pool := isolatedOutboxPool(t)
		w := copyRuntime(t, pool, &copyBoard{}, 2)
		seedLiveTree(t, pool, tree)
		seedWaitingRoot(t, pool, waiter, "B")
		seedTickMirror(t, pool, blocker, "in_progress", 1)

		applyCopyFact(t, w, "api", "waves/adopt", intake.ChildAdoption{Tree: tree, Children: []intake.AdoptedChild{
			{Issue: waiter, Parent: tree, Status: "todo", BlockedBy: []string{blocker}, Seq: 4}}})

		if got := copied(t, pool, waiter); !slices.Equal(got.blockers, []string{blocker}) || got.blockersSeq != 4 {
			t.Fatalf("the copy holds %s waiting on %q at %d, want %s at the read's 4", waiter, got.blockers, got.blockersSeq, blocker)
		}
		recorded, err := w.recordedIssue(context.Background(), waiter)
		if err != nil {
			t.Fatalf("read the record of %s: %v", waiter, err)
		}
		if recorded == nil || recorded.Tree != tree || recorded.Phase != phase.Admitted {
			t.Fatalf("%s = %#v, want it in the tree of %s at admitted: its first phase waits on %s", waiter, recorded, tree, blocker)
		}
		// The tree's root, in planning, now waits on its open child W in awaiting_children and holds
		// no slot (dispatch://INFRA-3320 R11), so the one slot this case can show is one W took.
		if got := recordedPhase(t, w, tree); got != phase.AwaitingChildren {
			t.Fatalf("%s after the adoption = %s, want awaiting_children: %s is its open child", tree, got, waiter)
		}
		if slotted := tickSlotted(t, pool); len(slotted) != 0 {
			t.Fatalf("slots = %v, want none: %s waits on %s, which is in progress, and %s holds no slot in awaiting_children", slotted, waiter, blocker, tree)
		}
	})
}

// An event that moves a child to another parent is in the copy before that same event's decisions
// read it: P is a root at admitted whose design gate is open, and its one open child C, in backlog
// under P in the copy at 5, holds P's first phase by the children-first rule
// (record.ChildrenFirstCandidates, record.ChildWaitsOf), with a slot free. A fact that changes
// nothing leaves P waiting. C's event at 6 shows C under another parent, and after that one fact P
// has started its first phase: the event's own admission pass read C under its new parent, so the
// wait ended in that pass and not at a later fact. With the mirror after admission in the handler
// list, P would still wait after the event, so the test holds the order of the handlers.
func TestAnEventsParentIsInTheCopyBeforeItsDecisions(t *testing.T) {
	t.Parallel()
	const root, child, elsewhere = "CAPTURE-1", "CAPTURE-3", "CAPTURE-9"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	w := copyRuntime(t, pool, &copyBoard{}, 1)
	// The seeds of admit's children-first tests (cfTree, seedMirrorChild): the root holds its
	// slot at admitted, its tree runs and its spec's gate is registered and not yet approved.
	seedTickIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "CAPTURE", Title: root, Tree: root, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true})
	holdSlot(t, pool, root)
	openTree(t, pool, "CAPTURE", root)
	seedTickMirror(t, pool, root, "in_progress", 1)
	putOutboxGate(t, pool, record.NewStore(), record.DesignGate{Issue: root, ArtifactID: "artifact-1", LatestVersion: 1})
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(ctx, tx, record.DispatchReading{Key: child, Project: "CAPTURE", Seq: 5,
			Status: record.Carry("backlog"), Parent: record.Carry(root), Blockers: record.Carry[[]string](nil)})
	}); err != nil {
		t.Fatalf("seed the copy's row of %s: %v", child, err)
	}
	// A person's approval of the root's spec opens its gate (admit's openGate).
	applyCopyFact(t, w, "dispatch", "gate-approved", intake.DispatchArtifact{Key: root, ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: record.EventMark{Seq: 4, At: time.Now().Add(-time.Hour).UTC()}})

	applyCopyFact(t, w, "controller", "tick", intake.ControllerTick{})
	if got := recordedPhase(t, w, root); got != phase.Admitted {
		t.Fatalf("%s after a fact that changes nothing = %s, want it at admitted: %s, open under it, holds its first phase", root, got, child)
	}

	applyCopyFact(t, w, "dispatch", "moved", capturedDispatchEvent(t, child, 6,
		eventEdit{`"parent":null`, `"parent":` + jsonOf(t, elsewhere)}, eventEdit{`"status":"todo"`, `"status":"backlog"`}))

	if got := copied(t, pool, child); got.parent != elsewhere || got.parentSeq != 6 {
		t.Fatalf("the copy holds %s under %q at %d, want %s at the event's 6", child, got.parent, got.parentSeq, elsewhere)
	}
	if got := recordedPhase(t, w, root); got != phase.Planning {
		t.Fatalf("%s after the event that moved %s = %s, want it planning: that event's own admission pass reads %s under %s",
			root, child, got, child, elsewhere)
	}
}

// copyRuntime is the boot's own workflow runtime (openWorkflow) over pool, as bootedTickWorkflow
// builds it: its handlers are the production list, the mirror before the engine and admission,
// and its Dispatch client is the production one (dispatch.HTTPClient) on board, with slots
// admission slots and the project's controller registered. pool is a database of the test's own
// (isolatedOutboxPool), never the package's shared run database: a refill mark acts on every row
// of its project in the database it is in.
func copyRuntime(t *testing.T, pool *store.Pool, board *copyBoard, slots int) *workflowRuntime {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	registerProjectController(t, pool, "CAPTURE")
	cfg := config.Config{Project: "CAPTURE", DispatchURL: dispatchBoard(t, board.serve(t)), AdmissionCap: slots,
		Projects: map[string]config.Project{"CAPTURE": {}}}
	tokens := loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	w, err := openWorkflow(ctx, cfg, st, roleApps(deploymentApps(cfg.Roles), planRoleOf(st)), "project-id", dispatch.BearerTransport(workflowDispatchToken, nil), nil, newRepositories(cfg.Project, cfg.RepositoryRenames),
		appauth.NewInstalled(), slog.New(slog.NewTextHandler(io.Discard, nil)), tokens, repositoriesGitHub(t), nil, time.Now, nil, nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}
	w.bootID = "test-boot"
	return w
}

// copyBoard is Dispatch as a copyRuntime reads it, through the daemon's own client: the calls
// every boot makes (servedBootCall), the project's listing, and each issue it serves by key, to a
// read of that issue and to the daemon's status write on it, which it answers with the issue as
// the write leaves it. A test fills it before the runtime reads it and never changes it after.
// Any other request fails the test.
type copyBoard struct {
	listing []map[string]any
	issues  map[string]map[string]any
}

func (b *copyBoard) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if servedBootCall(t, w, r, "CAPTURE") {
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
		var body any
		switch issue := b.issues[key]; {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			body = b.listing
		case r.Method == http.MethodGet && issue != nil:
			body = issue
		case r.Method == http.MethodPatch && issue != nil:
			var write struct {
				Status string `json:"status"`
			}
			if err := json.NewDecoder(r.Body).Decode(&write); err != nil {
				t.Errorf("decode the daemon's write of %s: %v", key, err)
			}
			written := maps.Clone(issue)
			written["status"] = write.Status
			body = written
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "not served", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write %s: %v", r.URL.Path, err)
		}
	}
}

// boardSummary is key as Dispatch's listing serves it (model.IssueSummary) at seq: in status,
// carrying labels and rank, waiting on blockedBy, under no parent, claimed by nobody, and on the
// project's one repository.
func boardSummary(key, status string, seq int64, labels []string, rank string, blockedBy ...string) map[string]any {
	return map[string]any{"key": key, "title": key, "status": status, "rank": rank, "labels": labels, "parent": nil,
		"blocked_by": append([]string{}, blockedBy...), "last_seq": seq, "claim": nil, "resolved_repo": "acme/widgets"}
}

// boardIssue is key as Dispatch's read of one issue, and its answer to a write, serve it
// (model.Issue): the summary's fields and the issue's project.
func boardIssue(key, status string, seq int64, labels []string, rank string, blockedBy ...string) map[string]any {
	issue := boardSummary(key, status, seq, labels, rank, blockedBy...)
	issue["project"] = "CAPTURE"
	return issue
}

// eventEdit is one change a test makes to the captured event's JSON text: the first from in it
// becomes to.
type eventEdit struct{ from, to string }

// capturedDispatchEvent is Dispatch's captured issue.updated event (intake's testdata), as
// Dispatch's event log serves it, made the event of key at seq on the project's one repository,
// with each of edits written into its own JSON text, and decoded by the daemon's own decoder. The
// captured project is this daemon's, and the captured issue is in todo, carries no label and no
// parent, waits on nothing (the payload has no blocked_by) and is ranked UUU.
func capturedDispatchEvent(t *testing.T, key string, seq int64, edits ...eventEdit) intake.Fact {
	t.Helper()
	captured, err := os.ReadFile("../intake/testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read the captured Dispatch issue.updated event: %v", err)
	}
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(captured, &envelope); err != nil {
		t.Fatalf("decode the captured envelope: %v", err)
	}
	event := strings.ReplaceAll(envelope.Payload, `"CAPTURE-3"`, strconv.Quote(key))
	for _, edit := range append([]eventEdit{
		{`"seq":2,`, fmt.Sprintf(`"seq":%d,`, seq)},
		{`"last_seq":2,`, fmt.Sprintf(`"last_seq":%d,`, seq)},
		{`"resolved_repo":"sjawhar/legion"`, `"resolved_repo":"acme/widgets"`},
	}, edits...) {
		edited := strings.Replace(event, edit.from, edit.to, 1)
		if edited == event {
			t.Fatalf("the captured event has no %s to replace", edit.from)
		}
		event = edited
	}
	fact, _, err := intake.DecodeDispatchEvent("CAPTURE", key, []byte(event))
	if err != nil || fact == nil {
		t.Fatalf("decode the captured event = %#v, %v; want an issue fact", fact, err)
	}
	return fact
}

// jsonOf is value's JSON text, for an edit of the captured event.
func jsonOf(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return string(encoded)
}

// applyCopyFact applies fact through the runtime's own handlers, as the daemon applies every fact
// (intake.ApplyFact), and fails the test on an error or a refusal: no fact here is one a handler
// refuses.
func applyCopyFact(t *testing.T, w *workflowRuntime, source, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), w.pool, source, id, fact, w.handlers...)
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	if result.Refusal != nil {
		t.Fatalf("apply %s: refused %#v", id, result.Refusal)
	}
}

// copiedValues is a row of the copy of Dispatch's issues as these tests read it by SQL: each of
// its five values with the sequence of the reading that wrote it, a parent of none read as "".
type copiedValues struct {
	status      string
	statusSeq   int64
	parent      string
	parentSeq   int64
	blockers    []string
	blockersSeq int64
	labels      []string
	labelsSeq   int64
	rank        string
	rankSeq     int64
}

// copied is key's row of the copy, read by SQL.
func copied(t *testing.T, pool *store.Pool, key string) copiedValues {
	t.Helper()
	var row copiedValues
	if err := pool.QueryRow(context.Background(), `select status, status_seq, coalesce(parent, ''), parent_seq, blockers, blockers_seq,
		labels, labels_seq, rank, rank_seq from dispatch_issues where key = $1`, key).Scan(&row.status, &row.statusSeq, &row.parent,
		&row.parentSeq, &row.blockers, &row.blockersSeq, &row.labels, &row.labelsSeq, &row.rank, &row.rankSeq); err != nil {
		t.Fatalf("read the row of %s in the copy: %v", key, err)
	}
	return row
}

// assertLabelsAndRank requires key's row of the copy to hold labels and rank, each at seq, the
// sequence of the reading that carried them.
func assertLabelsAndRank(t *testing.T, pool *store.Pool, key string, labels []string, rank string, seq int64) {
	t.Helper()
	got := copied(t, pool, key)
	if !slices.Equal(got.labels, labels) || got.labelsSeq != seq || got.rank != rank || got.rankSeq != seq {
		t.Fatalf("the copy holds %s's labels %q at %d and rank %q at %d, want the reading's %q and %q, both at %d",
			key, got.labels, got.labelsSeq, got.rank, got.rankSeq, labels, rank, seq)
	}
}

// recordedPhase is the phase the daemon's record of key stands in, which must be recorded.
func recordedPhase(t *testing.T, w *workflowRuntime, key string) phase.Phase {
	t.Helper()
	recorded, err := w.recordedIssue(context.Background(), key)
	if err != nil || recorded == nil {
		t.Fatalf("read the record of %s = %#v, %v; want it recorded", key, recorded, err)
	}
	return recorded.Phase
}

// seedLiveTree records tree as a running root of this project in planning, as api's
// adoptionHarness records its live tree: holding an admission slot, its tree lifecycle open under
// the workflow and its spec's gate approved, so a todo child that joins it with a slot free
// starts planning at once.
func seedLiveTree(t *testing.T, pool *store.Pool, tree string) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{Key: tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "CAPTURE", Title: tree, Tree: tree, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "A",
		HandedOver: true, LastDispatchSeq: 1})
	holdSlot(t, pool, tree)
	openTree(t, pool, "CAPTURE", tree)
	approved := 1
	putOutboxGate(t, pool, record.NewStore(), record.DesignGate{Issue: tree, ArtifactID: "artifact-" + tree, LatestVersion: 1, ApprovedVersion: &approved})
}
