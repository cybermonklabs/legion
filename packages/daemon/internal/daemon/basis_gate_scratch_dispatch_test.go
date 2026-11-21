package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The proofs of dispatch://INFRA-3320's checks k1 and k2 on the person-handover rig
// (person_handover_scratch_dispatch_test.go): the real Dispatch server with its events on, a real
// Postgres, a real NATS, and the daemon in this process. A root R that ran as a tree of its own
// registers its spec step on the approval of an ancestor P, a view's way: P's primary document
// marks requirement blocks and a person approved its latest version, and R's documents hold no
// requirement of their own while each check of R's Verification document serves P2. A person's
// act is a request with the trusted identity header, a session's one with the agent bearer.
//
// What the rig cannot build runs where it can, as each test says: an approval a session or
// Dispatch itself gave, and the criteria step, on the API tests' fake client
// (internal/api/gate_basis_test.go); a root waiting on a person, the phase tasks after planning,
// the production check's task and the merger's start, in internal/workflow and internal/record;
// and a caller at a contract below 214, whose narrowing needs a caller that states such a contract
// or one in a pod, which the rig runs neither of (callerPodOf), in
// internal/api/gate_basis_wire_test.go.

// basisOpenedLine is the first line of the message the daemon posts on a root for each step an
// ancestor's approval opened (workflow's basisOpenedLine, R2), by which a reader counts them.
const basisOpenedLine = "Design gate opened on an ancestor's approval."

// basisReasonOf is an architect's reason of exactly n characters, each past the first ASCII ones
// taking two bytes, so a bound is read in characters and not in bytes.
func basisReasonOf(n int) string {
	prefix := "The checks serve P2; both fresh reviews passed. "
	return prefix + strings.Repeat("é", n-utf8.RuneCountInString(prefix))
}

// basisCall is a register_gate body of R's spec step: root's primary document at version, on
// basis, an ancestor's document, at basisVersion, with reason.
func basisCall(grant string, root scratchIssue, version int, basis string, basisVersion int, reason string) map[string]any {
	return map[string]any{
		"grantId": grant, "issue": root.Key, "artifactId": root.PrimaryArtifactID, "version": version,
		"step": string(record.DesignStepSpec), "basisArtifactId": basis, "basisVersion": basisVersion, "reason": reason,
	}
}

// with is body with key set to value, or taken out where value is nil.
func with(body map[string]any, key string, value any) map[string]any {
	if value == nil {
		delete(body, key)
		return body
	}
	body[key] = value
	return body
}

// gateCall sends body to register_gate as the architect's plugin does and answers its status and,
// for a refusal, the code and the message it names.
func (r *personHandoverRig) gateCall(body map[string]any) (int, api.Failure) {
	r.t.Helper()
	status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", body, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(answer, &failure); err != nil {
			r.t.Fatalf("decode the refusal %d %s: %v", status, answer, err)
		}
	}
	return status, failure
}

// registerBasis is root's register_gate at version on version basisVersion of parent's primary
// document, with reason, under grant, which must answer 200.
func (r *personHandoverRig) registerBasis(grant string, root scratchIssue, version int, parent scratchIssue, basisVersion int, reason string) {
	r.t.Helper()
	if status, refusal := r.gateCall(basisCall(grant, root, version, parent.PrimaryArtifactID, basisVersion, reason)); status != http.StatusOK {
		r.t.Fatalf("%s's registration at version %d on version %d of %s's document = %d %+v, want 200", root.Key, version, basisVersion, parent.Key, status, refusal)
	}
}

// gateAnswer is what one register_gate call sent off the test's goroutine came to.
type gateAnswer struct {
	status int
	body   []byte
	err    error
}

// gateCallAsync sends body to register_gate on a goroutine of its own, with a bound long enough
// for a proof to hold the call between its reads and its commit (registrationPause), and answers
// what the call comes to.
func (r *personHandoverRig) gateCallAsync(body map[string]any) <-chan gateAnswer {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("encode the registration: %v", err)
	}
	client := &http.Client{Transport: r.d.transport, Timeout: 2 * time.Minute}
	answered := make(chan gateAnswer, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, r.d.base+"/legion/v1/gates/register", bytes.NewReader(encoded))
		if err != nil {
			answered <- gateAnswer{err: err}
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			answered <- gateAnswer{err: err}
			return
		}
		defer response.Body.Close()
		read, err := io.ReadAll(response.Body)
		answered <- gateAnswer{status: response.StatusCode, body: read, err: err}
	}()
	return answered
}

// registerApprovedGate registers artifactID, key's primary document a person approved at version,
// as key's architect, and waits for the daemon to record the spec step open on that approval.
func (r *personHandoverRig) registerApprovedGate(key, artifactID string, version int, grant func() string) {
	t := r.t
	t.Helper()
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: key, ArtifactID: artifactID, Version: version, Step: record.DesignStepSpec,
	}, false); status != http.StatusOK {
		t.Fatalf("the architect's design-gate registration on %s = %d; body %s", key, status, answer)
	}
	settled(t, r.d, "open the design gate of "+key+" on the person's approval", func(s api.State) bool {
		return stepOf(s, key).Status == api.GatedStepApproved
	})
}

// stepOf is the spec step of key as the state answers it, the zero entry for none.
func stepOf(state api.State, key string) api.GatedStepView {
	for _, step := range state.Issues[key].GatedSteps {
		if step.Step == record.DesignStepSpec {
			return step
		}
	}
	return api.GatedStepView{}
}

// gateFacts counts the register_gate facts the daemon has applied for key (api's gateRegister
// names each `gate:<issue>:…`): what a refused registration leaves as it found it.
func (r *personHandoverRig) gateFacts(key string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from processed_events where source = 'api' and event_id like 'gate:' || $1 || ':%'`, key).
		Scan(&count); err != nil {
		r.t.Fatalf("count the gate facts of %s: %v", key, err)
	}
	return count
}

// outboxCount counts the rows of kind on key still in the daemon's outbox.
func (r *personHandoverRig) outboxCount(key string, kind record.OutboxKind) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from outbox where kind = $1 and issue = $2`, string(kind), key).Scan(&count); err != nil {
		r.t.Fatalf("count the %s rows of %s: %v", kind, key, err)
	}
	return count
}

