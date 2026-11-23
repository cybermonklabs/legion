// The design gate of an issue whose repository is a spec source of its project, on the whole
// daemon over the real Dispatch of this checkout (R23, R24, R26; checks v-approval and
// v-gate-scope; D8 for spec.request_stale).
//
// Each test boots the whole daemon (startDaemonWithin) on a real Postgres, consuming a real NATS
// server on which the scratch Dispatch publishes its issue events, so every reading below is the
// daemon's answer to an event Dispatch made or to a route a role calls: the architect's gate
// registration, each role's completion, the reviewer's and the author's outcome lists, the
// person's edits, approvals and answers. GitHub is specGateGitHub, whose pull request head the
// test moves; Envoy's listener is the liveProof's noticeListener, which keeps every notice and
// role message the daemon publishes; the agents are the package's fake runtime, driven as their
// panes drive the daemon (stepClaimant). The merger is played by specGateMerger, which does what
// the merger's role prompt says: it follows the design gate's closed and opened messages, reads
// Dispatch's ending condition for the head it is about to enqueue, and enqueues only on ready.
//
// The approved spec every test reads is written the way production writes it: a person approves
// the change request of an issue that is not Legion's, and that approval writes its statements and
// its check into the approved spec of the repository (G1).
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The approved spec of liveRepo every test starts from: a statement with a statement under it,
// and a check of the first. The restore of fixture 1 restores the first, which has the second
// under it (v-approval: "restores a statement that has statements under it").
const (
	specGateParent = "a-payment-is-taken-once"
	specGateChild  = "a-second-payment-is-refused"
	specGateCheck  = "a-second-charge-fails"
	specGateSeed   = ":::requirement{#" + specGateParent + " kind=\"behavior\"}\n" +
		"A payment of one order is taken once.\n:::\n\n" +
		":::requirement{#" + specGateChild + " kind=\"behavior\" under=\"" + specGateParent + "\"}\n" +
		"A second payment of one order is refused.\n:::\n"
)

// specGateCheckBlock is the check of specGateParent: a new one where base is "", else the text the
// writer read at base with claim as its claim. A document keeps no soft line break, so each field
// of a check is a paragraph of its own.
func specGateCheckBlock(base, claimText string) string {
	return ":::check{#" + specGateCheck + " statement=\"" + specGateParent + "\" subject=\"code\" procedure=\"program\" " +
		"runner=\"go\" selector=\"example.com/billing TestASecondChargeFails\" job=\"go-test\"" + baseAttr(base) + "}\n" +
		"Claim: " + claimText + "\n\n" +
		"Procedure: The test charges one order two times.\n\n" +
		"Failing example: A build that takes the second charge.\n:::\n"
}

// baseAttr is ` base="<base>"`, or nothing for a node its writer adds.
func baseAttr(base string) string {
	if base == "" {
		return ""
	}
	return ` base="` + base + `"`
}

// The heads of a pull request of these walks: the one every walk's implementer completes at and
// the reviewer approves (stepRecordsHead), one the reviewer read before it, and the one a push
// moves the pull request to while its merger runs.
const (
	specGateOlderHead  = "0aa11bb22cc33dd44ee55ff66aa77bb88cc99dd0"
	specGateSecondHead = "f00dfeedbeefcafe0123456789abcdef01234567"
)

// The outcomes of fixture 1's change for a caller, in the line form both the outcome lists and the
// conclusion write: one the change restores for the restored statement, one the approved child
// statement requires, and one that no approved statement covers.
var (
	specGateRestored  = specGateOutcome{Text: "A second payment of one order is refused again.", Restored: true}
	specGateRequired  = specGateOutcome{Text: "The refusal names the order it refuses.", Statement: specGateChild}
	specGateUncovered = specGateOutcome{Text: "The refusal also writes a line to the payments log."}
)

// specGateOutcome is one outcome as the outcome routes take it.
type specGateOutcome struct {
	Text      string `json:"text"`
	Statement string `json:"statement,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
}

// line is the outcome as a conclusion's `Outcome:` paragraph writes it.
func (o specGateOutcome) line() string {
	switch {
	case o.Statement != "":
		return "Outcome: (requires " + o.Statement + ") " + o.Text
	case o.Restored:
		return "Outcome: (restored) " + o.Text
	}
	return "Outcome: " + o.Text
}

// specGateConclusion is a conclusion block of id with attrs (each ` name="value"`) and the
// paragraphs, each its own paragraph.
func specGateConclusion(id, attrs string, paragraphs ...string) string {
	return ":::conclusion{#" + id + attrs + "}\n" + strings.Join(paragraphs, "\n\n") + "\n:::\n"
}

// specGateEvidence is the evidence paragraph every complete conclusion of these walks carries.
const specGateEvidence = "Evidence: TestASecondChargeFails fails on the code before the fix and passes after it."

// specGateFixture is one issue a rig creates: its title (no two share a word Dispatch counts), its
// spec, its Verification document ("" for none), its repository, whether Legion holds it as a root
// at admitted, and whether its gate was approved by a person before this gate (R24).
type specGateFixture struct {
	title, spec, checks, repo string
	legion, approvedBefore    bool
}

// specGateIssue is one issue a rig created: its key, its spec's document and its pull request.
type specGateIssue struct {
	key, primary string
	number       int
}

// specGateRig is stepRecordsRig's walk over a scratch Dispatch whose events reach the daemon, a
// project that names liveRepo a spec source with an approved spec, and a GitHub whose heads move.
type specGateRig struct {
	*stepRecordsRig
	gh     *specGateGitHub
	issues map[string]specGateIssue
}

// newSpecGateRig starts the NATS server, the daemon's store in a database of its own and the
// scratch Dispatch on that server, writes the project, its spec source and its approved spec,
// creates each fixture, seeds each Legion root at admitted with no design gate registered
// (approved where the fixture says so), boots the daemon and meets each root's architect. The
// database is the daemon's own for the reason blockersDatabase gives: this Dispatch numbers its
// events from one, and in the run's database the intake would drop each one an earlier test of the
// run applied under the same id.
func newSpecGateRig(t *testing.T, fixtures ...specGateFixture) *specGateRig {
	t.Helper()
	natsURL := testnats.URL(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = blockersDatabase(t)
	proof := newLiveProofOf(t, cfg)
	dispatch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "SG" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	r := &specGateRig{
		stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch, claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)},
		gh:             newSpecGateGitHub(t),
		issues:         map[string]specGateIssue{},
	}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Spec gate"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": r.cfg.Project, "default": true}, http.StatusOK, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/spec-sources/"+liveRepo, map[string]any{"branch": "main"}, http.StatusOK, nil)
	seed := r.create(specGateFixture{title: "Approved billing statements", spec: specGateSeed,
		checks: specGateCheckBlock("", "A second charge of one order fails."), repo: liveRepo})
	r.approve(seed, "session-seed", true)
	r.requireApproved(specGateParent)
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for place, fixture := range fixtures {
		issue := r.create(fixture)
		if !fixture.legion {
			r.issues[fixture.title] = issue
			continue
		}
		if fixture.approvedBefore {
			r.approve(issue, "session-before", false)
		}
		var created createdIssue
		r.person(http.MethodPatch, "/api/v1/issues/"+issue.key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		issue.number = int(number) + 1 + place
		r.issues[fixture.title] = issue
		r.stepRecordsRig.issues = append(r.stepRecordsRig.issues, walkIssue{key: issue.key, title: fixture.title, number: issue.number})
		r.on(issue.key)
		r.gh.open("legion/"+issue.key, issue.number)
		r.seedRoot(fixture, issue, created)
	}
	r.boot()
	for _, issue := range r.stepRecordsRig.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	return r
}

// create creates fixture's issue as the run's person, with its Verification document, and answers
// it.
func (r *specGateRig) create(fixture specGateFixture) specGateIssue {
	r.t.Helper()
	var created struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	r.person(http.MethodPost, "/api/v1/issues", map[string]any{"project": r.cfg.Project, "title": fixture.title, "spec": fixture.spec,
		"repo": fixture.repo, "force": true}, http.StatusCreated, &created)
	if created.Key == "" || created.PrimaryArtifactID == "" {
		r.t.Fatalf("Dispatch created %q with no key or primary document: %+v", fixture.title, created)
	}
	if fixture.checks != "" {
		r.person(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts", map[string]any{"name": "verification.md", "content": fixture.checks},
			http.StatusCreated, nil)
	}
	return specGateIssue{key: created.Key, primary: created.PrimaryArtifactID}
}

// seedRoot records the issue as the daemon holds a root handed to Legion the moment its architect
// starts writing the spec: at admitted, its lifecycle under the workflow, its architect's claim a
// launch the boot relaunches, and its pull request open at stepRecordsHead. Its gate is registered
// by nobody yet, unless the fixture's gate was approved under the gate before this one: then the
// row is the one that daemon wrote, approved at version 1, with no rule of its own.
func (r *specGateRig) seedRoot(fixture specGateFixture, issue specGateIssue, created createdIssue) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: issue.key, Repo: fixture.repo, DefaultBranch: "main", GitHubRepo: fixture.repo, Tree: issue.key, Project: r.cfg.Project,
			Title: fixture.title, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: created.Rank, LastDispatchSeq: created.LastSeq, HandedOver: true,
		}); err != nil {
			return err
		}
		if fixture.approvedBefore {
			approved := 1
			if err := r.records.PutGate(ctx, tx, record.DesignGate{Issue: issue.key, ArtifactID: issue.primary, LatestVersion: 1,
				ApprovedVersion: &approved, EverOpened: true}); err != nil {
				return err
			}
		}
		pr := stepRecordsPullRequest(issue.key, issue.number)
		pr.Repo = fixture.repo
		return r.records.PutPullRequest(ctx, tx, pr)
	}); err != nil {
		t.Fatalf("seed %s: %v", issue.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, issue.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", issue.key, err)
	}
	architect := r.tokenOf(issue.key, claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: issue.key, Issue: issue.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", issue.key, err)
	}
	writePrompt(t, r.cfg, architect)
}

// boot starts the whole daemon on the store as it stands, over a fake runtime of its own, with
// specGateGitHub as GitHub.
func (r *specGateRig) boot() {
	r.t.Helper()
	r.rt, r.built = fake.NewRuntime(), &built{}
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.gh.url
	r.d = startDaemonWithin(r.t, r.cfg, o, r.log(), stepRecordsWait)
}

// agentCall sends one request to Dispatch as session, through the agent bearer with the session
// named as the body's actor, as the `dispatch` command does, and answers the status and body.
func (r *specGateRig) agentCall(method, path, session string, body map[string]any) (int, []byte) {
	r.t.Helper()
	body["actor"] = map[string]string{"kind": "session", "id": session}
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	return r.send(request)
}

// read is GET path on Dispatch as the run's person, decoded into into; any answer but 200 fails.
func (r *specGateRig) read(path string, into any) {
	r.t.Helper()
	r.person(http.MethodGet, path, nil, http.StatusOK, into)
}

// approve opens session's approval request of the issue's spec (with its Verification document
// where verification), and the run's person approves it, reading the ask's revision first as a
// person answers the request she sees.
func (r *specGateRig) approve(issue specGateIssue, session string, verification bool) {
	r.t.Helper()
	ask := r.requestApproval(issue, session, verification)
	if status, answer := r.answer(ask); status != http.StatusOK {
		r.t.Fatalf("the person's approval of %s = %d %s, want 200", issue.key, status, answer)
	}
}

// requestApproval opens session's approval request of the issue's spec and answers the ask's id.
func (r *specGateRig) requestApproval(issue specGateIssue, session string, verification bool) string {
	r.t.Helper()
	status, answer := r.agentCall(http.MethodPost, "/api/v1/artifacts/"+issue.primary+"/approval-requests", session,
		map[string]any{"include_verification": verification})
	if status != http.StatusCreated {
		r.t.Fatalf("request approval of %s = %d %s, want 201", issue.key, status, answer)
	}
	var opened struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(answer, &opened); err != nil || opened.Ask.ID == "" {
		r.t.Fatalf("decode the approval request of %s: %v (%s)", issue.key, err, answer)
	}
	return opened.Ask.ID
}

// answer is the run's person approving the approval request ask, and Dispatch's status and body.
func (r *specGateRig) answer(ask string) (int, []byte) {
	r.t.Helper()
	var read struct {
		Ask struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	r.read("/api/v1/asks/"+ask, &read)
	encoded, err := json.Marshal(map[string]any{"selected": []string{"Approve"}, "expected_edited_at": read.Ask.EditedAt})
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.dispatch.base+"/api/v1/asks/"+ask+"/answer", bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "step-records")
	return r.send(request)
}

// requireApproved fails unless liveRepo's approved spec holds the node id.
func (r *specGateRig) requireApproved(id string) {
	r.t.Helper()
	r.read("/api/v1/projects/"+r.cfg.Project+"/spec/"+liveRepo+"/nodes/"+id, nil)
}

// gate is the design gate of key as GET /legion/v1/state shows it, read as raw JSON so that a
// field the daemon does not write reads as absent.
type specGateView struct {
	ArtifactID          string `json:"artifactId"`
	CurrentVersion      int    `json:"currentVersion"`
	ApprovedVersion     *int   `json:"approvedVersion"`
	ExemptVersion       *int   `json:"exemptVersion"`
	Rule                string `json:"rule"`
	VerificationVersion int    `json:"verificationVersion"`
	ChecksUnapproved    bool   `json:"checksUnapproved"`
}

func (r *specGateRig) gate(key string) (specGateView, string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		r.t.Fatalf("GET /legion/v1/state = %d %s", status, body)
	}
	var state struct {
		Issues map[string]struct {
			Phase      string        `json:"phase"`
			DesignGate *specGateView `json:"designGate"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		r.t.Fatalf("decode the state: %v", err)
	}
	issue, ok := state.Issues[key]
	if !ok || issue.DesignGate == nil {
		return specGateView{}, issue.Phase
	}
	return *issue.DesignGate, issue.Phase
}

