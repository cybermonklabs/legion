// The rig of the whole-daemon tests of new work on a closed issue whose work merged
// (merged_new_work_test.go, D17 of the done-by-checks plan): the workflow's engine and admission as
// the daemon wires them (the mirror, the engine and admission, in that order), the daemon's outbox
// runner over them with the merge read's executor among the kinds it runs, the controller's tick as
// the daemon applies it, and a Dispatch and a GitHub of the test's own. The Dispatch is the real one
// where a test reads Dispatch's own routes (mnwDispatch), and otherwise the merge read's board
// (mergeReadBoard of outbox_merge_read_test.go), which answers the issue read and the results read
// in their own shape and counts each read. TestTheMergedNewWorkRigReadsAMergeThroughTheResultsRoute
// is the rig's own acceptance: a merge read of a child whose record shows no merge finds the merge
// Dispatch's results route names. It asserts nothing of what the read's fact starts.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// mnwProject is the rig's project, in Dispatch and in the daemon's record, and mnwToken its
	// claim token's spelling, which claims, tree lifecycles and the outbox's project are keyed by.
	mnwProject = "ACME"
	mnwToken   = "acme"
	// mnwRoot is the root of every tree the tests seed and mnwChild its child: ACME-1 and ACME-2,
	// the first two issues a person makes in a scratch Dispatch's project.
	mnwRoot  = "ACME-1"
	mnwChild = "ACME-2"
	// mnwSeq is the Dispatch sequence a seeded record stands at where no Dispatch of the test's own
	// names one; a request comes after it.
	mnwSeq = 5
	// mnwMergeUnread is the kind of the controller's notice that names a request whose merge read
	// found no answering candidate, or stopped.
	mnwMergeUnread = "merge-unread"
	// mnwCheck is the one check of the child's Verification document in a real Dispatch, which runs
	// after the merge (mnwVerification).
	mnwCheck = "c-merge-reported"
)

// mnwVerification is the child's Verification document in a real Dispatch: one check, mnwCheck,
// that runs after the merge, so Dispatch's results route compares it with the merge of the work.
const mnwVerification = "# Verification\n\n:::check{#" + mnwCheck +
	` surface="cli" when="after_merge" run="go test ./internal/widgets/"}` + "\nThe widgets report their merge once it reaches main.\n:::\n"

// mnwStart is when a rig's clock stands as it begins: 20:00 UTC two days after today. The clock
// then never stands behind the database's own, so a row a door makes due at Postgres's now() is
// due at once, and a test that crosses UTC midnights crosses them at known hours.
func mnwStart() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour).Add(2*24*time.Hour + 20*time.Hour)
}

// mnwRig is one daemon's workflow over a record of its own: the engine and admission on the rig's
// clock, the outbox runner over the same handlers, and the controller's tick of the same runtime.
// seen records every fact the handlers applied, after the deciders took it, so a test reads what
// the merge read carried back. notices records every notice the outbox published.
type mnwRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	seen      *outboxFactHandler
	handlers  []intake.Handler
	notices   *outboxPublisher
	runner    *outbox
	w         *workflowRuntime
	mu        sync.Mutex
	now       time.Time
	facts     int
}

// newMNWRig is the rig over client, the daemon's Dispatch, and the GitHub at githubAPI. The engine
// and admission take the rig's clock, as the daemon gives both its one clock, and the handlers are
// the daemon's three in its order (openWorkflow), with seen after them.
func newMNWRig(t *testing.T, client dispatch.Client, githubAPI string) *mnwRig {
	t.Helper()
	r := &mnwRig{t: t, ctx: context.Background(), pool: isolatedOutboxPool(t), records: record.NewStore(), seen: &outboxFactHandler{},
		notices: &outboxPublisher{}, now: mnwStart()}
	r.engine = workflow.New(r.records, workflow.Config{Project: mnwProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Clock: r.clock}, quietLogger())
	r.admission = admit.New(r.records, r.engine, 4, mnwProject, quietLogger())
	r.admission.UseClock(r.clock)
	r.handlers = []intake.Handler{blockers.NewMirror(r.records, mnwProject), r.engine, r.admission, r.seen}
	r.runner = &outbox{log: quietLogger(), pool: r.pool, records: r.records, dispatch: client, notices: r.notices, handlers: r.handlers,
		tokens: outboxTokens{}, githubAPI: githubAPI, dispatchProject: mnwProject, project: mnwToken, stateDir: t.TempDir(), now: r.clock}
	r.w = &workflowRuntime{pool: r.pool, records: r.records, handlers: r.handlers, dispatch: client, dispatchProject: mnwProject,
		bootID: "mnw-boot", gatedSteps: config.DesignGateRootIssues.Steps(), log: quietLogger()}
	return r
}

