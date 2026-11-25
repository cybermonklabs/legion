package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The proofs of dispatch://INFRA-3320's checks k3 and k11 on the person-handover rig
// (person_handover_scratch_dispatch_test.go), beside k1 and k2 (basis_gate_scratch_dispatch_test.go):
// a step that an ancestor's approval opened stays on the root's own document whatever is done to
// the ancestor's (k3, R3), and a step registered on an ancestor's open approval request waits on
// that request as the daemon's own state until the person's approval opens it or another act ends
// the wait (k11, R1). Every run takes issues of its own, so a run reads nothing another left.

// smoke is the person of the scratch Dispatch, as the daemon's Dispatch client names an actor.
var smoke = dispatch.Actor{Kind: "user", ID: "smoke"}

// restart stops the daemon and boots it again on the same configuration and store, as the boot
// proof's second boot does (TestABootListingQueuesAPersonsLabelAgainstScratchDispatch): a fresh
// runtime, the same database, the same Dispatch and the same NATS, whose durable consumer hands
// the new daemon every event published while none ran.
func (r *personHandoverRig) restart() {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
	settled(r.t, r.d, "answer its state after the restart", func(api.State) bool { return true })
}

// catchUpNow has the one composer of a root architect's catch-up (workflow.QueueCatchUp, which the
// architect's ready writes through) state key's tree as the record holds it now, for the launch
// admit registered, and answers its spec step: what a relaunched architect of key reads. A ready
// is one fact per launch, so a second ready of the same launch tells nothing; the composer is read
// directly so a proof can read the catch-up after every act.
func (r *personHandoverRig) catchUpNow(key string) record.CatchUpGate {
	r.t.Helper()
	return catchUpGate(r.t, r.composeCatchUp(key).notice)
}

// queuedCatchUp is one catch-up row of a root's architect in the outbox: its id, the row whose
// telling it is (the row itself, or the row a re-held copy copies: record.Notice.ResendOf), and
// the reason it tells.
type queuedCatchUp struct {
	id, telling int64
	reason      string
}

// composedCatchUp is one composition of a root architect's catch-up (composeCatchUp): the
// catch-up rows queued for that architect just before it, the outbox row it queued and the notice
// that row carries.
type composedCatchUp struct {
	queued []queuedCatchUp
	id     int64
	notice record.Notice
}

// composeCatchUp is catchUpNow's composition, answering the whole of it. The composer first drops
// the catch-ups still queued for the architect that record.Store.DropCatchUps may drop (it keeps
// one whose telling is recorded as told under record.KeptToldSource, and one carrying kept
// decisions while a row of its telling is under a live lease), so the rows queued before it are
// read in the same transaction, under the advisory lock that drop takes and every composition of a
// catch-up of key waits on (record.LockCatchUps): no catch-up the daemon queues lands between that
// read and the drop, and none lands beside the row this composition queued, which is then the
// newest catch-up of key that copies no other.
func (r *personHandoverRig) composeCatchUp(key string) composedCatchUp {
	t := r.t
	t.Helper()
	root := r.record(key)
	launch := r.architects[key].launch.Generation
	var composed composedCatchUp
	if err := store.Transact(t.Context(), r.pool, func(_ context.Context, tx pgx.Tx) error {
		if err := record.LockCatchUps(t.Context(), tx, key); err != nil {
			return err
		}
		for _, row := range catchUpRows(t, tx, key) {
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				return err
			}
			notice := payload.(record.Notice)
			composed.queued = append(composed.queued, queuedCatchUp{id: row.ID, telling: cmp.Or(notice.ResendOf, row.ID), reason: notice.Reason})
		}
		if err := workflow.QueueCatchUp(t.Context(), tx, record.NewStore(), r.cfg.Gates.Steps(), root, launch, "", time.Now().UTC(), time.Time{}); err != nil {
			return err
		}
		var payload []byte
		if err := tx.QueryRow(t.Context(), `select id, payload from outbox where kind = $1 and issue = $2 and payload->>'kind' = 'catch-up'
			and payload->>'resend_of' is null order by id desc limit 1`, string(record.OutboxKindNotice), key).Scan(&composed.id, &payload); err != nil {
			return err
		}
		return json.Unmarshal(payload, &composed.notice)
	}); err != nil {
		t.Fatalf("compose the catch-up of the architect of %s: %v", key, err)
	}
	if composed.notice.CatchUp == nil {
		t.Fatalf("the catch-up composed for the architect of %s carries no tree: %+v", key, composed.notice)
	}
	return composed
}