// register is key's architect registering version of its spec as the spec step's document, as
// register_gate does, and the daemon's status and body.
func (r *specGateRig) register(key string, version int) (int, string) {
	r.t.Helper()
	architect := r.workerOf(key, claim.RoleArchitect)
	status, body := r.d.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(), "issue": key, "artifactId": r.issueNamed(key).primary, "version": version,
	}, false)
	return status, string(body)
}

// issueNamed is the created issue key.
func (r *specGateRig) issueNamed(key string) specGateIssue {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.key == key {
			return issue
		}
	}
	r.t.Fatalf("%s is no issue of the run", key)
	return specGateIssue{}
}

// requireExempt waits for the gate of key to stand open with no person: open at the version
// Dispatch holds by ExemptVersion, never by a person's approval, under the change request's rule.
func (r *specGateRig) requireExempt(key, what string) specGateView {
	r.t.Helper()
	var gate specGateView
	var stands string
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		gate, stands = r.gate(key)
		return gate.ExemptVersion != nil && *gate.ExemptVersion == gate.CurrentVersion && gate.CurrentVersion > 0
	}) {
		r.t.Fatalf("%s: the design gate of %s is %+v in %s; want it open with no person, its exempt version the current one", what, key, gate, stands)
	}
	if gate.ApprovedVersion != nil {
		r.t.Fatalf("%s: the design gate of %s records a person's approval of version %d; the exemption is never recorded as an approval in a person's name", what, key, *gate.ApprovedVersion)
	}
	if gate.Rule != "change_request" {
		r.t.Fatalf("%s: the design gate of %s started under the rule %q, want change_request", what, key, gate.Rule)
	}
	return gate
}

// requireWaitsForThePerson holds for liveSettle that the gate of key stands closed, approved by
// nobody and exempt from nothing, and the root at admitted: the gate waits for the person.
func (r *specGateRig) requireWaitsForThePerson(key, what string) {
	r.t.Helper()
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		gate, stands := r.gate(key)
		if gate.ExemptVersion != nil || gate.ApprovedVersion != nil || stands != string(phase.Admitted) {
			r.t.Fatalf("%s: %s stands in %s with the design gate %+v; want it at admitted, its gate waiting for the person", what, key, stands, gate)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// walkToMerging walks key from planning to merging: the planner, one implementer round at
// stepRecordsHead, the tester's pass, the reviewer's round a person approves, the architect's
// sign-off and the implementer's retro.
func (r *specGateRig) walkToMerging(key string) {
	r.t.Helper()
	r.on(key)
	r.waitPhase(phase.Planning)
	r.plan()
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
}

// outcomes records the list of outcomes of key's pull request at head as session, and answers
// Dispatch's status and body.
func (r *specGateRig) outcomes(key, session, list, head string, outcomes ...specGateOutcome) (int, string) {
	r.t.Helper()
	if outcomes == nil {
		outcomes = []specGateOutcome{}
	}
	status, body := r.agentCall(http.MethodPost, "/api/v1/issues/"+key+"/outcomes", session, map[string]any{
		"pull_request": liveRepo + "#" + strconv.Itoa(r.issueNamed(key).number), "head": head, "list": list, "outcomes": outcomes,
	})
	return status, string(body)
}

// recordOutcomes is outcomes that must be recorded.
func (r *specGateRig) recordOutcomes(key, session, list, head string, outcomes ...specGateOutcome) {
	r.t.Helper()
	if status, body := r.outcomes(key, session, list, head, outcomes...); status != http.StatusCreated {
		r.t.Fatalf("record the %s list of %s at %s as %s = %d %s, want 201", list, key, head, session, status, body)
	}
}

// session is the session of role's claimant on key.
func (r *specGateRig) session(key string, role claim.Role) string {
	r.t.Helper()
	return r.workerOf(key, role).session
}

// editSpec applies ops to key's spec as the run's person and answers the version the edit wrote.
func (r *specGateRig) editSpec(key string, ops ...map[string]any) int {
	r.t.Helper()
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	r.person(http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{"ops": ops, "summary": "the conclusion"}, http.StatusOK, &edited)
	if edited.Version == nil || edited.Version.Number < 2 {
		r.t.Fatalf("the edit of %s's spec wrote no version: %+v", key, edited)
	}
	return edited.Version.Number
}

// insertAtEnd and deleteBlock are the two edits a conclusion is written and replaced with.
func insertAtEnd(markdown string) map[string]any {
	return map[string]any{"op": "insert", "after": "end", "markdown": markdown}
}

func deleteBlock(id string) map[string]any {
	return map[string]any{"op": "delete", "block": id}
}

// specGateEnding is Dispatch's ending condition of an issue at a head.
type specGateEnding struct {
	Issue   string `json:"issue"`
	Head    string `json:"head"`
	State   string `json:"state"`
	Reasons []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"reasons"`
	Judgment        string                `json:"judgment"`
	JudgmentReasons []specGateJudgmentWhy `json:"judgment_reasons"`
}

// specGateJudgmentWhy is one reason of a conclusion's judgment.
type specGateJudgmentWhy struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome"`
}

// codes is each reason code of the ending, in order.
func (e specGateEnding) codes() []string {
	codes := make([]string, len(e.Reasons))
	for place, reason := range e.Reasons {
		codes[place] = reason.Code
	}
	return codes
}

// ending is GET /api/v1/issues/{key}/change-request/ending?head=head.
func (r *specGateRig) ending(key, head string) specGateEnding {
	r.t.Helper()
	var ending specGateEnding
	r.read("/api/v1/issues/"+key+"/change-request/ending?head="+url.QueryEscape(head), &ending)
	return ending
}

// specGateChangeRequest is Dispatch's read of an issue's change request.
type specGateChangeRequest struct {
	Version             int  `json:"version"`
	VerificationVersion int  `json:"verification_version"`
	SpecSource          bool `json:"spec_source"`
	Proposes            bool `json:"proposes"`
	ChangesChecks       bool `json:"changes_checks"`
	Conclusion          *struct {
		Block   string `json:"block"`
		Answers []struct {
			Body string `json:"body"`
		} `json:"answers"`
	} `json:"conclusion"`
	Judgment        string                `json:"judgment"`
	JudgmentReasons []specGateJudgmentWhy `json:"judgment_reasons"`
}

// changeRequest is GET /api/v1/issues/{key}/change-request at the latest version.
func (r *specGateRig) changeRequest(key string) specGateChangeRequest {
	r.t.Helper()
	var read specGateChangeRequest
	r.read("/api/v1/issues/"+key+"/change-request", &read)
	return read
}

// launched is whether the daemon has launched token's claim.
func (r *specGateRig) launched(token claim.Token) bool {
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			return true
		}
	}
	return false
}