// clock is the rig's clock: the engine's, admission's and the outbox's.
func (r *mnwRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// at moves the rig's clock to at.
func (r *mnwRig) at(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = at
}

// transact runs write in a transaction of its own and fails the test on its error.
func (r *mnwRig) transact(write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.TransactWith(r.ctx, r.pool, pgx.TxOptions{}, write); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// read runs read in a read-only transaction of its own and fails the test on its error.
func (r *mnwRig) read(read func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.TransactWith(r.ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, read); err != nil {
		r.t.Fatalf("read the record: %v", err)
	}
}

// mnwRank is the rank each issue's record and each of its events carry.
func mnwRank(key string) string {
	if key == mnwRoot {
		return "A"
	}
	return "B"
}

// mnwIssue is key's record standing at at with status, in tree under parent, at generation 1, on
// acme/widgets at main, handed to Legion, at Dispatch's sequence seq.
func mnwIssue(key, tree string, parent *string, at phase.Phase, status string, seq int64) record.Issue {
	return record.Issue{Key: key, Tree: tree, Parent: parent, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Project: mnwProject,
		Title: key, Phase: at, Generation: 1, TreeGeneration: 1, Status: status, DispatchStatus: status, Rank: mnwRank(key), HandedOver: true,
		LastDispatchSeq: seq}
}

// liveTree records the root working in implementing in a tree that runs, holding its admission
// slot and its tree's lifecycle as admission leaves them, and its child closed at generation 1.
// Each holds its design gate approved. The child's record holds no row that bears on its merge:
// no merge snapshot, no pull request row, no cleared clock and no production check (NONE), as a
// whole-tree clear before this release left a merged child. rootSeq and childSeq are the Dispatch
// sequences each record stands at.
func (r *mnwRig) liveTree(rootSeq, childSeq int64) {
	r.t.Helper()
	r.transact(func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, mnwIssue(mnwRoot, mnwRoot, nil, phase.Implementing, "in_progress", rootSeq)); err != nil {
			return err
		}
		if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: mnwRoot, Index: 0, AdmittedAt: r.clock()}); err != nil {
			return err
		}
		if _, err := r.records.OpenTreeLifecycle(ctx, tx, mnwToken, mnwRoot, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		return r.records.PutIssue(ctx, tx, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", childSeq))
	})
	r.gate(mnwRoot)
	r.gate(mnwChild)
	if class := r.class(mnwChild); class != record.MergeHistoryNone {
		r.t.Fatalf("the class of %s as seeded = %s, want NONE: its record holds no row of its merge", mnwChild, class)
	}
}

// gate records key's spec approved at version 3, its gate opened.
func (r *mnwRig) gate(key string) {
	r.t.Helper()
	approved := 3
	r.transact(func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutGate(ctx, tx, record.DesignGate{Issue: key, ArtifactID: "spec-" + key, LatestVersion: 3, ApprovedVersion: &approved, EverOpened: true})
	})
}

// gates is what key's gate rows name of the design: its document and the version a person
// approved, for each step.
func (r *mnwRig) gates(key string) map[record.DesignStep]string {
	r.t.Helper()
	identity := map[record.DesignStep]string{}
	r.read(func(ctx context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, key)
		for step, gate := range gates {
			approved := "none"
			if gate.ApprovedVersion != nil {
				approved = strconv.Itoa(*gate.ApprovedVersion)
			}
			identity[step] = gate.ArtifactID + "@" + approved
		}
		return err
	})
	return identity
}

// issue is key's record as it stands.
func (r *mnwRig) issue(key string) record.Issue {
	r.t.Helper()
	var got *record.Issue
	r.read(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = r.records.Issue(ctx, tx, key)
		return err
	})
	if got == nil {
		r.t.Fatalf("the record holds no issue %s", key)
	}
	return *got
}