// noticeReasons is the reason of every notice of any kind the daemon has queued on key, still in
// its outbox.
func (r *personHandoverRig) noticeReasons(key string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select coalesce(payload->>'reason', '') from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindNotice), key)
	if err != nil {
		r.t.Fatalf("read the notices of %s: %v", key, err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			r.t.Fatalf("scan a notice of %s: %v", key, err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the notices of %s: %v", key, err)
	}
	return reasons
}

// basisMessages is every message on key whose first line is basisOpenedLine.
func (r *personHandoverRig) basisMessages(key string) []string {
	r.t.Helper()
	var opened []string
	for _, body := range r.messages(key) {
		if first, _, _ := strings.Cut(body, "\n"); first == basisOpenedLine {
			opened = append(opened, body)
		}
	}
	return opened
}

// oneBasisMessage waits for the outbox to post the message of a basis opening on key and answers
// it, failing unless key holds exactly one.
func (r *personHandoverRig) oneBasisMessage(key string) string {
	t := r.t
	t.Helper()
	testwait.Eventually(t, "the daemon to post on "+key+" which approval opened its step", func() bool {
		return len(r.basisMessages(key)) > 0
	})
	opened := r.basisMessages(key)
	if len(opened) != 1 {
		t.Fatalf("%s holds %d messages opening %q, want one: %q", key, len(opened), basisOpenedLine, opened)
	}
	return opened[0]
}

// latestCatchUp waits for a catch-up of key's architect in the outbox, which its ready queues (the
// rig's ready) in place of any its earlier launches never had delivered, and answers the newest.
func (r *personHandoverRig) latestCatchUp(key string) record.Notice {
	t := r.t
	t.Helper()
	var caught []record.Notice
	testwait.Eventually(t, "the daemon to queue the catch-up of the architect of "+key, func() bool {
		caught = r.notices(key, "catch-up")
		return len(caught) > 0
	})
	return caught[len(caught)-1]
}

// catchUpGate is the spec step of a catch-up.
func catchUpGate(t *testing.T, notice record.Notice) record.CatchUpGate {
	t.Helper()
	if notice.CatchUp == nil {
		t.Fatalf("the catch-up %+v carries no tree", notice)
	}
	for _, gate := range notice.CatchUp.Gates {
		if gate.Step == record.DesignStepSpec {
			return gate
		}
	}
	t.Fatalf("the catch-up %+v names no spec step", notice.CatchUp)
	return record.CatchUpGate{}
}

// linkUnder sets parent as root's Dispatch parent, as the person does at the dashboard. Where the
// daemon records parent in a live tree (live), it waits for the daemon to tell root and parent's
// architect that root stays a tree of its own, and requires those words to name R1's path, the
// view on parent, and not that root's work reaches parent's tree only through its own issues.
// Where it records parent in none, it waits only for the daemon to apply the link: that the link
// told nobody is read once root's step has opened (untoldAtTheLink).
func (r *personHandoverRig) linkUnder(root, parent scratchIssue, live bool) {
	t := r.t
	t.Helper()
	r.patch(root.Key, asPerson, map[string]any{"parent": parent.Key})
	if !live {
		r.quiet(root.Key)
		return
	}
	path := fmt.Sprintf("So the architect of %s can write it as a view on %s.", root.Key, parent.Key)
	names := func(words string) bool {
		return strings.Contains(words, path) && strings.Contains(words, "with register_gate's basis.") &&
			!strings.Contains(words, "only through that tree's own issues")
	}
	testwait.Eventually(t, "the daemon to tell "+root.Key+" that it can be written as a view on "+parent.Key, func() bool {
		return slices.ContainsFunc(r.messages(root.Key), names)
	})
	if notices := childStatusNotices(t, r.pool, parent.Key); !slices.ContainsFunc(notices, names) {
		t.Fatalf("the child-status notices to the architect of %s = %q, want one naming R1's path (%q)", parent.Key, notices, path)
	}
	t.Logf("%s, set under %s, and the architect of %s were told R1's path: %q", root.Key, parent.Key, parent.Key, path)
}

// untoldAtTheLink fails unless the link of root under parent, whose tree is not live, posted no
// message on root and queued no notice to an architect of parent's tree: such a tree has no
// architect to tell (workflow's tellReparented). It is read once root's step has opened and the
// outbox has finished every Dispatch row of root (dispatchRowsDone), so a re-parent seed the link
// queued would have run and its message been posted by now. A child-status notice keyed by
// parent, the root of parent's tree, is read where it stands in the outbox and in the daemon's
// log, where the outbox writes one line for a notice it finishes undelivered: a tree with no
// architect finishes it so (notice_routing.go), and the row is gone by the time this reads it.
func (r *personHandoverRig) untoldAtTheLink(root, parent scratchIssue) {
	t := r.t
	t.Helper()
	r.dispatchRowsDone(root.Key)
	stays := fmt.Sprintf("%s is now a child of %s in Dispatch", root.Key, parent.Key)
	for _, body := range r.messages(root.Key) {
		if strings.Contains(body, stays) {
			t.Fatalf("the daemon posted on %s at its link under %s, whose tree is not live: %q", root.Key, parent.Key, body)
		}
	}
	if notices := childStatusNotices(t, r.pool, parent.Key); len(notices) != 0 {
		t.Fatalf("the child-status notices queued for an architect of %s's tree = %q, want none: the tree is not live", parent.Key, notices)
	}
	for _, line := range loggedLines(t, r.logs.String(), `"kind":"child-status"`) {
		if line["issue"] == parent.Key {
			t.Fatalf("the outbox ran a child-status notice for an architect of %s's tree, which is not live: %v", parent.Key, line)
		}
	}
	t.Logf("the link of %s under %s, whose tree is not live, posted no message on %s and queued no notice to an architect of %s's tree",
		root.Key, parent.Key, root.Key, parent.Key)
}

// openedOnBasis is what every opening of root's spec step on parent's approval reads, once its
// registration answered 200: the state shows the step open on the basis at the root's version,
// the root leaves admitted for planning, its architect is queued one design-approved notice that
// names parent's document and version as the basis and the person, and the root holds one
// message recording the opening. It answers the state and that message.
func (r *personHandoverRig) openedOnBasis(root, parent scratchIssue, rootVersion, basisVersion int) (api.State, string) {
	t := r.t
	t.Helper()
	state, body := settled(t, r.d, root.Key+" to start planning on the approval of "+parent.Key, func(s api.State) bool {
		return s.Issues[root.Key].Phase == phase.Planning && stepOf(s, root.Key).Status == api.GatedStepOpenOnBasis
	})
	step := stepOf(state, root.Key)
	if step.ApprovedVersion == nil || *step.ApprovedVersion != rootVersion || step.CurrentVersion != rootVersion || step.Basis == nil ||
		step.Basis.Issue != parent.Key || step.Basis.ArtifactID != parent.PrimaryArtifactID || step.Basis.Version != basisVersion ||
		step.Basis.Approver != "user:smoke" || step.Basis.ApprovedAt == nil {
		t.Fatalf("the spec step of %s = %+v (basis %+v), want open on version %d of %s's primary document, approved by user:smoke, at its own version %d\nstate %s",
			root.Key, step, step.Basis, basisVersion, parent.Key, rootVersion, body)
	}
	if gate := state.Issues[root.Key].DesignGate; gate == nil || gate.ApprovedVersion == nil || *gate.ApprovedVersion != rootVersion {
		t.Fatalf("the design gate of %s = %+v, want it at its own version %d", root.Key, gate, rootVersion)
	}
	approved := r.notices(root.Key, "design-approved")
	if len(approved) != 1 {
		t.Fatalf("design-approved notices of %s = %+v, want one", root.Key, approved)
	}
	if notice := approved[0]; notice.Version != basisVersion || notice.Actor != "user:smoke" || notice.Ref != "dispatch://"+parent.Key+"/spec" ||
		!strings.Contains(notice.Reason, "No person approved that version.") ||
		!strings.Contains(notice.Reason, fmt.Sprintf("registered version %d of its own", rootVersion)) {
		t.Fatalf("the design-approved notice of %s = %+v, want version %d of dispatch://%s/spec approved by user:smoke as its basis, and no person's approval of version %d",
			root.Key, notice, basisVersion, parent.Key, rootVersion)
	}
	return state, r.oneBasisMessage(root.Key)
}

// ancestorOpening is one case of k1 once its registration answered 200: root's spec step open at
// its own version rootVersion on approval, the person's approval of parent's primary document,
// registered with reason. The fields past those say where the case's world is not the first
// case's: parentLive is false for a parent the daemon records in no live tree while the step opens
// (P on the backlog); ownApproval is the version of root's own document a person approved, older
// than rootVersion (R at v4 holding the person's approval of v2), 0 for none; and whileOpen, when
// set, runs once every read of the open root is made and before root is taken to done.
type ancestorOpening struct {
	root, parent scratchIssue
	rootVersion  int
	approval     scratchReview
	reason       string
	parentLive   bool
	ownApproval  int
	whileOpen    func()
}

// readAncestorOpening is what every case of k1 reads once root's registration on parent's approval
// answered 200: the first case's steps 7 and 8, which each repeat reads whole. The opening
// (openedOnBasis) and its message, line by line; no ask on root; the state's treeRuns and the view
// fact; the planner's task; root's catch-up naming the basis and no person's approval; parent's
// architect told once that root rests on it; parent's catch-up naming root, and parent's first
// phase waiting while root is open; no notice queued on root or on parent, and no role message of
// root's tree, saying a person approved root's document, nor, where a person approved an older
// version of it, naming that version; and, root taken to done, parent's architect told so and
// parent's first phase started.
func (r *personHandoverRig) readAncestorOpening(c ancestorOpening) {
	t := r.t
	t.Helper()
	root, parent := c.root, c.parent
	if c.ownApproval != 0 && c.ownApproval == c.approval.Version {
		t.Fatalf("the person approved version %d of both %s's and %s's documents, so no read can tell a reader naming %s's own approval; give the case versions apart",
			c.ownApproval, root.Key, parent.Key, root.Key)
	}
	state, message := r.openedOnBasis(root, parent, c.rootVersion, c.approval.Version)
	t.Logf("%s opened at version %d on %s's approval of version %d and plans; the message on it:\n%s", root.Key, c.rootVersion, parent.Key, c.approval.Version, message)
	p2 := specBlock(parent.Key, "p2")
	for _, want := range []string{
		"- The step is spec.",
		fmt.Sprintf("- The primary document of %s for this step is dispatch://%s/spec (%s).", root.Key, root.Key, root.PrimaryArtifactID),
		fmt.Sprintf("- The architect of %s registered version %d of this document.", root.Key, c.rootVersion),
		fmt.Sprintf("- No person approved version %d of this document.", c.rootVersion),
		fmt.Sprintf("- The ancestor is %s.", parent.Key),
		fmt.Sprintf("- The primary document of %s for this step is dispatch://%s/spec (%s).", parent.Key, parent.Key, parent.PrimaryArtifactID),
		fmt.Sprintf("- A person approved version %d of this document.", c.approval.Version),
		"- The person is user:smoke.",
		"- Dispatch recorded this approval at " + c.approval.CreatedAt.UTC().Format(time.RFC3339) + ".",
		fmt.Sprintf("  - %s@v%d", p2, c.approval.Version),
		fmt.Sprintf("- The architect session %s registered this step.", r.architects[root.Key].session),
		"> " + c.reason,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the message on %s does not say %q:\n%s", root.Key, want, message)
		}
	}
	if asks := r.asks(root.Key); len(asks) != 0 {
		t.Fatalf("%s holds asks %+v, want none: nobody is asked to approve a step a basis opened", root.Key, asks)
	}
	if runs := state.Issues[root.Key].TreeRuns; runs == nil || !*runs {
		t.Fatalf("the state serves %s's treeRuns = %v, want true", root.Key, runs)
	}
	if viewOf := r.record(root.Key).ViewOf; viewOf != parent.Key {
		t.Fatalf("the view fact recorded at %s's registration = %q, want %s", root.Key, viewOf, parent.Key)
	}
	viewSentence := fmt.Sprintf("%s is a view on %s: the requirements of %s are the parts of the requirements of %s that the Delivers: line in the spec of %s names.",
		root.Key, parent.Key, root.Key, parent.Key, root.Key)
	if task := r.entryTask(root.Key, claim.RolePlanner); !strings.Contains(task, viewSentence) {
		t.Fatalf("the planner's task of %s = %q, want the view sentence %q", root.Key, task, viewSentence)
	}
	r.ready(root.Key)
	caught := catchUpGate(t, r.latestCatchUp(root.Key))
	if !caught.Open || caught.Pending || caught.Approved != 0 || caught.Version != c.rootVersion || caught.Basis == nil || caught.Basis.Issue != parent.Key ||
		caught.Basis.Artifact != parent.PrimaryArtifactID || caught.Basis.Version != c.approval.Version || caught.Basis.Approver != "user:smoke" {
		t.Fatalf("the catch-up of %s's architect states its spec step %+v (basis %+v), want open at version %d on version %d of %s's primary document with no person's approval",
			root.Key, caught, caught.Basis, c.rootVersion, c.approval.Version, parent.Key)
	}

	// P's architect is told that R rests on P where the daemon records P in a live tree. P on the
	// backlog is in none, so while R opens no architect of P runs to be told or to catch up: the
	// link of R under P posted no message on R and queued no notice to an architect of P's tree,
	// the daemon queues no resting-root row, and P's architect hears of R in its start task once
	// the person sets P todo and P is admitted. P's architect then registers P's own gate, as the
	// first case's step 5 does, so that from here on P stands where the first case's P stands.
	told := 1
	if c.parentLive {
		resting := r.notices(parent.Key, "resting-root")
		if len(resting) != 1 || resting[0].Version != c.approval.Version || resting[0].Ref != "dispatch://"+parent.Key+"/spec" ||
			!strings.Contains(resting[0].Reason, root.Key) || !strings.Contains(resting[0].Reason, "spec step") ||
			!strings.Contains(resting[0].Reason, fmt.Sprintf("%s@v%d", p2, c.approval.Version)) {
			t.Fatalf("the resting-root notices to %s's architect = %+v, want one naming %s, its spec step and P2", parent.Key, resting, root.Key)
		}
	} else {
		told = 0
		r.untoldAtTheLink(root, parent)
		if resting := r.notices(parent.Key, "resting-root"); len(resting) != 0 {
			t.Fatalf("the daemon queued resting-root notices %+v on %s, which it records in no live tree; want none", resting, parent.Key)
		}
		parentGrant := r.admit(parent.Key)
		startTask := fmt.Sprintf("These roots rest on the approval of %s and are still in the workflow: %s (its spec step, in planning).", parent.Key, root.Key)
		started := r.d.claim(r.architectToken(parent.Key))
		if started.Pending == nil || !strings.Contains(started.Pending.Task, startTask) {
			t.Fatalf("the start task of %s's architect at its admission = %+v, want it naming %q", parent.Key, started.Pending, startTask)
		}
		t.Logf("%s rested on %s while it was on the backlog with no resting-root row, and %s's start task names it: %q",
			root.Key, parent.Key, parent.Key, started.Pending.Task)
		r.registerApprovedGate(parent.Key, parent.PrimaryArtifactID, c.approval.Version, parentGrant)
	}
	r.ready(parent.Key)
	restingClause := fmt.Sprintf("resting on the approval of %s and still in the workflow: %s (its spec step, in planning)", parent.Key, root.Key)
	if notice := r.latestCatchUp(parent.Key); !strings.Contains(notice.Reason, restingClause) {
		t.Fatalf("the catch-up of %s's architect = %q, want it naming %q", parent.Key, notice.Reason, restingClause)
	}
	if standing := r.record(parent.Key).Phase; standing != phase.Admitted {
		t.Fatalf("%s stands at %s while %s is open, want admitted: its first phase waits for its open child", parent.Key, standing, root.Key)
	}
	t.Logf("%s's architect was told %d time(s) that %s rests on it, its catch-up names %s, and %s waits at admitted while %s is open",
		parent.Key, told, root.Key, root.Key, parent.Key, root.Key)

	// The first registration of a step moves no standing gate, which an unregistered step leaves
	// standing open (classify.StandingGate), so it sends the tree's roles nothing; the opening that
	// follows a close is read in TestBasisGateRefusals. Whatever is queued, on R for its architect,
	// on P for P's architect or to a role of R's tree, nothing says a person approved R's
	// document: in R's own words "its" is R, and P's words would name R. The phrase read in R's
	// words keeps its subject, so the daemon's own "No person approved version N of its spec
	// document" (workflow's gate messages) is not read as an approval.
	versions := []int{c.rootVersion}
	if c.ownApproval != 0 {
		versions = append(versions, c.ownApproval)
	}
	var ofItsOwn, namingRoot []string
	for _, version := range versions {
		ofItsOwn = append(ofItsOwn, fmt.Sprintf("a person approved version %d of its", version))
		namingRoot = append(namingRoot, fmt.Sprintf("approved version %d of the primary document of %s", version, root.Key),
			fmt.Sprintf("approved %s's spec document at version %d", root.Key, version))
	}
	for index, phrase := range namingRoot {
		namingRoot[index] = strings.ToLower(phrase)
	}
	rootNotices, parentNotices, roleMessages := r.noticeReasons(root.Key), r.noticeReasons(parent.Key), r.roleMessages(root.Key)
	// saysApproved fails when one of said holds one of phrases, all lower case, in any case.
	saysApproved := func(whose string, said, phrases []string) {
		for _, words := range said {
			lowered := strings.ToLower(words)
			for _, phrase := range phrases {
				if strings.Contains(lowered, phrase) {
					t.Fatalf("%s says a person approved %s's own document (%q): %q", whose, root.Key, phrase, words)
				}
			}
		}
	}
	saysApproved("a notice queued on "+root.Key+" or a role message of its tree", slices.Concat(rootNotices, roleMessages), slices.Concat(ofItsOwn, namingRoot))
	saysApproved("a notice queued on "+parent.Key+" for its architect", parentNotices, namingRoot)
	t.Logf("none of the %d notices queued on %s, the %d queued on %s and the %d role messages of %s's tree says a person approved %s's document: role messages %q",
		len(rootNotices), root.Key, len(parentNotices), parent.Key, len(roleMessages), root.Key, root.Key, roleMessages)
	if c.ownApproval != 0 {
		// R holds the person's approval of an older version of its own document, and the step stands
		// open at R's latest on P's approval: no reader names that older version, as the step's
		// approval or beside the version the step opened at.
		older := fmt.Sprintf("version %d", c.ownApproval)
		view, err := json.Marshal(state.Issues[root.Key])
		if err != nil {
			t.Fatalf("encode the state of %s: %v", root.Key, err)
		}
		if strings.Contains(string(view), fmt.Sprintf(`"approvedVersion":%d`, c.ownApproval)) || strings.Contains(message, older) {
			t.Fatalf("a reader names %s, the person's approval, as the step's approval at version %d: state %s, message:\n%s", older, c.rootVersion, view, message)
		}
		for _, words := range slices.Concat(rootNotices, roleMessages) {
			if strings.Contains(words, older) {
				t.Fatalf("a notice queued on %s or a role message of its tree names %s, the person's approval, beside the step open at version %d: %q",
					root.Key, older, c.rootVersion, words)
			}
		}
		t.Logf("neither the state, the message, the %d notices queued on %s nor the %d role messages of its tree name %s, the person's approval: notices %q",
			len(rootNotices), root.Key, len(roleMessages), older, rootNotices)
	}
	if c.whileOpen != nil {
		c.whileOpen()
	}

	r.setStatus(root.Key, "done")
	testwait.Eventually(t, "the daemon to tell the architect of "+parent.Key+" that "+root.Key+" is done", func() bool {
		return len(r.notices(parent.Key, "resting-root")) > told
	})
	if resting := r.notices(parent.Key, "resting-root"); len(resting) != told+1 || !strings.Contains(resting[told].Reason, root.Key+" is done.") {
		t.Fatalf("the resting-root notices to %s's architect = %+v, want %d, the last naming %s done", parent.Key, resting, told+1, root.Key)
	}
	settled(t, r.d, "start the first phase of "+parent.Key+" once "+root.Key+" is done", func(s api.State) bool {
		return s.Issues[parent.Key].Phase == phase.Planning
	})
	t.Logf("%s is done, %s's architect was told so, and %s plans", root.Key, parent.Key, parent.Key)
}