// requireMergerHeld holds for liveSettle that key's merger is not launched, naming what holds it.
func (r *specGateRig) requireMergerHeld(key, what string) {
	r.t.Helper()
	token := r.tokenOf(key, claim.RoleMerger)
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if r.launched(token) {
			r.t.Fatalf("the daemon launched the merger of %s %s; want its start to wait", key, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// architectNoticesOf is every notice of kind the daemon published to key's architect, oldest
// first, each payload replaced by the notice's reason, as liveProof's architectNotices reads them.
// The architect's topic carries role messages too, the design gate's moves among them
// (workflow's tellGateMoved), whose payload is the message's text and not a notice: those are
// passed over here, where liveProof's reader, written for proofs whose architects hear no role
// message, fails on them.
func (r *specGateRig) architectNoticesOf(key, kind string) []listenerPublish {
	r.t.Helper()
	var got []listenerPublish
	for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.tokenOf(key, claim.RoleArchitect))) {
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(published.Payload), &payload) != nil || payload.Kind != kind {
			continue
		}
		published.Payload = payload.Reason
		got = append(got, published)
	}
	return got
}

// endingNotices is the reason of each spec-ending-waits notice the daemon published to key's
// architect, oldest first.
func (r *specGateRig) endingNotices(key string) []string {
	r.t.Helper()
	var reasons []string
	for _, notice := range r.architectNoticesOf(key, "spec-ending-waits") {
		reasons = append(reasons, notice.Payload)
	}
	return reasons
}

// requireEndingNotices waits for key's architect to have been told each of want once, then holds
// for liveSettle that it was told nothing more: one notice per reason, whatever number of times
// the merger's start was tried.
func (r *specGateRig) requireEndingNotices(key string, want ...string) {
	r.t.Helper()
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, fmt.Sprintf("%s's architect to be told %v", key, want), func() bool {
		told := r.endingNotices(key)
		for _, reason := range want {
			if !slices.Contains(told, reason) {
				return false
			}
		}
		return true
	})
	time.Sleep(liveSettle)
	told := r.endingNotices(key)
	for _, reason := range want {
		if count := countOf(told, reason); count != 1 {
			r.t.Fatalf("%s's architect was told the ending reason %s %d times (%v), want once", key, reason, count, told)
		}
	}
}

func countOf(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

// requireStaysExempt waits for the gate of key to stand open by exemption at version: a version
// of the spec that proposes nothing keeps the gate it found open.
func (r *specGateRig) requireStaysExempt(key string, version int, what string) {
	r.t.Helper()
	var gate specGateView
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		gate, _ = r.gate(key)
		return gate.CurrentVersion == version && gate.ExemptVersion != nil && *gate.ExemptVersion == version
	}) {
		r.t.Fatalf("%s: the design gate of %s is %+v after version %d; want it open by exemption at that version", what, key, gate, version)
	}
}

// requireJudgment waits for Dispatch's ending of key at head to judge the conclusion want with the
// reason code, "" for none.
func (r *specGateRig) requireJudgment(key, head, want, code, what string) specGateEnding {
	r.t.Helper()
	ending := r.ending(key, head)
	if ending.Judgment != want {
		r.t.Fatalf("%s: Dispatch judges the conclusion of %s %q (%+v), want %q", what, key, ending.Judgment, ending.JudgmentReasons, want)
	}
	if code != "" && !slices.ContainsFunc(ending.JudgmentReasons, func(why specGateJudgmentWhy) bool { return why.Code == code }) {
		r.t.Fatalf("%s: the judgment of %s's conclusion gives %+v, want the reason %s", what, key, ending.JudgmentReasons, code)
	}
	return ending
}

// mergeAt is GitHub's merge of key's pull request at head, published as Envoy relays it; the
// issue then reaches done.
func (r *specGateRig) mergeAt(key, head string) {
	t := r.t
	t.Helper()
	number := r.issueNamed(key).number
	r.gh.merge(number)
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(number), "merged": "true",
		"merge_commit_sha": "merge-" + head, "head_sha": head, "base_ref": "main", "head_ref": "legion/" + key,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.publish(fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), number), "github", fmt.Sprintf("pr-%d-merged", number), string(payload))
	r.waitPhaseOf(key, phase.Done)
}

// moveHead is a push that moves key's pull request to head, as GitHub reports it.
func (r *specGateRig) moveHead(key, head string) {
	r.t.Helper()
	issue := r.issueNamed(key)
	r.gh.move(issue.number, head)
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(issue.number),
		"head_sha": head, "base_ref": "main", "head_ref": claim.IssueBranch(key), "title": key,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(issue.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	// The envelope as Envoy relays GitHub's synchronize (intake's decodePullRequest reads it), as
	// github_envelopes_live_test.go's publishSynchronizeOf writes it; that file builds only under the
	// e2e tag.
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), issue.number)
	r.waitProcessed(r.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", issue.number, head[:8]), string(payload)))
}

// specGateMerger plays the merger as its role prompt says: it reads the design gate's messages on
// its role topic in order, a closed one holding it and dequeuing what it queued, an opened one
// releasing it; and before each enqueue it reads Dispatch's ending condition for the head and
// enqueues only on ready. actions is each enqueue and dequeue it made.
//
// It reads each message's text, the listener's message field: the payload field carries the same
// text as a JSON string (notify.HTTPPublisher). And it reads only what was sent once it runs, from
// the topic's count when the test makes it, after the merger took its task: Envoy's listener
// refuses a role message to a role nobody holds, and the noticeListener takes every publish, so a
// message the daemon sent before the merger ran is one no merger was given.
type specGateMerger struct {
	r       *specGateRig
	key     string
	topic   string
	from    int
	read    int
	held    bool
	queued  string
	actions []string
}

func (r *specGateRig) merger(key string) *specGateMerger {
	topic := notify.RoleTopicPrefix + string(r.tokenOf(key, claim.RoleMerger))
	from := len(r.notices.acceptedOn(topic))
	return &specGateMerger{r: r, key: key, topic: topic, from: from, read: from}
}

// messages is every gate message of kind ("closed" or "opened") the daemon has sent the merger
// since it runs.
func (m *specGateMerger) messages(kind string) []string {
	var got []string
	for _, published := range m.r.notices.acceptedOn(m.topic)[m.from:] {
		if strings.HasPrefix(published.Message, "The design gate of "+m.key+" "+kind+":") {
			got = append(got, published.Message)
		}
	}
	return got
}

// waitMessages waits for the merger to have been sent count gate messages of kind.
func (m *specGateMerger) waitMessages(kind string, count int, what string) {
	m.r.t.Helper()
	testwait.WithinPolling(m.r.t, stepRecordsWait, 100*time.Millisecond, fmt.Sprintf("%s: the merger of %s to be sent %d %s-gate messages", what, m.key, count, kind), func() bool {
		return len(m.messages(kind)) >= count
	})
	if got := m.messages(kind); len(got) != count {
		m.r.t.Fatalf("%s: the merger of %s was sent %d %s-gate messages %q, want %d", what, m.key, len(got), kind, got, count)
	}
}