// class is key's merge history class as the record derives it (record.Store.MergeHistoryOf).
func (r *mnwRig) class(key string) record.MergeHistoryClass {
	r.t.Helper()
	var class record.MergeHistoryClass
	r.read(func(ctx context.Context, tx pgx.Tx) error {
		row, err := r.records.MergeHistoryOf(ctx, tx, key)
		class = row.Class
		return err
	})
	return class
}

// apply applies fact from source through the rig's handlers, under an event id of its own, as the
// daemon applies an event, and fails the test on its error.
func (r *mnwRig) apply(source string, fact intake.Fact) {
	r.t.Helper()
	r.facts++
	if _, err := intake.ApplyFact(r.ctx, r.pool, source, fmt.Sprintf("mnw-%s-%d", source, r.facts), fact, r.handlers...); err != nil {
		r.t.Fatalf("apply %T: %v", fact, err)
	}
}

// mnwPerson is the Dispatch user whose events the tests apply: the person a scratch Dispatch
// trusts (dispatchCall's identity).
const mnwPerson = "smoke"

// mnwMove is key's move to status at seq under parent ("" for a root), as who made it: "person"
// is a Dispatch user's event, and "controller" the controller's set_status, a route's own write.
func mnwMove(key, parent, status string, seq int64, who string) intake.DispatchIssue {
	fact := intake.DispatchIssue{Key: key, Seq: seq, Type: "issue.updated", Status: status, Title: key, Parent: parent, Rank: mnwRank(key),
		HandedOver: true, ResolvedRepo: liveRepo}
	switch who {
	case "person":
		fact.PersonActor = mnwPerson
	case "controller":
		fact.DaemonActor, fact.StatusMove, fact.Requester = true, true, claim.RoleController
	}
	return fact
}

// pending is key's merge_read row as the outbox holds it: its payload and when it is due next, and
// whether one stands. It fails the test where more than one stands: one row stands for an issue at
// a time.
func (r *mnwRig) pending(key string) (record.MergeRead, time.Time, bool) {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select payload, next_at from outbox where issue = $1 and kind = $2 order by id`, key,
		string(record.OutboxKindMergeRead))
	if err != nil {
		r.t.Fatalf("read the merge_read rows of %s: %v", key, err)
	}
	defer rows.Close()
	var reads []record.MergeRead
	var due time.Time
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload, &due); err != nil {
			r.t.Fatalf("read a merge_read row of %s: %v", key, err)
		}
		var read record.MergeRead
		if err := json.Unmarshal(payload, &read); err != nil {
			r.t.Fatalf("decode a merge_read row of %s: %v", key, err)
		}
		reads = append(reads, read)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the merge_read rows of %s: %v", key, err)
	}
	switch len(reads) {
	case 0:
		return record.MergeRead{}, time.Time{}, false
	case 1:
		return reads[0], due.UTC(), true
	}
	r.t.Fatalf("%s holds %d merge_read rows, want at most one", key, len(reads))
	return record.MergeRead{}, time.Time{}, false
}

// mnwTurnKinds are the kinds the rig's turn runs: the merge read's rows and the controller's
// notices. The rows of every other kind stand, so a test reads the notices to an architect as the
// workflow queued them.
var mnwTurnKinds = []record.OutboxKind{record.OutboxKindMergeRead, record.OutboxKindControllerNotice}

// turn runs each row of mnwTurnKinds due at the rig's clock, as their lanes run them, until none is
// due.
func (r *mnwRig) turn() {
	r.t.Helper()
	if drained, _ := r.runner.drain(r.ctx, "", mnwTurnKinds); !drained {
		r.t.Fatal("the outbox's turn left a due row it could not claim")
	}
}

// readMerge runs key's merge read turn by turn, each at the moment its row is due, until the row is
// gone or waits unread or stopped with no refresh asked, for at most limit turns.
func (r *mnwRig) readMerge(key string, limit int) {
	r.t.Helper()
	for range limit {
		read, due, standing := r.pending(key)
		if !standing || read.Progress.State != "" && !read.Progress.Refresh {
			return
		}
		if due.After(r.clock()) {
			r.at(due)
		}
		r.turn()
	}
	r.t.Fatalf("the merge read of %s did not end in %d turns", key, limit)
}

// runAll runs every row of every kind due at the rig's clock once, as the daemon's lanes would
// (outbox.RunOnce): a test whose runner holds a supervisor and a tree store runs its starts and its
// closes so.
func (r *mnwRig) runAll() {
	r.t.Helper()
	if err := r.runner.RunOnce(r.ctx); err != nil {
		r.t.Fatalf("run the outbox: %v", err)
	}
}

// slotted says whether key holds an admission slot.
func (r *mnwRig) slotted(key string) bool {
	r.t.Helper()
	held := false
	r.read(func(ctx context.Context, tx pgx.Tx) error {
		slots, err := r.records.Slots(ctx, tx)
		for _, slot := range slots {
			held = held || slot.Issue == key
		}
		return err
	})
	return held
}

// tick is the controller's tick at the rig's clock, as the daemon's wake applies it: its re-read of
// what the project waits on, then the tick fact through every handler (applyControllerTick).
func (r *mnwRig) tick() {
	r.t.Helper()
	r.w.applyControllerTick(r.ctx, r.clock())
}

// told is how many controller notices of kind the workflow queued: the rows that stand, and the
// notices the outbox published to the controller. Only one issue of a rig waits on its merge read,
// so the count is that issue's.
func (r *mnwRig) told(kind string) int {
	r.t.Helper()
	var standing int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = $1 and payload->>'kind' = $2`,
		string(record.OutboxKindControllerNotice), kind).Scan(&standing); err != nil {
		r.t.Fatalf("count the %s controller notices: %v", kind, err)
	}
	published := 0
	for _, publish := range r.notices.publishes() {
		if notice, ok := publish.payload.(record.Notice); ok && string(notice.Kind) == kind {
			published++
		}
	}
	return standing + published
}

