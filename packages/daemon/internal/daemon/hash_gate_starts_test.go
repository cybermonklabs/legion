package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The tree of TestHashGateStartsConfirmWithDispatch: a root gated on its spec, which the record
// holds approved at its latest version 4, and a child of it.
const (
	startsRoot  = "LEGION-720"
	startsChild = "LEGION-721"
	startsDoc   = "artifact-720"
)

// startsStanding is Dispatch's approval of the root's spec standing for its latest version with a
// Dispatch that sends the hash gate's fields: the approval of version 3, current for version 4.
func startsStanding() dispatch.Approval {
	return dispatch.Approval{IssueKey: startsRoot, State: "approved", Version: new(3), LatestVersion: 4, DigestOwed: new(false),
		CurrentSHA256: new("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
}

// startsNotStanding is that approval once a version 5 is written whose digest Dispatch still owes:
// Dispatch shows the approval, but it does not stand for the latest version (Stands).
func startsNotStanding() dispatch.Approval {
	return dispatch.Approval{IssueKey: startsRoot, State: "approved", Version: new(3), LatestVersion: 5, DigestOwed: new(true)}
}

// standingDispatch is a Dispatch that shows every document's approval standing for its latest
// version (dispatch.Approval.Stands), and reaches the nil Client it embeds for every other call:
// the read before each start the design gate holds (designGateStandsInDispatch), for a test whose
// gate is the record's alone. It is no gate seed's answer: it names no version.
type standingDispatch struct{ dispatch.Client }

func (standingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{State: "approved", DigestOwed: new(false)}, nil
}

// startsDispatch is a Dispatch that answers each document's approval with answer and counts the
// reads. Every other call reaches the nil Client it embeds: a start reads nothing else of it.
type startsDispatch struct {
	dispatch.Client
	mu     sync.Mutex
	read   int
	answer func(ctx context.Context, artifactID string) (dispatch.Approval, error)
}

func (d *startsDispatch) Approval(ctx context.Context, artifactID string) (dispatch.Approval, error) {
	d.mu.Lock()
	d.read++
	answer := d.answer
	d.mu.Unlock()
	return answer(ctx, artifactID)
}

// reads is how many approval reads the fake has answered.
func (d *startsDispatch) reads() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.read
}

// answers makes the fake answer every read with approval.
func (d *startsDispatch) answers(approval dispatch.Approval) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = func(context.Context, string) (dispatch.Approval, error) { return approval, nil }
}

// startsRig is the tree on a database of its own, with the outbox that runs its starts over the
// fake Dispatch, a fake runtime and a clock of its own, logging JSON lines to logs.
type startsRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records record.Store
	client  *startsDispatch
	runner  *outbox
	sup     *supervisor
	rt      *fake.Runtime
	trees   *store.Store
	logs    *syncBuffer
	clock   time.Time
}

// newStartsRig records the root and the child in planning, the root's spec gate open at version 4
// (put, unless gate is nil, which records none), and an outbox gated on steps, whose Dispatch
// shows the approval standing.
func newStartsRig(t *testing.T, steps []record.DesignStep, gate *record.DesignGate) *startsRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	r := &startsRig{t: t, ctx: context.Background(), pool: pool, records: records, client: &startsDispatch{}, sup: sup, rt: rt,
		trees: outboxTreeStore(t, pool, startsRoot), logs: &syncBuffer{}, clock: time.Now().UTC()}
	r.client.answers(startsStanding())
	r.runner = &outbox{log: jsonLogger(r.logs), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, dispatch: r.client,
		trees: r.trees, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), gatedSteps: steps, notices: &holderPublisher{},
		now: func() time.Time { return r.clock },
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + request.Issue}, nil
		},
	}
	parent := startsRoot
	for _, issue := range []record.Issue{
		{Key: startsRoot, Tree: startsRoot, Rank: "U"},
		{Key: startsChild, Tree: startsRoot, Parent: &parent, Rank: "V"},
	} {
		issue.Repo, issue.DefaultBranch, issue.GitHubRepo, issue.Project = "acme/widgets", "main", "acme/widgets", "LEGION"
		issue.Title, issue.Phase, issue.Generation, issue.Status = issue.Key, phase.Planning, 1, "in_progress"
		putOutboxIssue(t, pool, records, issue)
	}
	if gate != nil {
		putOutboxGate(t, pool, records, *gate)
	}
	// The holders took version 4, so a merger's start is owed no news.
	putOutboxTold(t, pool, records, startsRoot, map[record.DesignStep]int{record.DesignStepSpec: 4})
	return r
}