// follow reads the gate messages it has not read yet, in order.
func (m *specGateMerger) follow() {
	published := m.r.notices.acceptedOn(m.topic)
	for ; m.read < len(published); m.read++ {
		switch body := published[m.read].Message; {
		case strings.HasPrefix(body, "The design gate of "+m.key+" closed:"):
			m.held = true
			if m.queued != "" {
				m.actions = append(m.actions, "dequeue "+m.queued)
				m.queued = ""
			}
		case strings.HasPrefix(body, "The design gate of "+m.key+" opened:"):
			m.held = false
		}
	}
}

// enqueue enqueues the pull request at head if nothing holds the merge, and says whether it did.
func (m *specGateMerger) enqueue(head string) bool {
	m.r.t.Helper()
	m.follow()
	if m.held || m.r.ending(m.key, head).State != "ready" {
		return false
	}
	m.queued = head
	m.actions = append(m.actions, "enqueue "+head)
	return true
}

// TestGateFixtures is v-approval's four fixtures on the gate of a spec source (R23, R26, D10).
// Fixture 1 proposes no statement and restores one with a statement under it: its gate opens with
// no person, before its conclusion exists, and its merge is held by the ending condition at the
// head that would land until the reviewer's final list for that head and a complete conclusion
// exist; it holds a running merger on a head move and on a revised final list that adds an
// uncovered outcome, until the person approves the whole spec. On the way, the conclusion controls,
// the omitted outcome and the person's answer. Fixtures 2, 3 and 4 wait for the person.
func TestGateFixtures(t *testing.T) {
	restore := specGateFixture{title: "Restore the refusal of a second payment", repo: liveRepo, legion: true,
		spec: "Since the last release a second payment of one order is taken. The change restores the refusal.\n"}
	two := specGateFixture{title: "Refunds return payments", repo: liveRepo, legion: true,
		spec: ":::requirement{#a-refund-returns-the-payment kind=\"behavior\"}\nA refund returns the payment of an order.\n:::\n\n" +
			":::requirement{#" + specGateChild + " kind=\"behavior\" under=\"" + specGateParent + "\" base=\"1\"}\n" +
			"A second payment of one order is refused, and its sender is told.\n:::\n"}
	three := specGateFixture{title: "Cancelled orders refunded twice", repo: liveRepo, legion: true,
		spec: "A report: a cancelled order is refunded two times, and no statement covers it.\n\n" +
			":::requirement{#a-cancelled-order-is-refunded-once kind=\"behavior\"}\nA cancelled order is refunded once.\n:::\n"}
	four := specGateFixture{title: "Weaker charge check", repo: liveRepo, legion: true,
		spec:   "The check of the charge is too strict for the slow bank; the author and the reviewer agree it must be weaker.\n",
		checks: specGateCheckBlock("1", "A second charge of one order fails within a day.")}
	r := newSpecGateRig(t, restore, two, three, four)
	key := r.issues[restore.title].key

	// Fixture 1 starts with no person: its registration reads a change request that proposes
	// nothing, and the root starts planning with no approval of anybody's.
	if read := r.changeRequest(key); !read.SpecSource || read.Proposes || read.ChangesChecks || read.Judgment != "none" {
		t.Fatalf("Dispatch reads fixture 1's change request as %+v; want a spec source's request that proposes nothing, with no conclusion yet", read)
	}
	if status, body := r.register(key, 1); status != http.StatusOK {
		t.Fatalf("the registration of fixture 1's spec = %d %s, want 200", status, body)
	}
	r.requireExempt(key, "fixture 1 at its registration")
	// Fixtures 2, 3 and 4 wait for the person: each proposes something or changes an approved check.
	for _, fixture := range []specGateFixture{two, three, four} {
		other := r.issues[fixture.title].key
		read := r.changeRequest(other)
		if !read.SpecSource || (!read.Proposes && !read.ChangesChecks) {
			t.Fatalf("Dispatch reads the change request of %q as %+v; want one that proposes something or changes an approved check", fixture.title, read)
		}
		if status, body := r.register(other, 1); status != http.StatusOK {
			t.Fatalf("the registration of %q = %d %s, want 200", fixture.title, status, body)
		}
	}
	for _, fixture := range []specGateFixture{two, three, four} {
		r.requireWaitsForThePerson(r.issues[fixture.title].key, fixture.title)
	}

	r.walkToMerging(key)
	implementer, reviewer := r.session(key, claim.RoleImplementer), r.session(key, claim.RoleReviewer)
	mergerToken := r.tokenOf(key, claim.RoleMerger)

	// The merger waits while the head has no final list and no conclusion exists, and the owner is
	// told each reason once.
	r.requireEndingNotices(key, "no_final_list_for_head", "no_conclusion")
	r.requireMergerHeld(key, "while the head has no final list and no conclusion exists")

	// It waits while the only final list is of an earlier head. The author's list leaves out the
	// outcome the approved child requires; the author writes no reviewer or final list. The author
	// records a list at each head it pushed, the earlier one too, since a final list waits for the
	// author's list at its own head.
	r.recordOutcomes(key, implementer, "author", specGateOlderHead, specGateRestored)
	r.recordOutcomes(key, implementer, "author", stepRecordsHead, specGateRestored)
	for _, list := range []string{"reviewer", "final"} {
		if status, body := r.outcomes(key, implementer, list, stepRecordsHead, specGateRestored); status != http.StatusForbidden ||
			!strings.Contains(body, "OUTCOME_LIST_BY_AUTHOR") {
			t.Fatalf("the author's %s list = %d %s, want 403 OUTCOME_LIST_BY_AUTHOR: the author cannot write the reviewer's lists", list, status, body)
		}
	}
	r.recordOutcomes(key, reviewer, "reviewer", specGateOlderHead, specGateRestored, specGateRequired)
	r.recordOutcomes(key, reviewer, "final", specGateOlderHead, specGateRestored, specGateRequired)
	if codes := r.ending(key, stepRecordsHead).codes(); !slices.Equal(codes, []string{"final_list_of_another_head", "no_conclusion"}) {
		t.Fatalf("the ending of %s at its head with a final list of an earlier head gives %v", key, codes)
	}
	r.requireEndingNotices(key, "no_final_list_for_head", "no_conclusion", "final_list_of_another_head")
	r.requireMergerHeld(key, "while the only final list is of an earlier head")

	// The reviewer's own list, then its final list, at the head: now only the conclusion is
	// missing.
	r.recordOutcomes(key, reviewer, "reviewer", stepRecordsHead, specGateRestored, specGateRequired)
	r.recordOutcomes(key, reviewer, "final", stepRecordsHead, specGateRestored, specGateRequired)
	if codes := r.ending(key, stepRecordsHead).codes(); !slices.Equal(codes, []string{"no_conclusion"}) {
		t.Fatalf("the ending of %s with the head's final list and no conclusion gives %v, want [no_conclusion]", key, codes)
	}
	r.requireMergerHeld(key, "while no conclusion exists")

	// The conclusion controls. Each version of the spec proposes nothing, so the gate stays open
	// by exemption at it.
	statement, kind := ` statement="`+specGateParent+`"`, ` difference="code_breaks"`
	version := r.editSpec(key, insertAtEnd(specGateConclusion("c-kind-only", kind, specGateEvidence, specGateRestored.line(), specGateRequired.line())))
	r.requireStaysExempt(key, version, "a conclusion that names a kind and no statement")
	r.requireJudgment(key, stepRecordsHead, "incomplete", "no_statement_or_kind", "a kind of difference and no statement")
	version = r.editSpec(key, deleteBlock("c-kind-only"),
		insertAtEnd(specGateConclusion("c-statement-only", statement, specGateEvidence, specGateRestored.line(), specGateRequired.line())))
	r.requireStaysExempt(key, version, "a conclusion that names a statement and no kind")
	r.requireJudgment(key, stepRecordsHead, "incomplete", "no_statement_or_kind", "a statement and no kind of difference")
	version = r.editSpec(key, deleteBlock("c-statement-only"),
		insertAtEnd(specGateConclusion("c-no-evidence", statement+kind, specGateRestored.line(), specGateRequired.line())))
	r.requireStaysExempt(key, version, "a conclusion with no evidence")
	r.requireJudgment(key, stepRecordsHead, "incomplete", "no_evidence", "a conclusion with no evidence")
	version = r.editSpec(key, deleteBlock("c-no-evidence"),
		insertAtEnd(specGateConclusion("c-no-statement", ` no_statement="true"`, specGateEvidence, specGateRestored.line(), specGateRequired.line())))
	r.requireStaysExempt(key, version, "a conclusion that says no statement applies")
	// No statement applies in place of both: accepted, and an outcome restored for no statement is
	// then behavior no statement covers.
	r.requireJudgment(key, stepRecordsHead, "uncovered", "uncovered_outcome", "a conclusion that says no statement applies")
	// The omitted outcome: the author's list and the conclusion both leave out the outcome the
	// reviewer's final list holds, and the gate stays closed.
	version = r.editSpec(key, deleteBlock("c-no-statement"),
		insertAtEnd(specGateConclusion("c-omits", statement+kind, specGateEvidence, specGateRestored.line())))
	r.requireStaysExempt(key, version, "a conclusion that omits an outcome")
	omitted := r.requireJudgment(key, stepRecordsHead, "incomplete", "omits_outcome", "a conclusion that omits an outcome of the final list")
	if !slices.ContainsFunc(omitted.JudgmentReasons, func(why specGateJudgmentWhy) bool { return why.Outcome == specGateRequired.Text }) {
		t.Fatalf("the omitted outcome is judged %+v, want it named", omitted.JudgmentReasons)
	}
	r.requireMergerHeld(key, "while the conclusion omits an outcome of the final list")
	// The complete conclusion: the merger starts.
	version = r.editSpec(key, deleteBlock("c-omits"),
		insertAtEnd(specGateConclusion("c-complete", statement+kind, specGateEvidence, specGateRestored.line(), specGateRequired.line())))
	r.requireStaysExempt(key, version, "the complete conclusion")
	r.requireJudgment(key, stepRecordsHead, "complete", "", "the complete conclusion")
	r.awaitLaunch(mergerToken)
	merger := r.workerOf(key, claim.RoleMerger)
	if task := merger.receive().Message; !strings.Contains(task, "dispatch spec-ending --issue "+key+" --head") {
		t.Fatalf("the merger's task of an issue whose gate opened with no person does not name the ending condition:\n%s", task)
	}
	m := r.merger(key)
	if !m.enqueue(stepRecordsHead) {
		t.Fatalf("the merger could not enqueue %s at its head with the final list and a complete conclusion", key)
	}

	// With that merger running the head moves: the merger is told the gate closed, dequeues, and
	// enqueues nothing at the new head.
	r.moveHead(key, specGateSecondHead)
	m.waitMessages("closed", 1, "a head with no final list")
	if m.enqueue(specGateSecondHead) {
		t.Fatalf("the merger enqueued %s at %s, a head with no final list, after the closed-gate message", key, specGateSecondHead)
	}
	if codes := r.ending(key, specGateSecondHead).codes(); !slices.Equal(codes, []string{"final_list_of_another_head"}) {
		t.Fatalf("the ending at the moved head gives %v, want [final_list_of_another_head]", codes)
	}
	// The new head's final list releases it, after the author's list at that head.
	r.recordOutcomes(key, implementer, "author", specGateSecondHead, specGateRestored)
	r.recordOutcomes(key, reviewer, "reviewer", specGateSecondHead, specGateRestored, specGateRequired)
	r.recordOutcomes(key, reviewer, "final", specGateSecondHead, specGateRestored, specGateRequired)
	m.waitMessages("opened", 1, "the new head's final list")
	if !m.enqueue(specGateSecondHead) {
		t.Fatalf("the merger could not enqueue %s at %s after the opened-gate message", key, specGateSecondHead)
	}

	// At one head, a revised final list adds an outcome no approved statement covers: the running
	// merger is held until the person approves the whole spec. The conclusion lists it first, so
	// what holds the merge is the uncovered behavior and not an omission.
	version = r.editSpec(key, deleteBlock("c-complete"),
		insertAtEnd(specGateConclusion("c-uncovered", statement+kind, specGateEvidence, specGateRestored.line(), specGateRequired.line(), specGateUncovered.line())))
	r.requireStaysExempt(key, version, "the conclusion that lists the uncovered outcome")
	r.recordOutcomes(key, reviewer, "final", specGateSecondHead, specGateRestored, specGateRequired, specGateUncovered)
	m.waitMessages("closed", 2, "a revised final list with an uncovered outcome")
	if codes := r.ending(key, specGateSecondHead).codes(); !slices.Equal(codes, []string{"uncovered_without_approval"}) {
		t.Fatalf("the ending with an uncovered outcome gives %v, want [uncovered_without_approval]", codes)
	}
	if m.enqueue(specGateSecondHead) {
		t.Fatalf("the merger enqueued %s with an uncovered outcome no person approved", key)
	}

	// The person's answer on the conclusion is recorded as every reply is, and the change request
	// shows it.
	const answer = "Keep the log line; it is what the auditors read."
	r.person(http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "TestASecondChargeFails fails on the code before the fix"}, "body": answer,
	}, http.StatusCreated, nil)
	read := r.changeRequest(key)
	if read.Conclusion == nil || !slices.ContainsFunc(read.Conclusion.Answers, func(a struct {
		Body string `json:"body"`
	}) bool {
		return a.Body == answer
	}) {
		t.Fatalf("the change request of %s shows the conclusion %+v; want the person's answer among its answers", key, read.Conclusion)
	}

	// The person approves the whole spec, and the merge is released.
	r.approve(r.issues[restore.title], r.session(key, claim.RoleArchitect), false)
	m.waitMessages("opened", 2, "the person's approval of the whole spec")
	if !m.enqueue(specGateSecondHead) {
		t.Fatalf("the merger could not enqueue %s after the person approved the whole spec", key)
	}
	want := []string{"enqueue " + stepRecordsHead, "dequeue " + stepRecordsHead, "enqueue " + specGateSecondHead, "dequeue " + specGateSecondHead, "enqueue " + specGateSecondHead}
	if !slices.Equal(m.actions, want) {
		t.Fatalf("the merger's queue actions are %v, want %v", m.actions, want)
	}
	r.mergeAt(key, specGateSecondHead)

	// Fixtures 2, 3 and 4 still wait for the person.
	for _, fixture := range []specGateFixture{two, three, four} {
		r.requireWaitsForThePerson(r.issues[fixture.title].key, fixture.title+", at the end of the run")
	}
}