// noticed is how many notices of kind to an architect stand queued on key.
func (r *mnwRig) noticed(key, kind string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where issue = $1 and kind = $2 and payload->>'kind' = $3`,
		key, string(record.OutboxKindNotice), kind).Scan(&count); err != nil {
		r.t.Fatalf("count %s's %s notices: %v", key, kind, err)
	}
	return count
}

// mnwRequest is one request row (kind request) as these tests read it: who asked, and the
// generation that carries it.
type mnwRequest struct {
	role      claim.Role
	carriedBy uint64
}

// requests is every request row of key, oldest first.
func (r *mnwRig) requests(key string) []mnwRequest {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select role, coalesce(carried_by, 0) from defects where issue = $1 and kind = 'request' order by id`, key)
	if err != nil {
		r.t.Fatalf("read the requests of %s: %v", key, err)
	}
	defer rows.Close()
	var got []mnwRequest
	for rows.Next() {
		var role string
		var carried int64
		if err := rows.Scan(&role, &carried); err != nil {
			r.t.Fatalf("read a request of %s: %v", key, err)
		}
		got = append(got, mnwRequest{role: claim.Role(role), carriedBy: uint64(carried)})
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the requests of %s: %v", key, err)
	}
	return got
}

// waits fails the test unless the request on key waits for its merge read, as the door leaves it:
// the record stands at generation at done with the status todo and no change started, one
// merge_read row of that generation stands, asked by requester, no request row is written, and
// nothing re-entered the issue (no child-status notice).
func (r *mnwRig) waits(key string, generation uint64, requester claim.Role) record.MergeRead {
	r.t.Helper()
	got := r.issue(key)
	read, _, standing := r.pending(key)
	if got.Generation != generation || got.Phase != phase.Done || got.Status != "todo" || got.NextChange != 0 || !standing {
		r.t.Fatalf("after the todo %s stands at generation %d in %s with status %s and change %d, with a merge_read row %t; "+
			"want generation %d at done with todo, no change and one merge_read row: the request waits for its merge read",
			key, got.Generation, got.Phase, got.Status, got.NextChange, standing, generation)
	}
	if read.Generation != generation || read.Requester != requester {
		r.t.Fatalf("%s's merge read = %+v, want one of generation %d asked by %s", key, read, generation, requester)
	}
	if requests := r.requests(key); len(requests) != 0 {
		r.t.Fatalf("the requests of %s = %+v, want none before its merge is read", key, requests)
	}
	if reentered := r.noticed(key, "child-status"); reentered != 0 {
		r.t.Fatalf("child-status notices of %s = %d, want none: nothing re-entered it", key, reentered)
	}
	return read
}