// registrationPause holds the register_gate call of one issue between its Dispatch reads and its
// fact (overrides.gateRegistrationRead) until the proof releases it.
type registrationPause struct {
	mu      sync.Mutex
	issue   string
	reached chan struct{}
	release chan struct{}
}

// arm holds the next registration of issue: reached closes when it is held, and release lets it go.
func (p *registrationPause) arm(issue string) (<-chan struct{}, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issue, p.reached, p.release = issue, make(chan struct{}), make(chan struct{})
	release := p.release
	var once sync.Once
	return p.reached, func() { once.Do(func() { close(release) }) }
}

func (p *registrationPause) hook(ctx context.Context, issue string) {
	p.mu.Lock()
	if p.issue == "" || p.issue != issue {
		p.mu.Unlock()
		return
	}
	reached, release := p.reached, p.release
	p.issue = ""
	p.mu.Unlock()
	close(reached)
	select {
	case <-release:
	case <-ctx.Done():
	}
}

// outboxHold holds the outbox rows of one kind of an issue (overrides.holdOutbox) until the proof
// releases them.
type outboxHold struct {
	mu   sync.Mutex
	held map[string]record.OutboxKind
}

func (h *outboxHold) hold(issue string, kind record.OutboxKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = map[string]record.OutboxKind{}
	}
	h.held[issue] = kind
}

func (h *outboxHold) release(issue string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, issue)
}

func (h *outboxHold) holds(row record.OutboxRow) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	kind, held := h.held[row.Issue]
	return held && kind == row.Kind
}

// consumerDrop drops the one fact the Dispatch consumer delivers that the proof armed it for
// (overrides.dropConsumedEvent), and counts what it dropped.
type consumerDrop struct {
	mu      sync.Mutex
	want    func(intake.Fact) bool
	dropped int
}

func (d *consumerDrop) arm(want func(intake.Fact) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.want = want
}

func (d *consumerDrop) drops(fact intake.Fact) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.want == nil || !d.want(fact) {
		return false
	}
	d.want = nil
	d.dropped++
	return true
}

func (d *consumerDrop) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// insertParagraph is the edit that appends one paragraph to a document: a new version that changes
// no block.
func insertParagraph(text string) map[string]string {
	return insertAtEnd(text + "\n")
}

// insertAtEnd is the edit that appends markdown, as written, to the end of a document.
func insertAtEnd(markdown string) map[string]string {
	return map[string]string{"op": "insert", "after": "end", "markdown": markdown}
}

// k1 (dispatch://INFRA-3320, R1, R2, R4 and R8). A root R that ran as a tree of its own, set under
// P, whose primary document marks P1 to P3 and a person approved at its latest version, registers
// its spec step on that approval: the step opens at the registration, R plans, nobody is asked,
// and every reader names the basis and no person's approval of R's own document. The cap is 8, so
// that each run's R and P hold a slot of their own.
//
//  1. R is labelled, set todo and admitted, and its architect launched: R ran as a root.
//  2. P, marking P1 to P3, is approved by the person at its latest version, labelled and admitted.
//  3. P is set as R's parent: the message on R and the child-status notice to P's architect name
//     R1's path. R's Verification document's one check serves P2.
//  4. The negative control: P gains a version the person has not approved, and R's registration
//     on it is refused BASIS_NOT_APPROVED; R's step stays unregistered, R stays admitted, and R
//     holds no new message. The person approves that version.
//  5. P's architect registers P's own gate, open on the person's approval.
//  6. R's architect registers on P's approved version with a reason. The call is held after its
//     Dispatch reads (registrationPause): R's step is still unregistered and no fact applied. Its
//     gate seed is held as well (outboxHold).
//  7. Released, the call answers 200 and the step opens before its seed runs: R plans, nobody is
//     asked, the design-approved notice names P's document and version, one message on R records
//     the opening whole, the state and R's record name the basis, the view fact reads P, the
//     planner's task carries the view sentence, R's catch-up names the basis, P's architect is
//     told once that R rests on it, P's catch-up names R, P's first phase waits while R is open,
//     and no notice queued on R or on P and no role message of R's tree says a person approved
//     R's document. The seed then runs and leaves the step open.
//  8. R is taken to done: P's architect is told again, and P's first phase starts.
//  9. The repeats, each read as steps 7 and 8 read the first case (readAncestorOpening): P's
//     approval carried by Dispatch in the person's name; P labelled and on the backlog, no live
//     tree while R opens, so the link of R under P posts no message on R and queues no notice to
//     an architect of P's tree, no resting-root row is queued and no architect of P catches up
//     until P is admitted, whose start task then names R; and R holding the person's approval of
//     v2 of its own document while at v4, where no reader names v2: not the state, the message, a
//     notice queued on R or a role message of its tree.
//
// Where the rig reaches no phase — the implementer's, the tester's and the reviewer's tasks, the
// production check's task, the merger's start — the workflow and record tests do:
// TestEveryPhaseTaskOfAViewNamesTheAncestorsRequirements and
// TestTheProductionCheckTaskOfAViewNamesTheAncestorsRequirements (internal/workflow), and
// TestABasisOpeningTakesTheRegistrationsMomentInTheLine (internal/record: GateOpen, the tree's gate
// and its first opening, which the merger's start reads, and TreeGate.OpenedAt). A caller below
// contract 214 is TestACallerBelowTheGateBasisContractIsServedNoTreeRunsAndNoStepOnABasis
// (internal/api): the rig runs no pod and no caller of it states a contract below this daemon's
// own, so no caller here is narrowed.
func TestBasisGateOpensOnAnAncestorApproval(t *testing.T) {
	pause, held := &registrationPause{}, &outboxHold{}
	r := newPersonHandoverRig(t, 8)
	r.tune = func(o *overrides) {
		o.gateRegistrationRead = pause.hook
		o.holdOutbox = held.holds
	}
	r.boot()
	labelled := map[string]any{"labels": []string{"legion"}}
	parentOf := func(title string, extra string) map[string]any {
		return map[string]any{"labels": []string{"legion"}, "spec": requirementsMarkdown(title, "p1", "p2", "p3") + extra}
	}
	reason := basisReasonOf(400)

	// 1. R runs as a root of its own.
	root := r.create(asSession, "Ledger export rewrite", labelled)
	rootGrant := r.admit(root.Key)

	// 2. P, approved by the person at its latest version, labelled and admitted. A session labels
	// it and the person approves it in triage, so neither act is a person's handover.
	parent := r.create(asSession, "Quarterly billing overhaul", parentOf("Billing", ""))
	r.approve(parent.PrimaryArtifactID)
	parentGrant := r.admit(parent.Key)

	// 3. P is set as R's parent, and R's one check serves P2.
	r.linkUnder(root, parent, true)
	p2 := specBlock(parent.Key, "p2")
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2}}))

	// 4. The negative control: P's latest version is one the person has not approved.
	stale := r.editDocument(parent.PrimaryArtifactID, insertParagraph("A later paragraph nobody approved."))
	r.quiet(root.Key, parent.Key)
	messages := len(r.messages(root.Key))
	status, refusal := r.gateCall(basisCall(rootGrant(), root, 1, parent.PrimaryArtifactID, stale, reason))
	if status != http.StatusConflict || refusal.Code != "BASIS_NOT_APPROVED" ||
		!strings.Contains(refusal.Error, fmt.Sprintf("an approval of version %d by user:smoke, older than its latest version %d", stale-1, stale)) {
		t.Fatalf("R's registration on P's unapproved version %d = %d %+v, want 409 BASIS_NOT_APPROVED naming the older approval", stale, status, refusal)
	}
	state, body := settled(t, r.d, "answer its state after the refusal", func(api.State) bool { return true })
	if step := stepOf(state, root.Key); step.Status != api.GatedStepUnregistered || state.Issues[root.Key].Phase != phase.Admitted {
		t.Fatalf("after the refusal %s stands at %s with its spec step %+v, want admitted and unregistered\nstate %s", root.Key, state.Issues[root.Key].Phase, step, body)
	}
	if facts, now := r.gateFacts(root.Key), len(r.messages(root.Key)); facts != 0 || now != messages {
		t.Fatalf("the refusal applied %d gate facts and left %d messages on %s (was %d), want none and no new message", facts, now, root.Key, messages)
	}
	t.Logf("the negative control: %s's registration on %s's unapproved version %d was refused %s: %s", root.Key, parent.Key, stale, refusal.Code, refusal.Error)
	approval := r.approve(parent.PrimaryArtifactID)

	// 5. P's architect registers P's own gate.
	r.registerApprovedGate(parent.Key, parent.PrimaryArtifactID, approval.Version, parentGrant)

	// 6. R's registration on P's approved version, held after its reads, its gate seed held.
	held.hold(root.Key, record.OutboxKindGateSeed)
	reached, release := pause.arm(root.Key)
	answered := r.gateCallAsync(basisCall(rootGrant(), root, 1, parent.PrimaryArtifactID, approval.Version, reason))
	select {
	case <-reached:
	case answer := <-answered:
		t.Fatalf("the registration answered %d %s before it was held: %v", answer.status, answer.body, answer.err)
	case <-time.After(time.Minute):
		t.Fatal("the registration was never held after its Dispatch reads")
	}
	state, body = settled(t, r.d, "answer its state while the registration is held", func(api.State) bool { return true })
	if step := stepOf(state, root.Key); step.Status != api.GatedStepUnregistered || r.gateFacts(root.Key) != 0 {
		t.Fatalf("while the registration is held after its reads, %s's spec step = %+v with %d gate facts, want unregistered and none\nstate %s",
			root.Key, step, r.gateFacts(root.Key), body)
	}
	release()
	var answer gateAnswer
	select {
	case answer = <-answered:
	case <-time.After(time.Minute):
		t.Fatal("the released registration never answered")
	}
	if answer.err != nil || answer.status != http.StatusOK {
		t.Fatalf("R's registration on P's approved version %d = %d %s (%v), want 200", approval.Version, answer.status, answer.body, answer.err)
	}

	// 7 and 8. What the opening reads, its seed held until every read of the open root is made, and
	// R taken to done.
	r.readAncestorOpening(ancestorOpening{root: root, parent: parent, rootVersion: 1, approval: approval, reason: reason, parentLive: true,
		whileOpen: func() {
			if seeds := r.outboxCount(root.Key, record.OutboxKindGateSeed); seeds == 0 {
				t.Fatalf("no gate seed of %s is held in the outbox; the opening was to be read before the seed ran", root.Key)
			}
			held.release(root.Key)
			testwait.Eventually(t, "the held gate seed of "+root.Key+" to run", func() bool {
				return r.outboxCount(root.Key, record.OutboxKindGateSeed) == 0
			})
			settled(t, r.d, "keep the spec step of "+root.Key+" open on the basis after its gate seed ran", func(s api.State) bool {
				return stepOf(s, root.Key).Status == api.GatedStepOpenOnBasis
			})
			t.Logf("%s's gate seed ran after the opening and left the step open on %s's approval", root.Key, parent.Key)
		}})

	// 9a. P's approval carried by Dispatch in the person's name.
	carriedRoot := r.create(asSession, "Harbor crane scheduler", labelled)
	carriedGrant := r.admit(carriedRoot.Key)
	carriedParent := r.create(asSession, "Shipping manifest platform", parentOf("Shipping", personHandoverBlock))
	ask := r.blockAsk(carriedParent.Key, "wave")
	before := r.approve(carriedParent.PrimaryArtifactID)
	r.answerBlock(ask, personHandoverAnswer)
	carried := r.carriedReview(carriedParent.PrimaryArtifactID, before.Version)
	carriedParentGrant := r.admit(carriedParent.Key)
	r.linkUnder(carriedRoot, carriedParent, true)
	r.writeDocument(carriedRoot.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(carriedParent.Key, "p2")}}))
	r.registerApprovedGate(carriedParent.Key, carriedParent.PrimaryArtifactID, carried.Version, carriedParentGrant)
	r.registerBasis(carriedGrant(), carriedRoot, 1, carriedParent, carried.Version, reason)
	t.Logf("Dispatch carried the person's approval of version %d of %s to version %d, and %s registered on it", before.Version, carriedParent.Key, carried.Version, carriedRoot.Key)
	r.readAncestorOpening(ancestorOpening{root: carriedRoot, parent: carriedParent, rootVersion: 1, approval: carried, reason: reason, parentLive: true})

	// 9b. P labelled and on the backlog: no live tree while R opens, so the link posts no message
	// on R and queues no notice to an architect of P's tree, no resting-root row is queued, and no
	// architect of P runs until P's admission, whose start task names R.
	backlogRoot := r.create(asSession, "Glacier telemetry probe", labelled)
	backlogGrant := r.admit(backlogRoot.Key)
	backlogParent := r.create(asSession, "Arctic sensor network", parentOf("Sensors", ""))
	backlogApproval := r.approve(backlogParent.PrimaryArtifactID)
	// The daemon reads the issue a person's approval is on as Dispatch shows it when the event is
	// delivered (workflow's personHandoverQueues), so the approval is applied while P is still in
	// triage: one read after the move to the backlog would be the person's handover of P.
	r.applied(r.newestEvent(backlogParent.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == backlogApproval.Version }))
	r.setStatus(backlogParent.Key, "backlog")
	r.linkUnder(backlogRoot, backlogParent, false)
	r.writeDocument(backlogRoot.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(backlogParent.Key, "p2")}}))
	r.registerBasis(backlogGrant(), backlogRoot, 1, backlogParent, backlogApproval.Version, reason)
	r.readAncestorOpening(ancestorOpening{root: backlogRoot, parent: backlogParent, rootVersion: 1, approval: backlogApproval, reason: reason})

	// 9c. R holding the person's approval of v2 of its own document while at v4.
	olderRoot := r.create(asSession, "Violin tuning assistant", labelled)
	olderGrant := r.admit(olderRoot.Key)
	second := r.editDocument(olderRoot.PrimaryArtifactID, insertParagraph("The assistant listens before it tunes."))
	if approved := r.approve(olderRoot.PrimaryArtifactID); approved.Version != second {
		t.Fatalf("the person approved %s's spec at version %d, want %d", olderRoot.Key, approved.Version, second)
	}
	r.editDocument(olderRoot.PrimaryArtifactID, insertParagraph("It tunes one string at a time."))
	fourth := r.editDocument(olderRoot.PrimaryArtifactID, insertParagraph("It stops when the string holds."))
	olderParent := r.create(asSession, "Orchestra rehearsal planner", parentOf("Rehearsals", ""))
	olderApproval := r.approve(olderParent.PrimaryArtifactID)
	olderParentGrant := r.admit(olderParent.Key)
	r.linkUnder(olderRoot, olderParent, true)
	r.writeDocument(olderRoot.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(olderParent.Key, "p2")}}))
	r.registerApprovedGate(olderParent.Key, olderParent.PrimaryArtifactID, olderApproval.Version, olderParentGrant)
	r.registerBasis(olderGrant(), olderRoot, fourth, olderParent, olderApproval.Version, reason)
	r.readAncestorOpening(ancestorOpening{root: olderRoot, parent: olderParent, rootVersion: fourth, approval: olderApproval, reason: reason, parentLive: true,
		ownApproval: second})
	t.Logf("%s, holding the person's approval of version %d and at version %d, opened at %d on %s's approval, and no reader names version %d",
		olderRoot.Key, second, fourth, fourth, olderParent.Key, second)
}