// TestGateScope is v-gate-scope's three controls (R24): a repository with no spec source keeps
// today's gate; an issue approved before this gate keeps its approval under the gate it started
// with, and its merge is not held by the ending condition; an issue of a spec source, outside
// every form root (this Dispatch names none), takes the new gate, and a root's own proposal waits
// for the person under it.
func TestGateScope(t *testing.T) {
	plain := specGateFixture{title: "Tidy the tools runner", repo: "acme/tools", legion: true,
		spec: "The runner of the tools repository logs each step.\n"}
	before := specGateFixture{title: "Earlier approved cleanup", repo: liveRepo, legion: true, approvedBefore: true,
		spec: "The cleanup removes unused fixtures.\n"}
	outside := specGateFixture{title: "Speed the nightly build", repo: liveRepo, legion: true,
		spec: "The nightly build caches its modules.\n"}
	own := specGateFixture{title: "Receipts for each payment", repo: liveRepo, legion: true,
		spec: ":::requirement{#a-receipt-follows-a-payment kind=\"behavior\"}\nA receipt follows each payment.\n:::\n"}
	r := newSpecGateRig(t, plain, before, outside, own)

	// A repository with no spec source keeps the gate of today: the person approves it.
	plainKey := r.issues[plain.title].key
	if read := r.changeRequest(plainKey); read.SpecSource {
		t.Fatalf("Dispatch reads %s, an issue of a repository with no spec source, as holding a change request: %+v", plainKey, read)
	}
	if status, body := r.register(plainKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", plainKey, status, body)
	}
	r.requireWaitsForThePerson(plainKey, "an issue of a repository with no spec source")
	r.approve(r.issues[plain.title], r.session(plainKey, claim.RoleArchitect), false)
	r.on(plainKey)
	r.waitPhase(phase.Planning)
	if gate, _ := r.gate(plainKey); gate.ApprovedVersion == nil || gate.ExemptVersion != nil || gate.Rule != "approval" {
		t.Fatalf("the gate of %s is %+v, want approved by the person under today's rule", plainKey, gate)
	}

	// An issue of a spec source outside every form root takes the new gate.
	outsideKey := r.issues[outside.title].key
	if status, body := r.register(outsideKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", outsideKey, status, body)
	}
	r.requireExempt(outsideKey, "an issue of a spec source outside every form root")

	// A root's own proposal waits for the person, under the new gate.
	ownKey := r.issues[own.title].key
	if status, body := r.register(ownKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", ownKey, status, body)
	}
	r.requireWaitsForThePerson(ownKey, "a root's own proposal")
	r.approve(r.issues[own.title], r.session(ownKey, claim.RoleArchitect), false)
	r.requireApproved("a-receipt-follows-a-payment")
	r.on(ownKey)
	r.waitPhase(phase.Planning)
	if gate, _ := r.gate(ownKey); gate.ApprovedVersion == nil || gate.ExemptVersion != nil || gate.Rule != "change_request" {
		t.Fatalf("the gate of %s is %+v, want approved by the person under the change request's rule", ownKey, gate)
	}

	// An issue approved before this gate keeps its approval and its rule when its architect
	// registers the spec again, and its merge waits for no final list and no conclusion.
	beforeKey := r.issues[before.title].key
	if status, body := r.register(beforeKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", beforeKey, status, body)
	}
	r.on(beforeKey)
	r.waitPhase(phase.Planning)
	if gate, _ := r.gate(beforeKey); gate.ApprovedVersion == nil || *gate.ApprovedVersion != 1 || gate.ExemptVersion != nil || gate.Rule != "approval" {
		t.Fatalf("the gate of %s, approved before this gate, is %+v after its registration; want its approval kept under today's rule", beforeKey, gate)
	}
	r.walkToMerging(beforeKey)
	r.awaitLaunch(r.tokenOf(beforeKey, claim.RoleMerger))
}