// startsGate is the root's spec gate as the record holds it open: version 4, approved at 4.
func startsGate() *record.DesignGate {
	return &record.DesignGate{Issue: startsRoot, Step: record.DesignStepSpec, ArtifactID: startsDoc, LatestVersion: 4, ApprovedVersion: new(4),
		ApprovedMark: record.EventMark{Seq: 7, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}}
}

// closedGate is the root's spec gate as a new version leaves it on the record: version 5, the
// approval still at 4.
func closedGate() *record.DesignGate {
	gate := startsGate()
	gate.LatestVersion = 5
	return gate
}

// moves records key in at, and role's phases row of key on entry 1, raised by the move out of
// from ("" for none), as the start of each case finds it.
func (r *startsRig) moves(key string, at phase.Phase, role claim.Role, from phase.Phase) {
	r.t.Helper()
	issue := *readIssue(r.t, r.pool, key)
	issue.Phase = at
	if at == phase.Merging {
		issue.Status = "retro"
	}
	putOutboxIssue(r.t, r.pool, r.records, issue)
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: role, Entry: 1, EntryFrom: from, EntryTask: "Work it.",
			EntryRaisedAt: r.clock})
	}); err != nil {
		r.t.Fatalf("record the %s row of %s: %v", role, key, err)
	}
}

// phaseStart is the start of role in at, on a member of the tree, that entry 1 of role's row is
// of.
func phaseStart(role claim.Role, at phase.Phase) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: startsRoot, Role: role, Generation: 1, Phase: at, Entry: 1, Task: "Work it."}
}

// firstPhase records key's planner on the entry the move out of admitted raised, and answers the
// start of it.
func (r *startsRig) firstPhase(key string) record.SuperviseRequest {
	r.moves(key, phase.Planning, claim.RolePlanner, phase.Admitted)
	return phaseStart(claim.RolePlanner, phase.Planning)
}

// merger records the root in merging and answers its merger's start.
func (r *startsRig) merger() record.SuperviseRequest {
	issue := *readIssue(r.t, r.pool, startsRoot)
	issue.Phase, issue.Status = phase.Merging, "retro"
	putOutboxIssue(r.t, r.pool, r.records, issue)
	return record.SuperviseRequest{Op: "start", Tree: startsRoot, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging, Task: "Merge it."}
}

// execute runs start, a start of key, once.
func (r *startsRig) execute(key string, start record.SuperviseRequest) error {
	r.t.Helper()
	return r.runner.execute(r.ctx, mustOutboxRow(r.t, key, start, r.clock))
}

// spawns is how many sessions the runtime spawned.
func (r *startsRig) spawns() int { return len(r.rt.CallsOf("Spawn")) }

