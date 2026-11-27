//go:build e2e

// The live proof's driver for dispatch://INFRA-3320's check k14 (R9 to R12): a parent starts no
// phase while a Dispatch child of it is open. A root implementing in its slot, whose architect
// releases a child under it, stands in awaiting_children at the next admission pass, holding no
// slot; its implementer, in the middle of the phase, is told on its own role topic that the phase
// ended and why, and its architect gets one awaiting-children notice; a completion of the ended
// phase is refused HANDOFF_NOT_CURRENT_PHASE while the root waits; and once the child's pull
// request has merged and the child is done, the root goes back to the start it held,
// implementing, and its implementer is started with a task that names the child that merged and
// asks for no merge of the default branch. scripts/e2e/parent-waits.sh builds this driver
// with `-tags e2e` and owns the nats-server it runs against, through the run
// lib/live-daemon-proof.sh makes around every whole-daemon live proof. The PostgreSQL is the one
// LEGION_TEST_PG_DSN names, and the script starts none.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin, by way of liveIssueProof's boot), so the proof exercises
// the real configuration, the real store and its migrations, the real HTTP API, the real claim
// supervision and its worker stream, the real openWorkflow, the real JetStream consumers, the real
// admission pass and the real outbox lanes, the role-message lane that carries the phase-ended
// message among them. Facts enter only as production's do: Dispatch's issue envelopes on the
// ENVOY_NOTIFICATIONS stream for the child's moves, and GitHub's pull-request envelopes on the
// same stream for the opening and the merge of the child's pull request; the daemon's own HTTP
// routes for each role's registration, readiness, grants, the architect's release_children and the
// implementer's completion; and the daemon's worker stream for the implementer's turns. Every step
// is read back from GET /legion/v1/state, from what the daemon wrote to the Dispatch stand-in, from
// what it published to the Envoy listener stand-in, and, for the start of step 4, from the prompt
// the daemon sends the implementer's own connection, which is where a start is delivered.
//
// What the proof stands in for lies outside the change. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. Dispatch is the board the package's whole-daemon
// live proofs share (liveDispatch), which lists no issue at boot, so the child is in no mirror row
// until its release's own event. Envoy's listener is the package's noticeListener. GitHub is the
// harness's REST stand-in for the root's one pull request (liveGitHub), grown by the default
// branch's ref and the create of an issue branch, which the child's first phase asks for. The two
// Apps' tokens are workflowTokenRecorder's placeholders. No agent runs: every claim is launched by
// the package's fake runtime, which provisions each workspace in the claim's own pod, and each
// role the proof acts as is driven over the daemon's own worker stream and HTTP routes. The root,
// its pull request, its slot, its approved design gate and its implementer's and architect's
// claims are seeded through the store, as every daemon test seeds its fixture; the child is
// recorded by the daemon from its own release and envelope.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// parentWaitsBase is the root's base branch, the repository's default, which the child's pull
// request merges into; parentWaitsHead is the head of the root's pull request, which its
// implementer's completion reports; parentWaitsBaseHead is the commit the default branch stands
// at, where the child's issue branch is created; and parentWaitsChildHead and
// parentWaitsChildMerge are the head the child's pull request merges at and its merge commit.
const (
	parentWaitsBase       = "main"
	parentWaitsHead       = "pw1"
	parentWaitsBaseHead   = "base-main"
	parentWaitsChildHead  = "pw-child"
	parentWaitsChildMerge = "pw-child-merge"
)

// parentWaitsLive is one run of the proof: the booted daemon, the root it drives and the child
// released under it with the number of the child's pull request, the root's implementer and
// architect as their agents reach the daemon, and the Dispatch sequence the next envelope the
// driver publishes carries.
type parentWaitsLive struct {
	liveIssueProof
	child   string
	childPR int
	imp     *liveClaimant
	arch    *liveSession
	seq     int64
}

// TestParentWaitsLive drives k14's four steps against one booted daemon, in the order each step's
// state follows from the last: the root in its slot with its implementer at work, the release of
// a child that moves the root to awaiting_children and tells its worker and its architect, the
// worker's completion of the ended phase refused, and the child's merge and done that take the
// root back.
func TestParentWaitsLive(t *testing.T) {
	r := newParentWaitsLive(t)
	r.step("step1-a-root-in-implementing-holds-a-slot-and-its-implementer-is-registered", r.rootHoldsASlot)
	r.step("step2-a-released-child-moves-the-root-to-awaiting-children-and-its-worker-and-architect-are-told", r.releaseMovesTheRoot)
	r.step("step3-the-implementers-completion-of-the-ended-phase-is-refused", r.endedPhaseRefused)
	r.step("step4-the-childs-merge-and-done-take-the-root-back-to-implementing-naming-the-child", r.rootComesBack)
}