// TestNoStatementLandsWithNoPerson is R23's operational change: an issue that proposes nothing
// ends in a conclusion that says no statement applies, the reviewer's final list is recorded empty
// for its head, and it lands with no person at any step. A second such issue holds an ordinary
// acceptance check, one that names no statement: a check of the issue's own, no part of its change
// request, so it too starts with no person.
func TestNoStatementLandsWithNoPerson(t *testing.T) {
	cache := specGateFixture{title: "Cache the dashboard query", repo: liveRepo, legion: true,
		spec: "The dashboard query is cached for one minute.\n"}
	export := specGateFixture{title: "Export names its file", repo: liveRepo, legion: true,
		spec:   "The export writes one file and names it in its log.\n",
		checks: ":::check{#c-export-names surface=\"cli\" run=\"go test ./...\"}\nThe export's log names the file it wrote.\n:::\n"}
	r := newSpecGateRig(t, cache, export)
	key := r.issues[cache.title].key
	if status, body := r.register(key, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", key, status, body)
	}
	r.requireExempt(key, "an operational change")
	r.walkToMerging(key)
	r.requireMergerHeld(key, "before its outcome lists and its conclusion exist")
	r.recordOutcomes(key, r.session(key, claim.RoleImplementer), "author", stepRecordsHead)
	reviewer := r.session(key, claim.RoleReviewer)
	r.recordOutcomes(key, reviewer, "reviewer", stepRecordsHead)
	r.recordOutcomes(key, reviewer, "final", stepRecordsHead)
	version := r.editSpec(key, insertAtEnd(specGateConclusion("c-operational", ` no_statement="true"`,
		"Evidence: the dashboard loads in two seconds after the change, and in nine before it.")))
	r.requireStaysExempt(key, version, "the operational conclusion")
	r.requireJudgment(key, stepRecordsHead, "complete", "", "a no_statement conclusion over an empty final list")
	r.awaitLaunch(r.tokenOf(key, claim.RoleMerger))
	r.mergeAt(key, stepRecordsHead)
	if gate, _ := r.gate(key); gate.ApprovedVersion != nil {
		t.Fatalf("the gate of %s records a person's approval %+v; the change landed with no person", key, gate)
	}

	exportKey := r.issues[export.title].key
	if read := r.changeRequest(exportKey); read.VerificationVersion < 1 || read.Proposes || read.ChangesChecks {
		t.Fatalf("the change request of %s, which holds only an acceptance check that names no statement, reads %+v; "+
			"want its Verification document read and no proposal and no changed check", exportKey, read)
	}
	if status, body := r.register(exportKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", exportKey, status, body)
	}
	r.requireExempt(exportKey, "an operational change that holds its own acceptance check")
	r.on(exportKey)
	r.waitPhase(phase.Planning)
}

// TestUncoveredBehaviorWaits is R26 before the first version is complete: an outcome that no
// approved statement covers holds the merge until the person approves the whole issue spec.
func TestUncoveredBehaviorWaits(t *testing.T) {
	ledger := specGateFixture{title: "Restore the payment refusal", repo: liveRepo, legion: true,
		spec: "A second payment of one order is taken again. The change restores the refusal.\n"}
	r := newSpecGateRig(t, ledger)
	key := r.issues[ledger.title].key
	if status, body := r.register(key, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", key, status, body)
	}
	r.requireExempt(key, "a restore with an uncovered outcome")
	r.walkToMerging(key)
	r.recordOutcomes(key, r.session(key, claim.RoleImplementer), "author", stepRecordsHead, specGateRestored, specGateUncovered)
	reviewer := r.session(key, claim.RoleReviewer)
	r.recordOutcomes(key, reviewer, "reviewer", stepRecordsHead, specGateRestored, specGateUncovered)
	r.recordOutcomes(key, reviewer, "final", stepRecordsHead, specGateRestored, specGateUncovered)
	version := r.editSpec(key, insertAtEnd(specGateConclusion("c-ledger", ` statement="`+specGateParent+`" difference="code_breaks"`,
		specGateEvidence, specGateRestored.line(), specGateUncovered.line())))
	r.requireStaysExempt(key, version, "the conclusion with an uncovered outcome")
	r.requireJudgment(key, stepRecordsHead, "uncovered", "uncovered_outcome", "an outcome no approved statement covers")
	r.requireEndingNotices(key, "uncovered_without_approval")
	r.requireMergerHeld(key, "while an uncovered outcome waits for the person")
	r.approve(r.issues[ledger.title], r.session(key, claim.RoleArchitect), false)
	if ending := r.ending(key, stepRecordsHead); ending.State != "ready" {
		t.Fatalf("the ending of %s after the person approved the whole spec is %+v, want ready", key, ending)
	}
	r.awaitLaunch(r.tokenOf(key, claim.RoleMerger))
}

// TestAStaleRequestReachesItsOwner is D8 for spec.request_stale: Dispatch refuses the approval of a
// change request written against a statement that another approval changed, and the daemon turns
// its event into one durable notice to the request's owner, the architect of its root; a request
// whose issue Legion holds no owner of reaches the controller instead (v-approval: "A build that
// refuses it and sends the owner of the second no event of the refusal fails").
func TestAStaleRequestReachesItsOwner(t *testing.T) {
	change := func(title, text string, legion bool) specGateFixture {
		return specGateFixture{title: title, repo: liveRepo, legion: legion,
			spec: ":::requirement{#" + specGateChild + " kind=\"behavior\" under=\"" + specGateParent + "\" base=\"1\"}\n" + text + "\n:::\n"}
	}
	first := change("Refusal names the order", "A second payment of one order is refused, naming the order.", true)
	second := change("Refusal tells the sender", "A second payment of one order is refused, and the sender is told.", true)
	unowned := change("Refusal within a minute", "A second payment of one order is refused within one minute.", false)
	r := newSpecGateRig(t, first, second, unowned)
	firstKey, secondKey, unownedKey := r.issues[first.title].key, r.issues[second.title].key, r.issues[unowned.title].key
	for _, key := range []string{firstKey, secondKey} {
		if status, body := r.register(key, 1); status != http.StatusOK {
			t.Fatalf("the registration of %s = %d %s, want 200", key, status, body)
		}
	}
	firstAsk := r.requestApproval(r.issues[first.title], r.session(firstKey, claim.RoleArchitect), false)
	secondAsk := r.requestApproval(r.issues[second.title], r.session(secondKey, claim.RoleArchitect), false)
	unownedAsk := r.requestApproval(r.issues[unowned.title], "session-unowned", false)
	if status, body := r.answer(firstAsk); status != http.StatusOK {
		t.Fatalf("the approval of %s = %d %s, want 200", firstKey, status, body)
	}
	for _, ask := range []string{secondAsk, unownedAsk} {
		if status, body := r.answer(ask); status != http.StatusConflict || !strings.Contains(string(body), "SPEC_REQUEST_STALE") {
			t.Fatalf("the approval of a request written against the changed statement = %d %s, want 409 SPEC_REQUEST_STALE", status, body)
		}
	}
	var told []listenerPublish
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the owner of "+secondKey+" to be told its stale request", func() bool {
		told = r.architectNoticesOf(secondKey, "spec-request-stale")
		return len(told) > 0
	})
	time.Sleep(liveSettle)
	if again := r.architectNoticesOf(secondKey, "spec-request-stale"); len(again) != 1 {
		t.Fatalf("the owner of %s was told the stale request %d times (%+v), want once", secondKey, len(again), again)
	}
	// The notice is the stale request's: published as it names its issue, and carrying Dispatch's
	// reason for the refusal.
	if !strings.Contains(told[0].Message, secondKey) || told[0].Payload == "" {
		t.Fatalf("the owner's notice is %+v; want it to name %s and carry the reason of the refusal", told[0], secondKey)
	}
	if stray := r.architectNoticesOf(firstKey, "spec-request-stale"); len(stray) != 0 {
		t.Fatalf("the owner of %s, whose request was approved, was told a stale request: %+v", firstKey, stray)
	}
	var toController []listenerPublish
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the controller to be told the stale request of "+unownedKey, func() bool {
		toController = nil
		for _, published := range r.notices.acceptedOn(notify.ControllerTopic(r.project)) {
			if strings.Contains(published.Message+published.Payload, "spec-request-stale") && strings.Contains(published.Message+published.Payload, unownedKey) {
				toController = append(toController, published)
			}
		}
		return len(toController) > 0
	})
	time.Sleep(liveSettle)
	for _, published := range r.notices.acceptedOn(notify.ControllerTopic(r.project)) {
		if strings.Contains(published.Message+published.Payload, "spec-request-stale") && strings.Contains(published.Message+published.Payload, secondKey) {
			t.Fatalf("the controller was told the stale request of %s, whose owner holds its role: %+v", secondKey, published)
		}
	}
}

// specGateOwnCheck is an acceptance check of an issue's own: a check block that names no
// statement, so no part of its change request (specchange.OfTheSpec).
const specGateOwnCheck = "c-footer-words"

// specGateOwnCheckBlock is specGateOwnCheck with claim as its text.
func specGateOwnCheckBlock(claimText string) string {
	return ":::check{#" + specGateOwnCheck + " surface=\"cli\" run=\"go test ./...\"}\n" + claimText + "\n:::\n"
}

// editVerification applies ops to key's Verification document as the run's person.
func (r *specGateRig) editVerification(key string, ops ...map[string]any) {
	r.t.Helper()
	r.person(http.MethodPost, "/api/v1/issues/"+key+"/artifacts/verification-md/edits", map[string]any{"ops": ops, "summary": "the checks"},
		http.StatusOK, nil)
}