// started fails the test unless key's one next change started at generation, carrying one request
// of role, and no merge read stands for it any more.
func (r *mnwRig) started(key string, generation uint64, role claim.Role) record.Issue {
	r.t.Helper()
	got := r.issue(key)
	if got.Generation != generation || got.NextChange != generation || got.NextChanges != 1 || got.Phase == phase.Done {
		r.t.Fatalf("%s stands at generation %d in %s with status %s, change %d of %d; want its next change %d started, the first",
			key, got.Generation, got.Phase, got.Status, got.NextChange, got.NextChanges, generation)
	}
	if requests := r.requests(key); len(requests) != 1 || requests[0].role != role || requests[0].carriedBy != generation {
		r.t.Fatalf("the requests of %s = %+v, want one of %s carried by generation %d", key, requests, role, generation)
	}
	if read, _, standing := r.pending(key); standing {
		r.t.Fatalf("%s still holds its merge read %+v once its next change started, want it finished", key, read)
	}
	return got
}

// mnwMerged fails the test unless got's merge snapshot is pull request number of acme/widgets,
// merged as sha from the final head head into main, at a time GitHub named.
func mnwMerged(t *testing.T, got record.Issue, number int, head, sha string) {
	t.Helper()
	if got.Merge.Number != number || got.Merge.Head != head || got.Merge.SHA != sha || got.Merge.Base != "main" || got.Merge.Repo != liveRepo ||
		got.Merge.MergedAt.IsZero() {
		t.Fatalf("%s's merge snapshot = %+v, want %s#%d merged as %s from %s into main", got.Key, got.Merge, liveRepo, number, sha, head)
	}
}

// mnwCountedDispatch is the daemon's client of a real Dispatch, with each issue read and each
// results read it sends counted, as the merge read's board counts them.
type mnwCountedDispatch struct {
	dispatch.Client
	mu    sync.Mutex
	reads map[string]int
}

// newMNWCountedDispatch is the daemon's client of the scratch Dispatch at base, with the agent
// bearer token, as the daemon reads it.
func newMNWCountedDispatch(base, token string) *mnwCountedDispatch {
	return &mnwCountedDispatch{Client: dispatch.New(base, dispatch.BearerTransport(token, nil)), reads: map[string]int{}}
}

func (d *mnwCountedDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.count("issue")
	return d.Client.GetIssue(ctx, key)
}

func (d *mnwCountedDispatch) IssueResults(ctx context.Context, key string) (dispatch.IssueResults, error) {
	d.count("results")
	return d.Client.IssueResults(ctx, key)
}

// count counts one read of read.
func (d *mnwCountedDispatch) count(read string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads[read]++
}

// counted is how many reads of read the client sent.
func (d *mnwCountedDispatch) counted(read string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads[read]
}

// mnwDispatch is a real Dispatch of the test's own (startScratchDispatchOn) holding the project
// ACME, its root ACME-1 and the child ACME-2 under it, each made by a person, and the sequence each
// stood at once made.
type mnwDispatch struct {
	t      *testing.T
	server *scratchDispatch
	js     jetstream.JetStream
	github *doneByChecksGitHub
	root   createdIssue
	child  createdIssue
}

// newMNWDispatch starts a scratch Dispatch with NATS and no App, and makes the project, the root
// and the child. Dispatch then reads no GitHub, and what it answers of the child is its own
// fields: its status, its links and an empty results read.
func newMNWDispatch(t *testing.T) *mnwDispatch {
	t.Helper()
	d := &mnwDispatch{t: t, server: startScratchDispatchOn(t, scratchDispatchOptions{})}
	dispatchCall(t, d.server.base, http.MethodPost, "/api/v1/projects", map[string]string{"key": mnwProject, "name": "Merged new work"}, nil)
	d.issues()
	return d
}