// newParentWaitsLive boots the daemon over the script's broker with the root recorded as
// seedImplementing records it, its design gate approved and its architect's claim mid-launch too,
// and the child filed beneath it on the Dispatch stand-in alone, in backlog, as an architect files
// a child it has not released.
func newParentWaitsLive(t *testing.T) *parentWaitsLive {
	t.Helper()
	r := &parentWaitsLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/parent-waits.sh", parentWaitsBase, "the parent whose child goes first")}
	r.child, r.childPR, r.seq = r.cfg.Project+"-2", r.number+1, 1
	r.serveParentWaitsGitHub()
	r.seed()
	r.boot(nil)
	return r
}

// serveParentWaitsGitHub starts the harness's GitHub stand-in for the root's pull request at its
// head, answering besides the default branch's ref and the create of an issue branch, the two
// calls the child's first phase makes before its planner starts (outbox's issueBranch).
func (r *parentWaitsLive) serveParentWaitsGitHub() {
	r.t.Helper()
	g := &liveGitHub{t: r.t, key: r.key, number: r.number, base: r.base, sha: parentWaitsHead, also: map[string]any{
		"/git/ref/heads/" + parentWaitsBase: map[string]any{"ref": "refs/heads/" + parentWaitsBase, "object": map[string]string{"sha": parentWaitsBaseHead}},
	}}
	g.url = githubtest.Server(r.t, githubtest.Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/repos/"+liveRepo+"/git/refs" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		g.ServeHTTP(w, req)
	}})
	r.github = g
}

// seed records the root as seedImplementing does, on an open pull request at parentWaitsHead,
// with its implementer serving run 1 and no phases row, so its implementing phase stands under way
// with no start written and not delivered: the case where the admission pass stops the worker in
// the middle of its phase. Its design gate is approved, so a child released under it starts its
// first phase, and its architect's claim is a launch a previous daemon began and never recorded,
// which the boot relaunches. The child is on the Dispatch stand-in alone, unlabelled in backlog
// under the root: the board lists nothing at boot, so the daemon has neither a record nor a
// mirror row of it until its release.
func (r *parentWaitsLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.seedImplementing(parentWaitsHead)
	approved := 1
	r.write("the root's approved design gate", func(tx pgx.Tx) error {
		return r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: r.key, ArtifactID: "spec-" + r.key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		})
	})
	architect := r.token(claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, architect)
	r.board.place(r.child, "backlog", r.key, false)
	r.note("seeded: %s implementing on pull request #%d at %s, base %s, holding its slot, its design gate approved, its implementer mid-launch serving run 1 and its architect mid-launch",
		r.key, r.number, parentWaitsHead, parentWaitsBase)
	r.note("seeded: %s on the Dispatch stand-in alone, backlog under %s, unlabelled", r.child, r.key)
}

// publishIssue publishes Dispatch's own issue.updated event of key at status under parent, which
// the daemon's consumer reads as it reads any issue event: the echo of the daemon's own write
// (daemon true, the daemon's session its actor) or a person's move (daemon false). The board moves
// with it, as Dispatch's board moves, and each event carries the next sequence.
func (r *parentWaitsLive) publishIssue(key, status, parent string, daemon bool) {
	r.t.Helper()
	r.board.place(key, status, parent, false)
	r.seq++
	actor := map[string]string{"kind": "user", "id": "operator"}
	if daemon {
		actor = map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}
	}
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": actor, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": key, "status": status, "title": key, "parent": parent, "rank": "M",
			"labels": liveDispatchLabels(false), "last_seq": r.seq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event of %s: %v", key, err)
	}
	r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, r.seq), string(payload))
	r.note("drove Dispatch's issue.updated envelope of %s: %s under %s, actor %s %s, sequence %d", key, status, parent, actor["kind"], actor["id"], r.seq)
}