// TestAVerificationEditOfAnApprovedCheckClosesTheGate is R23 and R25 for an edit of the
// Verification document alone, the spec left as it stood. A change request that proposes nothing
// opens its gate with no person, and its merger runs and enqueues. An edit that changes the
// approved check closes the gate: the running merger is told, takes the pull request out of the
// queue and enqueues nothing, and the ending waits for the person
// (changes_checks_without_approval) until the person approves the spec with its Verification
// document. The same edit of an acceptance check that names no statement is no part of the change
// request: that gate stays open by exemption, and its merger is told nothing.
func TestAVerificationEditOfAnApprovedCheckClosesTheGate(t *testing.T) {
	approved := specGateFixture{title: "Repair the receipt output", repo: liveRepo, legion: true,
		spec: "The receipt output is repaired.\n", checks: specGateCheckBlock("1", "A second charge of one order fails.")}
	own := specGateFixture{title: "Reword the receipt footer", repo: liveRepo, legion: true,
		spec: "The receipt footer is reworded.\n", checks: specGateOwnCheckBlock("The receipt footer reads the new words.")}
	r := newSpecGateRig(t, approved, own)
	// Each walks to merging with no person: its three lists empty at the head, its conclusion says
	// no statement applies, and its merger runs and enqueues the head.
	walk := func(fixture specGateFixture) (string, *specGateMerger) {
		key := r.issues[fixture.title].key
		if read := r.changeRequest(key); !read.SpecSource || read.VerificationVersion != 1 || read.Proposes || read.ChangesChecks {
			t.Fatalf("Dispatch reads the change request of %q as %+v; want one at Verification version 1 that proposes nothing and changes no check",
				fixture.title, read)
		}
		if status, body := r.register(key, 1); status != http.StatusOK {
			t.Fatalf("the registration of %q = %d %s, want 200", fixture.title, status, body)
		}
		r.requireExempt(key, fixture.title+" at its registration")
		r.walkToMerging(key)
		r.recordOutcomes(key, r.session(key, claim.RoleImplementer), "author", stepRecordsHead)
		reviewer := r.session(key, claim.RoleReviewer)
		r.recordOutcomes(key, reviewer, "reviewer", stepRecordsHead)
		r.recordOutcomes(key, reviewer, "final", stepRecordsHead)
		version := r.editSpec(key, insertAtEnd(specGateConclusion("c-receipt", ` no_statement="true"`,
			"Evidence: the receipt test passes after the change, and fails before it.")))
		r.requireStaysExempt(key, version, fixture.title+"'s conclusion")
		r.awaitLaunch(r.tokenOf(key, claim.RoleMerger))
		m := r.merger(key)
		if !m.enqueue(stepRecordsHead) {
			t.Fatalf("the merger of %q could not enqueue its head with the final list and a complete conclusion", fixture.title)
		}
		return key, m
	}
	ownKey, ownMerger := walk(own)
	key, merger := walk(approved)

	// The acceptance check changes: no part of the change request, so the gate does not move. The
	// spec's next version, which the daemon applies after the edit of the same issue, shows the edit
	// applied.
	r.editVerification(ownKey, deleteBlock(specGateOwnCheck), insertAtEnd(specGateOwnCheckBlock("The receipt footer reads the new words, in bold.")))
	if read := r.changeRequest(ownKey); read.VerificationVersion != 2 || read.Proposes || read.ChangesChecks {
		t.Fatalf("the change request after an edit of the acceptance check reads %+v; want Verification version 2 and no change", read)
	}
	later := r.editSpec(ownKey, insertAtEnd("The footer is set in the receipt's own type.\n"))
	r.requireStaysExempt(ownKey, later, "an edit of the acceptance check alone, then a version that proposes nothing")
	if closed := ownMerger.messages("closed"); len(closed) != 0 {
		t.Fatalf("the merger of %s was told %q after an edit of its acceptance check; want nothing", ownKey, closed)
	}
	if ending := r.ending(ownKey, stepRecordsHead); ending.State != "ready" {
		t.Fatalf("the ending of %s after an edit of its acceptance check is %+v, want ready", ownKey, ending)
	}

	// The approved check changes, and nothing else: the gate closes under the running merger.
	r.editVerification(key, deleteBlock(specGateCheck), insertAtEnd(specGateCheckBlock("1", "A second charge of one order fails within a day.")))
	if read := r.changeRequest(key); read.VerificationVersion != 2 || read.Proposes || !read.ChangesChecks {
		t.Fatalf("the change request after an edit of the approved check reads %+v; want Verification version 2 and a changed check alone", read)
	}
	merger.waitMessages("closed", 1, "an edit of the approved check alone")
	if closed := merger.messages("closed")[0]; !strings.Contains(closed, "Verification document") {
		t.Fatalf("the closed-gate message is %q; want it to name the Verification document", closed)
	}
	if gate, stands := r.gate(key); (gate.ExemptVersion != nil && *gate.ExemptVersion == gate.CurrentVersion) || gate.ApprovedVersion != nil ||
		stands != string(phase.Merging) {
		t.Fatalf("the design gate of %s after the edit of its approved check is %+v in %s; want it closed in merging, exempt and approved by nothing", key, gate, stands)
	}
	if merger.enqueue(stepRecordsHead) {
		t.Fatalf("the merger enqueued %s after the closed-gate message of a changed approved check", key)
	}
	if codes := r.ending(key, stepRecordsHead).codes(); !slices.Equal(codes, []string{"changes_checks_without_approval"}) {
		t.Fatalf("the ending after the edit of the approved check gives %v, want [changes_checks_without_approval]", codes)
	}

	// The person approves the spec with its Verification document, and the merge is released.
	r.approve(r.issues[approved.title], r.session(key, claim.RoleArchitect), true)
	merger.waitMessages("opened", 1, "the person's approval of the spec with its Verification document")
	if !merger.enqueue(stepRecordsHead) {
		t.Fatalf("the merger could not enqueue %s after the person approved the spec with its Verification document", key)
	}
	if want := []string{"enqueue " + stepRecordsHead, "dequeue " + stepRecordsHead, "enqueue " + stepRecordsHead}; !slices.Equal(merger.actions, want) {
		t.Fatalf("the merger's queue actions are %v, want %v", merger.actions, want)
	}
	r.mergeAt(key, stepRecordsHead)
}

// specGateConcluded is a spec of these walks: text, then a conclusion that says no statement
// applies, with its evidence.
func specGateConcluded(text string) string {
	return text + "\n\n" + specGateConclusion("c-concluded", ` no_statement="true"`, "Evidence: the test of the change passes after it, and fails before it.")
}