// rootSnapshot is what a registration of root would change, as the daemon records it and Dispatch
// shows it: the record's phase, status, generation, view fact and Dispatch sequence, its gate rows,
// the gate facts applied for it, and the issue's sequence and status in Dispatch.
type rootSnapshot struct {
	phase          phase.Phase
	status         string
	generation     uint64
	viewOf         string
	lastSeq        int64
	gates          map[record.DesignStep]record.DesignGate
	gateFacts      int
	dispatchSeq    int64
	dispatchStatus string
}

func (r *personHandoverRig) snapshot(key string) rootSnapshot {
	r.t.Helper()
	recorded := r.record(key)
	shown := r.issue(key)
	return rootSnapshot{phase: recorded.Phase, status: recorded.Status, generation: recorded.Generation, viewOf: recorded.ViewOf,
		lastSeq: recorded.LastDispatchSeq, gates: r.gates(key), gateFacts: r.gateFacts(key), dispatchSeq: shown.LastSeq, dispatchStatus: shown.Status}
}

// basisCases are the two verdicts a case of k2 is held to on one root: refused, with its code and
// what its message names, recording nothing and leaving the root's record and Dispatch issue as
// they were; or opened, answering 200, with the step the registration left read by check.
type basisCases struct {
	r    *personHandoverRig
	root scratchIssue
}

// dispatchRowsDone waits until the outbox holds none of key's rows that read or write Dispatch for
// it: a status, a message an opening posts, the gate documents' publish, a gate seed's read and
// what it applies, a pending-basis or re-parent seed. Notices and role messages stay queued (the
// rig's Envoy answers nothing) and read nothing of Dispatch, so they are left out.
func (r *personHandoverRig) dispatchRowsDone(key string) {
	t := r.t
	t.Helper()
	testwait.Eventually(t, "the outbox to finish its Dispatch rows of "+key, func() bool {
		var rows int
		if err := r.pool.QueryRow(t.Context(), `select count(*) from outbox where issue = $1 and kind = any($2)`, key,
			[]string{string(record.OutboxKindDispatchStatus), string(record.OutboxKindDispatchMessage), string(record.OutboxKindDesignGatesPublish),
				string(record.OutboxKindGateSeed), string(record.OutboxKindPendingBasisSeed), string(record.OutboxKindReparentSeed)}).Scan(&rows); err != nil {
			t.Fatalf("count the Dispatch rows of %s: %v", key, err)
		}
		return rows == 0
	})
}

func (c basisCases) refused(name string, call map[string]any, status int, code string, says ...string) {
	t := c.r.t
	t.Helper()
	// The daemon's own writes to Dispatch for the root run first, so nothing of them lands
	// between the two snapshots.
	c.r.dispatchRowsDone(c.root.Key)
	c.r.quiet(c.root.Key)
	before := c.r.snapshot(c.root.Key)
	got, failure := c.r.gateCall(call)
	if got != status || failure.Code != code {
		t.Fatalf("%s: the registration of %s = %d %+v, want %d %s", name, c.root.Key, got, failure, status, code)
	}
	for _, want := range says {
		if !strings.Contains(failure.Error, want) {
			t.Errorf("%s: the refusal %q does not name %q", name, failure.Error, want)
		}
	}
	if after := c.r.snapshot(c.root.Key); !reflect.DeepEqual(before, after) {
		t.Fatalf("%s: the refused registration changed %s: before %+v, after %+v", name, c.root.Key, before, after)
	}
	t.Logf("%s: refused %d %s: %s", name, got, failure.Code, failure.Error)
}

func (c basisCases) opens(name string, call map[string]any, check func(record.DesignGate) bool) record.DesignGate {
	t := c.r.t
	t.Helper()
	if got, failure := c.r.gateCall(call); got != http.StatusOK {
		t.Fatalf("%s: the registration of %s = %d %+v, want 200", name, c.root.Key, got, failure)
	}
	gate := c.r.gates(c.root.Key)[record.DesignStepSpec]
	if !record.GateOpen(gate) || !check(gate) {
		t.Fatalf("%s: the spec step of %s = %+v (basis %+v), want it open as the case says", name, c.root.Key, gate, gate.Basis)
	}
	t.Logf("%s: opened at version %d (basis %+v)", name, gate.LatestVersion, gate.Basis)
	return gate
}

// onBasis checks a step open on version of issue's primary document whose checks serve served,
// pinned to that version, where served names any.
func onBasis(issue string, version int, served ...string) func(record.DesignGate) bool {
	return func(gate record.DesignGate) bool {
		return gate.OnBasis() && gate.Basis.Issue == issue && gate.Basis.Version == version && (len(served) == 0 || slices.Equal(gate.Basis.Serves, served))
	}
}