// newMNWDispatchWithMerge starts a scratch Dispatch whose App reads a GitHub stand-in of the
// test's own (doneByChecksGitHub), with its events on NATS, and makes the project on acme/widgets
// with the delivery settings that turn Dispatch's intake of GitHub's events on, then the root and
// the child with mnwVerification as the child's Verification document. GitHub opens pull request
// #41 from the child's branch and merges it, and Dispatch takes both events. It returns once
// Dispatch's results route compares the child's check with that merge, as it does for every merge
// of the work Dispatch holds.
func newMNWDispatchWithMerge(t *testing.T) *mnwDispatch {
	t.Helper()
	natsURL := workflowNATS(t)
	d := &mnwDispatch{t: t, js: workflowJetStream(t, natsURL), github: newDoneByChecksGitHub(t)}
	d.server = startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: d.github.url})
	base := d.server.base
	dispatchCall(t, base, http.MethodPost, "/api/v1/projects", map[string]string{"key": mnwProject, "name": "Merged new work"}, nil)
	dispatchCall(t, base, http.MethodPut, "/api/v1/projects/"+mnwProject+"/default-repository", map[string]any{"repo": liveRepo}, nil)
	dispatchCall(t, base, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo": liveRepo, "deploy_workflow_path": ".github/workflows/deploy.yaml", "production_job_names": []string{"deploy"},
		"pr_checks_workflow_path": ".github/workflows/ci.yaml", "population_authors": []string{stepRecordsPerson},
	}, nil)
	d.issues()
	dispatchCall(t, base, http.MethodPost, "/api/v1/issues/"+mnwChild+"/artifacts",
		map[string]string{"name": "verification.md", "content": mnwVerification}, nil)
	d.github.open(41, claim.IssueBranch(mnwChild), stepRecordsHead)
	d.publishPull("opened")
	d.github.merge()
	d.publishPull("closed")
	d.awaitComparedWith(mnwChild, doneByChecksMergeCommit)
	return d
}

// issues makes the root and the child under it as the person, and keeps the sequence each stood at.
func (d *mnwDispatch) issues() {
	d.t.Helper()
	dispatchCall(d.t, d.server.base, http.MethodPost, "/api/v1/issues", map[string]any{"project": mnwProject, "title": mnwRoot}, &d.root)
	dispatchCall(d.t, d.server.base, http.MethodPost, "/api/v1/issues", map[string]any{"project": mnwProject, "title": mnwChild, "parent": mnwRoot},
		&d.child)
	if d.root.Key != mnwRoot || d.child.Key != mnwChild {
		d.t.Fatalf("Dispatch made the project's issues %q and %q, want %s and %s", d.root.Key, d.child.Key, mnwRoot, mnwChild)
	}
}

// patch is the person's edit of key with body, as the dashboard and `dispatch issue-update` make
// it, and answers the sequence Dispatch stood at after it.
func (d *mnwDispatch) patch(key string, body map[string]any) int64 {
	d.t.Helper()
	var edited createdIssue
	dispatchCall(d.t, d.server.base, http.MethodPatch, "/api/v1/issues/"+key, body, &edited)
	if edited.LastSeq == 0 {
		d.t.Fatalf("Dispatch's answer to the edit of %s names no sequence", key)
	}
	return edited.LastSeq
}

// publishPull publishes GitHub's pull_request event of action on the stand-in's pull request, as
// the stand-in holds it now, on notifications.github.acme.widgets.pr.<n>, the subject Dispatch's
// intake reads, with the fields Envoy's listener relays (contracts.githubPayload): the head
// repository Dispatch holds the attribution to, and a merge's commit, time and author.
func (d *mnwDispatch) publishPull(action string) {
	d.t.Helper()
	pull := d.github.pullRequest()
	fields := map[string]string{
		"action": action, "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(pull.number), "title": doneByChecksTitle,
		"url": pull.url(), "body": "", "author": stepRecordsPerson, "head_ref": pull.branch, "head_sha": pull.head,
		"head_repo": liveRepo, "base_ref": "main", "merged": strconv.FormatBool(pull.merged),
		"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
	}
	if pull.merged {
		fields["merge_commit_sha"], fields["merged_by"] = pull.merge, stepRecordsPerson
		fields["merged_at"] = pull.mergedAt.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		d.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.acme.widgets.pr.%d", pull.number)
	eventID := fmt.Sprintf("%s-pr-%d-%s", mnwProject, pull.number, action)
	envelope, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": "github", "source_event_id": eventID, "topic": subject, "dedupe_key": eventID,
		"issued_at": time.Now().UnixMilli(), "payload_summary": subject, "payload": string(payload), "trace_id": eventID,
	})
	if err != nil {
		d.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.js.Publish(ctx, subject, envelope); err != nil {
		d.t.Fatalf("publish %s: %v", subject, err)
	}
}