// seeds is every gate_seed row the root holds.
func (r *startsRig) seeds() []record.GateSeed {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select payload from outbox where kind = $1 and issue = $2 order by id`, string(record.OutboxKindGateSeed), startsRoot)
	if err != nil {
		r.t.Fatalf("read the gate seeds of %s: %v", startsRoot, err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		r.t.Fatalf("read the gate seeds of %s: %v", startsRoot, err)
	}
	seeds := make([]record.GateSeed, 0, len(payloads))
	for _, payload := range payloads {
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindGateSeed, Payload: payload})
		if err != nil {
			r.t.Fatalf("decode a gate seed of %s: %v", startsRoot, err)
		}
		seeds = append(seeds, decoded.(record.GateSeed))
	}
	return seeds
}

// released creates role's claim on the root as an idle release left it, and records it where the
// runner reads claims (releasedMergerOf).
func (r *startsRig) released(role claim.Role) *supervise.Machine {
	r.t.Helper()
	if role == claim.RoleMerger {
		return releasedMergerOf(r.t, r.sup, r.trees, startsRoot)
	}
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: mustClaimToken(r.t, startsRoot, role), Project: "legion", Tree: startsRoot,
		Issue: startsRoot, Role: role, Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased, Session: "ses-" + string(role), SessionFile: r.t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		r.t.Fatalf("create the released %s's claim: %v", role, err)
	}
	if err := r.trees.PutClaim(r.ctx, machine.Claim()); err != nil {
		r.t.Fatalf("record the released %s's claim: %v", role, err)
	}
	return machine
}

// heldWake runs a held message to role's released claim on the root (deliverHeld) once: a merge
// holds change, for the merger, as its executor delivers it, and for any other role the message
// deliverHeld is handed.
func (r *startsRig) heldWake(role claim.Role) error {
	r.t.Helper()
	if role == claim.RoleMerger {
		return r.runner.execute(r.ctx, mustOutboxRow(r.t, startsRoot, record.MergeHoldsChanged{Project: "LEGION"}, r.clock))
	}
	row := mustOutboxRow(r.t, startsRoot, record.MergeHoldsChanged{Project: "LEGION"}, r.clock)
	return r.runner.deliverHeld(r.ctx, row, heldMessage{issue: *readIssue(r.t, r.pool, startsRoot), token: mustClaimToken(r.t, startsRoot, role),
		role: role, what: "the held message of " + startsRoot, message: "a held message", claimEnded: "ended", delivered: "delivered"})
}

// Before each start the design gate holds, the daemon reads Dispatch for each document the tree is
// gated on (R4 of dispatch://INFRA-6065): a member's first phase, the root's or a child's, a
// merger's start, and a held message that would wake a released merger (deliverHeld). The record
// is read first: a gate it holds closed holds the start with no read, and a read never opens it.
// Where the record shows the gate open, the start acts only once Dispatch shows each approval
// standing (dispatch.Approval.Stands); otherwise it waits and queues one gate seed of the step,
// which a second try does not queue again. A read that fails holds the start on its backoff,
// logged with the reason. A step on an ancestor's basis and a carried copy are not read, and a
// copy an older daemon carried unmarked is passed over on Dispatch naming its document on another
// issue. A merger already at work, whose start carries news alone, reads the record alone, and
// no other start reads Dispatch: a later phase, a held retry, a re-admitted mid-phase member, a
// return from awaiting_children, a held wake of another role. A tree with no gate row, and a
// deployment that gates no step, read nothing. Each case counts the fake's reads.
func TestHashGateStartsConfirmWithDispatch(t *testing.T) {
	t.Parallel()
	steps := config.DesignGateRootIssues.Steps()
	type kind struct {
		name string
		// run makes the start of the kind ready on r and runs it once.
		run func(r *startsRig) error
		// acted says whether the start acted once it was let through.
		acted func(r *startsRig) bool
	}
	spawned := func(r *startsRig) bool { return r.spawns() == 1 }
	kinds := []kind{
		{name: "the root's first phase", run: func(r *startsRig) error { return r.execute(startsRoot, r.firstPhase(startsRoot)) }, acted: spawned},
		{name: "a child's first phase", run: func(r *startsRig) error { return r.execute(startsChild, r.firstPhase(startsChild)) }, acted: spawned},
		{name: "a merger's start", run: func(r *startsRig) error { return r.execute(startsRoot, r.merger()) }, acted: spawned},
		{name: "a held message to a released merger", run: func(r *startsRig) error {
			r.merger()
			if _, found := r.sup.Machine(mustClaimToken(r.t, startsRoot, claim.RoleMerger)); !found {
				r.released(claim.RoleMerger)
			}
			return r.heldWake(claim.RoleMerger)
		}, acted: func(r *startsRig) bool {
			machine, _ := r.sup.Machine(mustClaimToken(r.t, startsRoot, claim.RoleMerger))
			return !machine.Claim().Released()
		}},
	}
	// A held message waits for the pod it woke whether or not the gate let it through, so whether a
	// kind was held or let through is told by acted.
	for _, k := range kinds {
		t.Run(k.name+" acts once Dispatch shows the approval standing", func(t *testing.T) {
			r := newStartsRig(t, steps, startsGate())
			err := k.run(r)
			if err != nil && !errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("the start = %v, want it let through", err)
			}
			if !k.acted(r) || r.client.reads() != 1 || len(r.seeds()) != 0 {
				t.Fatalf("acted = %t, reads = %d, seeds = %v (%v); want it acted on one read and no seed", k.acted(r), r.client.reads(), r.seeds(), err)
			}
		})
		t.Run(k.name+" waits while the approval does not stand, and seeds the step once", func(t *testing.T) {
			r := newStartsRig(t, steps, startsGate())
			r.client.answers(startsNotStanding())
			want := []record.GateSeed{{ArtifactID: startsDoc, Step: record.DesignStepSpec, Version: 4, Generation: 1}}
			for try := 1; try <= 2; try++ {
				r.clock = r.clock.Add(time.Minute)
				if err := k.run(r); !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), startsDoc) {
					t.Fatalf("try %d: the start = %v, want it held (wait.ErrWaiting) naming the document", try, err)
				}
				if k.acted(r) || r.client.reads() != try {
					t.Fatalf("try %d: acted = %t, reads = %d; want it held after %d reads", try, k.acted(r), r.client.reads(), try)
				}
				if got := r.seeds(); len(got) != 1 || got[0] != want[0] {
					t.Fatalf("try %d: gate seeds = %+v, want the one seed of the step %+v", try, got, want)
				}
			}
			if lines := loggedLines(t, r.logs.String(), "Dispatch does not show the approval of a gated step standing"); len(lines) != 2 || lines[0]["step"] != "spec" {
				t.Fatalf("the hold's log lines = %v, want one per try naming the step", lines)
			}
		})
		t.Run(k.name+" waits on a closed record with no read", func(t *testing.T) {
			r := newStartsRig(t, steps, closedGate())
			if err := k.run(r); !errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("the start = %v, want it held (wait.ErrWaiting)", err)
			}
			if k.acted(r) || r.client.reads() != 0 || len(r.seeds()) != 0 {
				t.Fatalf("acted = %t, reads = %d, seeds = %v; want it held with no read, though Dispatch shows the approval standing", k.acted(r), r.client.reads(), r.seeds())
			}
		})
	}

	// A read that fails holds the start on the row's backoff, logged with the reason, and spawns
	// nothing: the real client's answer to a server that refuses the connection, one that does not
	// answer in time, a 5xx, a 404 and a body that is no document.
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
		bound   time.Duration
		reason  string
	}{
		{name: "a read that fails", closed: true, reason: "connect"},
		{name: "a read that times out", handler: func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() },
			bound: 50 * time.Millisecond, reason: "deadline exceeded"},
		{name: "a read answered 5xx", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		}, reason: "upstream unavailable"},
		{name: "a read answered 404", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"ARTIFACT_NOT_FOUND","error":"artifact not found"}`)
		}, reason: "ARTIFACT_NOT_FOUND"},
		{name: "a read answered with no document", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>a sign-in page</html>")
		}, reason: "decode"},
	} {
		t.Run(tc.name+" holds a merger's start on its backoff", func(t *testing.T) {
			r := newStartsRig(t, steps, startsGate())
			handler := tc.handler
			if handler == nil {
				handler = func(http.ResponseWriter, *http.Request) {}
			}
			server := httptest.NewServer(handler)
			if tc.closed {
				server.Close()
			} else {
				t.Cleanup(server.Close)
			}
			client := dispatch.New(server.URL, dispatch.BearerTransport("dispatch-token", nil))
			r.client.answer = func(ctx context.Context, artifactID string) (dispatch.Approval, error) {
				if tc.bound > 0 {
					bounded, cancel := context.WithTimeout(ctx, tc.bound)
					defer cancel()
					ctx = bounded
				}
				return client.Approval(ctx, artifactID)
			}
			row := mustOutboxRow(t, startsRoot, r.merger(), r.clock.Add(-time.Minute))
			enqueueOutbox(t, r.pool, r.records, row)
			if err := r.runner.RunOnce(r.ctx); err != nil {
				t.Fatalf("drain the merger's start: %v", err)
			}
			var attempts int
			var next time.Time
			var last string
			if err := r.pool.QueryRow(r.ctx, `select attempts, next_at, last_error from outbox where kind = $1 and issue = $2`,
				string(record.OutboxKindSupervise), startsRoot).Scan(&attempts, &next, &last); err != nil {
				t.Fatalf("read the merger's start after the drain: %v", err)
			}
			if attempts != 1 || !next.After(r.clock) || !strings.Contains(last, tc.reason) {
				t.Fatalf("the start after the drain = %d attempts, next at %s (now %s), last error %q; want it retried on its backoff naming %q",
					attempts, next, r.clock, last, tc.reason)
			}
			failed := loggedLines(t, r.logs.String(), "outbox row failed")
			if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0]["error"]), tc.reason) || !strings.Contains(fmt.Sprint(failed[0]["error"]), startsDoc) {
				t.Fatalf("the failure's log lines = %v, want one naming the document and %q", failed, tc.reason)
			}
			if r.spawns() != 0 || r.client.reads() != 1 || len(r.seeds()) != 0 {
				t.Fatalf("spawns = %d, reads = %d, seeds = %v; want nothing spawned or seeded on one read", r.spawns(), r.client.reads(), r.seeds())
			}
		})
	}

	// A step that stands open on an ancestor's approval, and a carried copy, are not read: the
	// start acts with Dispatch showing the root's own approval not standing.
	for _, tc := range []struct {
		name string
		gate func() *record.DesignGate
	}{
		{name: "a step open on a basis", gate: func() *record.DesignGate {
			// No person approved the root's version 4, which the basis opens the step at.
			unapproved := startsGate()
			unapproved.ApprovedVersion, unapproved.ApprovedMark = nil, record.EventMark{}
			opened := record.OpenOnBasis(*unapproved, record.GateBasis{Issue: "LEGION-100", ArtifactID: "artifact-100", Version: 5,
				Approval: record.EventMark{Seq: 40, At: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, Approver: "user:alice",
				Serves: []string{"dispatch://LEGION-100/spec#b-p2@v5"}, Reason: "The checks serve P2.", Session: "ses-architect",
				RegisteredAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)})
			return &opened
		}},
		{name: "a carried copy", gate: func() *record.DesignGate {
			copied := startsGate()
			copied.Carried = true
			return copied
		}},
	} {
		t.Run(tc.name+" is not read", func(t *testing.T) {
			gate := tc.gate()
			if !record.GateOpen(*gate) {
				t.Fatalf("the case's gate %+v reads closed, want it open on the record", gate)
			}
			r := newStartsRig(t, steps, gate)
			r.client.answers(startsNotStanding())
			if err := r.execute(startsRoot, r.merger()); err != nil {
				t.Fatalf("the merger's start = %v, want it run", err)
			}
			if r.spawns() != 1 || r.client.reads() != 0 {
				t.Fatalf("spawns = %d, reads = %d; want the merger started with no read", r.spawns(), r.client.reads())
			}
		})
	}

	// A copy a daemon from before the carried column carried (Departure D3): Dispatch names its
	// document on the old root, so the step is logged and passed over, neither held nor seeded.
	t.Run("an unflagged carried copy is passed over", func(t *testing.T) {
		r := newStartsRig(t, steps, startsGate())
		old := startsNotStanding()
		old.IssueKey = "LEGION-100"
		r.client.answers(old)
		if err := r.execute(startsRoot, r.firstPhase(startsRoot)); err != nil {
			t.Fatalf("the first phase's start = %v, want it run", err)
		}
		if r.spawns() != 1 || r.client.reads() != 1 || len(r.seeds()) != 0 {
			t.Fatalf("spawns = %d, reads = %d, seeds = %v; want it started on one read and nothing seeded", r.spawns(), r.client.reads(), r.seeds())
		}
		if lines := loggedLines(t, r.logs.String(), "whose document Dispatch names on another issue"); len(lines) != 1 || lines[0]["document_issue"] != "LEGION-100" {
			t.Fatalf("the pass-over's log lines = %v, want one naming LEGION-100", lines)
		}
	})

	// A merger already at work is not read again (R3): its start queued for news alone reads the
	// record, which holds it while the gate is closed there, and never Dispatch.
	for _, tc := range []struct {
		name string
		gate *record.DesignGate
		held bool
	}{
		{name: "open", gate: startsGate()},
		{name: "closed", gate: closedGate(), held: true},
	} {
		t.Run("a merger's news-only start on a record "+tc.name+" reads the record alone", func(t *testing.T) {
			r := newStartsRig(t, steps, tc.gate)
			r.client.answers(startsNotStanding())
			start := r.merger()
			start.Task, start.News = "", true
			r.released(claim.RoleMerger)
			err := r.execute(startsRoot, start)
			if held := errors.Is(err, wait.ErrWaiting); held != tc.held || (err != nil && !held) {
				t.Fatalf("the news-only start = %v, want held %t", err, tc.held)
			}
			if r.client.reads() != 0 || len(r.seeds()) != 0 {
				t.Fatalf("reads = %d, seeds = %v; want no read of Dispatch", r.client.reads(), r.seeds())
			}
		})
	}

	// No other start reads Dispatch, whatever it shows.
	for _, tc := range []struct {
		name  string
		key   string
		start func(r *startsRig) record.SuperviseRequest
	}{
		{name: "a later phase's start", key: startsRoot, start: func(r *startsRig) record.SuperviseRequest {
			r.moves(startsRoot, phase.Implementing, claim.RoleImplementer, phase.Planning)
			return phaseStart(claim.RoleImplementer, phase.Implementing)
		}},
		{name: "a held retry of the first phase", key: startsRoot, start: func(r *startsRig) record.SuperviseRequest {
			start := r.firstPhase(startsRoot)
			start.ResendEntry = true
			return start
		}},
		{name: "a re-admitted mid-phase member's start", key: startsChild, start: func(r *startsRig) record.SuperviseRequest {
			r.moves(startsChild, phase.Planning, claim.RolePlanner, "")
			return phaseStart(claim.RolePlanner, phase.Planning)
		}},
		{name: "a return from awaiting_children", key: startsRoot, start: func(r *startsRig) record.SuperviseRequest {
			r.moves(startsRoot, phase.Implementing, claim.RoleImplementer, phase.AwaitingChildren)
			return phaseStart(claim.RoleImplementer, phase.Implementing)
		}},
	} {
		t.Run(tc.name+" reads nothing", func(t *testing.T) {
			r := newStartsRig(t, steps, startsGate())
			r.client.answers(startsNotStanding())
			if err := r.execute(tc.key, tc.start(r)); err != nil {
				t.Fatalf("the start = %v, want it run", err)
			}
			if r.spawns() != 1 || r.client.reads() != 0 {
				t.Fatalf("spawns = %d, reads = %d; want it started with no read", r.spawns(), r.client.reads())
			}
		})
	}
	t.Run("a held wake of another role reads nothing", func(t *testing.T) {
		r := newStartsRig(t, steps, startsGate())
		r.client.answers(startsNotStanding())
		machine := r.released(claim.RoleImplementer)
		if err := r.heldWake(claim.RoleImplementer); !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "released pod") {
			t.Fatalf("the held wake = %v, want it waiting for the pod it woke", err)
		}
		if machine.Claim().Released() || r.client.reads() != 0 {
			t.Fatalf("released = %t, reads = %d; want the implementer woken with no read", machine.Claim().Released(), r.client.reads())
		}
	})

	// A tree with no gate row, and a deployment that gates no step (whose registration writes its
	// row approved), read nothing.
	for _, tc := range []struct {
		name  string
		steps []record.DesignStep
		gate  *record.DesignGate
	}{
		{name: "a tree with no gate row", steps: steps},
		{name: "a deployment that gates no step", steps: config.DesignGateOff.Steps(), gate: startsGate()},
	} {
		t.Run(tc.name+" reads nothing", func(t *testing.T) {
			r := newStartsRig(t, tc.steps, tc.gate)
			r.client.answers(startsNotStanding())
			if err := r.execute(startsRoot, r.firstPhase(startsRoot)); err != nil {
				t.Fatalf("the first phase's start = %v, want it run", err)
			}
			if err := r.execute(startsRoot, r.merger()); err != nil {
				t.Fatalf("the merger's start = %v, want it run", err)
			}
			if r.spawns() != 2 || r.client.reads() != 0 {
				t.Fatalf("spawns = %d, reads = %d; want both started with no read", r.spawns(), r.client.reads())
			}
		})
	}
}