// otherNotices is every notice the daemon has queued on key for its architect but the catch-ups,
// which a read holds apart (stepReads), each as `<kind>: <reason>`, oldest first.
func (r *personHandoverRig) otherNotices(key string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select (payload->>'kind') || ': ' || coalesce(payload->>'reason', '') from outbox
		where kind = $1 and issue = $2 and payload->>'kind' <> 'catch-up' order by id`, string(record.OutboxKindNotice), key)
	if err != nil {
		r.t.Fatalf("read the notices of %s: %v", key, err)
	}
	notices, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the notices of %s: %v", key, err)
	}
	return notices
}

// stepReads is what k3 reads of a root after each act: its spec step as the state serves it
// (which `read_record` parses), its spec row as the daemon stores it, its phase, the whole
// catch-up its architect would read now, and every notice queued for its architect but
// catch-ups. The catch-ups are read apart: queued counts those queued for the architect when the
// read composed its own, and foreign names each of them that no read of the proof composed (own).
// stepJSON, rowJSON and caughtJSON are the step, the row and the catch-up as JSON, encoded once
// at the read, which stays compares.
type stepReads struct {
	step                          api.GatedStepView
	row                           record.DesignGate
	phase                         phase.Phase
	caught                        record.Notice
	stepJSON, rowJSON, caughtJSON string
	notices                       []string
	queued                        int
	foreign                       []string
	own                           *ownCatchUps
}

// ownCatchUps is every catch-up of one root that the proof's reads of it composed (composeCatchUp),
// by outbox id, beside those already queued when the first read was made: the catch-ups a later
// read may find queued for the root's architect, as themselves or as re-held copies. Any other is
// one the daemon queued since the last read, which the next composition would drop unseen
// (record.Store.DropCatchUps), so each read reads the catch-ups queued before it composes its own.
type ownCatchUps struct{ ids []int64 }

// stepReads reads key as k3 does, holding the catch-ups queued for its architect to own, the
// catch-ups the proof's earlier reads of key composed; nil for the first read, which takes every
// catch-up queued before it as its own, since no act of the proof came before it.
func (r *personHandoverRig) stepReads(key string, own *ownCatchUps) stepReads {
	t := r.t
	t.Helper()
	state, _ := settled(t, r.d, "answer its state", func(api.State) bool { return true })
	reads := stepReads{step: stepOf(state, key), row: r.gates(key)[record.DesignStepSpec], phase: state.Issues[key].Phase, own: own}
	composed := r.composeCatchUp(key)
	if reads.own == nil {
		reads.own = &ownCatchUps{}
		for _, queued := range composed.queued {
			reads.own.ids = append(reads.own.ids, queued.telling)
		}
	}
	for _, queued := range composed.queued {
		if !slices.Contains(reads.own.ids, queued.telling) {
			reads.foreign = append(reads.foreign, fmt.Sprintf("row %d: %s", queued.id, queued.reason))
		}
	}
	reads.own.ids = append(reads.own.ids, composed.id)
	reads.queued, reads.caught, reads.notices = len(composed.queued), composed.notice, r.otherNotices(key)
	reads.stepJSON, reads.rowJSON, reads.caughtJSON = encodeRead(t, key, reads.step), encodeRead(t, key, reads.row), encodeRead(t, key, reads.caught)
	return reads
}

// encodeRead is value, a read of key, as JSON.
func encodeRead(t *testing.T, key string, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T of %s: %v", value, key, err)
	}
	return string(encoded)
}

// firstReads is the read every run of k3 holds its later reads to (stays), made once the run's
// opening was read and, past the first run, once its gate seed and Dispatch rows ran
// (dispatchRowsDone). It must find R's step open on its basis in the state, the stored row and the
// catch-up, at the version registered and on P's document at the version approved: stays holds
// later reads to this one whatever it holds, so a seed that closed or moved the step before it
// would otherwise pass as the step that stays.
func (r *personHandoverRig) firstReads(run basisRun) stepReads {
	t := r.t
	t.Helper()
	root, parent := run.root.Key, run.parent
	reads := r.stepReads(root, nil)
	if step := reads.step; step.Status != api.GatedStepOpenOnBasis || step.ApprovedVersion == nil || *step.ApprovedVersion != run.rootVersion ||
		step.CurrentVersion != run.rootVersion || step.Basis == nil || step.Basis.Issue != parent.Key || step.Basis.ArtifactID != parent.PrimaryArtifactID ||
		step.Basis.Version != run.basisVersion {
		t.Fatalf("at the first read the state serves %s's spec step %+v (basis %+v), want it %s at its version %d on version %d of %s's primary document",
			root, step, step.Basis, api.GatedStepOpenOnBasis, run.rootVersion, run.basisVersion, parent.Key)
	}
	if row := reads.row; !record.GateOpen(row) || !row.OnBasis() || *row.ApprovedVersion != run.rootVersion || row.Basis.Issue != parent.Key ||
		row.Basis.ArtifactID != parent.PrimaryArtifactID || row.Basis.Version != run.basisVersion {
		t.Fatalf("at the first read the daemon stores %s's spec row %+v (basis %+v), want it open at its version %d on version %d of %s's primary document",
			root, row, row.Basis, run.rootVersion, run.basisVersion, parent.Key)
	}
	if caught := catchUpGate(t, reads.caught); !caught.Open || caught.Pending || caught.Version != run.rootVersion || caught.Basis == nil ||
		caught.Basis.Issue != parent.Key || caught.Basis.Artifact != parent.PrimaryArtifactID || caught.Basis.Version != run.basisVersion {
		t.Fatalf("at the first read the catch-up of %s's architect states its spec step %+v (basis %+v), want it open at its version %d on version %d of %s's primary document",
			root, caught, caught.Basis, run.rootVersion, run.basisVersion, parent.Key)
	}
	t.Logf("%s, first read: its spec step %s at its version %d on version %d of %s's primary document in the state, the row and the catch-up; %d catch-up(s) queued before it",
		root, reads.step.Status, run.rootVersion, run.basisVersion, parent.Key, reads.queued)
	return reads
}

// stays holds key's step to want after act, as k3's negative control reads it: the state and the
// stored row name the same step open at the version registered with its basis, the root stands
// in the same phase, the whole catch-up its architect would read is the same, and its architect
// was told nothing: no notice, and no catch-up but those the proof's own reads composed.
func (r *personHandoverRig) stays(act, key string, want stepReads) {
	t := r.t
	t.Helper()
	got := r.stepReads(key, want.own)
	switch {
	case got.stepJSON != want.stepJSON:
		t.Fatalf("%s: the state serves %s's spec step %s, want it as it stood: %s", act, key, got.stepJSON, want.stepJSON)
	case got.rowJSON != want.rowJSON:
		t.Fatalf("%s: the daemon stores %s's spec row %s, want it as it stood: %s", act, key, got.rowJSON, want.rowJSON)
	case got.phase != want.phase:
		t.Fatalf("%s: %s stands at %s, want %s: no act on the ancestor moves it", act, key, got.phase, want.phase)
	case got.caughtJSON != want.caughtJSON:
		t.Fatalf("%s: the catch-up of %s's architect reads %s, want it as it stood: %s", act, key, got.caughtJSON, want.caughtJSON)
	case len(got.foreign) > 0:
		t.Fatalf("%s: the daemon queued the catch-ups %q for %s's architect since the last read, want none but the proof's own (rows %v)",
			act, got.foreign, key, want.own.ids)
	case !slices.Equal(got.notices, want.notices):
		t.Fatalf("%s: the notices queued for %s's architect = %q, want them as they stood: %q", act, key, got.notices, want.notices)
	}
	t.Logf("%s: %s stays at %s with its spec step %s; its whole catch-up as it stood (decided %q, kept %s); %d catch-up(s) queued before this read, none but the proof's own",
		act, key, got.phase, got.stepJSON, got.caught.CatchUp.Decided, encodeRead(t, key, got.caught.CatchUp.Kept), got.queued)
}

// launches counts the launches the runtime made of token.
func (r *personHandoverRig) launches(token string) int {
	count := 0
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && string(call.Spec.Claim) == token {
			count++
		}
	}
	return count
}

// rerunFromAbove starts a new run of run's root from the architect above it, as R20 of
// dispatch://INFRA-3320 has it: that architect writes backlog on the root in Dispatch as its own
// session, so the event names that session as its actor, and the daemon takes the write of the
// architect of an issue above as a person's move (decision 24 of dispatch://INFRA-4730) and stops
// the root where it stands; the architect then calls rerun_child, which opens a new run of a child
// Dispatch shows in backlog, whoever wrote it (decision 8). Where the run left P with no architect
// at work, the person hands P to Legion first and P's architect registers P's own gate on the
// person's approval, as openRun's live P does, so that an architect stands above the root and its
// new run waits on no gate above it (R19). It answers the operator's view of the root's architect
// claim once the new run launched it again, whose pending task is the new run's start task (none
// where the start names nothing), and the state the new run left.
func (r *personHandoverRig) rerunFromAbove(run *basisRun) (api.OperatorClaim, api.State) {
	t := r.t
	t.Helper()
	root, parent := run.root.Key, run.parent.Key
	if run.parentGrant == nil {
		run.parentGrant = r.admit(parent)
		r.registerApprovedGate(parent, run.parent.PrimaryArtifactID, run.basisVersion, run.parentGrant)
	}
	before := r.record(root).Generation
	session := r.architects[parent].session
	if status, answer := dispatchAnswerWith(t, r.baseURL, http.MethodPatch, "/api/v1/issues/"+root,
		map[string]any{"status": "backlog", "actor": map[string]string{"kind": "session", "id": session}}, bearing(scratchAgentToken)); status != http.StatusOK {
		t.Fatalf("the backlog of %s by the session %s of %s's architect = %d: %s", root, session, parent, status, answer)
	}
	parked := r.newestEvent(root, "issue.updated", func(e scratchEvent) bool { return e.status() == "backlog" })
	if parked.Actor != (scratchActor{Kind: "session", ID: session}) {
		t.Fatalf("the backlog event of %s = %+v, want it naming the session %s of %s's architect as its actor", root, parked, session, parent)
	}
	r.applied(parked)
	// The stop is read where the record takes it, its slot freed and its status the backlog. The
	// rig runs no agent for R's planner, which the stop's suspension of it waits on (the outbox's
	// suspend is held for the agent's turn, supervise.ErrSuspendHeld), so nothing here waits for
	// that row: the new run rerun_child opens fences the rows of the run before it.
	settled(t, r.d, "stop "+root+" on the backlog its architect above wrote", func(s api.State) bool {
		return !slices.Contains(s.Admission.Active, root) && s.Issues[root].Status == "backlog"
	})
	token := r.architectToken(root)
	launched := r.launches(string(token))
	if status, body := r.d.request(http.MethodPost, "/legion/v1/children/rerun", api.RerunChildRequest{GrantID: run.parentGrant(), Issue: root}, false); status != http.StatusOK {
		t.Fatalf("the rerun_child of %s by %s's architect = %d: %s", root, parent, status, body)
	}
	state, _ := settled(t, r.d, "start a new run of "+root, func(s api.State) bool {
		return slices.Contains(s.Admission.Active, root) && s.Issues[root].Generation == before+1
	})
	// The new run starts as a re-admitted root's does (R20): its promotion starts the architect,
	// which launches the claim the stop suspended again, in its session.
	if !testwait.Holds(2*time.Minute, 200*time.Millisecond, func() bool { return r.launches(string(token)) > launched }) {
		t.Fatalf("the architect claim of %s after rerun_child = %+v, launched %d times as before, want the new run's start to launch it; outbox rows of %s: %q",
			root, r.d.claim(token), launched, root, r.outboxDump(root))
	}
	launch := lastLaunch(t, r.rt, token)
	session, secret := registerLaunch(t, r.d, launch)
	r.architects[root] = rigArchitect{launch: launch, session: session, secret: secret}
	// A root at admitted is always handed a start task (workflow.ArchitectStartTask), which the
	// claim holds once its relaunch registered.
	var started api.OperatorClaim
	testwait.Eventually(t, "the architect of "+root+" to hold its new run's start task", func() bool {
		started = r.d.claim(token)
		return started.Pending != nil
	})
	return started, state
}

// outboxDump is each row of key the outbox holds, as its kind, its payload and its last error.
func (r *personHandoverRig) outboxDump(key string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), "select kind || ' ' || payload::text || ' ' || coalesce(last_error, '') from outbox where issue = $1 order by id", key)
	if err != nil {
		r.t.Fatalf("read the outbox rows of %s: %v", key, err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the outbox rows of %s: %v", key, err)
	}
	return held
}

// approvedVersionPattern finds each version a start task names as approved, in the words of the
// kept approval's sentence (workflow.KeptDecisionsTask: `A person approved version <N> of …`),
// subject and all, so the daemon's `No person approved version <N>` is not counted.
var approvedVersionPattern = regexp.MustCompile(`(?i)a person approved version (\d+)`)

// namesNoApprovalFrom fails unless task, key's start task in a new run, names no approval of key's
// own document at version from or later: no `A person approved version N` with N of from or more,
// and no decision on key's spec that opened its step in the line that names the decisions kept for
// the tree (workflow.KeptDecisionsTask: `<ref> (design-approved by <actor>)`, which names no
// version). It answers the versions the task names as approved.
func (r *personHandoverRig) namesNoApprovalFrom(key, task string, from int) []int {
	t := r.t
	t.Helper()
	var approved []int
	for _, match := range approvedVersionPattern.FindAllStringSubmatch(task, -1) {
		version, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("the start task of %s names approved version %q, which is no number: %v", key, match[1], err)
		}
		if version >= from {
			t.Fatalf("the start task of %s in its new run names approved version %d, want no approval of its document at version %d or later: %q",
				key, version, from, task)
		}
		approved = append(approved, version)
	}
	for _, decision := range []record.DecisionKind{record.DecisionDesignApproved, record.DecisionCarriedGateOpened} {
		if own := fmt.Sprintf("dispatch://%s/spec (%s by ", key, decision); strings.Contains(task, own) {
			t.Fatalf("the start task of %s in its new run names the kept decision %q, want no approval of its own document kept: %q", key, own, task)
		}
	}
	return approved
}

// stayingOptions shape one run of k3: P admitted as a live Legion root whose own gate names its
// document (live), R holding the person's approval of v2 of its own document while at v4
// (approvedV2), P under an issue G marking requirements of its own (grandparent), and the
// registration's gate seed held back until the proof releases it (holdSeed).
type stayingOptions struct {
	live, approvedV2, grandparent, holdSeed bool
}

// basisRun is one run's issues: R, a root that ran as a tree of its own and is set under P; P,
// labelled, marking p1 to p3 in its primary document; G above P where the run has one. grant mints
// R's architect grants and parentGrant P's, where P is admitted. rootVersion is the version of R's
// primary document registered, basisVersion the version of P's primary document its step rests
// on, and ownApproval the person's approval of R's own document the run made, if any.
type basisRun struct {
	root, parent, grand       scratchIssue
	grant, parentGrant        func() string
	rootVersion, basisVersion int
	ownApproval               *scratchReview
}

// basisReason is the reason every registration of these proofs carries.
const basisReason = "The checks serve the ancestor's requirement; both fresh reviews passed."

// ancestor creates an issue of the rig's project as a session with fields, under title, and
// writes the requirement blocks ids into its primary document as its next version, which it
// answers with the issue: a primary document that marks requirements, as requirementsMarkdown's
// does. The blocks come by an edit rather than the create's spec because Dispatch refuses a
// create whose text its near-copy gate reads as an open issue's (409 POSSIBLE_DUPLICATE), and the
// blocks every ancestor of a proof that makes many of them marks read so; an edit is no create.
func (r *personHandoverRig) ancestor(title string, fields map[string]any, ids ...string) (scratchIssue, int) {
	r.t.Helper()
	issue := r.create(asSession, title, fields)
	return issue, r.editDocument(issue.PrimaryArtifactID, insertAtEnd(requirementBlocks(ids...)))
}

// plainCall is a register_gate body of root's spec step at version with no basis: today's
// registration, which waits for a person's approval of the root's own document.
func plainCall(grant string, root scratchIssue, version int) map[string]any {
	return with(with(with(basisCall(grant, root, version, "", 0, ""), "basisArtifactId", nil), "basisVersion", nil), "reason", nil)
}

// reopenedOnBasis is openedOnBasis for a run whose root a person's approval of its own document
// started before: the step stands open on the basis at the root's version registered, the newest
// design-approved notice names the ancestor's document and version as the basis, and the root
// holds one message recording the basis opening.
func (r *personHandoverRig) reopenedOnBasis(run basisRun) {
	t := r.t
	t.Helper()
	settled(t, r.d, "open the spec step of "+run.root.Key+" on the approval of "+run.parent.Key, func(s api.State) bool {
		step := stepOf(s, run.root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.ApprovedVersion != nil && *step.ApprovedVersion == run.rootVersion &&
			step.Basis != nil && step.Basis.Issue == run.parent.Key && step.Basis.Version == run.basisVersion
	})
	approved := r.notices(run.root.Key, "design-approved")
	if len(approved) == 0 {
		t.Fatalf("%s holds no design-approved notice of its opening on %s's approval", run.root.Key, run.parent.Key)
	}
	if newest := approved[len(approved)-1]; newest.Version != run.basisVersion || newest.Actor != "user:smoke" ||
		newest.Ref != "dispatch://"+run.parent.Key+"/spec" || !strings.Contains(newest.Reason, "No person approved that version.") {
		t.Fatalf("the newest design-approved notice of %s = %+v, want version %d of dispatch://%s/spec as its basis", run.root.Key, newest, run.basisVersion, run.parent.Key)
	}
	r.oneBasisMessage(run.root.Key)
}

// hear has key's architect hear the person's approval of version of key's own primary document:
// the design-approved notice that approval's opening queued for it (workflow's applyArtifact) is
// finished as the notice executor finishes a notice whose publish the architect's session took
// (noticeAnswered), which deletes the row. The architect of a running root hears that approval
// when the person gives it; the rig's Envoy answers nothing, so the notice would stay queued,
// failing at each attempt of its backoff (record.RetryBackoff), for the rest of the run. An attempt
// that lands while a person's move to the backlog has stopped the issue finishes it undelivered
// and keeps the approval as a decision made while nobody could hear it (the outbox's
// parkDecision), and the next run's start then names that decision
// (workflow.KeptDecisionsTask): whether it did would turn on where the backoff fell.
func (r *personHandoverRig) hear(key string, version int) {
	t := r.t
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("the architect of %s to hear the person's approval of version %d", key, version), func() bool {
		finished, err := r.pool.Exec(t.Context(), `delete from outbox where kind = $1 and issue = $2 and payload->>'kind' = 'design-approved'
			and payload->>'ref' = $3 and (payload->>'version')::int = $4 and (lease_until is null or lease_until <= now())`,
			string(record.OutboxKindNotice), key, "dispatch://"+key+"/spec", version)
		if err != nil {
			t.Fatalf("finish the design-approved notice of version %d of %s: %v", version, key, err)
		}
		return finished.RowsAffected() == 1
	})
}

// openRun builds one run of k3 and opens R's step as k1 does: R runs as a root of its own; P,
// approved by the person at its latest version, is set as R's parent; R's one check serves P2; R's
// architect registers R's primary document on P's approved version, and the step opens on the
// basis (openedOnBasis). names are R's title, P's and, for a grandparent, G's: Dispatch refuses
// a title near another of its project, so each shares no word with any other.
func (r *personHandoverRig) openRun(held *outboxHold, names []string, opt stayingOptions) basisRun {
	t := r.t
	t.Helper()
	labelled := map[string]any{"labels": []string{"legion"}}
	run := basisRun{root: r.create(asSession, names[0], labelled), rootVersion: 1}
	run.grant = r.admit(run.root.Key)
	if opt.approvedV2 {
		// R's own gate records the person's approval of v2 as any root's does: its architect
		// registers v2, the person approves it, its architect hears that approval (hear) and the
		// tree starts, and two later versions close the step again, its row keeping that approval
		// as one of an older version.
		second := r.editDocument(run.root.PrimaryArtifactID, insertParagraph("The first revision names the inputs."))
		if status, refusal := r.gateCall(plainCall(run.grant(), run.root, second)); status != http.StatusOK {
			t.Fatalf("%s's registration of version %d with no basis = %d %+v, want 200", run.root.Key, second, status, refusal)
		}
		approved := r.approve(run.root.PrimaryArtifactID)
		if approved.Version != second {
			t.Fatalf("the person approved %s at version %d, want %d", run.root.Key, approved.Version, second)
		}
		settled(t, r.d, "open the spec step of "+run.root.Key+" on the person's approval of version "+fmt.Sprint(second), func(s api.State) bool {
			return stepOf(s, run.root.Key).Status == api.GatedStepApproved && s.Issues[run.root.Key].Phase == phase.Planning
		})
		r.hear(run.root.Key, second)
		run.ownApproval = &approved
		r.editDocument(run.root.PrimaryArtifactID, insertParagraph("The second revision names the outputs."))
		run.rootVersion = r.editDocument(run.root.PrimaryArtifactID, insertParagraph("The third revision names the limits."))
		r.quiet(run.root.Key)
	}
	fields := map[string]any{"labels": []string{"legion"}}
	if opt.grandparent {
		run.grand, _ = r.ancestor(names[2], map[string]any{"labels": []string{"legion"}}, "g1", "g2")
		fields["parent"] = run.grand.Key
	}
	run.parent, _ = r.ancestor(names[1], fields, "p1", "p2", "p3")
	approval := r.approve(run.parent.PrimaryArtifactID)
	run.basisVersion = approval.Version
	if opt.live {
		run.parentGrant = r.admit(run.parent.Key)
		r.registerApprovedGate(run.parent.Key, run.parent.PrimaryArtifactID, approval.Version, run.parentGrant)
	} else {
		// The person moves P into work, and G above it first, as Dispatch's parent status rule asks
		// of every parent of an issue in work (R24 of dispatch://INFRA-3320).
		if opt.grandparent {
			r.setStatus(run.grand.Key, "in_progress")
		}
		r.setStatus(run.parent.Key, "in_progress")
	}
	r.linkUnder(run.root, run.parent, opt.live)
	r.writeDocument(run.root.Key, "verification.md", checksMarkdown(verificationCheck{id: "k1", serves: []string{specBlock(run.parent.Key, "p2")}}))
	if opt.holdSeed {
		held.hold(run.root.Key, record.OutboxKindGateSeed)
	}
	r.registerOn(run)
	if opt.approvedV2 {
		r.reopenedOnBasis(run)
	} else {
		r.openedOnBasis(run.root, run.parent, run.rootVersion, run.basisVersion)
	}
	if opt.holdSeed && r.outboxCount(run.root.Key, record.OutboxKindGateSeed) == 0 {
		t.Fatalf("no gate seed of %s is held in the outbox; the run reads the step while its seed is still due", run.root.Key)
	}
	if !opt.holdSeed {
		r.dispatchRowsDone(run.root.Key)
	}
	return run
}

// k3 (dispatch://INFRA-3320, R3 and R2). A step that rests on an ancestor's approval behaves as an
// approved step of the root's own document, and nothing done to the ancestor's document moves it.
// Each run opens R's step on P's approval as k1 does, reads R's step in the state and in the
// daemon's row, R's phase, R's whole catch-up, the catch-ups and the other notices queued for R's
// architect, and holds every read after each act to its first read (stays), which must find the
// step open on P's approval at the version registered (firstReads); the person then approves P
// again and the reads are held once more.
//
//  1. P is labelled and in progress, recorded by no tree. The registration's gate seed is held back
//     (outboxHold) while the person writes a new version of P's primary document and asks for
//     changes on it; the seed then runs, and the daemon restarts on the same store.
//  2. P holds an open approval request of a newer version when the daemon starts again.
//  3. P is itself a live Legion root whose own gate names P's document: a new version closes P's
//     own gate, the person's changes request on it reaches P's architect, and the approval opens
//     it again, while R's step stays.
//  4. R holds the person's approval of v2 of its own document when its step opens at v4 on the
//     basis, and that approval's event is delivered again: its fact applied again under an id of
//     its own (applyAsTheDaemon), since the consumer answers a redelivery under the event's own id
//     as a duplicate before any handler reads it.
//  5. R's architect writes a new version of R's Verification document that adds a check serving a
//     requirement of G, the issue above P, which the person moved into work before P.
//
// Then, in fresh runs from an open step: a new version of R's primary document closes it; the
// person's changes request on it closes it and reaches R's architect as design-changes-requested;
// the person's approval of it leaves it open on that approval with no basis; a registration of it
// with no basis leaves it closed until the person approves; and a new version of it followed by a
// new run that the architect above R starts (rerunFromAbove: it writes backlog on R in Dispatch as
// its own session, the event naming that session as its actor, and then calls rerun_child, R20)
// leaves R with no gate and a start task naming no approval of R's document at the version the
// basis opened or later, neither as an approved version nor as a kept decision, once with no
// earlier approval of R and once from the fourth run's R, whose start task names the person's
// approval of v2, asks for a request of the latest version and names R1's path.
func TestBasisGateStaysOnTheRootDocument(t *testing.T) {
	held := &outboxHold{}
	r := newPersonHandoverRig(t, 16)
	r.tune = func(o *overrides) { o.holdOutbox = held.holds }
	r.boot()

	// 1. The held seed, P's new version and the changes request on it, the seed, the restart.
	one := r.openRun(held, []string{"Kestrel nest monitor", "Raptor habitat survey"}, stayingOptions{holdSeed: true})
	base := r.firstReads(one)
	newer := r.editAs(one.parent.PrimaryArtifactID, asPerson, insertParagraph("The survey adds the northern ridge."))
	r.quiet(one.parent.Key)
	r.stays(fmt.Sprintf("run 1, the person's version %d of P's primary document, the seed still due", newer), one.root.Key, base)
	r.requestChanges(one.parent.PrimaryArtifactID, "Name the ridge's access road.")
	r.quiet(one.parent.Key)
	r.stays(fmt.Sprintf("run 1, the person's changes request on version %d of P, the seed still due", newer), one.root.Key, base)
	if seeds := r.outboxCount(one.root.Key, record.OutboxKindGateSeed); seeds == 0 {
		t.Fatalf("the gate seed of %s ran before the proof released it", one.root.Key)
	}
	held.release(one.root.Key)
	testwait.Eventually(t, "the held gate seed of "+one.root.Key+" to run", func() bool {
		return r.outboxCount(one.root.Key, record.OutboxKindGateSeed) == 0
	})
	r.dispatchRowsDone(one.root.Key)
	r.stays("run 1, the gate seed run after both acts", one.root.Key, base)
	r.restart()
	r.dispatchRowsDone(one.root.Key)
	r.quiet(one.root.Key, one.parent.Key)
	r.stays("run 1, the daemon started again on the same store", one.root.Key, base)
	r.approve(one.parent.PrimaryArtifactID)
	r.quiet(one.parent.Key)
	r.stays("run 1, the person's approval of P again", one.root.Key, base)

	// 2. P's open approval request when the daemon starts again.
	two := r.openRun(held, []string{"Lantern festival lighting", "Riverside carnival logistics"}, stayingOptions{})
	base = r.firstReads(two)
	requested := r.editDocument(two.parent.PrimaryArtifactID, insertParagraph("The carnival moves upstream."))
	ask := r.requestApproval(two.parent.PrimaryArtifactID, "The carnival moves upstream.")
	r.quiet(two.parent.Key)
	r.stays(fmt.Sprintf("run 2, P's version %d and its approval request %s", requested, ask), two.root.Key, base)
	r.restart()
	r.dispatchRowsDone(two.root.Key)
	r.quiet(two.root.Key, two.parent.Key)
	if open := slices.ContainsFunc(r.asks(two.parent.Key), func(a scratchAsk) bool { return a.ID == ask && a.State == "open" }); !open {
		t.Fatalf("the approval request %s of %s is not open when the daemon starts again: %+v", ask, two.parent.Key, r.asks(two.parent.Key))
	}
	r.stays("run 2, the daemon started again while P's approval request is open", two.root.Key, base)
	r.approve(two.parent.PrimaryArtifactID)
	r.quiet(two.parent.Key)
	r.stays("run 2, the person's approval of P's newer version", two.root.Key, base)

	// 3. P a live Legion root whose own gate names P's document.
	three := r.openRun(held, []string{"Orchard irrigation timer", "Valley water allocation"}, stayingOptions{live: true})
	base = r.firstReads(three)
	closed := r.editDocument(three.parent.PrimaryArtifactID, insertParagraph("The allocation counts the wells."))
	r.quiet(three.parent.Key)
	settled(t, r.d, "close the gate of "+three.parent.Key+" at its version "+fmt.Sprint(closed), func(s api.State) bool {
		step := stepOf(s, three.parent.Key)
		return step.Status == api.GatedStepWaiting && step.CurrentVersion == closed
	})
	r.stays(fmt.Sprintf("run 3, version %d of P, which closes P's own gate", closed), three.root.Key, base)
	r.requestChanges(three.parent.PrimaryArtifactID, "Count the springs too.")
	r.quiet(three.parent.Key)
	testwait.Eventually(t, "the architect of "+three.parent.Key+" to be told the changes request on its own document", func() bool {
		return len(r.notices(three.parent.Key, "design-changes-requested")) == 1
	})
	r.stays(fmt.Sprintf("run 3, the person's changes request on version %d of P, told to P's architect", closed), three.root.Key, base)
	r.approve(three.parent.PrimaryArtifactID)
	r.quiet(three.parent.Key)
	settled(t, r.d, "open the gate of "+three.parent.Key+" on the person's approval", func(s api.State) bool {
		return stepOf(s, three.parent.Key).Status == api.GatedStepApproved
	})
	r.stays("run 3, the person's approval of P again, which opens P's own gate", three.root.Key, base)

	// 4. R holding the person's approval of v2, open at v4, and that approval delivered again.
	four := r.openRun(held, []string{"Telescope mirror polisher", "Observatory dome renovation"}, stayingOptions{approvedV2: true})
	if kept := r.gates(four.root.Key)[record.DesignStepSpec].ApprovedByPerson(); kept == nil || kept.Version != four.ownApproval.Version {
		t.Fatalf("the spec row of %s keeps the person's approval %+v, want version %d held apart", four.root.Key, kept, four.ownApproval.Version)
	}
	base = r.firstReads(four)
	event := r.newestEvent(four.root.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == four.ownApproval.Version })
	r.applyAsTheDaemon("dispatch", "late-"+event.factID(), intake.DispatchArtifact{Key: four.root.Key, ArtifactID: four.root.PrimaryArtifactID,
		Kind: intake.DispatchArtifactApproved, Version: four.ownApproval.Version, Mark: record.EventMark{Seq: event.Seq, At: four.ownApproval.CreatedAt}, Actor: smoke})
	r.stays(fmt.Sprintf("run 4, the person's approval of version %d delivered again", four.ownApproval.Version), four.root.Key, base)
	r.approve(four.parent.PrimaryArtifactID)
	r.quiet(four.parent.Key)
	r.stays("run 4, the person's approval of P again", four.root.Key, base)

	// 5. A new version of R's Verification document adding a check of G's requirement.
	five := r.openRun(held, []string{"Glacier melt sensor", "Alpine hydrology study", "Mountain climate programme"}, stayingOptions{grandparent: true})
	base = r.firstReads(five)
	r.writeDocument(five.root.Key, "verification.md", checksMarkdown(verificationCheck{id: "k1", serves: []string{specBlock(five.parent.Key, "p2")}},
		verificationCheck{id: "k2", serves: []string{specBlock(five.grand.Key, "g1")}}))
	r.quiet(five.root.Key)
	r.stays("run 5, R's Verification document adding a check of G's g1", five.root.Key, base)
	r.approve(five.parent.PrimaryArtifactID)
	r.quiet(five.parent.Key)
	r.stays("run 5, the person's approval of P again", five.root.Key, base)

	// The fresh runs. A new version of R's primary document closes the step.
	fresh := r.openRun(held, []string{"Harpsichord string gauge", "Baroque instrument workshop"}, stayingOptions{})
	version := r.editDocument(fresh.root.PrimaryArtifactID, insertParagraph("The gauge reads brass strings too."))
	r.quiet(fresh.root.Key)
	state, body := settled(t, r.d, "close the spec step of "+fresh.root.Key+" at its version "+fmt.Sprint(version), func(s api.State) bool {
		step := stepOf(s, fresh.root.Key)
		return step.Status == api.GatedStepWaiting && step.CurrentVersion == version
	})
	if step := stepOf(state, fresh.root.Key); step.Basis != nil || step.ApprovedVersion != nil {
		t.Fatalf("after a new version the spec step of %s = %+v (basis %+v), want it closed with no basis and no approval\nstate %s", fresh.root.Key, step, step.Basis, body)
	}
	if gate := r.gates(fresh.root.Key)[record.DesignStepSpec]; record.GateOpen(gate) || gate.Basis != nil || !slices.Equal(gate.RestsOn, []string{fresh.parent.Key}) {
		t.Fatalf("after a new version the spec row of %s = %+v, want it closed with no basis, still resting on %s", fresh.root.Key, gate, fresh.parent.Key)
	}
	if standing := state.Issues[fresh.root.Key].Phase; standing != phase.Planning {
		t.Fatalf("%s stands at %s after the new version, want planning: a closed step moves no phase back", fresh.root.Key, standing)
	}
	for _, kind := range []string{"design-changes-requested", "basis-ended"} {
		if told := r.notices(fresh.root.Key, kind); len(told) != 0 {
			t.Fatalf("a new version of %s's document queued %s notices %+v, want none", fresh.root.Key, kind, told)
		}
	}
	t.Logf("version %d of %s's primary document closed the step a basis opened", version, fresh.root.Key)

	// The person's changes request on R's primary document closes it, told as design-changes-requested.
	changes := r.openRun(held, []string{"Clocktower chime scheduler", "Municipal heritage restoration"}, stayingOptions{})
	r.requestChanges(changes.root.PrimaryArtifactID, "Name the quiet hours.")
	r.quiet(changes.root.Key)
	settled(t, r.d, "close the spec step of "+changes.root.Key+" on the person's changes request", func(s api.State) bool {
		step := stepOf(s, changes.root.Key)
		return step.Status == api.GatedStepChangesRequested && step.ChangesReason == "Name the quiet hours." && step.Basis == nil
	})
	testwait.Eventually(t, "the daemon to tell the architect of "+changes.root.Key+" the changes request", func() bool {
		return len(r.notices(changes.root.Key, "design-changes-requested")) == 1
	})
	if told := r.notices(changes.root.Key, "design-changes-requested")[0]; told.Reason != "Name the quiet hours." || told.Version != changes.rootVersion ||
		told.Ref != "dispatch://"+changes.root.Key+"/spec" {
		t.Fatalf("the design-changes-requested notice of %s = %+v, want the person's reason on version %d of its own document", changes.root.Key, told, changes.rootVersion)
	}
	t.Logf("the person's changes request on %s's own document closed the step and reached its architect", changes.root.Key)

	// The person's approval of R's primary document leaves it open, on that approval.
	own := r.openRun(held, []string{"Beehive humidity logger", "Apiary pollination network"}, stayingOptions{})
	ownReview := r.approve(own.root.PrimaryArtifactID)
	r.quiet(own.root.Key)
	state, body = settled(t, r.d, "leave the spec step of "+own.root.Key+" open on the person's approval", func(s api.State) bool {
		return stepOf(s, own.root.Key).Status == api.GatedStepApproved
	})
	if step := stepOf(state, own.root.Key); step.Basis != nil || step.ApprovedVersion == nil || *step.ApprovedVersion != own.rootVersion {
		t.Fatalf("after the person's approval the spec step of %s = %+v (basis %+v), want it approved at version %d with no basis\nstate %s",
			own.root.Key, step, step.Basis, own.rootVersion, body)
	}
	if gate := r.gates(own.root.Key)[record.DesignStepSpec]; !record.GateOpen(gate) || gate.Basis != nil ||
		gate.ApprovedByPerson() == nil || gate.ApprovedByPerson().Version != ownReview.Version || gate.ApprovedMark.At.IsZero() {
		t.Fatalf("after the person's approval the spec row of %s = %+v, want it open on the person's approval of version %d", own.root.Key, gate, ownReview.Version)
	}
	if approved := r.notices(own.root.Key, "design-approved"); len(approved) != 1 {
		t.Fatalf("the design-approved notices of %s = %+v, want the opening's alone: the step was already open", own.root.Key, approved)
	}
	t.Logf("the person's approval of %s's own document left its step open, now on that approval", own.root.Key)

	// A registration of R's primary document with no basis leaves it closed until the person approves.
	plain := r.openRun(held, []string{"Lighthouse lens cleaner", "Harbour safety upgrade"}, stayingOptions{})
	if status, refusal := r.gateCall(plainCall(plain.grant(), plain.root, plain.rootVersion)); status != http.StatusOK {
		t.Fatalf("%s's registration with no basis = %d %+v, want 200", plain.root.Key, status, refusal)
	}
	settled(t, r.d, "close the spec step of "+plain.root.Key+" on its registration with no basis", func(s api.State) bool {
		step := stepOf(s, plain.root.Key)
		return step.Status == api.GatedStepWaiting && step.Basis == nil
	})
	if gate := r.gates(plain.root.Key)[record.DesignStepSpec]; record.GateOpen(gate) || gate.Basis != nil {
		t.Fatalf("after the registration with no basis the spec row of %s = %+v, want it closed with no basis", plain.root.Key, gate)
	}
	r.approve(plain.root.PrimaryArtifactID)
	r.quiet(plain.root.Key)
	settled(t, r.d, "open the spec step of "+plain.root.Key+" on the person's approval", func(s api.State) bool {
		return stepOf(s, plain.root.Key).Status == api.GatedStepApproved
	})
	testwait.Eventually(t, "the daemon to tell the architect of "+plain.root.Key+" the person's approval", func() bool {
		return len(r.notices(plain.root.Key, "design-approved")) == 2
	})
	if approved := r.notices(plain.root.Key, "design-approved")[1]; approved.Ref != "dispatch://"+plain.root.Key+"/spec" || approved.Actor != "user:smoke" {
		t.Fatalf("the second design-approved notice of %s = %+v, want the person's approval of its own document", plain.root.Key, approved)
	}
	t.Logf("%s registered with no basis stayed closed until the person approved its own document", plain.root.Key)

	// A new version of R's document, then a new run from the architect above it: no earlier
	// approval of R.
	rerun := r.openRun(held, []string{"Windmill blade inspector", "Coastal energy cooperative"}, stayingOptions{})
	r.editDocument(rerun.root.PrimaryArtifactID, insertParagraph("The inspector climbs at low wind."))
	r.quiet(rerun.root.Key)
	started, state := r.rerunFromAbove(&rerun)
	if gates := r.gates(rerun.root.Key); len(gates) != 0 || stepOf(state, rerun.root.Key).Status != api.GatedStepUnregistered {
		t.Fatalf("after its new run %s holds the gates %+v and the state serves %+v, want none: a new generation keeps no gate", rerun.root.Key, gates,
			stepOf(state, rerun.root.Key))
	}
	var task string
	if started.Pending != nil {
		task = started.Pending.Task
	}
	if strings.Contains(task, "approved version") {
		t.Fatalf("the start task of %s in its new run = %q, want no approval of its document named", rerun.root.Key, task)
	}
	r.namesNoApprovalFrom(rerun.root.Key, task, rerun.rootVersion)
	t.Logf("%s in a new run from its architect above with no gate; its start task %+v names no approval, and no kept decision of its own document",
		rerun.root.Key, started.Pending)

	// The fourth run's R: its start task names the person's approval of v2 and R1's path.
	latest := r.editDocument(four.root.PrimaryArtifactID, insertParagraph("The polisher stops at the rim."))
	r.quiet(four.root.Key)
	started, state = r.rerunFromAbove(&four)
	if gates := r.gates(four.root.Key); len(gates) != 0 || stepOf(state, four.root.Key).Status != api.GatedStepUnregistered {
		t.Fatalf("after its new run %s holds the gates %+v, want none", four.root.Key, gates)
	}
	kept := fmt.Sprintf("A person approved version %d of the primary document, and the primary document is now at version %d. Request approval of version %d,",
		four.ownApproval.Version, latest, latest)
	path := fmt.Sprintf("The other way is to write %s as a view on an approved ancestor and register the step on that ancestor's approval", four.root.Key)
	if started.Pending == nil || !strings.Contains(started.Pending.Task, kept) || !strings.Contains(started.Pending.Task, path) {
		t.Fatalf("the start task of %s in its new run = %+v, want it naming %q and %q", four.root.Key, started.Pending, kept, path)
	}
	approved := r.namesNoApprovalFrom(four.root.Key, started.Pending.Task, four.rootVersion)
	t.Logf("%s in a new run from its architect above with no gate; its start task names approved versions %v, each below its version %d registered on the basis (latest %d), and no kept decision of its own document: %q",
		four.root.Key, approved, four.rootVersion, latest, started.Pending.Task)
}

// applyAsTheDaemon applies fact under source and id through an engine of the daemon's own
// configuration (engineConfig), as the record writes the rig makes of an issue's phase are applied
// in close_scratch_dispatch_test.go (mergeClosesTheIssue): the one way the rig reaches a fact no
// act on Dispatch or the daemon's API makes here, such as a phase completion of a role it runs no
// agent for. It answers what the engine answered.
func (r *personHandoverRig) applyAsTheDaemon(source, id string, fact intake.Fact) intake.Result {
	r.t.Helper()
	engine := workflow.New(record.NewStore(), engineConfig(r.cfg, "", nil), quietLogger())
	result, err := intake.ApplyFact(r.t.Context(), r.pool, source, id, fact, engine)
	if err != nil {
		r.t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// moveRecord writes key's record in phase to, as close_scratch_dispatch_test.go writes a record in
// merging, once every write the daemon queued on key has run: a phase the rig runs no agent to
// reach. Its task is moved with it (recordTaskAt).
func (r *personHandoverRig) moveRecord(key string, to phase.Phase) {
	t := r.t
	t.Helper()
	r.dispatchRowsDone(key)
	r.quiet(key)
	if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		issue, err := records.Issue(ctx, tx, key)
		if err != nil || issue == nil {
			return fmt.Errorf("read the record of %s: %v", key, err)
		}
		// A parent the admission pass moved to awaiting_children for its open children (R9 of
		// dispatch://INFRA-3320) carries the wait that phase holds, and the move out of it drops it.
		issue.Phase = to
		if to != phase.AwaitingChildren {
			issue.ChildrenWait = nil
		}
		if err := records.PutIssue(ctx, tx, *issue); err != nil {
			return err
		}
		return recordTaskAt(ctx, tx, records, *issue)
	}); err != nil {
		t.Fatalf("record %s in %s: %v", key, to, err)
	}
}

// recordTaskAt moves the task of issue's run to the place that stands in issue.Phase, as the
// workflow's own move does (workflow's moveTask), for a record a test writes in a phase by hand:
// issues.phase and the task's place are two records of where the issue stands, and every later
// write of the issue (workflow.Engine.WriteIssue) refuses a record in which they disagree. The task
// runs at the step that runs as the phase, at its retro or at its merge, and waits at no act at
// admitted. A run with no plan rows has only its phase. It and seedTransact are the two seams of
// the approved-version-news seed that its negative control gives an older base a version of
// (scripts/e2e/approved-version-news.sh, control_compat), so they live here, in a file the control
// does not copy.
func recordTaskAt(ctx context.Context, tx pgx.Tx, records record.Store, issue record.Issue) error {
	stored, err := records.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return err
	}
	task, err := stored.Task()
	if err != nil {
		return err
	}
	place, state := record.TaskPlace{}, record.TaskRunning
	switch issue.Phase {
	case phase.Admitted:
		state = record.TaskWaiting
	case phase.Retro:
		place.Kind = record.PlaceRetro
	case phase.Merging:
		place.Kind = record.PlaceMerge
	default:
		roles, steps := planwords.BuiltinRoles(), stored.Steps[task.Task]
		at := slices.IndexFunc(steps, func(step record.PlanStep) bool { return roles[step.Role].Phase == issue.Phase })
		if at < 0 {
			return fmt.Errorf("the task %s of %s runs no step as %s", task.Task, issue.Key, issue.Phase)
		}
		place = record.TaskPlace{Kind: record.PlaceStep, Position: steps[at].Position}
	}
	return records.MoveTask(ctx, tx, issue.Key, issue.Generation, task.Task, place, state)
}

// seedTransact runs write in one transaction on pool whose commit runs the hooks its writes
// register (store.Transact), as a fact the daemon applies writes them: the approved-version-news
// seed takes its transaction here (recordTaskAt says why it is a seam).
func seedTransact(ctx context.Context, pool *store.Pool, write func(ctx context.Context, tx pgx.Tx) error) error {
	return store.Transact(ctx, pool, write)
}

// requestApprovalWithVerification asks the person, as an architect's session does with
// `dispatch request-approval --include-verification`, to approve the latest version of
// artifactID, an issue's spec, together with that issue's Verification document, and answers the
// ask Dispatch opened for the request.
func (r *personHandoverRig) requestApprovalWithVerification(artifactID, summary string) string {
	r.t.Helper()
	return r.approvalRequest(artifactID, map[string]any{"summary": summary, "include_verification": true})
}

// resolveAsk closes ask with no answer as identity, as `dispatch ask-resolve` does.
func (r *personHandoverRig) resolveAsk(ask string, identity scratchIdentity) {
	r.t.Helper()
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/asks/"+ask+"/resolve", map[string]any{"kind": "resolved", "reason": "The request is withdrawn."}, nil, identity)
}

// pendingPlace is where a run of k11 places P: a live Legion root of its own tree, a member issue
// of another live tree, labelled and in progress in no tree the daemon records, or a labelled
// Legion root the person handed to Legion that waits in the line for its first slot.
type pendingPlace int

const (
	ancestorLive pendingPlace = iota
	ancestorMember
	ancestorUnrecorded
	ancestorWaiting
)

// recorded says whether the daemon records P in a live tree, where a notice keyed by P is queued
// for the architect that owns it.
func (p pendingPlace) recorded() bool { return p != ancestorUnrecorded }

// pendingOptions shape one run of k11 beyond P's place: P at v3 with no v4 (noV4); v4 written with
// no approval request opened on it (noRequest); R's step opened on P's v3 before, closed again by
// v2 of R's document, with R's record in implementing (openedBefore); a member of R's tree, titled
// member, started in its first phase by that earlier opening; and a sibling root under P, titled
// sibling, that registers on P's v4 beside R.
type pendingOptions struct {
	place           pendingPlace
	noV4, noRequest bool
	openedBefore    bool
	member, sibling string
}

// pendingRun is one run of k11: R and P as a basisRun names them, the version of P's document the
// registration names (basisVersion) and P's approval request of it (ask); owner, the root of the
// live tree P is a member of; member, the member of R's tree in a phase; sibling, a second root
// under P.
type pendingRun struct {
	basisRun
	ask     string
	place   pendingPlace
	owner   scratchIssue
	member  scratchIssue
	sibling *basisRun
}

// pendingWorld builds one run of k11 up to R's registration: R runs as a root of its own; P marks
// p1 to p3, its primary document at v3, which the person approves, and stands where opt.place puts
// it; P is set as R's parent while its latest version is the approved one, so the re-parent read
// names R1's path; then v4 adds the requirement block p4, the person's approval request of v4 is
// opened, and R's one check serves p4 (p2 for noV4). names are R's title, P's and, for a member P,
// the owning root's.
func (r *personHandoverRig) pendingWorld(names []string, opt pendingOptions) pendingRun {
	t := r.t
	t.Helper()
	labelled := map[string]any{"labels": []string{"legion"}}
	run := pendingRun{place: opt.place}
	run.root, run.rootVersion = r.create(asSession, names[0], labelled), 1
	run.grant = r.admit(run.root.Key)
	// R's architect registers R's own design, which waits on the person: R holds a step of its own
	// from here on, as a root whose architect is at work does, so once it stands under P it still
	// waits for that step (R19 of dispatch://INFRA-3320: an issue under a parent waits for each gate
	// step it holds itself) whatever the gates above it do, until a registration opens it.
	r.registerWaitingGate(run.root.Key, run.root.PrimaryArtifactID, run.rootVersion, run.grant)
	fields := map[string]any{"labels": []string{"legion"}}
	if opt.place == ancestorMember {
		run.owner = r.create(asSession, names[2], labelled)
		ownerGrant := r.admit(run.owner.Key)
		// The owner's architect registers the owner's own gate on the person's approval: every
		// issue under the owner in Legion's work, R among them, waits for that gate (R19 of
		// dispatch://INFRA-3320).
		ownerApproval := r.approve(run.owner.PrimaryArtifactID)
		r.registerApprovedGate(run.owner.Key, run.owner.PrimaryArtifactID, ownerApproval.Version, ownerGrant)
		fields["parent"] = run.owner.Key
	}
	run.parent, _ = r.ancestor(names[1], fields, "p1", "p2", "p3")
	r.editDocument(run.parent.PrimaryArtifactID, insertParagraph("The third version names the dates."))
	approved := r.approve(run.parent.PrimaryArtifactID)
	if approved.Version != 3 {
		t.Fatalf("the person approved %s at version %d, want 3", run.parent.Key, approved.Version)
	}
	run.basisVersion = approved.Version
	switch opt.place {
	case ancestorLive:
		run.parentGrant = r.admit(run.parent.Key)
		// P's architect registers P's own gate on the person's approval, as openRun's live P
		// does: every issue under P in Legion's work waits for P's gate (R19 of
		// dispatch://INFRA-3320), so R's step opening on P's approval is what then starts R.
		r.registerApprovedGate(run.parent.Key, run.parent.PrimaryArtifactID, approved.Version, run.parentGrant)
	case ancestorMember:
		r.setStatus(run.parent.Key, "todo")
		testwait.Eventually(t, "the daemon to record "+run.parent.Key+" as a member of "+run.owner.Key+"'s tree", func() bool {
			recorded := readIssue(t, r.pool, run.parent.Key)
			return recorded != nil && recorded.Tree == run.owner.Key
		})
	case ancestorUnrecorded:
		r.setStatus(run.parent.Key, "in_progress")
	case ancestorWaiting:
		// The person hands P to Legion while the project stands at its cap, which the run's own R
		// filled (the caller lowers the cap to it), so P waits in the line for its first slot and no
		// architect of P runs (R19 of dispatch://INFRA-3320).
		r.setStatus(run.parent.Key, "todo")
		r.waiting(run.parent.Key)
		r.quiet(run.parent.Key)
	}
	r.linkUnder(run.root, run.parent, opt.place == ancestorLive)
	if opt.sibling != "" {
		sibling := basisRun{root: r.create(asSession, opt.sibling, labelled), parent: run.parent, rootVersion: 1}
		sibling.grant = r.admit(sibling.root.Key)
		r.registerWaitingGate(sibling.root.Key, sibling.root.PrimaryArtifactID, sibling.rootVersion, sibling.grant)
		r.linkUnder(sibling.root, run.parent, opt.place == ancestorLive)
		run.sibling = &sibling
	}
	if opt.openedBefore || opt.member != "" {
		r.writeDocument(run.root.Key, "verification.md", checksMarkdown(verificationCheck{id: "k1", serves: []string{specBlock(run.parent.Key, "p2")}}))
		r.registerBasis(run.grant(), run.root, 1, run.parent, run.basisVersion, basisReason)
		r.openedOnBasis(run.root, run.parent, 1, run.basisVersion)
		r.dispatchRowsDone(run.root.Key)
		if opt.member != "" {
			run.member = r.create(asSession, opt.member, map[string]any{"labels": []string{"legion"}, "parent": run.root.Key})
			r.setStatus(run.member.Key, "todo")
			// The member is an open Dispatch child of the root, whose own phases have started, so
			// the admission pass moves the root to awaiting_children, where it holds no slot, for
			// as long as the member is open (dispatch://INFRA-3320 R9, R11).
			settled(t, r.d, "start the first phase of "+run.member.Key+", a member of "+run.root.Key+"'s tree, and move "+run.root.Key+" to awaiting_children", func(s api.State) bool {
				return s.Issues[run.member.Key].Phase == phase.Planning && s.Issues[run.member.Key].Tree == run.root.Key &&
					s.Issues[run.root.Key].Phase == phase.AwaitingChildren
			})
		}
		if opt.openedBefore {
			r.moveRecord(run.root.Key, phase.Implementing)
		}
		run.rootVersion = r.editDocument(run.root.PrimaryArtifactID, insertParagraph("The second version narrows the work."))
		r.quiet(run.root.Key)
		settled(t, r.d, "close the spec step of "+run.root.Key+" at its version "+fmt.Sprint(run.rootVersion), func(s api.State) bool {
			step := stepOf(s, run.root.Key)
			return step.Status == api.GatedStepWaiting && step.CurrentVersion == run.rootVersion
		})
	}
	served := "p2"
	if !opt.noV4 {
		run.basisVersion = r.editDocument(run.parent.PrimaryArtifactID, insertAtEnd(requirementBlock("p4")))
		if !opt.noRequest {
			run.ask = r.requestApproval(run.parent.PrimaryArtifactID, fmt.Sprintf("Version %d adds p4, which %s delivers.", run.basisVersion, run.root.Key))
		}
		r.quiet(run.parent.Key)
		served = "p4"
	}
	r.writeDocument(run.root.Key, "verification.md", checksMarkdown(verificationCheck{id: "k1", serves: []string{specBlock(run.parent.Key, served)}}))
	if run.sibling != nil {
		run.sibling.basisVersion = run.basisVersion
		r.writeDocument(run.sibling.root.Key, "verification.md", checksMarkdown(verificationCheck{id: "k1", serves: []string{specBlock(run.parent.Key, served)}}))
	}
	return run
}

// registerWaitingGate is key's architect registering key's own spec step at version with no basis
// while no person has approved that version: the step is registered and waits on the person.
func (r *personHandoverRig) registerWaitingGate(key, artifactID string, version int, grant func() string) {
	t := r.t
	t.Helper()
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: key, ArtifactID: artifactID, Version: version, Step: record.DesignStepSpec,
	}, false); status != http.StatusOK {
		t.Fatalf("the design-gate registration of %s at version %d = %d; body %s", key, version, status, answer)
	}
	settled(t, r.d, "register the spec step of "+key+" waiting on the person", func(s api.State) bool {
		step := stepOf(s, key)
		return step.Status == api.GatedStepWaiting && step.CurrentVersion == version
	})
}

// registerOn is run's architect's register_gate of its root's document on the version of P's
// document the run names, with the proofs' reason, which must answer 200.
func (r *personHandoverRig) registerOn(run basisRun) {
	r.t.Helper()
	r.registerBasis(run.grant(), run.root, run.rootVersion, run.parent, run.basisVersion, basisReason)
}

// pendingRegistered is what k11's negative control reads after a pending registration of run's
// root: its step is closed, the state and the row read it as pending on P's document, the version
// and the request ask, its catch-up names that request, it keeps its phase (standing) and its
// slot, or holds none where it waits on its open children in awaiting_children, and its
// architect is told nothing of a basis; P's architect gets a resting-root notice
// naming the root wherever the daemon records P in a live tree (told), and no resting-root notice
// at all elsewhere. The notices are read as they were written (writtenNotices), whatever the
// outbox did with their rows since, so a notice wrongly queued and then finished undelivered is
// read as well.
func (r *personHandoverRig) pendingRegistered(run basisRun, ask string, told bool, standing phase.Phase) {
	t := r.t
	t.Helper()
	state, body := settled(t, r.d, run.root.Key+" to wait on "+ask, func(s api.State) bool {
		return stepOf(s, run.root.Key).Status == api.GatedStepPendingOnBasis
	})
	step := stepOf(state, run.root.Key)
	if step.ArtifactID != run.root.PrimaryArtifactID || step.CurrentVersion != run.rootVersion || step.Basis == nil || step.Basis.Issue != run.parent.Key ||
		step.Basis.ArtifactID != run.parent.PrimaryArtifactID || step.Basis.Version != run.basisVersion || step.Basis.AskID != ask || step.Basis.Approver != "" {
		t.Fatalf("the spec step of %s = %+v (basis %+v), want it pending at its version %d on version %d of %s's document and request %s\nstate %s",
			run.root.Key, step, step.Basis, run.rootVersion, run.basisVersion, run.parent.Key, ask, body)
	}
	if gate := r.gates(run.root.Key)[record.DesignStepSpec]; !gate.PendingOnBasis() || record.GateOpen(gate) || gate.Basis.Pending.Ask != ask ||
		gate.Basis.Version != run.basisVersion {
		t.Fatalf("the spec row of %s = %+v, want it closed, pending on %s", run.root.Key, gate, ask)
	}
	// A root in awaiting_children holds no slot while it waits on its open children
	// (dispatch://INFRA-3320 R11); every other phase here keeps the slot it was started with.
	issue := state.Issues[run.root.Key]
	slotted := issue.Slot != nil && slices.Contains(state.Admission.Active, run.root.Key)
	if issue.Phase != standing || slotted != (standing != phase.AwaitingChildren) {
		t.Fatalf("%s stands at %s with slot %+v after the pending registration, want it at %s, in its slot unless it waits on its children\nstate %s",
			run.root.Key, issue.Phase, issue.Slot, standing, body)
	}
	if ended := writtenNotices(t, r.pool, run.root.Key, "basis-ended"); len(ended) != 0 {
		t.Fatalf("the pending registration of %s wrote basis-ended notices %+v, want none", run.root.Key, ended)
	}
	if caught := r.catchUpNow(run.root.Key); !caught.Pending || caught.Open || caught.Basis == nil || caught.Basis.Ask != ask || caught.Basis.Version != run.basisVersion {
		t.Fatalf("the catch-up of %s's architect states its spec step %+v (basis %+v), want it pending on %s", run.root.Key, caught, caught.Basis, ask)
	}
	registered, tells := restingRegistration(run, ask)
	resting := writtenNotices(t, r.pool, run.parent.Key, "resting-root")
	if got := slices.ContainsFunc(resting, tells); got != told || (!told && len(resting) != 0) {
		t.Fatalf("the resting-root notices written for %s = %+v; a notice naming %q was written = %t, want %t, and none at all where the daemon records %s in no live tree",
			run.parent.Key, resting, registered, got, told, run.parent.Key)
	}
	t.Logf("%s waits on %s's request %s at version %d, at %s in its slot; resting-root written for %s: %t", run.root.Key, run.parent.Key, ask,
		run.basisVersion, standing, run.parent.Key, told)
}

// restingRegistration is the sentence of the resting-root notice that tells P's architect of the
// pending registration of run's root on P's request ask (workflow's tellRegisteredBasis), and
// whether a notice is that one.
func restingRegistration(run basisRun, ask string) (string, func(record.Notice) bool) {
	registered := fmt.Sprintf("%s registered its spec step on version %d of the primary document of %s", run.root.Key, run.basisVersion, run.parent.Key)
	return registered, func(n record.Notice) bool {
		return strings.Contains(n.Reason, registered) && strings.Contains(n.Reason, ask) && n.Version == run.basisVersion
	}
}

// pendingEnded is what k11's negative control reads after an act that ends run's root's wait
// without opening its step: the step is closed and registered on the root's own document with no
// basis in the state, the row and the catch-up, the root stands where it stood, its architect gets
// one basis-ended notice that names act, P's document, the version and the request, and nothing
// opened; P's architect gets one resting-root notice naming the act, beside the one of the
// registration (restingRegistration), wherever the daemon records P in a live tree (told), and no
// resting-root notice at all elsewhere. The notices of both architects are read as they were
// written (writtenNotices), whatever the outbox did with their rows since: an act that takes P out
// of a live tree (P set done, its label taken off) lets the outbox finish every notice keyed by P
// undelivered as it next runs it, as it does for an architect that has ended, so the end's row may
// be gone while the registration's still waits out its publish backoff.
func (r *personHandoverRig) pendingEnded(run basisRun, ask, act string, told bool, standing phase.Phase) {
	t := r.t
	t.Helper()
	state, body := settled(t, r.d, "end the wait of "+run.root.Key+" on "+ask, func(s api.State) bool {
		step := stepOf(s, run.root.Key)
		return step.Status != api.GatedStepPendingOnBasis && step.Status != api.GatedStepUnregistered
	})
	step := stepOf(state, run.root.Key)
	if step.Basis != nil || step.ArtifactID != run.root.PrimaryArtifactID || step.Status == api.GatedStepApproved || step.Status == api.GatedStepOpenOnBasis {
		t.Fatalf("the spec step of %s = %+v (basis %+v), want it closed and registered on its own document with no basis\nstate %s", run.root.Key, step, step.Basis, body)
	}
	if gate := r.gates(run.root.Key)[record.DesignStepSpec]; gate.Basis != nil || record.GateOpen(gate) || gate.ArtifactID != run.root.PrimaryArtifactID {
		t.Fatalf("the spec row of %s = %+v, want it closed on its own document with no basis", run.root.Key, gate)
	}
	if issue := state.Issues[run.root.Key]; issue.Phase != standing || issue.Slot == nil {
		t.Fatalf("%s stands at %s with slot %+v after its wait ended, want it at %s in its slot", run.root.Key, issue.Phase, issue.Slot, standing)
	}
	ended := writtenNotices(t, r.pool, run.root.Key, "basis-ended")
	waited := fmt.Sprintf("no longer waits on approval request %s of version %d", ask, run.basisVersion)
	if len(ended) != 1 || ended[0].Ref != "dispatch://"+run.parent.Key+"/spec" || ended[0].Version != run.basisVersion ||
		!strings.Contains(ended[0].Reason, act) || !strings.Contains(ended[0].Reason, waited) || !strings.Contains(ended[0].Reason, "The step stays closed") {
		t.Fatalf("the basis-ended notices of %s = %+v, want one naming %q and that the step %s", run.root.Key, ended, act, waited)
	}
	if opened := writtenNotices(t, r.pool, run.root.Key, "design-approved"); len(opened) != 0 {
		t.Fatalf("%s was written design-approved notices %+v, want none: nothing opened its step", run.root.Key, opened)
	}
	resting := writtenNotices(t, r.pool, run.parent.Key, "resting-root")
	named := 0
	for _, n := range resting {
		if strings.Contains(n.Reason, act) && strings.Contains(n.Reason, run.root.Key) && strings.Contains(n.Reason, waited) {
			named++
		}
	}
	_, tells := restingRegistration(run, ask)
	registered := slices.ContainsFunc(resting, tells)
	if told && (named != 1 || !registered) || !told && len(resting) != 0 {
		t.Fatalf("the resting-root notices written for %s = %+v; %d name the end %q, and the registration's is among them = %t; want one and true where the daemon records %s in a live tree (told = %t), and none at all elsewhere",
			run.parent.Key, resting, named, act, registered, run.parent.Key, told)
	}
	if caught := r.catchUpNow(run.root.Key); caught.Pending || caught.Open || caught.Basis != nil || caught.Artifact != run.root.PrimaryArtifactID {
		t.Fatalf("the catch-up of %s's architect states its spec step %+v (basis %+v), want it closed on its own document with no basis", run.root.Key, caught, caught.Basis)
	}
	t.Logf("the wait of %s ended, its step %s; basis-ended: %s", run.root.Key, step.Status, ended[0].Reason)
}

// reopenedOnPending is what the opening of a step that opened once before in the generation reads
// when the person approves the version it waited on: open on that basis at the root's version
// registered, the root where it stood, the newest design-approved notice naming P's version as the
// basis, and a second message on the root recording the basis opening.
func (r *personHandoverRig) reopenedOnPending(run basisRun, standing phase.Phase) {
	t := r.t
	t.Helper()
	state, body := settled(t, r.d, "open the spec step of "+run.root.Key+" again on "+run.parent.Key+"'s version "+fmt.Sprint(run.basisVersion), func(s api.State) bool {
		step := stepOf(s, run.root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.Basis != nil && step.Basis.Version == run.basisVersion
	})
	if step := stepOf(state, run.root.Key); step.ApprovedVersion == nil || *step.ApprovedVersion != run.rootVersion || step.Basis.Approver != "user:smoke" {
		t.Fatalf("the spec step of %s = %+v (basis %+v), want it open at its version %d on the person's approval\nstate %s", run.root.Key, step, step.Basis, run.rootVersion, body)
	}
	if issue := state.Issues[run.root.Key]; issue.Phase != standing {
		t.Fatalf("%s stands at %s after the opening, want %s", run.root.Key, issue.Phase, standing)
	}
	approved := r.notices(run.root.Key, "design-approved")
	if newest := approved[len(approved)-1]; len(approved) != 2 || newest.Version != run.basisVersion || newest.Ref != "dispatch://"+run.parent.Key+"/spec" ||
		!strings.Contains(newest.Reason, fmt.Sprintf("registered version %d of its own", run.rootVersion)) {
		t.Fatalf("the design-approved notices of %s = %+v, want a second naming version %d of %s's document as the basis", run.root.Key, approved, run.basisVersion, run.parent.Key)
	}
	testwait.Eventually(t, "the daemon to post on "+run.root.Key+" the second basis opening", func() bool { return len(r.basisMessages(run.root.Key)) == 2 })
	if messages := r.basisMessages(run.root.Key); !strings.Contains(messages[1], fmt.Sprintf("- A person approved version %d of this document.", run.basisVersion)) {
		t.Fatalf("the second basis message on %s does not name version %d:\n%s", run.root.Key, run.basisVersion, messages[1])
	}
}

// basisChangesTold is what a person's changes request on the version run's root's step stands
// open on reads (R3's one act passed on): the step stays open on that basis, the root's architect
// gets one basis-changes-requested notice naming P's document, the version, the person and reason,
// and so does the architect that owns P wherever the daemon records P in a live tree (told),
// naming the root. The notices keyed by P are read as they were written (writtenNotices), whatever
// the outbox did with their rows since.
func (r *personHandoverRig) basisChangesTold(run basisRun, reason string, told bool) {
	t := r.t
	t.Helper()
	testwait.Eventually(t, "the daemon to tell the architect of "+run.root.Key+" the changes request on its basis", func() bool {
		return len(r.notices(run.root.Key, "basis-changes-requested")) > 0
	})
	asked := fmt.Sprintf("user:smoke requested changes on version %d of the primary document of %s (dispatch://%s/spec)", run.basisVersion, run.parent.Key, run.parent.Key)
	quoted := fmt.Sprintf("The person's reason: %q", reason)
	notices := r.notices(run.root.Key, "basis-changes-requested")
	if len(notices) != 1 || notices[0].Ref != "dispatch://"+run.parent.Key+"/spec" || notices[0].Version != run.basisVersion || notices[0].Actor != "user:smoke" ||
		!strings.Contains(notices[0].Reason, asked) || !strings.Contains(notices[0].Reason, "The step stays open.") || !strings.Contains(notices[0].Reason, quoted) {
		t.Fatalf("the basis-changes-requested notices of %s = %+v, want one naming %q and %q", run.root.Key, notices, asked, quoted)
	}
	owner := writtenNotices(t, r.pool, run.parent.Key, "basis-changes-requested")
	if got := slices.ContainsFunc(owner, func(n record.Notice) bool {
		return strings.Contains(n.Reason, asked) && strings.Contains(n.Reason, "The step of "+run.root.Key+" stays open.") && strings.Contains(n.Reason, quoted)
	}); got != told {
		t.Fatalf("the basis-changes-requested notices written for %s = %+v; one naming %s was written = %t, want %t", run.parent.Key, owner, run.root.Key, got, told)
	}
	settled(t, r.d, "keep the spec step of "+run.root.Key+" open on its basis", func(s api.State) bool {
		step := stepOf(s, run.root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.Basis != nil && step.Basis.Version == run.basisVersion
	})
	t.Logf("the changes request on version %d of %s was passed on to %s's architect and, told to its owner: %t; the step stays open", run.basisVersion,
		run.parent.Key, run.root.Key, told)
}

// k11 (dispatch://INFRA-3320, R1, R2, R3 and R4). A root can wait on its ancestor's new version as
// the daemon's own state, and the person's approval of that version alone opens its step. Each run
// starts from P approved at v3, v4 of P's primary document adding the requirement block p4, the
// person's approval request of v4 open, and R, a root that ran, under P with its one check serving
// p4 (pendingWorld); R registers with P's v4 as a pending basis, and the proof reads R's record,
// the state, R's phase and slot, R's catch-up and the notices for R's and P's architects
// (pendingRegistered). The cap is 80, so that every run's roots hold slots of their own.
//
//   - The person approves v4: R's step opens as a registration on that approval would, with the
//     design-approved notice naming P's v4, and so does the step of S, a sibling root registered
//     on the same request; P's record reads treeRuns true.
//   - Each other act ends R's wait (pendingEnded): the person's changes request on v4; v5; a
//     session's close of the request; the person's close of it with no review; P set done; a new
//     version of R's own document; P losing legion; P in Dispatch's retro, after the daemon refused
//     P's sign-off, which the pending row holds (so the daemon never records P in retro while R
//     waits on it); the daemon's move of P into merging; and a person's changes request on R's own
//     document, told as design-changes-requested beside basis-ended.
//   - The person's approval of R's own document opens the step on that approval with no basis,
//     and sends both design-approved and basis-ended.
//   - The pending registration and the approval again with R's step opened before on P's v3 and
//     R implementing; with a member of R's tree in its planning; with P a member issue of another
//     live tree; and with P a labelled Legion root that waits in the line for its first slot,
//     whose admission once a slot frees starts its architect with a task naming R.
//   - The daemon stopped after the pending registration and its seed, the person's approval of v4
//     given while it is down and dropped from its consumer when it starts again: the boot's
//     pending-basis read alone opens the step.
//   - The approval inside the registration's window, the call held after its Dispatch reads until
//     the daemon has applied the approval's event: the step registers pending, and the pending seed
//     alone opens it.
//   - With P a member issue of another live tree, and with P unrecorded: the approval, then the
//     person's changes request on v4 from the document header; the step stays open, and R's
//     architect and P's owning architect, where one runs, are told.
//   - From P approved at v3 with no v4: R's registration on v3 held after its Dispatch read while
//     the person asks for changes on v3 from the header and the daemon applies it; released, the
//     step opens on v3 and stays open, and the gate seed tells both architects.
//   - From P approved at v3 and R's step open on v3: P's architect changes only P's Verification
//     document and requests approval with --include-verification, and the person answers Request
//     changes; the step stays open, and both architects are told.
//   - register_gate on P's v4 as a pending basis while v4 holds a changes request, and while no
//     request is open: refused by name, recording nothing.
//
// What the rig does not build runs where it can: treeRuns of a lingering root, of one re-admitted
// and waiting in the line, and of an answered root in the line, in
// TestTreeRunsIsServedForEveryRootState (internal/api); R waiting on a person, in
// TestAPendingBasisMeetsARootWaitingOnAPerson (internal/admit).
func TestPendingBasisWaitsOnTheAncestorRequest(t *testing.T) {
	pause, held, drop := &registrationPause{}, &outboxHold{}, &consumerDrop{}
	r := newPersonHandoverRig(t, 80)
	r.tune = func(o *overrides) {
		o.gateRegistrationRead = pause.hook
		o.holdOutbox = held.holds
		o.dropConsumedEvent = drop.drops
	}
	r.boot()
	// The rig's database holds the outbox from the boot's migration on, so the record of what the
	// daemon writes to it (writtenNotices) starts here, before any issue of the proof exists. The
	// trigger stays on the database, and the restart below boots on it.
	recordOutboxWrites(t, r.pool)

	// The person approves v4: R's step opens, and so does S's.
	approved := r.pendingWorld([]string{"Canal lock sequencer", "Inland waterway modernisation"},
		pendingOptions{place: ancestorLive, sibling: "Towpath lantern relay"})
	r.registerOn(approved.basisRun)
	r.pendingRegistered(approved.basisRun, approved.ask, true, phase.Admitted)
	r.registerOn(*approved.sibling)
	r.pendingRegistered(*approved.sibling, approved.ask, true, phase.Admitted)
	state, _ := settled(t, r.d, "answer its state", func(api.State) bool { return true })
	if runs := state.Issues[approved.parent.Key].TreeRuns; runs == nil || !*runs {
		t.Fatalf("the state serves %s's treeRuns = %v while its tree runs, want true", approved.parent.Key, runs)
	}
	r.approve(approved.parent.PrimaryArtifactID)
	_, message := r.openedOnBasis(approved.root, approved.parent, 1, approved.basisVersion)
	r.openedOnBasis(approved.sibling.root, approved.parent, 1, approved.basisVersion)
	for _, root := range []string{approved.root.Key, approved.sibling.root.Key} {
		opened := fmt.Sprintf("An ancestor's approval opened the spec step of %s.", root)
		if resting := writtenNotices(t, r.pool, approved.parent.Key, "resting-root"); !slices.ContainsFunc(resting, func(n record.Notice) bool { return strings.Contains(n.Reason, opened) }) {
			t.Fatalf("the resting-root notices written for %s name no opening of %s: %+v", approved.parent.Key, root, resting)
		}
	}
	t.Logf("the person's approval of version %d of %s opened %s and %s; the message on %s:\n%s", approved.basisVersion, approved.parent.Key,
		approved.root.Key, approved.sibling.root.Key, approved.root.Key, message)

	// Each act that ends the wait.
	document := func(run pendingRun) string {
		return fmt.Sprintf("the primary document of %s (dispatch://%s/spec)", run.parent.Key, run.parent.Key)
	}
	for _, tc := range []struct {
		name  string
		names []string
		act   func(run pendingRun) string
	}{
		{"the person's changes request on v4", []string{"Ferry ticket kiosk", "Island transit expansion"}, func(run pendingRun) string {
			r.requestChanges(run.parent.PrimaryArtifactID, "Split p4 in two.")
			return fmt.Sprintf("user:smoke requested changes on version %d of %s: %q.", run.basisVersion, document(run), "Split p4 in two.")
		}},
		{"v5 of P's document", []string{"Tram signal priority", "Metropolitan rail refit"}, func(run pendingRun) string {
			newer := r.editDocument(run.parent.PrimaryArtifactID, insertParagraph("The refit adds the depot."))
			return fmt.Sprintf("Version %d of %s is newer than version %d, which the request asks a person to approve.", newer, document(run), run.basisVersion)
		}},
		{"a session's close of the request", []string{"Curb meter auditor", "Downtown curbside reform"}, func(run pendingRun) string {
			r.resolveAsk(run.ask, asSession)
			return fmt.Sprintf("session:%s closed approval request %s of version %d of %s", scratchSession, run.ask, run.basisVersion, document(run))
		}},
		{"the person's close of the request with no review", []string{"Bicycle dock counter", "Greenway cycling network"}, func(run pendingRun) string {
			r.resolveAsk(run.ask, asPerson)
			return fmt.Sprintf("user:smoke closed approval request %s of version %d of %s", run.ask, run.basisVersion, document(run))
		}},
		{"P set done", []string{"Snowplow route planner", "Winter roads programme"}, func(run pendingRun) string {
			r.closeUnlinking(run.parent.Key)
			return fmt.Sprintf("Dispatch shows %s done, so %s is accepted.", run.parent.Key, run.parent.Key)
		}},
		{"a new version of R's own document", []string{"Streetlight dimmer schedule", "Municipal lighting retrofit"}, func(run pendingRun) string {
			newer := r.editDocument(run.root.PrimaryArtifactID, insertParagraph("The dimmer skips holidays."))
			return fmt.Sprintf("Version %d of the primary document of %s (dispatch://%s/spec), the root's own document, is newer than version %d, which the step was registered at.",
				newer, run.root.Key, run.root.Key, run.rootVersion)
		}},
		{"P losing the legion label", []string{"Pothole report triage", "Pavement repair initiative"}, func(run pendingRun) string {
			r.patch(run.parent.Key, asPerson, map[string]any{"labels": []string{"waves"}})
			return fmt.Sprintf("%s no longer carries the legion label.", run.parent.Key)
		}},
		{"P in Dispatch's retro after the daemon refused its sign-off", []string{"Recycling bin sensor", "Citywide waste reduction"}, func(run pendingRun) string {
			r.moveRecord(run.parent.Key, phase.Accepting)
			// R is under P in Legion's work, so the sign-off is refused for R as a child still in
			// the workflow, which acceptanceHeld reads before the roots resting on P (R8, R9 of
			// dispatch://INFRA-3320). The sign-off names P's architect, P's generation and the run
			// of the step P stands in, as the route reads them (api's signOff).
			var entry record.StepEntry
			if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				entry, err = record.NewStore().OpenStep(ctx, tx, run.parent.Key)
				return err
			}); err != nil {
				t.Fatalf("read the run of the step %s stands in: %v", run.parent.Key, err)
			}
			refused := r.applyAsTheDaemon("api", "sign-off-"+run.parent.Key, intake.SignOff{Issue: run.parent.Key,
				By: r.architectToken(run.parent.Key), Generation: r.record(run.parent.Key).Generation, Entry: entry})
			if refused.Refusal == nil || refused.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" || !strings.Contains(refused.Refusal.Message, run.root.Key) {
				t.Fatalf("the sign-off of %s while %s waits on its request = %+v, want SIGNOFF_CHILD_IN_WORKFLOW naming %s", run.parent.Key, run.root.Key, refused.Refusal, run.root.Key)
			}
			if standing := r.record(run.parent.Key).Phase; standing != phase.Accepting {
				t.Fatalf("%s stands at %s after its refused sign-off, want accepting: the daemon does not record it in retro", run.parent.Key, standing)
			}
			r.pendingRegistered(run.basisRun, run.ask, true, phase.Admitted)
			t.Logf("the sign-off of %s was refused while %s waits on its request: %s", run.parent.Key, run.root.Key, refused.Refusal.Message)
			r.setStatus(run.parent.Key, "retro")
			return fmt.Sprintf("Dispatch shows %s retro, so %s is accepted.", run.parent.Key, run.parent.Key)
		}},
		{"the daemon's move of P into merging", []string{"Fountain pump controller", "Public square renewal"}, func(run pendingRun) string {
			implementer := r.roleToken(run.parent.Key, claim.RoleImplementer)
			r.moveRecord(run.parent.Key, phase.Retro)
			if err := store.Transact(t.Context(), r.pool, func(_ context.Context, tx pgx.Tx) error {
				return record.NewStore().PutPhase(t.Context(), tx, record.PhaseRow{Issue: run.parent.Key, Role: claim.RoleImplementer, Claim: implementer})
			}); err != nil {
				t.Fatalf("record the implementer's retro phase of %s: %v", run.parent.Key, err)
			}
			if done := r.applyAsTheDaemon("api", "retro-"+run.parent.Key, intake.HandoffComplete{Issue: run.parent.Key, Role: claim.RoleImplementer,
				Claim: implementer, Generation: r.record(run.parent.Key).Generation, Summary: "Retro written.", Commit: "retro"}); done.Refusal != nil {
				t.Fatalf("the retro completion of %s = %+v, want it applied", run.parent.Key, done.Refusal)
			}
			if standing := r.record(run.parent.Key).Phase; standing != phase.Merging {
				t.Fatalf("%s stands at %s after its retro completed, want merging", run.parent.Key, standing)
			}
			return fmt.Sprintf("The daemon moved %s into merging, so %s is accepted.", run.parent.Key, run.parent.Key)
		}},
		{"the person's changes request on R's own document", []string{"Library return chute", "Neighbourhood reading rooms"}, func(run pendingRun) string {
			r.requestChanges(run.root.PrimaryArtifactID, "Name the fallback.")
			return fmt.Sprintf("user:smoke requested changes on version %d of the primary document of %s (dispatch://%s/spec), the root's own document: %q.",
				run.rootVersion, run.root.Key, run.root.Key, "Name the fallback.")
		}},
	} {
		run := r.pendingWorld(tc.names, pendingOptions{place: ancestorLive})
		r.registerOn(run.basisRun)
		r.pendingRegistered(run.basisRun, run.ask, true, phase.Admitted)
		// The registration's own rows that read or write Dispatch for R run before the act, its
		// pending-basis seed among them: the seed reads the daemon's store beside Dispatch
		// (judgeBasisRead), and the settle of the fact it applies runs an admission pass, so a seed
		// the outbox took up only after an act wrote P's record by hand (moveRecord) would read that
		// record first: it would end the wait as the daemon recording P in retro before the move
		// into merging, or move P from accepting to awaiting_children before its sign-off.
		r.dispatchRowsDone(run.root.Key)
		act := tc.act(run)
		r.pendingEnded(run.basisRun, run.ask, act, true, phase.Admitted)
		if tc.name == "the person's changes request on R's own document" {
			if changes := r.notices(run.root.Key, "design-changes-requested"); len(changes) != 1 || changes[0].Reason != "Name the fallback." {
				t.Fatalf("the design-changes-requested notices of %s = %+v, want the person's request beside basis-ended", run.root.Key, changes)
			}
		}
		t.Logf("%s ended the wait of %s", tc.name, run.root.Key)
	}

	// The person's approval of R's own document while the step waits: both notices. The step opens
	// on that approval, and R stays at admitted: P's own gate is shut by v4 while the request waits,
	// and R, under P in Legion's work, waits for it (R19 of dispatch://INFRA-3320).
	own := r.pendingWorld([]string{"Chimney sweep roster", "Historic district upkeep"}, pendingOptions{place: ancestorLive})
	r.registerOn(own.basisRun)
	r.pendingRegistered(own.basisRun, own.ask, true, phase.Admitted)
	r.approve(own.root.PrimaryArtifactID)
	state, body := settled(t, r.d, "open the spec step of "+own.root.Key+" on the person's approval of its own document", func(s api.State) bool {
		return stepOf(s, own.root.Key).Status == api.GatedStepApproved && s.Issues[own.root.Key].Phase == phase.Admitted
	})
	if step := stepOf(state, own.root.Key); step.Basis != nil {
		t.Fatalf("the spec step of %s = %+v (basis %+v), want it open on the person's approval with no basis\nstate %s", own.root.Key, step, step.Basis, body)
	}
	opened, ended := r.notices(own.root.Key, "design-approved"), r.notices(own.root.Key, "basis-ended")
	if len(opened) != 1 || opened[0].Ref != "dispatch://"+own.root.Key+"/spec" || opened[0].Version != own.rootVersion || opened[0].Actor != "user:smoke" {
		t.Fatalf("the design-approved notices of %s = %+v, want one naming its own document at version %d", own.root.Key, opened, own.rootVersion)
	}
	ownApproval := fmt.Sprintf("user:smoke approved version %d of the primary document of %s (dispatch://%s/spec), the root's own document. "+
		"That approval opened the spec step of %s and ended its wait on the ancestor's request.", own.rootVersion, own.root.Key, own.root.Key, own.root.Key)
	if len(ended) != 1 || !strings.Contains(ended[0].Reason, ownApproval) {
		t.Fatalf("the basis-ended notices of %s = %+v, want one saying %q", own.root.Key, ended, ownApproval)
	}
	t.Logf("the person's approval of %s's own document opened its step and sent design-approved and basis-ended: %s", own.root.Key, ended[0].Reason)

	// R's step opened before on P's v3, R implementing.
	before := r.pendingWorld([]string{"Bridge cable tensioner", "Estuary crossing works"}, pendingOptions{place: ancestorLive, openedBefore: true})
	r.registerOn(before.basisRun)
	r.pendingRegistered(before.basisRun, before.ask, true, phase.Implementing)
	r.approve(before.parent.PrimaryArtifactID)
	r.reopenedOnPending(before.basisRun, phase.Implementing)
	t.Logf("%s, implementing, opened again at its version %d on %s's version %d", before.root.Key, before.rootVersion, before.parent.Key, before.basisVersion)

	// A member of R's tree in its first phase.
	member := r.pendingWorld([]string{"Ski lift ticketing", "Mountain resort upgrade"}, pendingOptions{place: ancestorLive, member: "Gondola cabin heater"})
	r.registerOn(member.basisRun)
	r.pendingRegistered(member.basisRun, member.ask, true, phase.AwaitingChildren)
	r.approve(member.parent.PrimaryArtifactID)
	r.reopenedOnPending(member.basisRun, phase.AwaitingChildren)
	standsOpen := fmt.Sprintf("The spec step stands open on an ancestor's approval: a person approved version %d of the primary document of %s",
		member.basisVersion, member.parent.Key)
	// The planner is told the opening once: by a role message, or, where it is owed news of the
	// approved design, by the task that carries that news (openingWordsTold).
	testwait.Eventually(t, "the daemon to tell the planner of "+member.member.Key+" the opening on the ancestor's approval", func() bool {
		return r.openingWordsTold(member.root.Key, member.member.Key, standsOpen)
	})
	if standing := r.record(member.member.Key).Phase; standing != phase.Planning {
		t.Fatalf("the member %s stands at %s after its root's step opened again, want planning", member.member.Key, standing)
	}
	t.Logf("%s opened again with its member %s in planning, told %q", member.root.Key, member.member.Key, standsOpen)

	// P a member issue of another live tree.
	nested := r.pendingWorld([]string{"Weather balloon tracker", "Atmospheric sampling", "National forecasting overhaul"}, pendingOptions{place: ancestorMember})
	r.registerOn(nested.basisRun)
	r.pendingRegistered(nested.basisRun, nested.ask, true, phase.Admitted)
	r.approve(nested.parent.PrimaryArtifactID)
	r.openedOnBasis(nested.root, nested.parent, 1, nested.basisVersion)

	// The daemon stopped after the pending seed ran; the approval while it is down, its event
	// dropped when it starts again.
	restarted := r.pendingWorld([]string{"Tidal turbine monitor", "Offshore energy pilot"}, pendingOptions{place: ancestorLive})
	r.registerOn(restarted.basisRun)
	r.pendingRegistered(restarted.basisRun, restarted.ask, true, phase.Admitted)
	testwait.Eventually(t, "the pending-basis seed of "+restarted.root.Key+" to run", func() bool {
		return r.outboxCount(restarted.root.Key, record.OutboxKindPendingBasisSeed) == 0
	})
	r.dispatchRowsDone(restarted.root.Key)
	r.pendingRegistered(restarted.basisRun, restarted.ask, true, phase.Admitted)
	r.d.stop()
	downApproval := r.approve(restarted.parent.PrimaryArtifactID)
	dropped := drop.count()
	drop.arm(func(fact intake.Fact) bool {
		artifact, ok := fact.(intake.DispatchArtifact)
		return ok && artifact.ArtifactID == restarted.parent.PrimaryArtifactID && artifact.Kind == intake.DispatchArtifactApproved && artifact.Version == downApproval.Version
	})
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	testwait.Eventually(t, "the consumer to deliver and drop the person's approval of "+restarted.parent.Key, func() bool { return drop.count() == dropped+1 })
	r.applied(r.newestEvent(restarted.parent.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == downApproval.Version }))
	// The boot's read opens R's step on P's approval; R itself stays at admitted, since the event
	// the consumer dropped is the approval that would open P's own gate too, which R, under P in
	// Legion's work, waits for (R19 of dispatch://INFRA-3320).
	settled(t, r.d, "open the spec step of "+restarted.root.Key+" on the approval the consumer dropped", func(s api.State) bool {
		step := stepOf(s, restarted.root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.Basis != nil && step.Basis.Version == restarted.basisVersion &&
			s.Issues[restarted.root.Key].Phase == phase.Admitted
	})
	t.Logf("the boot's pending-basis read opened %s on the approval the consumer dropped", restarted.root.Key)

	// The approval inside the registration's window.
	window := r.pendingWorld([]string{"Reservoir level gauge", "Upland water security"}, pendingOptions{place: ancestorLive})
	held.hold(window.root.Key, record.OutboxKindPendingBasisSeed)
	reached, release := pause.arm(window.root.Key)
	answered := r.gateCallAsync(basisCall(window.grant(), window.root, window.rootVersion, window.parent.PrimaryArtifactID, window.basisVersion, basisReason))
	select {
	case <-reached:
	case answer := <-answered:
		t.Fatalf("the registration answered %d %s before it was held: %v", answer.status, answer.body, answer.err)
	case <-time.After(time.Minute):
		t.Fatal("the registration was never held after its Dispatch reads")
	}
	windowApproval := r.approve(window.parent.PrimaryArtifactID)
	r.applied(r.newestEvent(window.parent.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == windowApproval.Version }))
	// While the registration is held, R holds only the step of its own design its architect
	// registered before (pendingWorld), waiting on the person with no basis.
	if gate, gates := r.gates(window.root.Key)[record.DesignStepSpec], r.gates(window.root.Key); len(gates) != 1 || gate.Basis != nil || record.GateOpen(gate) {
		t.Fatalf("while the registration is held, %s holds the gates %+v, want its own spec step alone, waiting with no basis", window.root.Key, gates)
	}
	release()
	select {
	case answer := <-answered:
		if answer.err != nil || answer.status != http.StatusOK {
			t.Fatalf("the held registration of %s = %d %s (%v), want 200", window.root.Key, answer.status, answer.body, answer.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the released registration never answered")
	}
	r.pendingRegistered(window.basisRun, window.ask, true, phase.Admitted)
	held.release(window.root.Key)
	r.openedOnBasis(window.root, window.parent, 1, window.basisVersion)
	t.Logf("the approval inside the registration's window reached %s only through its pending seed", window.root.Key)

	// The approval, then a changes request on v4 from the header, with P a member issue and with P
	// unrecorded.
	for _, tc := range []struct {
		names []string
		place pendingPlace
	}{
		{[]string{"Avalanche beacon tester", "Alpine rescue coordination", "Mountain safety federation"}, ancestorMember},
		{[]string{"Sand dune fence", "Coastal erosion study"}, ancestorUnrecorded},
	} {
		header := r.pendingWorld(tc.names, pendingOptions{place: tc.place})
		r.registerOn(header.basisRun)
		r.pendingRegistered(header.basisRun, header.ask, tc.place.recorded(), phase.Admitted)
		r.approve(header.parent.PrimaryArtifactID)
		r.openedOnBasis(header.root, header.parent, 1, header.basisVersion)
		r.dispatchRowsDone(header.root.Key)
		r.requestChanges(header.parent.PrimaryArtifactID, "Split p4 into two.")
		r.quiet(header.parent.Key)
		r.basisChangesTold(header.basisRun, "Split p4 into two.", tc.place.recorded())
	}

	// From P approved at v3: a changes request on v3 inside the registration's window.
	inside := r.pendingWorld([]string{"Glasshouse vent opener", "Botanical garden renewal"}, pendingOptions{place: ancestorLive, noV4: true})
	reached, release = pause.arm(inside.root.Key)
	answered = r.gateCallAsync(basisCall(inside.grant(), inside.root, inside.rootVersion, inside.parent.PrimaryArtifactID, inside.basisVersion, basisReason))
	select {
	case <-reached:
	case answer := <-answered:
		t.Fatalf("the registration answered %d %s before it was held: %v", answer.status, answer.body, answer.err)
	case <-time.After(time.Minute):
		t.Fatal("the registration was never held after its Dispatch reads")
	}
	r.requestChanges(inside.parent.PrimaryArtifactID, "Name p2's owner.")
	r.applied(r.newestEvent(inside.parent.Key, "artifact.changes_requested", nil))
	// R holds only the step of its own design its architect registered before (pendingWorld),
	// waiting on the person with no basis, so the live event finds no step resting on P.
	gates := r.gates(inside.root.Key)
	if told, gate := r.notices(inside.root.Key, "basis-changes-requested"), gates[record.DesignStepSpec]; len(told) != 0 || len(gates) != 1 || gate.Basis != nil || record.GateOpen(gate) {
		t.Fatalf("while the registration of %s is held, it holds gates %+v and basis-changes-requested notices %+v, want its own waiting step alone and no notice: the live event found no step on P",
			inside.root.Key, gates, told)
	}
	release()
	select {
	case answer := <-answered:
		if answer.err != nil || answer.status != http.StatusOK {
			t.Fatalf("the held registration of %s = %d %s (%v), want 200", inside.root.Key, answer.status, answer.body, answer.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the released registration never answered")
	}
	// R's step opens on P's approval of v3, which the changes request does not take back (R3); R
	// itself stays at admitted, since the same request shuts P's own gate, which R, under P in
	// Legion's work, waits for (R19 of dispatch://INFRA-3320).
	settled(t, r.d, "open the spec step of "+inside.root.Key+" on version 3 of "+inside.parent.Key, func(s api.State) bool {
		step := stepOf(s, inside.root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.Basis != nil && step.Basis.Version == inside.basisVersion &&
			s.Issues[inside.root.Key].Phase == phase.Admitted
	})
	r.basisChangesTold(inside.basisRun, "Name p2's owner.", true)

	// From P approved at v3 and R open on v3: the changes request through a later request that
	// names v3 beside P's Verification document.
	later := r.pendingWorld([]string{"Planetarium seat booking", "Science museum expansion"}, pendingOptions{place: ancestorLive, noV4: true})
	r.registerOn(later.basisRun)
	r.openedOnBasis(later.root, later.parent, 1, later.basisVersion)
	r.dispatchRowsDone(later.root.Key)
	r.writeDocument(later.parent.Key, "verification.md", checksMarkdown(verificationCheck{id: "pk1", serves: []string{specBlock(later.parent.Key, "p1")}}))
	request := r.requestApprovalWithVerification(later.parent.PrimaryArtifactID, "The Verification document is new; the spec stands at version 3.")
	r.quiet(later.parent.Key)
	dispatchCall(t, r.baseURL, http.MethodPost, "/api/v1/asks/"+request+"/answer",
		map[string]any{"selected": []string{"Request changes"}, "text": "The checks miss p3.", "expected_edited_at": nil}, nil)
	r.quiet(later.parent.Key)
	r.basisChangesTold(later.basisRun, "The checks miss p3.", true)

	// register_gate on v4 as a pending basis while v4 holds a changes request, and while no
	// request is open.
	reviewed := r.pendingWorld([]string{"Orchestra pit lift", "Opera house refurbishment"}, pendingOptions{place: ancestorUnrecorded})
	r.requestChanges(reviewed.parent.PrimaryArtifactID, "Not yet.")
	r.quiet(reviewed.parent.Key)
	basisCases{r: r, root: reviewed.root}.refused("a pending basis on a version that holds a changes request",
		basisCall(reviewed.grant(), reviewed.root, reviewed.rootVersion, reviewed.parent.PrimaryArtifactID, reviewed.basisVersion, basisReason),
		http.StatusConflict, "BASIS_NOT_APPROVED", fmt.Sprintf("a changes request on version %d by user:smoke", reviewed.basisVersion))
	unasked := r.pendingWorld([]string{"Ballroom chandelier hoist", "Civic hall restoration"}, pendingOptions{place: ancestorUnrecorded, noRequest: true})
	basisCases{r: r, root: unasked.root}.refused("a pending basis on a version with no open request",
		basisCall(unasked.grant(), unasked.root, unasked.rootVersion, unasked.parent.PrimaryArtifactID, unasked.basisVersion, basisReason),
		http.StatusConflict, "BASIS_NOT_APPROVED", fmt.Sprintf("an approval of version 3 by user:smoke, older than its latest version %d, and no open approval request",
			unasked.basisVersion))

	// P a labelled Legion root that waits in the line for its first slot. The daemon boots again
	// with the cap lowered to the slots its roots hold and one more, which the run's R takes, so P
	// waits; the resting-root rows keyed by P are held so that they are read where they stand. The
	// approval opens R's step, and R stays admitted, waiting for P's own gate (R19). A person's done
	// on a root of an earlier run frees a slot, whose promotion admits P and starts its architect,
	// whose start task names R; P's architect registers P's gate on the approval, and R plans.
	state, _ = settled(t, r.d, "answer its state before the cap is lowered", func(api.State) bool { return true })
	r.cfg.AdmissionCap = len(state.Admission.Active) + 1
	r.restart()
	waiting := r.pendingWorld([]string{"Seed vault humidity", "Agricultural heritage archive"}, pendingOptions{place: ancestorWaiting})
	held.hold(waiting.parent.Key, record.OutboxKindNotice)
	r.registerOn(waiting.basisRun)
	r.pendingRegistered(waiting.basisRun, waiting.ask, true, phase.Admitted)
	r.approve(waiting.parent.PrimaryArtifactID)
	settled(t, r.d, "open the spec step of "+waiting.root.Key+" while "+waiting.parent.Key+" waits in the line", func(s api.State) bool {
		return stepOf(s, waiting.root.Key).Status == api.GatedStepOpenOnBasis && s.Issues[waiting.root.Key].Phase == phase.Admitted &&
			slices.Contains(s.Admission.Waiting, waiting.parent.Key)
	})
	architect := r.architectToken(waiting.parent.Key)
	launched := r.launches(string(architect))
	r.setStatus(approved.root.Key, "done")
	parentGrant := r.admitted(waiting.parent.Key, launched)
	restingTask := fmt.Sprintf("These roots rest on the approval of %s and are still in the workflow: %s (its spec step, in admitted).", waiting.parent.Key, waiting.root.Key)
	var started api.OperatorClaim
	testwait.Eventually(t, "the architect of "+waiting.parent.Key+" to hold its start task", func() bool {
		started = r.d.claim(architect)
		return started.Pending != nil
	})
	if !strings.Contains(started.Pending.Task, restingTask) {
		t.Fatalf("the start task of %s, admitted from the line, = %q, want it naming %q", waiting.parent.Key, started.Pending.Task, restingTask)
	}
	held.release(waiting.parent.Key)
	r.registerApprovedGate(waiting.parent.Key, waiting.parent.PrimaryArtifactID, waiting.basisVersion, parentGrant)
	r.openedOnBasis(waiting.root, waiting.parent, 1, waiting.basisVersion)
	t.Logf("the person's approval of %s, waiting in the line, opened %s; a slot admitted %s, whose start task names it", waiting.parent.Key, waiting.root.Key, waiting.parent.Key)
}

// closeUnlinking closes key as the person does at the dashboard while issues under it are still
// open: done with unlink_children through Dispatch's status-change route, which unlinks each open
// issue under key, at its own status, before the close, so Dispatch's parent rule (R24 of
// dispatch://INFRA-3320) holds throughout.
func (r *personHandoverRig) closeUnlinking(key string) {
	r.t.Helper()
	if status, body := dispatchAnswerAs(r.t, r.baseURL, http.MethodPost, "/api/v1/issues/"+key+"/status-change",
		map[string]any{"status": "done", "unlink_children": true}, asPerson); status != http.StatusOK {
		r.t.Fatalf("the person's close of %s with unlink_children = %d: %s", key, status, body)
	}
}