// quoted is GET /legion/v1/state now, with its `admission` member printed under the step as the
// route served it and one line for the root and one for the child naming the fields the steps
// turn on, so what the run prints is the answer the step was judged on.
func (r *parentWaitsLive) quoted(what string) api.State {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d %s, want 200", status, body)
	}
	var served struct {
		Admission json.RawMessage `json:"admission"`
	}
	var state api.State
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	r.note("%s: admission %s", what, served.Admission)
	for _, key := range []string{r.key, r.child} {
		issue, recorded := state.Issues[key]
		if !recorded {
			r.note("   issues.%s: the state records no such issue", key)
			continue
		}
		implementer := "none"
		if worker, ok := issue.Workers[claim.RoleImplementer]; ok {
			implementer = fmt.Sprintf("%s (session %s)", worker.Claim.State, worker.Claim.Session)
		}
		r.note("   issues.%s: phase %s, status %s, tree %s, slot %s, implementer %s, phaseEntry %d, givenEntry %d",
			key, issue.Phase, issue.Status, issue.Tree, slotOf(issue), implementer, issue.PhaseEntry, issue.GivenEntry)
	}
	return state
}

// waitForState polls GET /legion/v1/state until want holds of it, then prints and returns that
// answer under the step's heading. A wait that gives up prints the answer it last had.
func (r *parentWaitsLive) waitForState(what string, want func(api.State) bool) api.State {
	r.t.Helper()
	held := false
	defer func() {
		if !held {
			r.quoted("the last answer before the wait for " + what + " gave up")
		}
	}()
	testwait.Eventually(r.t, what, func() bool { return want(r.d.state()) })
	held = true
	return r.quoted(what)
}

// implementerTopic is the Envoy role topic of the root's implementer, where a phase-ended message
// to it is published (record.PhaseEnded, outbox's phaseEnded).
func (r *parentWaitsLive) implementerTopic() string {
	return notify.RoleTopicPrefix + string(r.token(claim.RoleImplementer))
}