// mnwComparedChecks is GET /api/v1/issues/{key}/results as the rig reads it: the merge commit of each
// basis each check is compared with.
type mnwComparedChecks struct {
	Checks []struct {
		ComparedWith []struct {
			MergeCommit string `json:"merge_commit"`
		} `json:"compared_with"`
	} `json:"checks"`
}

// compares says whether a check of read is compared with a basis whose merge commit is merge.
func (read mnwComparedChecks) compares(merge string) bool {
	for _, check := range read.Checks {
		for _, basis := range check.ComparedWith {
			if basis.MergeCommit == merge {
				return true
			}
		}
	}
	return false
}

// awaitComparedWith reads key's results from Dispatch as the person until a check is compared with
// a basis whose merge commit is merge, within stepRecordsWait, and fails the test with the last
// answer otherwise.
func (d *mnwDispatch) awaitComparedWith(key, merge string) {
	d.t.Helper()
	var last string
	if !testwait.Holds(stepRecordsWait, time.Second, func() bool {
		answer, err := d.personRead("/api/v1/issues/" + key + "/results")
		if err != nil {
			last = err.Error()
			return false
		}
		last = string(answer)
		var read mnwComparedChecks
		if err := json.Unmarshal(answer, &read); err != nil {
			last = fmt.Sprintf("%s: %v", answer, err)
			return false
		}
		return read.compares(merge)
	}) {
		d.t.Fatalf("Dispatch's results of %s name no basis at the merge %s within %s; the last read: %s", key, merge, stepRecordsWait, last)
	}
}

// personRead is GET path on the scratch Dispatch as the person reads it, or the error the read met.
func (d *mnwDispatch) personRead(path string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, d.server.base+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Dispatch-User", mnwPerson)
	response, err := scratchDispatchClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s = %d %s", path, response.StatusCode, bytes.TrimSpace(answer))
	}
	return answer, nil
}

// The rig's own acceptance. A real Dispatch holds the child's merged pull request #41 and a check
// of the child that runs after the merge, so its results route compares that check with the merge.
// The daemon's record of the child holds no row of its merge (NONE) and a request that waits on its
// merge read, whose row the test queues as a door queues it. The read, through the outbox runner,
// reads the child's links and results from Dispatch, the pull requests of the merge commit and
// then #41 from GitHub's stand-in, and posts one MergeRead fact of #41's merge. What the fact
// starts is no part of it.
func TestTheMergedNewWorkRigReadsAMergeThroughTheResultsRoute(t *testing.T) {
	t.Parallel()
	d := newMNWDispatchWithMerge(t)
	client := newMNWCountedDispatch(d.server.base, d.server.token)
	r := newMNWRig(t, client, d.github.url)
	r.liveTree(d.root.LastSeq, d.child.LastSeq)
	child := r.issue(mnwChild)
	child.Status = "todo"
	r.transact(func(ctx context.Context, tx pgx.Tx) error { return r.records.PutIssue(ctx, tx, child) })
	row, err := record.NewOutboxRow(mnwChild, record.MergeRead{Issue: mnwChild, Generation: 1, Requester: record.RequesterPerson,
		Request: "issue.updated: todo"}, r.clock())
	if err != nil {
		t.Fatalf("build the merge_read row: %v", err)
	}
	r.transact(func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, row) })

	r.readMerge(mnwChild, 20)

	var posted []intake.MergeRead
	for _, fact := range r.seen.facts() {
		if read, ok := fact.(intake.MergeRead); ok {
			posted = append(posted, read)
		}
	}
	if len(posted) != 1 || posted[0].Issue != mnwChild || posted[0].Generation != 1 || posted[0].Merge.Number != 41 ||
		posted[0].Merge.SHA != doneByChecksMergeCommit || posted[0].Merge.Head != stepRecordsHead || posted[0].Merge.Base != "main" {
		t.Fatalf("the merge read posted %+v, want one fact of %s at generation 1 carrying #41 merged as %s from %s into main",
			posted, mnwChild, doneByChecksMergeCommit, stepRecordsHead)
	}
	if client.counted("issue") != 1 || client.counted("results") != 1 {
		t.Fatalf("the read sent Dispatch %d issue reads and %d results reads, want one each", client.counted("issue"), client.counted("results"))
	}
}