// k2's rig half (dispatch://INFRA-3320, R1 and R2). The daemon opens a step on a basis only for a
// root that is a view on the nearest reviewed ancestor, on a person's approval of that ancestor's
// latest version, named in the call, where the ancestor carries legion and has not been accepted,
// for the latest version of a root's document whose newest review is no changes request and which
// has a Verification document beside it, with a reason inside its bound. Every case is the world
// k2's negative control names with that one case put in: P labelled, in progress and unrecorded,
// marking P1 to P3 and P:4 (whose id a reference percent-encodes) in its primary document, which
// the person approved at its latest version; R a root that ran, under P, with no requirement block
// and one check serving P2; and a reason of exactly 1,000 characters. Each refusal is its own code
// naming what Dispatch shows, and R's record and its Dispatch issue are as they were before the
// call; that world itself opens the step first, so each refusal comes from its case. A case that
// needs a check Dispatch refuses to write (CheckBlockPointers) is built as the verification file
// says: the check is written while R's parent is the issue it names, or before the named block of
// P is retyped or deleted in a version the person then approves.
//
// The cases the rig cannot build are the fake client's and the workflow's: an approval a session
// or Dispatch itself gave, and the criteria step, in TestBasisGateRefusals and
// TestTheCriteriaStepsBasisGateIsTheAncestorsVerificationDocument (internal/api); a check that
// serves a paragraph block of P's primary document, in TestBasisGateRefusals (internal/api), since
// every API route of the scratch Dispatch that writes R's Verification document refuses a
// `serves` pointer it introduces at a block that is no requirement (pmdoc.CheckBlockPointers), and no
// route gives a paragraph an id its writer chose (a paragraph's id is minted, and a retype keeps
// an id only into a typed block, pmdoc.RetypeBlock), so no check can come to name a paragraph the
// way kn comes to name a callout; and R waiting on a person, in
// TestARegistrationOnABasisAnswersAWaitingRootAsItLeavesTheStep (internal/workflow).
// P's live text after its approved version cannot be written on the rig, since every route of the
// scratch Dispatch that writes a document writes a version; the fake client holds it, in
// TestBasisGateRefusals and TestBasisGateOpenings (internal/api), where the view is judged at the
// approved version and not at the live text.
//
// The drop seam (overrides.dropConsumedEvent) is used once: the person's approval of R's latest
// version reaches the daemon as an event it drops, and the registration's own read carries that
// approval, opening the step on it and recording no basis.
func TestBasisGateRefusals(t *testing.T) {
	drop := &consumerDrop{}
	r := newPersonHandoverRig(t, 3)
	r.tune = func(o *overrides) { o.dropConsumedEvent = drop.drops }
	r.boot()
	labelled := map[string]any{"labels": []string{"legion"}}

	// R, a root that ran.
	root := r.create(asSession, "Lighthouse beacon controller", labelled)
	grant := r.admit(root.Key)

	// P: labelled by a session and in progress, so the daemon records nothing of it; its primary
	// document at version 2 marks P1 to P3, P:4, PN and PGONE, and the person approved it. Its
	// Verification document serves its own P1, and its decisions document marks D1.
	parent := r.create(asSession, "Coastal navigation program", map[string]any{"labels": []string{"legion"},
		"spec": requirementsMarkdown("Navigation", "p1", "p2", "p3", "p:4", "pn", "pgone")})
	r.setStatus(parent.Key, "in_progress")
	r.writeDocument(parent.Key, "decisions.md", requirementsMarkdown("Decisions", "d1"))
	parentVerification := r.writeDocument(parent.Key, "verification.md", checksMarkdown(rigCheck{id: "pk1", serves: []string{specBlock(parent.Key, "p1")}}))
	r.editDocument(parent.PrimaryArtifactID, insertParagraph("The program keeps its old charts."))
	pv := r.approve(parent.PrimaryArtifactID).Version

	// S, R's sibling under P, marking S1.
	sibling := r.create(asSession, "Tidal gauge calibration", map[string]any{"parent": parent.Key, "spec": requirementsMarkdown("Gauges", "s1")})

	// R under P, its one check serving P2.
	r.patch(root.Key, asPerson, map[string]any{"parent": parent.Key})
	p2 := specBlock(parent.Key, "p2")
	onlyP2 := checksMarkdown(rigCheck{id: "k1", serves: []string{p2}})
	r.writeDocument(root.Key, "verification.md", onlyP2)
	rv := 1
	reason := basisReasonOf(1000)
	call := func() map[string]any { return basisCall(grant(), root, rv, parent.PrimaryArtifactID, pv, reason) }
	cases := basisCases{r: r, root: root}
	pinned := func(id string) string { return fmt.Sprintf("%s@v%d", specBlock(parent.Key, id), pv) }

	// The case taken away: the world itself opens the step.
	cases.opens("the world with its case taken away, a reason of 1,000 characters", call(), onBasis(parent.Key, pv, pinned("p2")))
	r.oneBasisMessage(root.Key)

	// The call.
	cases.refused("no reason", with(call(), "reason", nil), http.StatusBadRequest, "BASIS_INCOMPLETE", "leaves out reason")
	cases.refused("a reason of 1,001 characters", with(call(), "reason", basisReasonOf(1001)), http.StatusBadRequest, "REASON_INVALID", "1001 characters", "1 to 1000")
	cases.refused("a reason holding a decision block", with(call(), "reason", "The checks serve P2.\n\n"+personHandoverBlock), http.StatusBadRequest,
		"REASON_HOLDS_DECISION_BLOCK", "holds a decision block")

	// The chain and the ancestor's document.
	cases.refused("a sibling's document", with(with(call(), "basisArtifactId", sibling.PrimaryArtifactID), "basisVersion", 1), http.StatusConflict,
		"BASIS_NOT_ON_CHAIN", sibling.PrimaryArtifactID, "on "+sibling.Key, root.Key+" under "+parent.Key)
	cases.refused("P's Verification document for the spec step", with(with(call(), "basisArtifactId", parentVerification), "basisVersion", 1), http.StatusConflict,
		"GATE_STEP_DOCUMENT_MISMATCH", parentVerification, "spec step's document of "+parent.Key)
	cases.refused("a version other than P's approved one", with(call(), "basisVersion", pv-1), http.StatusConflict,
		"BASIS_VERSION_MISMATCH", fmt.Sprintf("approved at its latest version %d", pv), fmt.Sprintf("names version %d", pv-1))

	// P's reviews.
	r.requestChanges(parent.PrimaryArtifactID, "Drop P3.")
	cases.refused("P's latest review a changes request", call(), http.StatusConflict, "BASIS_NOT_APPROVED",
		fmt.Sprintf("a changes request on version %d by user:smoke", pv))
	r.approve(parent.PrimaryArtifactID)
	newer := r.editDocument(parent.PrimaryArtifactID, insertParagraph("The charts move to the new datum."))
	cases.refused("the person's approval of a version older than P's latest", with(call(), "basisVersion", newer), http.StatusConflict, "BASIS_NOT_APPROVED",
		fmt.Sprintf("an approval of version %d by user:smoke, older than its latest version %d", pv, newer))
	pv = r.approve(parent.PrimaryArtifactID).Version

	// R's own documents.
	rv = r.editDocument(root.PrimaryArtifactID, insertAtEnd(":::requirement{#r1}\n**R1.** The beacon's own requirement.\n:::\n"))
	cases.refused("R's primary document holding a requirement block", call(), http.StatusConflict, "VIEW_HOLDS_REQUIREMENT",
		root.Key+"'s primary document", "holds requirement block r1")
	cases.refused("a version of R's document below its latest", with(call(), "version", rv-1), http.StatusConflict, "ROOT_VERSION_NOT_LATEST",
		fmt.Sprintf("at version %d, and the call names version %d", rv, rv-1))
	rv = r.editDocument(root.PrimaryArtifactID, map[string]string{"op": "delete", "block": "r1"})
	rv = r.editDocument(root.PrimaryArtifactID, insertAtEnd(fmt.Sprintf(":::check{#ks serves=\"%s\" surface=\"api\"}\nA check in the spec.\n:::\n", p2)))
	cases.refused("a check block in R's primary document that serves P2", call(), http.StatusConflict, "VIEW_CHECK_OUTSIDE_VERIFICATION",
		root.Key+"'s primary document", "holds check block ks")
	rv = r.editDocument(root.PrimaryArtifactID, map[string]string{"op": "delete", "block": "ks"})
	r.writeDocument(root.Key, "decisions.md", requirementsMarkdown("Decisions", "rd1"))
	cases.refused("a requirement block in R's decisions document", call(), http.StatusConflict, "VIEW_HOLDS_REQUIREMENT",
		root.Key+"'s decisions document", "holds requirement block rd1")
	r.writeDocument(root.Key, "decisions.md", "# Decisions\n\nNone yet.\n")
	r.writeDocument(root.Key, "verification.md", onlyP2+":::requirement{#rv1}\n**RV1.** The beacon's own requirement.\n:::\n")
	cases.refused("a requirement block in R's Verification document", call(), http.StatusConflict, "VIEW_HOLDS_REQUIREMENT",
		root.Key+"'s Verification document", "holds requirement block rv1")
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2}}, rigCheck{id: "k2"}))
	cases.refused("a check of R that serves nothing", call(), http.StatusConflict, "VIEW_CHECK_SERVES_NOTHING", "check k2 of "+root.Key)

	// Checks Dispatch would refuse under P: S's requirement is written while R stands under S.
	s1 := specBlock(sibling.Key, "s1")
	r.patch(root.Key, asPerson, map[string]any{"parent": sibling.Key})
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2}}, rigCheck{id: "k2", serves: []string{s1}}))
	r.patch(root.Key, asPerson, map[string]any{"parent": parent.Key})
	cases.refused("one check serving P2 and another serving only a requirement of S", call(), http.StatusConflict, "VIEW_SERVES_OUTSIDE_BASIS",
		"check k2", "serves "+s1)
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k2", serves: []string{s1}}))
	cases.refused("a check of R that serves a requirement block of S", call(), http.StatusConflict, "VIEW_SERVES_OUTSIDE_BASIS", "check k2", "serves "+s1)

	// A block of P retyped and one deleted after R's checks named them, in a version the person
	// approved.
	pn, pgone := specBlock(parent.Key, "pn"), specBlock(parent.Key, "pgone")
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2}}, rigCheck{id: "kn", serves: []string{pn}},
		rigCheck{id: "kg", serves: []string{pgone}}))
	r.editDocument(parent.PrimaryArtifactID, map[string]string{"op": "delete", "block": "pn"}, map[string]string{"op": "delete", "block": "pgone"},
		insertAtEnd(":::callout{#pn}\nA note that is no requirement.\n:::\n"))
	pv = r.approve(parent.PrimaryArtifactID).Version
	cases.refused("a check that serves a block of P's primary document that is no requirement", call(), http.StatusConflict, "VIEW_SERVES_OUTSIDE_BASIS",
		"check kn", "serves "+pn, "p1, p2, p3, p:4")
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2}}, rigCheck{id: "kg", serves: []string{pgone}}))
	cases.refused("a check that serves a block id P's document does not hold", call(), http.StatusConflict, "VIEW_SERVES_OUTSIDE_BASIS",
		"check kg", "serves "+pgone)
	decided := "dispatch://" + parent.Key + "/artifact/decisions-md#b-d1"
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{decided}}))
	cases.refused("a check that serves a requirement block of P's decisions document", call(), http.StatusConflict, "VIEW_SERVES_OUTSIDE_BASIS",
		"serves "+decided)

	// The checks that open.
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{p2 + "@v1"}}))
	cases.opens("a check whose serves names P2 pinned to another version", call(), onBasis(parent.Key, pv, pinned("p2")))
	// The step closed at R's later versions and this registration opened it again, which moves the
	// tree's standing gate: the tree's roles are told it opened on the ancestor's approval, and
	// nothing they are told says a person approved R's document.
	opened := fmt.Sprintf("The spec step stands open on an ancestor's approval: a person approved version %d of the primary document of %s", pv, parent.Key)
	ownVersion := fmt.Sprintf("No person approved version %d of its spec document", rv)
	roleMessages := r.roleMessages(root.Key)
	if !slices.ContainsFunc(roleMessages, func(body string) bool { return strings.Contains(body, opened) && strings.Contains(body, ownVersion) }) {
		t.Fatalf("the role messages of %s's tree = %q, want the opening naming %q and %q", root.Key, roleMessages, opened, ownVersion)
	}
	for _, body := range roleMessages {
		if strings.Contains(body, "a person approved version "+fmt.Sprint(rv)+" of its") {
			t.Fatalf("a role message of %s's tree says a person approved its own version %d: %q", root.Key, rv, body)
		}
	}
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(parent.Key, "p1"), specBlock(parent.Key, "p3")}}))
	cases.opens("a check that serves P1 and P3", call(), onBasis(parent.Key, pv, pinned("p1"), pinned("p3")))
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(parent.Key, "p:4")}}))
	cases.opens("a check whose serves names a block id that a reference percent-encodes", call(), onBasis(parent.Key, pv, pinned("p:4")))
	if pinned("p:4") != fmt.Sprintf("dispatch://%s/spec#b-p%%3A4@v%d", parent.Key, pv) {
		t.Fatalf("the reference to P:4 reads %s, want its id percent-encoded", pinned("p:4"))
	}
	r.writeDocument(root.Key, "verification.md", onlyP2)

	// R's own reviews. The person's approval of R's latest version reaches the daemon as an event
	// it drops, so the step stands on the basis until the registration reads that approval.
	// A gate seed still queued from the openings above would read that approval and apply it, so
	// they run first: the live event is then the approval's one carrier before the registration.
	r.dispatchRowsDone(root.Key)
	drop.arm(func(fact intake.Fact) bool {
		artifact, ok := fact.(intake.DispatchArtifact)
		return ok && artifact.ArtifactID == root.PrimaryArtifactID && artifact.Kind == intake.DispatchArtifactApproved
	})
	own := r.approve(root.PrimaryArtifactID)
	r.applied(r.newestEvent(root.Key, "artifact.approved", func(e scratchEvent) bool { return e.version() == own.Version }))
	if dropped := drop.count(); dropped != 1 {
		t.Fatalf("the consumer dropped %d facts, want the person's approval of %s's version %d", dropped, root.Key, own.Version)
	}
	if gate := r.gates(root.Key)[record.DesignStepSpec]; !gate.OnBasis() {
		t.Fatalf("after the dropped approval the spec step of %s = %+v, want it still open on the basis: no live event moved it", root.Key, gate)
	}
	cases.opens("R's latest version approved by the person, with a basis named as well", call(), func(gate record.DesignGate) bool {
		return gate.Basis == nil && gate.ApprovedVersion != nil && *gate.ApprovedVersion == rv
	})
	rv = r.editDocument(root.PrimaryArtifactID, insertParagraph("The beacon dims at dawn."))
	cases.opens("R holding the person's approval of an older version of its own document", call(), func(gate record.DesignGate) bool {
		kept := gate.ApprovedByPerson()
		return onBasis(parent.Key, pv, pinned("p2"))(gate) && kept != nil && kept.Version == rv-1 && *gate.ApprovedVersion == rv
	})
	r.requestChanges(root.PrimaryArtifactID, "Name the beacon's fallback.")
	cases.refused("a person's changes request as R's newest review", call(), http.StatusConflict, "ROOT_CHANGES_REQUESTED",
		fmt.Sprintf("changes request on version %d by user:smoke", rv))
	rv = r.editDocument(root.PrimaryArtifactID, insertParagraph("The beacon falls back to the old lamp."))
	r.requestApproval(root.PrimaryArtifactID, "The beacon names its fallback now.")
	cases.refused("a changes request on R's last version followed by a version with an open approval request", call(), http.StatusConflict,
		"ROOT_CHANGES_REQUESTED", fmt.Sprintf("changes request on version %d by user:smoke", rv-1), "whatever its approval shows")
	r.approve(root.PrimaryArtifactID)
	rv = r.editDocument(root.PrimaryArtifactID, insertParagraph("The old lamp is tested weekly."))
	cases.opens("a changes request on one version followed by the person's approval of the next, with R one past it", call(), func(gate record.DesignGate) bool {
		kept := gate.ApprovedByPerson()
		return onBasis(parent.Key, pv, pinned("p2"))(gate) && kept != nil && kept.Version == rv-1
	})

	// The chain: R under P under G, where only G is approved and marks requirements.
	grandparent := r.create(asSession, "Maritime safety initiative", map[string]any{"labels": []string{"legion"}, "spec": requirementsMarkdown("Safety", "g1", "g2")})
	gv := r.approve(grandparent.PrimaryArtifactID).Version
	middle := r.create(asSession, "Buoy maintenance roster", map[string]any{"labels": []string{"legion"}, "parent": grandparent.Key})
	chainRoot := r.create(asSession, "Foghorn volume limiter", labelled)
	chainGrant := r.admit(chainRoot.Key)
	r.patch(chainRoot.Key, asPerson, map[string]any{"parent": middle.Key})
	chain := basisCases{r: r, root: chainRoot}
	onG := func() map[string]any {
		return basisCall(chainGrant(), chainRoot, 1, grandparent.PrimaryArtifactID, gv, reason)
	}
	chain.refused("no Verification document on R", onG(), http.StatusConflict, "VIEW_NO_VERIFICATION", "no Verification document of "+chainRoot.Key)
	chain.refused("P's primary document when no review stands on it", basisCall(chainGrant(), chainRoot, 1, middle.PrimaryArtifactID, 1, reason),
		http.StatusConflict, "BASIS_NOT_APPROVED", "no review and no open approval request at its latest version 1")
	r.writeDocument(chainRoot.Key, "verification.md", "# Verification\n\nNo check yet.\n")
	chain.refused("R's Verification document holding no check", onG(), http.StatusConflict, "VIEW_NO_CHECK", "holds no check block")
	g1 := specBlock(grandparent.Key, "g1")
	r.writeDocument(chainRoot.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{g1}}))
	chain.opens("G's approval with P neither reviewed nor requested", onG(), onBasis(grandparent.Key, gv, fmt.Sprintf("%s@v%d", g1, gv)))
	r.requestApproval(middle.PrimaryArtifactID, "The roster is ready.")
	chain.refused("G's approval with only an open approval request on P", onG(), http.StatusConflict, "BASIS_NOT_NEAREST",
		middle.Key+" stands between "+chainRoot.Key+" and "+grandparent.Key, "open approval request")
	r.requestChanges(middle.PrimaryArtifactID, "Add the winter schedule.")
	chain.refused("G's approval with a review on P", onG(), http.StatusConflict, "BASIS_NOT_NEAREST",
		middle.Key+" stands between "+chainRoot.Key+" and "+grandparent.Key, "a changes request on version 1 by user:smoke")

	// The ancestor's status, each put back after its case. Every approval of P is applied first,
	// while P is in progress, so none is read as the person's handover of a P on the backlog.
	r.quiet(parent.Key)
	r.setStatus(parent.Key, "backlog")
	cases.opens("P on the backlog", call(), onBasis(parent.Key, pv))
	r.setStatus(parent.Key, "icebox")
	cases.opens("P in icebox", call(), onBasis(parent.Key, pv))
	r.setStatus(parent.Key, "in_progress")
	r.patch(parent.Key, asPerson, map[string]any{"labels": []string{"waves"}})
	cases.refused("P without the legion label", call(), http.StatusConflict, "BASIS_NOT_IN_LEGION", parent.Key+" carries no \"legion\" label", "waves")
	r.addLabel(parent.Key, asSession, "legion")
	r.setStatus(parent.Key, "retro")
	cases.refused("P in Dispatch status retro", call(), http.StatusConflict, "BASIS_ACCEPTED", "Dispatch shows "+parent.Key+" retro")
	r.setStatus(parent.Key, "done")
	cases.refused("P set done", call(), http.StatusConflict, "BASIS_ACCEPTED", "Dispatch shows "+parent.Key+" done")
	r.setStatus(parent.Key, "in_progress")
	r.editDocument(parent.PrimaryArtifactID, map[string]string{"op": "delete", "block": "p1"}, map[string]string{"op": "delete", "block": "p2"},
		map[string]string{"op": "delete", "block": "p3"}, map[string]string{"op": "delete", "block": "p:4"})
	pv = r.approve(parent.PrimaryArtifactID).Version
	cases.refused("P's primary document with no requirement block", call(), http.StatusConflict, "BASIS_NO_REQUIREMENTS",
		fmt.Sprintf("at version %d marks no requirement block", pv))
	// The daemon's record of P in merging, written as mergeClosesTheIssue writes one, once the
	// daemon has applied every event of P made so far: a close of P applied after the write
	// (the "P set done" case's) leaves the tree and writes P's record at done (workflow's leave),
	// so the case would read a P the daemon no longer records as accepted.
	r.quiet(parent.Key)
	putOutboxIssue(t, r.pool, record.NewStore(), record.Issue{Key: parent.Key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Tree: parent.Key, Project: r.cfg.Project, Title: parent.Key, Phase: phase.Merging, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, Rank: "U"})
	cases.refused("P recorded by the daemon in merging", call(), http.StatusConflict, "BASIS_ACCEPTED", "the daemon records "+parent.Key+" in merging")
}