// Step 1. The root is implementing and holds a slot of the cap; its implementer's relaunched
// claim is registered and ready, and works a turn of its own, the phase under way; its architect's
// is registered and ready too, which is the session that releases the child in step 2.
func (r *parentWaitsLive) rootHoldsASlot() {
	t := r.t
	r.arch = r.register(claim.RoleArchitect, "ses_architect")
	r.arch.ready()
	r.note("the architect is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.imp.startTurn("")
	r.note("the implementer is registered and ready on the launch this boot made, and began a turn of its own on its implementing phase")
	state := r.waitForState(r.key+" implementing in its slot with its implementer registered", func(state api.State) bool {
		root := state.Issues[r.key]
		return root.Phase == phase.Implementing && root.Slot != nil && slices.Contains(state.Admission.Active, r.key) &&
			root.Workers[claim.RoleImplementer].Claim.Session == r.imp.session
	})
	root := state.Issues[r.key]
	if worker := root.Workers[claim.RoleImplementer].Claim; worker.State != string(supervise.StateWorking) {
		t.Fatalf("%s's implementer is %s, want working the turn it began", r.key, worker.State)
	}
	if root.Architect == nil || root.Architect.Session != r.arch.session {
		t.Fatalf("%s's architect is %+v, want the registered session %s", r.key, root.Architect, r.arch.session)
	}
	if blocked := blockedBy(state, r.key); blocked != nil {
		t.Fatalf("admission.blocked names %s waiting on %v, want nothing: no child of it is open yet", r.key, blocked)
	}
	if _, recorded := state.Issues[r.child]; recorded {
		t.Fatalf("the state records %s before its release, want no record of it", r.child)
	}
	r.note("read back: %s in %s holding slot %s and in admission.active %v, its implementer %s on session %s, its architect %s on session %s",
		r.key, root.Phase, slotOf(root), state.Admission.Active, root.Workers[claim.RoleImplementer].Claim.State, r.imp.session,
		root.Architect.State, r.arch.session)
}

// Step 2. The architect releases the child with release_children: the release writes todo to
// Dispatch, and Dispatch's echo of that write records the child in the root's tree. The admission
// pass of that event finds the child open under the root, whose implementer is in the middle of
// its phase, so the root stands in awaiting_children: in admission.blocked naming the child, in
// neither admission.active nor admission.waiting, with no slot. Its implementer gets one message
// on its own role topic naming the child and saying its phase ended, and keeps its turn; its
// architect gets one awaiting-children notice. The role topic is read first: with no open child,
// nothing reaches it, and that is where a daemon that never moves the root fails this step.
func (r *parentWaitsLive) releaseMovesTheRoot() {
	t := r.t
	status, body := r.d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: r.arch.grant(), Issues: []string{r.child}}, false)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d %s, want 200", r.child, status, body)
	}
	r.note("drove POST /legion/v1/waves/release {%s} by %s's architect: %d %s", r.child, r.key, status, bytes.TrimSpace(body))
	if !r.board.wrote(r.child, "todo") {
		t.Fatalf("the Dispatch stand-in took writes %+v, want the release's todo for %s", r.board.writes(), r.child)
	}
	r.note("read back: the Dispatch stand-in took the release's status write of %s to todo", r.child)
	r.publishIssue(r.child, "todo", r.key, true)

	topic := r.implementerTopic()
	var told []listenerPublish
	testwait.Eventually(t, fmt.Sprintf("a message on %s's implementer's role topic %s after the release of %s", r.key, topic, r.child), func() bool {
		told = r.notices.acceptedOn(topic)
		return len(told) > 0
	})
	message := told[0].Message
	for _, want := range []string{
		r.key + ": the daemon ended your implementing phase, and the issue is in awaiting_children now",
		r.child + " (todo)",
		"A completion of implementing is refused (HANDOFF_NOT_CURRENT_PHASE).",
		"The daemon gives the issue its next task once its children are finished.",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the implementer was told %q on %s, want it to say %q", message, topic, want)
		}
	}
	r.note("read back: a message on %s — %s", topic, message)

	state := r.waitForState(r.key+" in awaiting_children, blocked on "+r.child, func(state api.State) bool {
		return state.Issues[r.key].Phase == phase.AwaitingChildren && slices.Contains(blockedBy(state, r.key), r.child)
	})
	root := state.Issues[r.key]
	if root.Slot != nil || slices.Contains(state.Admission.Active, r.key) || slices.Contains(state.Admission.Waiting, r.key) {
		t.Fatalf("%s holds slot %s, admission.active %v, admission.waiting %v; want no slot and in neither list while it waits on %s",
			r.key, slotOf(root), state.Admission.Active, state.Admission.Waiting, r.child)
	}
	if child := state.Issues[r.child]; child.Tree != r.key {
		t.Fatalf("%s is recorded in the tree of %q, want %s's", r.child, child.Tree, r.key)
	}
	r.note("read back: %s in %s, status %s, no slot, in admission.blocked waiting on %v, in neither admission.active %v nor admission.waiting %v",
		r.key, root.Phase, root.Status, blockedBy(state, r.key), state.Admission.Active, state.Admission.Waiting)

	notices := r.waitArchitectNotices(r.key, string(workflow.AwaitingChildrenNoticeKind), 1)
	for _, want := range []string{r.key + " is in awaiting_children, holding the start of implementing", r.child + " (todo)"} {
		if !strings.Contains(notices[0].Payload, want) {
			t.Fatalf("the architect's %s notice = %q, want it to say %q", workflow.AwaitingChildrenNoticeKind, notices[0].Payload, want)
		}
	}
	r.note("read back: one %s notice on %s's architect topic — %s", workflow.AwaitingChildrenNoticeKind, r.key, notices[0].Payload)

	// Nothing repeats either of them while the root waits, and the implementer keeps its turn.
	time.Sleep(liveSettle)
	if again := r.notices.acceptedOn(topic); len(again) != 1 {
		t.Fatalf("%s took %d messages over %s, want the one", topic, len(again), liveSettle)
	}
	if again := r.architectNotices(r.key, string(workflow.AwaitingChildrenNoticeKind)); len(again) != 1 {
		t.Fatalf("the architect took %d %s notices over %s, want the one", len(again), workflow.AwaitingChildrenNoticeKind, liveSettle)
	}
	if working := r.d.claim(r.imp.token); working.State != string(supervise.StateWorking) {
		t.Fatalf("the implementer's claim is %s after the move, want it still working the turn it began", working.State)
	}
	r.note("read back over %s: one message on %s and one %s notice, no other, and the implementer's claim still working its turn",
		liveSettle, topic, workflow.AwaitingChildrenNoticeKind)
}