// TestAVerificationEditOfAnApprovedCheckClosesAPersonsApproval is R25 for a gate a person's
// approval opened under the rule of a change request: the approval covers the checks the
// Verification document held then. An edit of the Verification document alone that changes an
// approved check closes the gate under the running merger, which is told and enqueues nothing,
// though the person's approval is kept, and the ending waits for the person
// (changes_checks_without_approval). The same edit of an acceptance check that names no statement
// leaves the gate open, and its merger is told nothing. A gate of today's rule — a repository with no
// spec source — is untouched by an edit of its Verification document (R24).
func TestAVerificationEditOfAnApprovedCheckClosesAPersonsApproval(t *testing.T) {
	proposing := specGateFixture{title: "Order numbers printed on invoices", repo: liveRepo, legion: true,
		spec:   specGateConcluded(":::requirement{#an-invoice-names-its-order kind=\"behavior\"}\nAn invoice names its order.\n:::"),
		checks: specGateCheckBlock("1", "A second charge of one order fails.")}
	own := specGateFixture{title: "Footer typography tweak", repo: liveRepo, legion: true,
		spec:   specGateConcluded(":::requirement{#an-invoice-prints-its-footer kind=\"behavior\"}\nAn invoice prints its footer.\n:::"),
		checks: specGateOwnCheckBlock("The invoice footer reads the new words.")}
	plain := specGateFixture{title: "Tools runner step logging", repo: "acme/tools", legion: true,
		spec:   specGateConcluded("The runner of the tools repository logs each step."),
		checks: specGateCheckBlock("1", "A second charge of one order fails.")}
	r := newSpecGateRig(t, proposing, own, plain)
	// Each waits for the person, who approves the spec with its Verification document; the root walks
	// to merging, its three lists recorded at the head, and its merger runs and enqueues the head.
	walk := func(fixture specGateFixture, rule string) (string, *specGateMerger) {
		key := r.issues[fixture.title].key
		if status, body := r.register(key, 1); status != http.StatusOK {
			t.Fatalf("the registration of %q = %d %s, want 200", fixture.title, status, body)
		}
		r.approve(r.issues[fixture.title], r.session(key, claim.RoleArchitect), true)
		var gate specGateView
		testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, fmt.Sprintf("the gate of %q to open by the person's approval", fixture.title), func() bool {
			gate, _ = r.gate(key)
			return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.CurrentVersion
		})
		if gate.ExemptVersion != nil || gate.Rule != rule {
			t.Fatalf("the gate of %q is %+v, want it opened by the person's approval alone under the rule %q", fixture.title, gate, rule)
		}
		r.walkToMerging(key)
		r.recordOutcomes(key, r.session(key, claim.RoleImplementer), "author", stepRecordsHead)
		reviewer := r.session(key, claim.RoleReviewer)
		r.recordOutcomes(key, reviewer, "reviewer", stepRecordsHead)
		r.recordOutcomes(key, reviewer, "final", stepRecordsHead)
		r.awaitLaunch(r.tokenOf(key, claim.RoleMerger))
		m := r.merger(key)
		if !m.enqueue(stepRecordsHead) {
			t.Fatalf("the merger of %q could not enqueue its head under the person's approval", fixture.title)
		}
		return key, m
	}

	// The approved check changes, and nothing else: the gate the person's approval opened closes
	// under the running merger.
	key, merger := walk(proposing, "change_request")
	r.editVerification(key, deleteBlock(specGateCheck), insertAtEnd(specGateCheckBlock("1", "A second charge of one order fails within a day.")))
	if read := r.changeRequest(key); read.VerificationVersion != 2 || !read.ChangesChecks {
		t.Fatalf("the change request after an edit of the approved check reads %+v; want Verification version 2 and a changed check", read)
	}
	merger.waitMessages("closed", 1, "an edit of the approved check under the person's approval")
	if closed := merger.messages("closed")[0]; !strings.Contains(closed, "Verification document") {
		t.Fatalf("the closed-gate message is %q; want it to name the Verification document", closed)
	}
	if gate, stands := r.gate(key); gate.ApprovedVersion == nil || *gate.ApprovedVersion != gate.CurrentVersion || !gate.ChecksUnapproved ||
		gate.VerificationVersion != 2 || stands != string(phase.Merging) {
		t.Fatalf("the design gate of %s after the edit of its approved check is %+v in %s; want the approval kept, held by the changed check "+
			"read at Verification version 2, in merging", key, gate, stands)
	}
	if merger.enqueue(stepRecordsHead) {
		t.Fatalf("the merger enqueued %s after the closed-gate message of a changed approved check", key)
	}
	if want := []string{"enqueue " + stepRecordsHead, "dequeue " + stepRecordsHead}; !slices.Equal(merger.actions, want) {
		t.Fatalf("the merger's queue actions are %v, want %v", merger.actions, want)
	}
	if codes := r.ending(key, stepRecordsHead).codes(); !slices.Equal(codes, []string{"changes_checks_without_approval"}) {
		t.Fatalf("the ending after the edit of the approved check gives %v, want [changes_checks_without_approval]", codes)
	}

	// The acceptance check changes: no part of the change request, so the gate the person opened
	// stands open once the daemon has read the edit, and its merger is told nothing.
	ownKey, ownMerger := walk(own, "change_request")
	r.editVerification(ownKey, deleteBlock(specGateOwnCheck), insertAtEnd(specGateOwnCheckBlock("The invoice footer reads the new words, in bold.")))
	var ownGate specGateView
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the daemon to read the edit of the acceptance check", func() bool {
		ownGate, _ = r.gate(ownKey)
		return ownGate.VerificationVersion == 2
	})
	if ownGate.ApprovedVersion == nil || *ownGate.ApprovedVersion != ownGate.CurrentVersion || ownGate.ChecksUnapproved {
		t.Fatalf("the design gate of %s after an edit of its acceptance check is %+v, want it open by the person's approval", ownKey, ownGate)
	}
	if closed := ownMerger.messages("closed"); len(closed) != 0 {
		t.Fatalf("the merger of %s was told %q after an edit of its acceptance check; want nothing", ownKey, closed)
	}
	if !ownMerger.enqueue(stepRecordsHead) {
		t.Fatalf("the merger of %s could not enqueue after an edit of its acceptance check", ownKey)
	}

	// A repository with no spec source keeps today's gate: the edit of its Verification document is
	// no input of it. Its root is approved by the person and planning, and the root's architect is
	// told each move of the gate as the merger is (workflow's tellGateMoved); the walk's GitHub
	// reviews name liveRepo alone, so this root does not walk to merging. The spec's next version,
	// applied after the edit, closes the gate as a version always has, and the one closed-gate
	// message the architect gets is that version's.
	plainKey := r.issues[plain.title].key
	if status, body := r.register(plainKey, 1); status != http.StatusOK {
		t.Fatalf("the registration of %s = %d %s, want 200", plainKey, status, body)
	}
	r.approve(r.issues[plain.title], r.session(plainKey, claim.RoleArchitect), true)
	r.on(plainKey)
	r.waitPhase(phase.Planning)
	if gate, _ := r.gate(plainKey); gate.ApprovedVersion == nil || *gate.ApprovedVersion != gate.CurrentVersion || gate.Rule != "approval" {
		t.Fatalf("the gate of %s is %+v, want it approved by the person under today's rule", plainKey, gate)
	}
	architectTopic := notify.RoleTopicPrefix + string(r.tokenOf(plainKey, claim.RoleArchitect))
	closedToArchitect := func() []string {
		var closed []string
		for _, published := range r.notices.acceptedOn(architectTopic) {
			if strings.HasPrefix(published.Message, "The design gate of "+plainKey+" closed:") {
				closed = append(closed, published.Message)
			}
		}
		return closed
	}
	r.editVerification(plainKey, deleteBlock(specGateCheck), insertAtEnd(specGateCheckBlock("1", "A second charge of one order fails within a day.")))
	r.editSpec(plainKey, insertAtEnd("The runner names its log file.\n"))
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the architect of "+plainKey+" to be told the gate closed", func() bool {
		return len(closedToArchitect()) > 0
	})
	time.Sleep(liveSettle)
	if closed := closedToArchitect(); len(closed) != 1 || strings.Contains(closed[0], "Verification document") || !strings.Contains(closed[0], "of its spec document") {
		t.Fatalf("the architect of %s was told %q; want one closed gate, its spec's version's, and none of its Verification document", plainKey, closed)
	}
	if gate, _ := r.gate(plainKey); gate.Rule != "approval" || gate.VerificationVersion != 0 || gate.ChecksUnapproved {
		t.Fatalf("the design gate of %s is %+v, want today's gate, which reads no Verification document", plainKey, gate)
	}
}

// specGateGitHub is GitHub's REST API as these walks' daemon reads it: every repository with
// default branch main, the pull request of each issue's branch at its current head, clean until it
// merges, the person of the reviews with write access, and a GraphQL root that knows no pull
// request. A push moves a pull request's head (move); every other route is a 404.
type specGateGitHub struct {
	t     *testing.T
	url   string
	mu    sync.Mutex
	pulls map[int]*specGatePull
}

// specGatePull is one pull request: its branch, its head and whether GitHub merged it.
type specGatePull struct {
	branch, head string
	merged       bool
}

func newSpecGateGitHub(t *testing.T) *specGateGitHub {
	t.Helper()
	g := &specGateGitHub{t: t, pulls: map[int]*specGatePull{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(path, "/")
		repository := len(parts) >= 2 && strings.HasPrefix(r.URL.Path, "/repos/")
		rest := ""
		if repository {
			rest = "/" + strings.Join(parts[2:], "/")
			if len(parts) == 2 {
				rest = ""
			}
		}
		pull, reviews := strings.CutSuffix(strings.TrimPrefix(rest, "/pulls/"), "/reviews")
		answer, known := "", false
		if number, err := strconv.Atoi(pull); err == nil && strings.HasPrefix(rest, "/pulls/") {
			answer, known = g.pullRequest(number)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && repository && rest == "":
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && repository && rest == "/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && repository && rest == "/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && repository && rest == "/pulls":
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(g.pullRequests(), ","))
		case r.Method == http.MethodGet && repository && known && !reviews:
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && repository && known && reviews:
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && repository && rest == "/collaborators/"+stepRecordsPerson+"/permission":
			_, _ = io.WriteString(w, `{"permission":"write","user":{"login":"`+stepRecordsPerson+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
	return g
}

// open is pull request number of branch, open at stepRecordsHead.
func (g *specGateGitHub) open(branch string, number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = &specGatePull{branch: branch, head: stepRecordsHead}
}

// move is a push that moves pull request number to head.
func (g *specGateGitHub) move(number int, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, ok := g.pulls[number]
	if !ok {
		g.t.Fatalf("GitHub moved pull request #%d, which the walk never opened", number)
	}
	pull.head = head
}

// merge is GitHub's merge of pull request number: every read from now on answers it merged.
func (g *specGateGitHub) merge(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, ok := g.pulls[number]
	if !ok {
		g.t.Fatalf("GitHub merged pull request #%d, which the walk never opened", number)
	}
	pull.merged = true
}

// pullRequests is each open or merged pull request as GitHub answers it now.
func (g *specGateGitHub) pullRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	numbers := make([]int, 0, len(g.pulls))
	for number := range g.pulls {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	answers := make([]string, len(numbers))
	for place, number := range numbers {
		answers[place] = g.pulls[number].answer(number)
	}
	return answers
}

// pullRequest is pull request number as GitHub answers it now, and whether it was opened.
func (g *specGateGitHub) pullRequest(number int) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull, ok := g.pulls[number]
	if !ok {
		return "", false
	}
	return pull.answer(number), true
}

// answer is the pull request as GitHub's REST API answers it: open at its head and clean until it
// merges, closed and merged from then on.
func (p *specGatePull) answer(number int) string {
	state, merged, mergeable := "open", false, "clean"
	if p.merged {
		state, merged, mergeable = "closed", true, "unknown"
	}
	return fmt.Sprintf(`{"number":%d,"state":%q,"merged":%t,"merge_commit_sha":%q,"mergeable":true,"mergeable_state":%q,"head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"updated_at":"2026-11-17T00:00:00Z"}`,
		number, state, merged, "merge-"+p.head, mergeable, p.branch, p.head)
}