// crossProjectRigs are two projects' daemons on one scratch Dispatch and one NATS server, each
// with a database of its own on the test server, as a deployment runs one daemon for each of its
// projects: legion of LEGION, the root's own project, and other of OTHR, another project. Neither
// is booted.
func crossProjectRigs(t *testing.T, admissionCap int) (legion, other *personHandoverRig) {
	t.Helper()
	natsURL := testnats.URL(t)
	scratch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})
	return newProjectRigOn(t, scratch, natsURL, "LEGION", "acme/widgets", admissionCap),
		newProjectRigOn(t, scratch, natsURL, "OTHR", "acme/gadgets", admissionCap)
}

// linkAcross writes child's Dispatch parent as parent, "" for none, straight into the scratch
// Dispatch's database: this checkout's Dispatch refuses a parent of another project on every route
// (validateIssueParent), so a link across projects is a row the proof writes there itself. It
// makes no event, so the daemon of neither project hears of the link. It answers an error rather
// than failing the test, so a seam the daemon runs on a goroutine of its own can call it.
func (r *personHandoverRig) linkAcross(ctx context.Context, child, parent string) error {
	conn, err := pgx.Connect(ctx, r.scratch.database)
	if err != nil {
		return fmt.Errorf("open the scratch Dispatch's database: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var link *string
	if parent != "" {
		link = &parent
	}
	tag, err := conn.Exec(ctx, `update issues set parent_key = $2 where key = $1`, child, link)
	if err != nil {
		return fmt.Errorf("set the parent of %s to %q: %w", child, parent, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("set the parent of %s to %q: %d rows changed, want 1", child, parent, tag.RowsAffected())
	}
	return nil
}

// mustLinkAcross is linkAcross on the test's own goroutine, failing the test on an error.
func (r *personHandoverRig) mustLinkAcross(child, parent string) {
	r.t.Helper()
	if err := r.linkAcross(r.t.Context(), child, parent); err != nil {
		r.t.Fatal(err)
	}
}

// controllerTick has the daemon apply one controller tick now, through the moments the rig hands
// its runtime (overrides.controllerTicks), and waits until the daemon has applied it.
func (r *personHandoverRig) controllerTick(ticks chan<- time.Time) {
	r.t.Helper()
	before := controllerTicks(r.t, r.pool)
	ticks <- time.Now()
	testwait.Eventually(r.t, "the daemon to apply the controller tick", func() bool {
		return controllerTicks(r.t, r.pool) > before
	})
}

// architectCall sends body to path as an architect's plugin does and answers its status and, for
// a refusal, the code and the message it names.
func (r *personHandoverRig) architectCall(path string, body map[string]any) (int, api.Failure) {
	r.t.Helper()
	status, answer := r.d.request(http.MethodPost, path, body, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(answer, &failure); err != nil {
			r.t.Fatalf("decode the refusal %d %s: %v", status, answer, err)
		}
	}
	return status, failure
}

// seamAnswer is what a proof's act inside a seam of the daemon came to: the registration it made,
// or why it could not make it.
type seamAnswer struct {
	gate gateAnswer
	err  error
}

// seamAct is a proof's act that a seam of the daemon runs once, for the issue it is armed for,
// on whatever goroutine the seam runs on: a route's or an outbox lane's.
type seamAct struct {
	mu     sync.Mutex
	issue  string
	act    func() seamAnswer
	ran    bool
	answer seamAnswer
}

// arm sets act to run at the next seam that names issue.
func (s *seamAct) arm(issue string, act func() seamAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issue, s.act = issue, act
}

// run is the seam's call: it runs the armed act once, for its issue, and holds the seam until the
// act is done.
func (s *seamAct) run(issue string) {
	s.mu.Lock()
	if s.act == nil || s.issue != issue {
		s.mu.Unlock()
		return
	}
	act := s.act
	s.act = nil
	s.mu.Unlock()
	answer := act()
	s.mu.Lock()
	s.ran, s.answer = true, answer
	s.mu.Unlock()
}

// result is whether the act has run, and what it came to.
func (s *seamAct) result() (bool, seamAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ran, s.answer
}

// statusWriteOf is the Dispatch status write row carries, and false for a row of another kind.
func statusWriteOf(row record.OutboxRow) (record.StatusWrite, bool) {
	if row.Kind != record.OutboxKindDispatchStatus {
		return record.StatusWrite{}, false
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return record.StatusWrite{}, false
	}
	write, ok := payload.(record.StatusWrite)
	return write, ok
}

// c5's first half (R1 across projects): a registration is refused once another project's daemon
// accepts the ancestor, at both moments that follow the ancestor's sign_off. OTHR-1 carries legion,
// marks two requirement blocks, holds the person's approval of its latest spec and stands in
// accepting in OTHR's daemon, with no child. LEGION-1, labelled and admitted in LEGION's daemon,
// has a spec that is a view on OTHR-1 and a Verification document whose one check serves OTHR-1's
// first block, written while LEGION-1 stood under OTHR-1, since Dispatch takes a check's pointer
// only at a block of an issue above it; LEGION-1 then has no parent, so OTHR-1's retro write meets
// no open child. OTHR's architect signs OTHR-1 off, and at the subtest's seam of OTHR's daemon the
// proof puts LEGION-1 under OTHR-1 and LEGION's architect registers LEGION-1's spec step on OTHR-1's
// approved version: (a) after the sign-off's Dispatch status write and before its fact
// (afterSignOffStatusWrite), and (b) after the fact and before the outbox's retro row
// (beforeOutboxStatusWrite). Each asserts that its seam ran, that the registration answers 409
// BASIS_ACCEPTED, and that LEGION-1's gate records nothing.
//
// In (b) OTHR's daemon drops the event of OTHR-1's own retro write: applied before the fact, that
// event would record the retro Dispatch shows, and the fact would then queue no retro row at all,
// which is no window but no seam either. Dropped, the fact always queues the row the seam holds.
func TestRegistrationIsRefusedOnceAnotherProjectsDaemonAcceptsTheAncestor(t *testing.T) {
	for _, seam := range []string{"afterSignOffStatusWrite", "beforeOutboxStatusWrite"} {
		t.Run(seam, func(t *testing.T) {
			legion, other := crossProjectRigs(t, 2)
			act, drop := &seamAct{}, &consumerDrop{}
			switch seam {
			case "afterSignOffStatusWrite":
				other.tune = func(o *overrides) {
					o.afterSignOffStatusWrite = func(_ context.Context, issue string) { act.run(issue) }
				}
			case "beforeOutboxStatusWrite":
				other.tune = func(o *overrides) {
					o.dropConsumedEvent = drop.drops
					o.beforeOutboxStatusWrite = func(row record.OutboxRow) {
						if write, ok := statusWriteOf(row); ok && write.Status == "retro" {
							act.run(row.Issue)
						}
					}
				}
			}
			legion.boot()
			other.boot()

			// OTHR-1, approved by the person at its latest version, admitted in OTHR's daemon and
			// standing in accepting there.
			parent := other.create(asSession, "Quarterly billing overhaul",
				map[string]any{"labels": []string{"legion"}, "spec": requirementsMarkdown("Billing", "o1", "o2")})
			approval := other.approve(parent.PrimaryArtifactID)
			parentGrant := other.admit(parent.Key)
			other.moveRecord(parent.Key, phase.Accepting)

			// LEGION-1, a view on OTHR-1 whose one check serves OTHR-1's first block, with no
			// parent.
			root := legion.create(asSession, "Ledger export rewrite", map[string]any{"labels": []string{"legion"}})
			rootGrant := legion.admit(root.Key)
			legion.mustLinkAcross(root.Key, parent.Key)
			legion.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(parent.Key, "o1")}}))
			legion.mustLinkAcross(root.Key, "")
			if shown := legion.issue(root.Key); shown.Parent != nil {
				t.Fatalf("%s stands under %s in Dispatch, want no parent before the sign-off", root.Key, *shown.Parent)
			}

			grant := rootGrant()
			act.arm(parent.Key, func() seamAnswer {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := legion.linkAcross(ctx, root.Key, parent.Key); err != nil {
					return seamAnswer{err: err}
				}
				return seamAnswer{gate: <-legion.gateCallAsync(basisCall(grant, root, 1, parent.PrimaryArtifactID, approval.Version, basisReasonOf(400)))}
			})
			drop.arm(func(fact intake.Fact) bool {
				issue, ok := fact.(intake.DispatchIssue)
				return ok && issue.Key == parent.Key && issue.Status == "retro"
			})
			status, refusal := other.architectCall("/legion/v1/signoff", map[string]any{"grantId": parentGrant(), "issue": parent.Key})
			if seam == "afterSignOffStatusWrite" {
				if ran, _ := act.result(); !ran {
					t.Fatalf("the sign-off of %s answered %d %+v and the seam %s never ran", parent.Key, status, refusal, seam)
				}
			} else {
				testwait.Eventually(t, "the seam "+seam+" to run for "+parent.Key+"'s retro row", func() bool {
					ran, _ := act.result()
					return ran
				})
			}
			_, answer := act.result()
			if answer.err != nil || answer.gate.err != nil {
				t.Fatalf("the act in the seam %s failed: %v, %v", seam, answer.err, answer.gate.err)
			}
			var failure api.Failure
			if answer.gate.status != http.StatusOK {
				if err := json.Unmarshal(answer.gate.body, &failure); err != nil {
					t.Fatalf("decode the refusal %d %s: %v", answer.gate.status, answer.gate.body, err)
				}
			}
			if answer.gate.status != http.StatusConflict || failure.Code != "BASIS_ACCEPTED" || !strings.Contains(failure.Error, parent.Key) {
				t.Fatalf("%s's registration on %s at the seam %s = %d %s, want 409 BASIS_ACCEPTED naming %s",
					root.Key, parent.Key, seam, answer.gate.status, answer.gate.body, parent.Key)
			}
			if status != http.StatusOK {
				t.Fatalf("the sign-off of %s = %d %+v, want 200", parent.Key, status, refusal)
			}
			if facts, gates := legion.gateFacts(root.Key), legion.gates(root.Key); facts != 0 || len(gates) != 0 {
				t.Fatalf("the refused registration left %d gate facts and the gate rows %+v on %s, want none", facts, gates, root.Key)
			}
			testwait.Eventually(t, "OTHR's daemon to record "+parent.Key+" in retro", func() bool {
				return other.record(parent.Key).Phase == phase.Retro
			})
			t.Logf("at the seam %s, %s's registration on %s was refused %s: %s", seam, root.Key, parent.Key, failure.Code, failure.Error)
		})
	}
}

