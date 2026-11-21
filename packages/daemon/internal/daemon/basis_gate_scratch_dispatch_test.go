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
// and a caller at a contract below 212, whose narrowing needs a caller that states such a contract
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
// daemon records parent in a live tree (live), it waits for R18's reparented notices
// (dispatch://INFRA-3320): one to root's own architect and one to parent's, the architect above
// it now, each queued on root and naming the architect it tells (record.Notice.To), and requires
// those words to name R1's path, the view on parent, and not that root's work reaches parent's
// tree only through its own issues. Where it records parent in none, it waits only for the daemon
// to apply the link: that the link told no architect of parent's tree is read once root's step has
// opened (untoldAtTheLink).
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
	toldTo := func(architect string) bool {
		return slices.ContainsFunc(r.notices(root.Key, string(record.ReparentedNotice)), func(notice record.Notice) bool {
			return notice.To == architect && names(notice.Reason)
		})
	}
	testwait.Eventually(t, "the daemon to tell "+root.Key+" that it can be written as a view on "+parent.Key, func() bool {
		return toldTo(root.Key) && toldTo(parent.Key)
	})
	t.Logf("%s, set under %s, and the architect of %s were told R1's path: %q", root.Key, parent.Key, parent.Key, path)
}

// untoldAtTheLink fails unless the link of root under parent, whose tree is not live, told no
// architect of parent's tree: R18's reparented notices go to each architect the owner walk names
// (workflow's reparentRead), and the walk from a parent with nothing in Legion's work at or above
// it names nobody, so only root's own architect can be told. It is read once root's step has opened
// and the outbox has finished every Dispatch row of root (dispatchRowsDone), so the re-parent seed
// the link queued has run and queued every notice it tells by now. A child-status notice keyed by
// parent, the root of parent's tree, is read where it stands in the outbox and in the daemon's
// log, where the outbox writes one line for a notice it finishes undelivered: a tree with no
// architect finishes it so (notice_routing.go), and the row is gone by the time this reads it.
func (r *personHandoverRig) untoldAtTheLink(root, parent scratchIssue) {
	t := r.t
	t.Helper()
	r.dispatchRowsDone(root.Key)
	for _, notice := range r.notices(root.Key, string(record.ReparentedNotice)) {
		if notice.To != root.Key {
			t.Fatalf("the daemon told the architect of %s of %s's link under %s, whose tree is not live: %+v", notice.To, root.Key, parent.Key, notice)
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
	t.Logf("the link of %s under %s, whose tree is not live, told no architect of %s's tree", root.Key, parent.Key, parent.Key)
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
	// link of R under P told no architect of P's tree, the daemon queues no resting-root row, and
	// P's architect hears of R in its start task once the person sets P todo and P is admitted. P's
	// architect then registers P's own gate, as the first case's step 5 does, so that from here on
	// P stands where the first case's P stands.
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
//  5. R's architect registers on P's approved version with a reason. The call is held after its
//     Dispatch reads (registrationPause): R's step is still unregistered and no fact applied. Its
//     gate seed is held as well (outboxHold). Released, the call answers 200 and the step opens
//     before its seed runs, while R stays admitted: R stands under P in Legion's work, so it
//     waits for P's own gate too (R19 of dispatch://INFRA-3320), which P's architect has not
//     registered yet. Registered before R's step, P's open gate would start R's first phase at
//     once, since R holds no step of its own until it registers one.
//  6. P's architect registers P's own gate, open on the person's approval.
//  7. R plans, nobody is asked, the design-approved notice names P's document and version, one
//     message on R records the opening whole, the state and R's record name the basis, the view
//     fact reads P, the planner's task carries the view sentence, R's catch-up names the basis,
//     P's architect is told once that R rests on it, P's catch-up names R, P's first phase waits
//     while R is open, and no notice queued on R or on P and no role message of R's tree says a
//     person approved R's document. The seed then runs and leaves the step open.
//  8. R is taken to done: P's architect is told again, and P's first phase starts.
//  9. The repeats, each read as steps 7 and 8 read the first case (readAncestorOpening): P's
//     approval carried by Dispatch in the person's name; P labelled and on the backlog, no live
//     tree while R opens, so the link of R under P tells no architect of P's tree, no resting-root
//     row is queued and no architect of P catches up until P is admitted, whose start task then
//     names R; and R holding the person's approval of v2 of its own document while at v4, where no
//     reader names v2: not the state, the message, a notice queued on R or a role message of its
//     tree.
//
// Where the rig reaches no phase — the implementer's, the tester's and the reviewer's tasks, the
// production check's task, the merger's start — the workflow and record tests do:
// TestEveryPhaseTaskOfAViewNamesTheAncestorsRequirements and
// TestTheProductionCheckTaskOfAViewNamesTheAncestorsRequirements (internal/workflow), and
// TestABasisOpeningTakesTheRegistrationsMomentInTheLine (internal/record: GateOpen, the tree's gate
// and its first opening, which the merger's start reads, and TreeGate.OpenedAt). A caller below
// contract 212 is TestACallerBelowTheGateBasisContractIsServedNoTreeRunsAndNoStepOnABasis
// (internal/api): the rig runs no pod and no caller of it states a contract below this daemon's
// own, so no caller here is narrowed.
//
// Dispatch's parent status rule (R24 of dispatch://INFRA-3320) is off for this test alone. The
// second repeat of step 9 stands R, in work, under P on the backlog: a child in work under a
// parent that is not, a world the rule refuses to write and that stands only before Dispatch's
// boot fix (R25) runs with the rule on, so the daemon reads it where Dispatch serves with the
// rule off. Every other step keeps the rule: each other P is admitted, and so in progress, before
// R is linked under it.
func TestBasisGateOpensOnAnAncestorApproval(t *testing.T) {
	pause, held := &registrationPause{}, &outboxHold{}
	r := newPersonHandoverRigOn(t, scratchDispatchOptions{natsURL: testnats.URL(t), env: []string{parentStatusRule + "=off"}}, 8)
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

	// 5. R's registration on P's approved version, held after its reads, its gate seed held.
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
	state, body = settled(t, r.d, "open the spec step of "+root.Key+" on the basis while P's own gate is unregistered", func(s api.State) bool {
		return stepOf(s, root.Key).Status == api.GatedStepOpenOnBasis
	})
	if got := state.Issues[root.Key].Phase; got != phase.Admitted {
		t.Fatalf("%s stands at %s once its step opened on %s's approval, want admitted: it waits for %s's own gate, which is unregistered (R19)\nstate %s",
			root.Key, got, parent.Key, parent.Key, body)
	}

	// 6. P's architect registers P's own gate, which starts R's first phase.
	r.registerApprovedGate(parent.Key, parent.PrimaryArtifactID, approval.Version, parentGrant)

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
	// R registers before P's architect registers P's gate, as in steps 5 and 6: R holds no step of
	// its own until it registers one, so P's open gate would start it first (R19). R's first
	// phase then starts at P's registration, a later phase entry of the run its opening's notices
	// were queued in, which they are delivered at all the same (the architect's ruling of
	// 2026-11-21 on dispatch://INFRA-3320): the reads find them as production delivers them.
	r.registerBasis(carriedGrant(), carriedRoot, 1, carriedParent, carried.Version, reason)
	r.registerApprovedGate(carriedParent.Key, carriedParent.PrimaryArtifactID, carried.Version, carriedParentGrant)
	t.Logf("Dispatch carried the person's approval of version %d of %s to version %d, and %s registered on it", before.Version, carriedParent.Key, carried.Version, carriedRoot.Key)
	r.readAncestorOpening(ancestorOpening{root: carriedRoot, parent: carriedParent, rootVersion: 1, approval: carried, reason: reason, parentLive: true})

	// 9b. P labelled and on the backlog: no live tree while R opens, so the link tells no architect
	// of P's tree, no resting-root row is queued, and no architect of P runs until P's admission,
	// whose start task names R.
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
	r.registerBasis(olderGrant(), olderRoot, fourth, olderParent, olderApproval.Version, reason)
	r.registerApprovedGate(olderParent.Key, olderParent.PrimaryArtifactID, olderApproval.Version, olderParentGrant)
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
// Dispatch's parent status rule (R24 of dispatch://INFRA-3320) is off for this test alone. The
// cases of the ancestor's status stand R, in work, under P on the backlog, in icebox and done: a
// parent that is not in work over a child that is, and a done parent over a child that is not,
// worlds the rule refuses to write and that stand only before Dispatch's boot fix (R25) runs with
// the rule on. Every other step keeps the rule as a person would: each issue R is linked under is
// moved into work first.
//
// The drop seam (overrides.dropConsumedEvent) is used once: the person's approval of R's latest
// version reaches the daemon as an event it drops, and the registration's own read carries that
// approval, opening the step on it and recording no basis.
func TestBasisGateRefusals(t *testing.T) {
	drop := &consumerDrop{}
	r := newPersonHandoverRigOn(t, scratchDispatchOptions{natsURL: testnats.URL(t), env: []string{parentStatusRule + "=off"}}, 3)
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

	// Checks Dispatch would refuse under P: S's requirement is written while R stands under S, which
	// the person moves into work first, as R24 asks of the parent of an issue in work.
	s1 := specBlock(sibling.Key, "s1")
	r.setStatus(sibling.Key, "in_progress")
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

	// The chain: R under P under G, where only G is approved and marks requirements. The person
	// moves G and then P into work before R is linked under P, as R24 asks of the parents of an
	// issue in work; a session labelled both, so neither move is a person's handover.
	grandparent := r.create(asSession, "Maritime safety initiative", map[string]any{"labels": []string{"legion"}, "spec": requirementsMarkdown("Safety", "g1", "g2")})
	gv := r.approve(grandparent.PrimaryArtifactID).Version
	middle := r.create(asSession, "Buoy maintenance roster", map[string]any{"labels": []string{"legion"}, "parent": grandparent.Key})
	r.setStatus(grandparent.Key, "in_progress")
	r.setStatus(middle.Key, "in_progress")
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
	// P's acceptance on Dispatch ends the step R has open on its approval (workflow's endOpenOn,
	// read off the copy): each move is applied before its call, so the call's own refusal is what
	// the case compares R before and after.
	r.setStatus(parent.Key, "retro")
	r.quiet(parent.Key)
	cases.refused("P in Dispatch status retro", call(), http.StatusConflict, "BASIS_ACCEPTED", "Dispatch shows "+parent.Key+" retro")
	r.setStatus(parent.Key, "done")
	r.quiet(parent.Key)
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