// Step 3. From the turn it kept, the implementer writes its implement record and reports its
// completion of the phase the pass ended, as a worker that had not read its message would. The
// daemon refuses it 409 HANDOFF_NOT_CURRENT_PHASE and records nothing, and the root stays in
// awaiting_children with no slot; then the implementer ends its turn.
func (r *parentWaitsLive) endedPhaseRefused() {
	t := r.t
	handoff := r.imp.handoff("implement")
	status, failure := r.imp.complete("implemented the parent's own part", parentWaitsHead, handoff)
	if status != http.StatusConflict || failure.Code != "HANDOFF_NOT_CURRENT_PHASE" {
		t.Fatalf("the implementer's completion of implementing at %s naming record %s = %d %s %s, want 409 HANDOFF_NOT_CURRENT_PHASE",
			parentWaitsHead, handoff, status, failure.Code, failure.Error)
	}
	r.note("drove the implementer's POST /legion/v1/handoff/complete at %s naming its own implement record %s: %d %s — %s",
		parentWaitsHead, handoff, status, failure.Code, failure.Error)
	r.imp.endTurn()
	r.note("drove the implementer's agent_end: its claim is idle")
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if root := r.issue(r.key); root.Phase != phase.AwaitingChildren || root.Slot != nil {
			t.Fatalf("%s is %s holding slot %s within %s of the refusal, want awaiting_children with no slot", r.key, root.Phase, slotOf(root), liveSettle)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.quoted(fmt.Sprintf("after %s", liveSettle))
	r.note("read back over %s: %s stayed in awaiting_children with no slot", liveSettle, r.key)
}

// Step 4. The child's pull request opens and merges, as GitHub's events say, and a person then
// closes the child, as Dispatch's event says. The admission pass of that event finds no open
// child under the root, which goes back to the start it held, implementing, takes a slot and is
// out of admission.blocked; its implementer, idle on its own connection, is started with the task
// it held, which now names the child that merged during the wait with its pull request, and which
// asks for no merge of the default branch (dispatch://INFRA-3320 R12).
func (r *parentWaitsLive) rootComesBack() {
	t := r.t
	childPullRequest := func(want record.PullRequestState) func(api.State) bool {
		return func(served api.State) bool {
			pr := served.Issues[r.child].PullRequest
			return pr != nil && pr.Number == r.childPR && pr.State == string(want)
		}
	}
	r.publishChildPullRequest("opened")
	r.waitForState(fmt.Sprintf("%s's pull request #%d recorded open", r.child, r.childPR), childPullRequest(record.PullRequestOpen))
	r.publishChildPullRequest("closed")
	r.waitForState(fmt.Sprintf("%s's pull request #%d recorded merged", r.child, r.childPR), childPullRequest(record.PullRequestMerged))
	r.note("read back: %s's pull request #%d recorded open, then merged", r.child, r.childPR)
	r.publishIssue(r.child, "done", r.key, false)
	state := r.waitForState(r.key+" back in implementing with a slot and its implementer given the entry", func(state api.State) bool {
		root := state.Issues[r.key]
		return state.Issues[r.child].Phase == phase.Done && root.Phase == phase.Implementing && root.Slot != nil &&
			root.PhaseEntry > 0 && root.GivenEntry == root.PhaseEntry
	})
	root := state.Issues[r.key]
	if blocked := blockedBy(state, r.key); blocked != nil {
		t.Fatalf("admission.blocked still names %s waiting on %v, want the wait over", r.key, blocked)
	}
	r.note("read back: %s done; %s in %s holding slot %s, out of admission.blocked, its implementer given entry %d",
		r.child, r.key, root.Phase, slotOf(root), root.GivenEntry)
	merged := fmt.Sprintf("These Dispatch children of %s merged during the wait: %s (pull request #%d).", r.key, r.child, r.childPR)
	prompt := r.imp.takeTask(merged)
	for _, unsaid := range []string{"merge the default branch", "full test round"} {
		if strings.Contains(prompt.Message, unsaid) {
			t.Fatalf("the implementer was prompted %q, want no %q in it: the return asks for no forward merge", prompt.Message, unsaid)
		}
	}
	r.note("read back: the implementer's connection was sent its start, which it took — %s", prompt.Message)
}

// publishChildPullRequest publishes GitHub's event of action on the child's pull request, from its
// issue branch into the default branch (publishPullRequest): "opened" records the pull request for
// the child, and "closed", carrying the merge commit, is its merge.
func (r *parentWaitsLive) publishChildPullRequest(action string) {
	r.t.Helper()
	event := livePullRequestEvent{action: action, number: r.childPR, issue: r.child, head: parentWaitsChildHead,
		base: parentWaitsBase, title: r.child, updated: time.Now().UTC()}
	if action == "closed" {
		event.merge = parentWaitsChildMerge
	}
	r.publishPullRequest(event, fmt.Sprintf("pr-%d-%s", r.childPR, action))
	r.note("drove GitHub's %s envelope of %s's pull request #%d, %s into %s at %s", action, r.child, r.childPR,
		claim.IssueBranch(r.child), parentWaitsBase, parentWaitsChildHead)
}