// R1 across projects against the daemon's own status repair: a controller tick that runs while a
// sign-off stands between its Dispatch write and its fact writes nothing over that write. OTHR-1
// stands in accepting in OTHR's daemon, and LEGION-1 is a view on it, set up as in the test above.
// OTHR's architect signs OTHR-1 off, and at the seam after the sign-off's retro write and before
// its fact (afterSignOffStatusWrite) the proof waits for OTHR's record to hold the event of that
// write, in accepting with Dispatch at retro, which the repair reads as a status to correct to
// needs_review, and then runs one real controller tick. Dispatch must keep retro. Once the sign-off
// has answered and OTHR's daemon records OTHR-1 in retro, Dispatch must still show retro and
// LEGION's architect's registration on OTHR-1's approved version must be refused 409
// BASIS_ACCEPTED. A retro row of OTHR's outbox is held at its seam (beforeOutboxStatusWrite) until
// then, so the proof reads Dispatch as the acceptance left it, and not as a later row repaired it.
func TestARepairTickDuringASignOffWritesNothingOverItsAcceptance(t *testing.T) {
	legion, other := crossProjectRigs(t, 2)
	ticks := make(chan time.Time)
	written := &seamAct{}
	retroRows := make(chan struct{})
	defer close(retroRows)
	other.tune = func(o *overrides) {
		o.controllerTicks = ticks
		o.afterSignOffStatusWrite = func(_ context.Context, issue string) { written.run(issue) }
		o.beforeOutboxStatusWrite = func(row record.OutboxRow) {
			if write, ok := statusWriteOf(row); ok && write.Status == "retro" {
				<-retroRows
			}
		}
	}
	legion.boot()
	other.boot()

	parent := other.create(asSession, "Quarterly billing overhaul",
		map[string]any{"labels": []string{"legion"}, "spec": requirementsMarkdown("Billing", "o1", "o2")})
	approval := other.approve(parent.PrimaryArtifactID)
	parentGrant := other.admit(parent.Key)
	other.moveRecord(parent.Key, phase.Accepting)

	root := legion.create(asSession, "Ledger export rewrite", map[string]any{"labels": []string{"legion"}})
	rootGrant := legion.admit(root.Key)
	legion.mustLinkAcross(root.Key, parent.Key)
	legion.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(parent.Key, "o1")}}))
	legion.mustLinkAcross(root.Key, "")

	written.arm(parent.Key, func() seamAnswer {
		testwait.Eventually(t, "OTHR's record of "+parent.Key+" to hold its sign-off's retro write while it stands in accepting", func() bool {
			shown := other.record(parent.Key)
			return shown.Phase == phase.Accepting && shown.DispatchStatus == "retro"
		})
		other.controllerTick(ticks)
		if testwait.Holds(5*time.Second, testwait.Poll, func() bool { return other.issue(parent.Key).Status != "retro" }) {
			t.Errorf("the controller tick moved %s off its sign-off's retro to %s in Dispatch before the sign-off's fact", parent.Key, other.issue(parent.Key).Status)
		}
		return seamAnswer{}
	})
	if status, refusal := other.architectCall("/legion/v1/signoff", map[string]any{"grantId": parentGrant(), "issue": parent.Key}); status != http.StatusOK {
		t.Fatalf("the sign-off of %s = %d %+v, want 200", parent.Key, status, refusal)
	}
	if ran, _ := written.result(); !ran {
		t.Fatalf("the seam after %s's retro write never ran", parent.Key)
	}
	testwait.Eventually(t, "OTHR's daemon to record "+parent.Key+" in retro", func() bool {
		return other.record(parent.Key).Phase == phase.Retro
	})
	// Each status row OTHR's outbox runs without a hold has settled: the issue's rows are run in
	// order on its lane, and the held retro row, if any, is the newest of them.
	testwait.Eventually(t, "OTHR's outbox to settle every status row of "+parent.Key+" ahead of a held retro row", func() bool {
		var ahead int
		if err := other.pool.QueryRow(t.Context(), `select count(*) from outbox where issue = $1 and kind = $2 and payload->>'status' <> 'retro'`,
			parent.Key, string(record.OutboxKindDispatchStatus)).Scan(&ahead); err != nil {
			t.Fatalf("count the status rows of %s: %v", parent.Key, err)
		}
		return ahead == 0
	})
	if shown := other.issue(parent.Key).Status; shown != "retro" {
		t.Errorf("Dispatch shows %s %s while OTHR's daemon records it accepted, in retro", parent.Key, shown)
	}
	legion.mustLinkAcross(root.Key, parent.Key)
	answer := <-legion.gateCallAsync(basisCall(rootGrant(), root, 1, parent.PrimaryArtifactID, approval.Version, basisReasonOf(400)))
	if answer.err != nil {
		t.Fatalf("the registration of %s on %s failed: %v", root.Key, parent.Key, answer.err)
	}
	var failure api.Failure
	if answer.status != http.StatusOK {
		if err := json.Unmarshal(answer.body, &failure); err != nil {
			t.Fatalf("decode the refusal %d %s: %v", answer.status, answer.body, err)
		}
	}
	if answer.status != http.StatusConflict || failure.Code != "BASIS_ACCEPTED" || !strings.Contains(failure.Error, parent.Key) {
		t.Fatalf("%s's registration on %s once OTHR's daemon recorded its acceptance = %d %s, want 409 BASIS_ACCEPTED naming %s",
			root.Key, parent.Key, answer.status, answer.body, parent.Key)
	}
	if facts, gates := legion.gateFacts(root.Key), legion.gates(root.Key); facts != 0 || len(gates) != 0 {
		t.Fatalf("the refused registration left %d gate facts and the gate rows %+v on %s, want none", facts, gates, root.Key)
	}
	t.Logf("after a controller tick inside %s's sign-off, %s's registration was refused %s: %s", parent.Key, root.Key, failure.Code, failure.Error)
}

// c5's second half (R1 across projects): sign_off and close_root refuse an open child of another
// project filed after the last controller tick, naming it, and sign_off takes the issue once that
// child is in icebox. LEGION-1 stands in accepting after a tick, Dispatch showing needs_review;
// OTHR-5, in triage, is then put under it and no tick runs. LEGION-2 stands at admitted, and
// OTHR-6, in triage, is put under it after the tick. Only LEGION's daemon runs: OTHR is a project
// of the same Dispatch whose daemon this proof needs no part of.
func TestSignOffAndCloseRootRefuseAChildOfAnotherProjectFiledAfterTheTick(t *testing.T) {
	ticks := make(chan time.Time)
	natsURL := testnats.URL(t)
	r := newProjectRigOn(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL}), natsURL, "LEGION", "acme/widgets", 2)
	r.tune = func(o *overrides) { o.controllerTicks = ticks }
	r.boot()
	scratchProject(t, r.baseURL, "OTHR", "acme/gadgets")
	labelled := map[string]any{"labels": []string{"legion"}}
	inOther := func(title string) scratchIssue {
		return r.create(asSession, title, map[string]any{"project": "OTHR"})
	}

	accepting := r.create(asSession, "Ledger export rewrite", labelled)
	acceptingGrant := r.admit(accepting.Key)
	r.setStatus(accepting.Key, "needs_review")
	r.moveRecord(accepting.Key, phase.Accepting)
	admitted := r.create(asSession, "Harbor crane scheduler", labelled)
	admittedGrant := r.admit(admitted.Key)
	r.controllerTick(ticks)
	if standing := r.record(accepting.Key).Phase; standing != phase.Accepting {
		t.Fatalf("%s stands at %s after the tick, want accepting", accepting.Key, standing)
	}

	// OTHR-5 filed under the issue in accepting after the tick; no tick runs.
	filed := inOther("Tidal gauge calibration")
	r.mustLinkAcross(filed.Key, accepting.Key)
	status, refusal := r.architectCall("/legion/v1/signoff", map[string]any{"grantId": acceptingGrant(), "issue": accepting.Key})
	if status != http.StatusConflict || refusal.Code != "SIGNOFF_CHILD_OPEN" || !strings.Contains(refusal.Error, filed.Key) {
		t.Fatalf("the sign-off of %s with %s open under it = %d %+v, want 409 SIGNOFF_CHILD_OPEN naming %s", accepting.Key, filed.Key, status, refusal, filed.Key)
	}
	if standing := r.record(accepting.Key).Phase; standing != phase.Accepting {
		t.Fatalf("%s stands at %s after its refused sign-off, want accepting", accepting.Key, standing)
	}
	if shown := r.issue(accepting.Key).Status; shown != "needs_review" {
		t.Fatalf("Dispatch shows %s %s after its refused sign-off, want needs_review", accepting.Key, shown)
	}
	t.Logf("the sign-off of %s was refused: %s", accepting.Key, refusal.Error)

	// OTHR-5 in icebox: the sign-off takes the issue.
	r.setStatus(filed.Key, "icebox")
	if status, refusal := r.architectCall("/legion/v1/signoff", map[string]any{"grantId": acceptingGrant(), "issue": accepting.Key}); status != http.StatusOK {
		t.Fatalf("the sign-off of %s with %s in icebox = %d %+v, want 200", accepting.Key, filed.Key, status, refusal)
	}
	testwait.Eventually(t, "the daemon to record "+accepting.Key+" in retro", func() bool {
		return r.record(accepting.Key).Phase == phase.Retro
	})
	if shown := r.issue(accepting.Key).Status; shown != "retro" {
		t.Fatalf("Dispatch shows %s %s after its sign-off, want retro", accepting.Key, shown)
	}

	// OTHR-6 filed under the admitted root after the tick.
	under := inOther("Buoy maintenance roster")
	r.mustLinkAcross(under.Key, admitted.Key)
	status, refusal = r.architectCall("/legion/v1/roots/close", map[string]any{"grantId": admittedGrant(), "issue": admitted.Key,
		"reason": "The human decided no change is needed."})
	if status != http.StatusConflict || refusal.Code != "SIGNOFF_CHILD_OPEN" || !strings.Contains(refusal.Error, under.Key) {
		t.Fatalf("the close of %s with %s open under it = %d %+v, want 409 SIGNOFF_CHILD_OPEN naming %s", admitted.Key, under.Key, status, refusal, under.Key)
	}
	if standing := r.record(admitted.Key).Phase; standing != phase.Admitted {
		t.Fatalf("%s stands at %s after its refused close, want admitted", admitted.Key, standing)
	}
	if shown := r.issue(admitted.Key).Status; shown != "in_progress" {
		t.Fatalf("Dispatch shows %s %s after its refused close, want in_progress", admitted.Key, shown)
	}
	t.Logf("the close of %s was refused: %s", admitted.Key, refusal.Error)
}

// c4's basis half (R1 across projects): a step waiting on the open approval request of an
// ancestor of another project opens at the controller tick after the person approves, and a
// changes request on that version reaches the root's own architect at the next tick while the
// step stays open. OTHR-1 carries legion, marks two requirement blocks, and its approval request
// is open on the person; LEGION-1, labelled, admitted and a view under OTHR-1, registers its spec
// step on OTHR-1's requested version, and the step is pending. LEGION's daemon drops every event of
// OTHR-1, so neither the person's approval nor their changes request moves anything until a tick
// reads OTHR-1 again. Nothing of it is keyed by OTHR-1 in LEGION's outbox: OTHR's daemon runs
// OTHR-1's tree. That half can fail only where the daemon records OTHR-1, which this rig's daemon
// never does; TestAGateSeedTellsNoArchitectOfAnotherProject holds the case where it does.
func TestPendingBasisOnAnotherProjectOpensAtTheTick(t *testing.T) {
	ticks := make(chan time.Time)
	natsURL := testnats.URL(t)
	r := newProjectRigOn(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL}), natsURL, "LEGION", "acme/widgets", 2)
	r.tune = func(o *overrides) { o.controllerTicks = ticks }
	r.boot()
	scratchProject(t, r.baseURL, "OTHR", "acme/gadgets")

	// OTHR-1, its approval request open on the person, and LEGION-1 a view under it, pending on
	// that request.
	ancestor := r.create(asSession, "Quarterly billing overhaul",
		map[string]any{"project": "OTHR", "labels": []string{"legion"}, "spec": requirementsMarkdown("Billing", "o1", "o2")})
	ask := r.requestApproval(ancestor.PrimaryArtifactID, "The billing overhaul is ready for review.")
	root := r.create(asSession, "Ledger export rewrite", map[string]any{"labels": []string{"legion"}})
	grant := r.admit(root.Key)
	r.mustLinkAcross(root.Key, ancestor.Key)
	r.writeDocument(root.Key, "verification.md", checksMarkdown(rigCheck{id: "k1", serves: []string{specBlock(ancestor.Key, "o1")}}))
	r.registerBasis(grant(), root, 1, ancestor, 1, basisReasonOf(400))
	settled(t, r.d, root.Key+" to wait on "+ask, func(s api.State) bool {
		step := stepOf(s, root.Key)
		return step.Status == api.GatedStepPendingOnBasis && step.Basis != nil && step.Basis.Issue == ancestor.Key && step.Basis.AskID == ask
	})

	// The person approves OTHR-1. A later event of LEGION-1, applied, follows the approval's own
	// events, which the daemon dropped; no tick has run, and the step still waits.
	ticked := controllerTicks(t, r.pool)
	approval := r.approve(ancestor.PrimaryArtifactID)
	r.patch(root.Key, asSession, map[string]any{"title": "Ledger export rewrite, phase one"})
	r.quiet(root.Key)
	if step := stepOf(r.d.state(), root.Key); step.Status != api.GatedStepPendingOnBasis || controllerTicks(t, r.pool) != ticked {
		t.Fatalf("after the person's approval of %s and before a tick, the spec step of %s = %+v (ticks %d, were %d), want it pending",
			ancestor.Key, root.Key, step, controllerTicks(t, r.pool), ticked)
	}

	// One tick: the step opens on the approval, and the message on LEGION-1 names OTHR-1.
	r.controllerTick(ticks)
	settled(t, r.d, "open the spec step of "+root.Key+" on "+ancestor.Key+"'s approval at the tick", func(s api.State) bool {
		step := stepOf(s, root.Key)
		return step.Status == api.GatedStepOpenOnBasis && step.Basis != nil && step.Basis.Issue == ancestor.Key && step.Basis.Version == approval.Version
	})
	if message := r.oneBasisMessage(root.Key); !strings.Contains(message, "- The ancestor is "+ancestor.Key+".") {
		t.Fatalf("the gate-opened message on %s does not name %s:\n%s", root.Key, ancestor.Key, message)
	}
	t.Logf("%s's step opened at the tick on %s's approval of version %d", root.Key, ancestor.Key, approval.Version)

	// A changes request on that version reaches LEGION-1's architect at the next tick, and the step
	// stays open.
	r.dispatchRowsDone(root.Key)
	r.requestChanges(ancestor.PrimaryArtifactID, "Name the rollback path.")
	r.patch(root.Key, asSession, map[string]any{"title": "Ledger export rewrite, phase two"})
	r.quiet(root.Key)
	if told := r.notices(root.Key, "basis-changes-requested"); len(told) != 0 {
		t.Fatalf("%s's architect was told %+v before a tick read the changes request, want nothing", root.Key, told)
	}
	r.controllerTick(ticks)
	testwait.Eventually(t, "the changes request on "+ancestor.Key+" to reach the architect of "+root.Key, func() bool {
		return len(r.notices(root.Key, "basis-changes-requested")) > 0
	})
	told := r.notices(root.Key, "basis-changes-requested")
	if len(told) != 1 || told[0].Ref != "dispatch://"+ancestor.Key+"/spec" || told[0].Version != approval.Version ||
		!strings.Contains(told[0].Reason, `"Name the rollback path."`) || !strings.Contains(told[0].Reason, "The step stays open.") {
		t.Fatalf("the basis-changes-requested notices to %s's architect = %+v, want one naming %s's version %d and the reason", root.Key, told, ancestor.Key, approval.Version)
	}
	r.dispatchRowsDone(root.Key)
	if step := stepOf(r.d.state(), root.Key); step.Status != api.GatedStepOpenOnBasis {
		t.Fatalf("the spec step of %s after the changes request = %+v, want it still open on %s's approval", root.Key, step, ancestor.Key)
	}
	var keyed int
	if err := r.pool.QueryRow(t.Context(), `select count(*) from outbox where issue = $1`, ancestor.Key).Scan(&keyed); err != nil || keyed != 0 {
		t.Fatalf("outbox rows keyed by %s = %d, %v; want none", ancestor.Key, keyed, err)
	}
	for _, line := range loggedLines(t, r.logs.String(), `"issue":"`+ancestor.Key+`"`) {
		if _, outboxRow := line["row"]; outboxRow {
			t.Fatalf("the outbox ran a row keyed by %s: %v", ancestor.Key, line)
		}
	}
	t.Logf("the changes request on %s reached %s's architect at the next tick and nothing was keyed by %s: %s", ancestor.Key, root.Key, ancestor.Key, told[0].Reason)
}
