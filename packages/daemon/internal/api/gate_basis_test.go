package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The issues of every basis case (dispatch://INFRA-3320, k2): R is the tree root that registers,
// P its Dispatch parent, G P's parent where a case sets one, and S R's sibling under P.
const (
	basisR = "LEGION-208"
	basisP = "LEGION-150"
	basisG = "LEGION-120"
	basisS = "LEGION-160"

	rSpec         = "a2080000-0000-4000-8000-000000000001"
	rVerification = "a2080000-0000-4000-8000-000000000002"
	rDecisions    = "a2080000-0000-4000-8000-000000000003"
	rDiagram      = "a2080000-0000-4000-8000-000000000004"
	pSpec         = "b1500000-0000-4000-8000-000000000001"
	pVerification = "b1500000-0000-4000-8000-000000000002"
	pDecisions    = "b1500000-0000-4000-8000-000000000003"
	gSpec         = "c1200000-0000-4000-8000-000000000001"
	sSpec         = "d1600000-0000-4000-8000-000000000001"
)

var (
	basisApprovedAt = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	basisAlice      = dispatch.Actor{Kind: "user", ID: "alice"}
	basisBob        = dispatch.Actor{Kind: "user", ID: "bob"}
)

// requirementBlocks is a document version that marks a requirement block of each id, with a
// callout and a paragraph beside them, which are no requirement.
func requirementBlocks(ids ...string) string {
	var text strings.Builder
	text.WriteString("# The design\n\nA paragraph that is no requirement.\n\n:::callout{#note}\nA note that is no requirement.\n:::\n\n")
	for _, id := range ids {
		fmt.Fprintf(&text, ":::requirement{#%s}\n**%s.** A requirement.\n:::\n\n", id, id)
	}
	return text.String()
}

// checkBlockOf is a check of the given id serving each reference, or serving nothing when none is
// given.
func checkBlockOf(id string, serves ...string) string {
	if len(serves) == 0 {
		return fmt.Sprintf(":::check{#%s surface=\"api\"}\nA check.\n:::\n\n", id)
	}
	return fmt.Sprintf(":::check{#%s serves=\"%s\" surface=\"api\"}\nA check.\n:::\n\n", id, strings.Join(serves, " "))
}

// p2 is the reference R's one check serves in the world every case starts from.
const p2 = "dispatch://LEGION-150/spec#b-p2"

// rViewSpec is R's primary document as a view writes it: no requirement block, and a Delivers:
// line naming the ancestor's requirement it delivers.
const rViewSpec = "# R\n\nDelivers: dispatch://LEGION-150/spec#b-p2@v3, the whole of P2.\n"

// basisReason is a reason of exactly n characters, each past the first ASCII ones taking two bytes,
// so the bound is read in characters and not in bytes.
func basisReason(n int) string {
	prefix := "R's check serves P2; both fresh reviews passed. "
	return prefix + strings.Repeat("é", n-utf8.RuneCountInString(prefix))
}

// basisWorld is the Dispatch every basis case starts from, the one k2's negative control names: P
// carries legion and is in progress; its primary document marks requirement blocks p1 to p3 and
// p:4, whose id a reference percent-encodes, and a person approved its latest version 3; its
// Verification document holds a person's approval of its latest version 2 and its decisions
// document marks a requirement block d1. R, at version 4 of its primary document, holds no review
// and no requirement block, lists a diagram file beside its documents, and its Verification
// document's one check serves P2. G is above nobody until a case makes it P's parent, and S, R's
// sibling under P, marks requirement block s1. seq is R's event sequence, which a case sets apart
// so that each registration that records is its own fact.
func basisWorld(seq int64) statusRecorder {
	p := basisP
	return statusRecorder{
		issues: map[string]dispatch.Issue{
			basisR: {Key: basisR, LastSeq: seq, Parent: &p, Status: "in_progress", Labels: []string{"legion"},
				PrimaryArtifactID: rSpec, VerificationArtifactID: rVerification},
			basisP: {Key: basisP, LastSeq: 40, Status: "in_progress", Labels: []string{"Legion"},
				PrimaryArtifactID: pSpec, VerificationArtifactID: pVerification, DecisionsArtifactID: pDecisions},
			basisG: {Key: basisG, LastSeq: 50, Status: "in_progress", Labels: []string{"legion"}, PrimaryArtifactID: gSpec},
			basisS: {Key: basisS, LastSeq: 60, Parent: &p, Status: "in_progress", Labels: []string{"legion"}, PrimaryArtifactID: sSpec},
		},
		documents: map[string]string{
			rSpec: basisR, rVerification: basisR, rDecisions: basisR, rDiagram: basisR,
			pSpec: basisP, pVerification: basisP, pDecisions: basisP, gSpec: basisG, sSpec: basisS,
		},
		approvals: map[string]dispatch.Approval{
			rSpec:         {State: "draft", LatestVersion: 4},
			rVerification: {State: "draft", LatestVersion: 2, Role: dispatch.ArtifactRoleVerification, Slug: dispatch.VerificationSlug},
			rDecisions:    {State: "draft", LatestVersion: 1, Role: dispatch.ArtifactRoleDecisions, Slug: dispatch.DecisionsSlug},
			pSpec:         {State: "approved", LatestVersion: 3, Version: new(3), By: basisAlice, Seq: 21, At: basisApprovedAt},
			pVerification: {State: "approved", LatestVersion: 2, Version: new(2), By: basisAlice, Seq: 22, At: basisApprovedAt,
				Role: dispatch.ArtifactRoleVerification, Slug: dispatch.VerificationSlug},
			pDecisions: {State: "draft", LatestVersion: 1, Role: dispatch.ArtifactRoleDecisions, Slug: dispatch.DecisionsSlug},
			gSpec:      {State: "draft", LatestVersion: 5},
			sSpec:      {State: "approved", LatestVersion: 1, Version: new(1), By: basisAlice, Seq: 61, At: basisApprovedAt},
		},
		reviews: map[string][]dispatch.Review{
			pSpec:         {{Version: 3, State: "approved", Kind: "person", Actor: basisAlice, At: basisApprovedAt}},
			pVerification: {{Version: 2, State: "approved", Kind: "person", Actor: basisAlice, At: basisApprovedAt}},
			sSpec:         {{Version: 1, State: "approved", Kind: "person", Actor: basisAlice, At: basisApprovedAt}},
		},
		versions: map[string]map[int]string{
			pSpec:         {3: requirementBlocks("p1", "p2", "p3", "p:4")},
			pVerification: {2: checkBlockOf("pk1", "dispatch://LEGION-150/spec#b-p1")},
			pDecisions:    {1: requirementBlocks("d1")},
			gSpec:         {5: requirementBlocks("g1", "g2")},
			sSpec:         {1: requirementBlocks("s1")},
			rSpec:         {4: rViewSpec},
			rVerification: {2: checkBlockOf("k1", p2)},
			rDecisions:    {1: "# Decisions\n\nNone yet.\n"},
		},
		artifacts: map[string][]dispatch.Artifact{
			basisR: {
				{ID: rSpec, Slug: "spec", Kind: "doc", Primary: true, LatestVersion: 4},
				{ID: rVerification, Slug: dispatch.VerificationSlug, Kind: "doc", Role: dispatch.ArtifactRoleVerification, LatestVersion: 2},
				{ID: rDiagram, Slug: "diagram-png", Kind: "file", LatestVersion: 1},
			},
		},
	}
}

// withVersion gives a document a version's text.
func (r *statusRecorder) withVersion(document string, version int, markdown string) {
	if r.versions[document] == nil {
		r.versions[document] = map[int]string{}
	}
	r.versions[document][version] = markdown
}

// withParent sets an issue's Dispatch parent.
func (r *statusRecorder) withParent(key, parent string) {
	issue := r.issues[key]
	issue.Parent = &parent
	r.issues[key] = issue
}

// basisBody is a register_gate call for R's primary document at version 4 on P's primary document
// at version 3, with a reason of exactly 1,000 characters.
func basisBody(grant string) map[string]any {
	return map[string]any{
		"grantId": grant, "issue": basisR, "artifactId": rSpec, "version": 4,
		"basisArtifactId": pSpec, "basisVersion": 3, "reason": basisReason(reasonRunes),
	}
}

// basisHarness is an architect harness gating steps, holding R as a recorded tree root with its
// live architect.
func basisHarness(t *testing.T, steps []record.DesignStep) (*harness, *factRecorder, *statusRecorder, liveClaim) {
	t.Helper()
	h, facts, statuses := newArchitectHarnessGating(t, steps, nil, nil)
	seedTree(t, h, basisR)
	return h, facts, statuses, newLiveClaim(t, h, basisR, claim.RoleArchitect)
}

// gateEventsOf is how many register_gate facts the intake has taken for issue: what a refused
// registration must leave as it found it.
func gateEventsOf(t *testing.T, h *harness, issue string) int {
	t.Helper()
	var count int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where source = 'api' and event_id like $1", "gate:"+issue+":%").Scan(&count); err != nil {
		t.Fatalf("count the gate facts of %s: %v", issue, err)
	}
	return count
}

// putBasisRecord records P as the daemon would record a tree root in phase at, and takes the record
// away when the test ends.
func putBasisRecord(t *testing.T, h *harness, at phase.Phase) {
	t.Helper()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: basisP, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: basisP, Project: testProject,
			Title: basisP, Phase: at, Status: "in_progress",
		})
	}); err != nil {
		t.Fatalf("record %s in %s: %v", basisP, at, err)
	}
	t.Cleanup(func() {
		if _, err := h.store.Pool().Exec(context.Background(), "delete from issues where key = $1", basisP); err != nil {
			t.Errorf("take the record of %s away: %v", basisP, err)
		}
	})
}

// Every case R1 names as a refusal is refused with its own code and a message naming what Dispatch
// shows, and records nothing: no fact reaches the intake. Each case is the world k2's negative
// control names with that one case put in, so each refusal comes from its case and not from the
// world; TestBasisGateOpenings opens the step on that same world.
func TestBasisGateRefusals(t *testing.T) {
	h, facts, statuses, architect := basisHarness(t, nil)
	for index, tc := range []struct {
		name   string
		change func(world *statusRecorder, body map[string]any)
		// record, when set, is the phase the daemon records P in for the case.
		record phase.Phase
		status int
		code   string
		says   []string
	}{
		// The call.
		{name: "a basis with no reason", change: func(_ *statusRecorder, body map[string]any) { delete(body, "reason") },
			status: http.StatusBadRequest, code: "BASIS_INCOMPLETE", says: []string{"leaves out reason"}},
		{name: "a reason with no basis", change: func(_ *statusRecorder, body map[string]any) {
			delete(body, "basisArtifactId")
			delete(body, "basisVersion")
		}, status: http.StatusBadRequest, code: "BASIS_INCOMPLETE", says: []string{"leaves out basisArtifactId and basisVersion"}},
		{name: "a basis document that is no UUID", change: func(_ *statusRecorder, body map[string]any) { body["basisArtifactId"] = "dispatch://LEGION-150/spec" },
			status: http.StatusBadRequest, code: "INVALID_ARTIFACT_ID", says: []string{"basisArtifactId"}},
		{name: "a basis version below 1", change: func(_ *statusRecorder, body map[string]any) { body["basisVersion"] = -1 },
			status: http.StatusBadRequest, code: "INVALID_VERSION", says: []string{"basisVersion"}},
		{name: "a reason of 1,001 characters", change: func(_ *statusRecorder, body map[string]any) { body["reason"] = basisReason(reasonRunes + 1) },
			status: http.StatusBadRequest, code: "REASON_INVALID", says: []string{"1001 characters", "1 to 1000"}},
		{name: "a reason holding a decision block", change: func(_ *statusRecorder, body map[string]any) {
			body["reason"] = "R serves P2.\n\n" + theDecisionBlock + "\n"
		}, status: http.StatusBadRequest, code: "REASON_HOLDS_DECISION_BLOCK", says: []string{"holds a decision block", "(ask)"}},

		// R's own document.
		{name: "a version of R's document below its latest", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "draft", LatestVersion: 5}
		}, status: http.StatusConflict, code: "ROOT_VERSION_NOT_LATEST", says: []string{"version 5", "names version 4"}},
		{name: "a person's changes request as R's newest review", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), By: basisBob, Reason: new("split it")}
			world.reviews[rSpec] = []dispatch.Review{{Version: 4, State: "changes_requested", Kind: "person", Actor: basisBob}}
		}, status: http.StatusConflict, code: "ROOT_CHANGES_REQUESTED", says: []string{"changes request on version 4 by user:bob"}},
		{name: "a changes request on R's v4 followed by v5 with an open approval request", change: func(world *statusRecorder, body map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 5, Version: new(4), By: basisBob, AskID: "ask-r5", WaitingOn: "human"}
			world.reviews[rSpec] = []dispatch.Review{{Version: 4, State: "changes_requested", Kind: "person", Actor: basisBob}}
			body["version"] = 5
		}, status: http.StatusConflict, code: "ROOT_CHANGES_REQUESTED", says: []string{"changes request on version 4 by user:bob", "whatever its approval shows"}},

		// The chain.
		{name: "a sibling's document", change: func(_ *statusRecorder, body map[string]any) {
			body["basisArtifactId"], body["basisVersion"] = sSpec, 1
		}, status: http.StatusConflict, code: "BASIS_NOT_ON_CHAIN", says: []string{sSpec, "on LEGION-160", "LEGION-208 under LEGION-150"}},
		{name: "R's own document", change: func(_ *statusRecorder, body map[string]any) {
			body["basisArtifactId"], body["basisVersion"] = rVerification, 2
		}, status: http.StatusConflict, code: "BASIS_NOT_ON_CHAIN", says: []string{"LEGION-208 itself"}},
		{name: "a document Dispatch does not have", change: func(_ *statusRecorder, body map[string]any) {
			body["basisArtifactId"] = "e0000000-0000-4000-8000-000000000001"
		}, status: http.StatusConflict, code: "BASIS_NOT_ON_CHAIN", says: []string{"Dispatch has no document"}},
		{name: "P's Verification document for the spec step", change: func(_ *statusRecorder, body map[string]any) {
			body["basisArtifactId"], body["basisVersion"] = pVerification, 2
		}, status: http.StatusConflict, code: "GATE_STEP_DOCUMENT_MISMATCH", says: []string{pVerification, "spec step's document of LEGION-150"}},
		{name: "G's approval with a review on P", change: func(world *statusRecorder, body map[string]any) {
			gUnderP(world)
			world.approvals[pSpec] = dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), By: basisBob}
			body["basisArtifactId"], body["basisVersion"] = gSpec, 5
		}, status: http.StatusConflict, code: "BASIS_NOT_NEAREST", says: []string{"LEGION-150 stands between LEGION-208 and LEGION-120", "a changes request on version 3 by user:bob"}},
		{name: "G's approval with only an open approval request on P", change: func(world *statusRecorder, body map[string]any) {
			gUnderP(world)
			world.approvals[pSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-p3", WaitingOn: "human"}
			body["basisArtifactId"], body["basisVersion"] = gSpec, 5
		}, status: http.StatusConflict, code: "BASIS_NOT_NEAREST", says: []string{"LEGION-150 stands between", "open approval request (ask ask-p3)"}},

		// The ancestor.
		{name: "P without the legion label", change: func(world *statusRecorder, _ map[string]any) {
			issue := world.issues[basisP]
			issue.Labels = []string{"infra"}
			world.issues[basisP] = issue
		}, status: http.StatusConflict, code: "BASIS_NOT_IN_LEGION", says: []string{"LEGION-150 carries no \"legion\" label", "infra"}},
		{name: "P done", change: func(world *statusRecorder, _ map[string]any) { setStatus(world, basisP, "done") },
			status: http.StatusConflict, code: "BASIS_ACCEPTED", says: []string{"Dispatch shows LEGION-150 done"}},
		{name: "P in Dispatch status retro", change: func(world *statusRecorder, _ map[string]any) { setStatus(world, basisP, "retro") },
			status: http.StatusConflict, code: "BASIS_ACCEPTED", says: []string{"Dispatch shows LEGION-150 retro"}},
		{name: "P recorded by the daemon in merging", record: phase.Merging,
			status: http.StatusConflict, code: "BASIS_ACCEPTED", says: []string{"the daemon records LEGION-150 in merging"}},
		{name: "P's document with no review", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "draft", LatestVersion: 3}
			world.reviews[pSpec] = nil
		}, status: http.StatusConflict, code: "BASIS_NOT_APPROVED", says: []string{"no review and no open approval request at its latest version 3"}},
		{name: "P's latest review a changes request", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), By: basisBob, Seq: 23}
			world.reviews[pSpec] = append([]dispatch.Review{{Version: 3, State: "changes_requested", Kind: "person", Actor: basisBob}}, world.reviews[pSpec]...)
		}, status: http.StatusConflict, code: "BASIS_NOT_APPROVED", says: []string{"a changes request on version 3 by user:bob"}},
		{name: "the person's approval of a version older than P's latest", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "stale", LatestVersion: 4, Version: new(3), By: basisAlice, Seq: 21}
		}, status: http.StatusConflict, code: "BASIS_NOT_APPROVED", says: []string{"an approval of version 3 by user:alice, older than its latest version 4"}},
		{name: "a session's approval", change: func(world *statusRecorder, _ map[string]any) {
			session := dispatch.Actor{Kind: "session", ID: "ses-outsider"}
			world.approvals[pSpec] = dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3), By: session, Seq: 21}
			world.reviews[pSpec] = []dispatch.Review{{Version: 3, State: "approved", Kind: "plan_review", Actor: session}}
		}, status: http.StatusConflict, code: "BASIS_NOT_BY_PERSON", says: []string{"given by session:ses-outsider, not by a person"}},
		{name: "Dispatch's own approval", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3), By: dispatch.SettlementActor, Seq: 21}
			world.reviews[pSpec] = []dispatch.Review{{Version: 3, State: "approved", Kind: "person", Actor: dispatch.SettlementActor}}
		}, status: http.StatusConflict, code: "BASIS_NOT_BY_PERSON", says: []string{"given by system:document-settlement, not by a person"}},
		{name: "a version other than P's approved one", change: func(_ *statusRecorder, body map[string]any) { body["basisVersion"] = 2 },
			status: http.StatusConflict, code: "BASIS_VERSION_MISMATCH", says: []string{"approved at its latest version 3", "names version 2"}},
		{name: "an older version than the one P's open request asks about", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 4, Version: new(3), By: basisAlice, AskID: "ask-p4", WaitingOn: "human"}
		}, status: http.StatusConflict, code: "BASIS_VERSION_MISMATCH", says: []string{"latest version 4 with an open approval request", "names version 3"}},
		{name: "P's open request handed back to its agent", change: func(world *statusRecorder, body map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 4, Version: new(3), By: basisAlice, AskID: "ask-p4", WaitingOn: "agent"}
			body["basisVersion"] = 4
		}, status: http.StatusConflict, code: "BASIS_REQUEST_NOT_OPEN", says: []string{"open approval request (ask ask-p4) at its latest version 4, waiting on the agent"}},
		{name: "P's open request on a version that already holds a changes request", change: func(world *statusRecorder, body map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 4, Version: new(4), By: basisBob, AskID: "ask-p4", WaitingOn: "human"}
			world.reviews[pSpec] = append([]dispatch.Review{{Version: 4, State: "changes_requested", Kind: "person", Actor: basisBob}}, world.reviews[pSpec]...)
			body["basisVersion"] = 4
		}, status: http.StatusConflict, code: "BASIS_REQUEST_NOT_OPEN", says: []string{"version 4 of LEGION-150's primary document", "changes_requested by user:bob"}},
		{name: "P's primary document with no requirement block", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(pSpec, 3, requirementBlocks())
		}, status: http.StatusConflict, code: "BASIS_NO_REQUIREMENTS", says: []string{"version 3 marks no requirement block"}},

		// The view.
		{name: "R's primary document holding a requirement block", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rSpec, 4, rViewSpec+"\n:::requirement{#r1}\nR's own.\n:::\n")
		}, status: http.StatusConflict, code: "VIEW_HOLDS_REQUIREMENT", says: []string{"LEGION-208's primary document spec at version 4 holds requirement block r1"}},
		{name: "a requirement block in R's Verification document", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", p2)+":::requirement{#rv1}\nR's own.\n:::\n")
		}, status: http.StatusConflict, code: "VIEW_HOLDS_REQUIREMENT", says: []string{"Verification document verification-md at version 2 holds requirement block rv1"}},
		{name: "a requirement block in R's decisions document", change: func(world *statusRecorder, _ map[string]any) {
			withDecisions(world, ":::requirement{#rd1}\nR's own.\n:::\n")
		}, status: http.StatusConflict, code: "VIEW_HOLDS_REQUIREMENT", says: []string{"decisions document decisions-md at version 1 holds requirement block rd1"}},
		{name: "no Verification document on R", change: func(world *statusRecorder, _ map[string]any) {
			world.artifacts[basisR] = slices.DeleteFunc(slices.Clone(world.artifacts[basisR]), func(a dispatch.Artifact) bool { return a.ID == rVerification })
		}, status: http.StatusConflict, code: "VIEW_NO_VERIFICATION", says: []string{"no Verification document of LEGION-208"}},
		{name: "R's Verification document holding no check", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, "# Verification\n\nNo check yet.\n")
		}, status: http.StatusConflict, code: "VIEW_NO_CHECK", says: []string{"verification-md at version 2 holds no check block"}},
		{name: "a check block in R's primary document that serves P2", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rSpec, 4, rViewSpec+"\n"+checkBlockOf("ks", p2))
		}, status: http.StatusConflict, code: "VIEW_CHECK_OUTSIDE_VERIFICATION", says: []string{"primary document spec at version 4 holds check block ks"}},
		{name: "a check of R that serves nothing", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", p2)+checkBlockOf("k2"))
		}, status: http.StatusConflict, code: "VIEW_CHECK_SERVES_NOTHING", says: []string{"check k2 of LEGION-208's Verification document"}},
		{name: "a check of R that serves a requirement block of S", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-160/spec#b-s1"))
		}, status: http.StatusConflict, code: "VIEW_SERVES_OUTSIDE_BASIS", says: []string{"check k1", "serves dispatch://LEGION-160/spec#b-s1", "p1, p2, p3, p:4"}},
		{name: "a check that serves a block of P's primary document that is no requirement", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/spec#b-note"))
		}, status: http.StatusConflict, code: "VIEW_SERVES_OUTSIDE_BASIS", says: []string{"serves dispatch://LEGION-150/spec#b-note"}},
		{name: "a check that serves a block id P's document does not hold", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/spec#b-p9"))
		}, status: http.StatusConflict, code: "VIEW_SERVES_OUTSIDE_BASIS", says: []string{"serves dispatch://LEGION-150/spec#b-p9"}},
		{name: "one check serving P2 and another serving only a requirement of S", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", p2)+checkBlockOf("k2", "dispatch://LEGION-160/spec#b-s1"))
		}, status: http.StatusConflict, code: "VIEW_SERVES_OUTSIDE_BASIS", says: []string{"check k2", "serves dispatch://LEGION-160/spec#b-s1"}},
		{name: "a check that serves a requirement block of P's decisions document", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/artifact/decisions-md#b-d1"))
		}, status: http.StatusConflict, code: "VIEW_SERVES_OUTSIDE_BASIS", says: []string{"serves dispatch://LEGION-150/artifact/decisions-md#b-d1"}},

		// A read Dispatch fails.
		{name: "a failed read of P's approved version", change: func(world *statusRecorder, _ map[string]any) { delete(world.versions, pSpec) },
			status: http.StatusBadGateway, code: "DISPATCH_FAILED", says: []string{"version 3 of " + pSpec, "retry register_gate"}},
		{name: "a failed read of R's documents", change: func(world *statusRecorder, _ map[string]any) {
			world.artifactFails = map[string]error{basisR: errors.New("connection reset")}
		}, status: http.StatusBadGateway, code: "DISPATCH_FAILED", says: []string{"the documents of LEGION-208"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*statuses = basisWorld(int64(100 + index))
			body := basisBody(architect.grant(t))
			if tc.change != nil {
				tc.change(statuses, body)
			}
			if tc.record != "" {
				putBasisRecord(t, h, tc.record)
			}
			refusal := refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", body, nil), tc.status, tc.code)
			for _, want := range tc.says {
				if !strings.Contains(refusal, want) {
					t.Errorf("refusal = %q, want it naming %q", refusal, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("the refused registration applied %#v, want nothing recorded", got)
			}
			if events := gateEventsOf(t, h, basisR); events != 0 {
				t.Fatalf("gate facts taken for %s = %d, want none", basisR, events)
			}
		})
	}
}

// gUnderP makes G the parent of P, approved by a person at its latest version 5, and R's one check
// a check of G's requirement g1.
func gUnderP(world *statusRecorder) {
	world.withParent(basisP, basisG)
	world.approvals[gSpec] = dispatch.Approval{State: "approved", LatestVersion: 5, Version: new(5), By: basisAlice, Seq: 51, At: basisApprovedAt}
	world.reviews[gSpec] = []dispatch.Review{{Version: 5, State: "approved", Kind: "person", Actor: basisAlice, At: basisApprovedAt}}
	world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-120/spec#b-g1"))
}

// withDecisions gives R a decisions document holding markdown at version 1.
func withDecisions(world *statusRecorder, markdown string) {
	world.artifacts[basisR] = append(slices.Clone(world.artifacts[basisR]),
		dispatch.Artifact{ID: rDecisions, Slug: dispatch.DecisionsSlug, Kind: "doc", Role: dispatch.ArtifactRoleDecisions, LatestVersion: 1})
	world.withVersion(rDecisions, 1, markdown)
}

func setStatus(world *statusRecorder, key, status string) {
	issue := world.issues[key]
	issue.Status = status
	world.issues[key] = issue
}

// The step opens on a basis exactly where R1 says it does: the world k2's negative control names
// opens it, with a reason of exactly 1,000 characters, and so do the cases R1 counts as fine. Each
// records one registration fact naming the basis it rests on, the requirement references R's
// checks serve, the reason and the registering session, and the view fact naming the ancestor.
func TestBasisGateOpenings(t *testing.T) {
	h, facts, statuses, architect := basisHarness(t, nil)
	served := func(refs ...string) []string { return refs }
	for index, tc := range []struct {
		name   string
		change func(world *statusRecorder, body map[string]any)
		// record, when set, is the phase the daemon records P in for the case.
		record phase.Phase
		want   func(seq int64) intake.GateRegistered
		id     string
	}{
		{name: "the world every refusal starts from",
			want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) },
			id:   ":basis:" + pSpec + ":3:view-of:LEGION-150"},
		{name: "P on the backlog", change: func(world *statusRecorder, _ map[string]any) { setStatus(world, basisP, "backlog") },
			want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "P in icebox", change: func(world *statusRecorder, _ map[string]any) { setStatus(world, basisP, "icebox") },
			want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "P recorded by the daemon at work", record: phase.Implementing,
			want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "a check whose serves names P2 pinned to another version", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", p2+"@v2"))
		}, want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "a check whose serves names a block id that a reference percent-encodes", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/spec#b-p%3A4@v3"))
		}, want: func(seq int64) intake.GateRegistered {
			fact := basisFact(seq, architect.session)
			fact.Basis.Serves = served("dispatch://LEGION-150/spec#b-p%3A4@v3")
			return fact
		}},
		{name: "a check that serves P1 and P3, and another P3 again", change: func(world *statusRecorder, _ map[string]any) {
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/spec#b-p1", "dispatch://LEGION-150/spec#b-p3")+
				checkBlockOf("k2", "dispatch://LEGION-150/spec#b-p3@v3"))
		}, want: func(seq int64) intake.GateRegistered {
			fact := basisFact(seq, architect.session)
			fact.Basis.Serves = served("dispatch://LEGION-150/spec#b-p1@v3", "dispatch://LEGION-150/spec#b-p3@v3")
			return fact
		}},
		{name: "R holding the person's approval of an older version of its own document", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "stale", LatestVersion: 4, Version: new(2), By: basisAlice, Seq: 5, At: basisApprovedAt}
			world.reviews[rSpec] = []dispatch.Review{{Version: 2, State: "approved", Kind: "person", Actor: basisAlice}}
		}, want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "a changes request on R's v2 followed by the person's approval of R's v3, with R at v4", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "stale", LatestVersion: 4, Version: new(3), By: basisAlice, Seq: 6, At: basisApprovedAt}
			world.reviews[rSpec] = []dispatch.Review{
				{Version: 3, State: "approved", Kind: "person", Actor: basisAlice},
				{Version: 2, State: "changes_requested", Kind: "person", Actor: basisAlice},
			}
		}, want: func(seq int64) intake.GateRegistered { return basisFact(seq, architect.session) }},
		{name: "G's approval with P neither reviewed nor requested", change: func(world *statusRecorder, body map[string]any) {
			gUnderP(world)
			world.approvals[pSpec] = dispatch.Approval{State: "draft", LatestVersion: 3}
			body["basisArtifactId"], body["basisVersion"] = gSpec, 5
		}, want: func(seq int64) intake.GateRegistered {
			fact := basisFact(seq, architect.session)
			fact.Basis.Issue, fact.Basis.ArtifactID, fact.Basis.Version = basisG, gSpec, 5
			fact.Basis.Approval = record.EventMark{Seq: 51, At: basisApprovedAt}
			fact.Basis.Serves = served("dispatch://LEGION-120/spec#b-g1@v5")
			fact.ViewOf = basisG
			return fact
		}, id: ":basis:" + gSpec + ":5:view-of:LEGION-120"},
		// A pending basis: P's latest version 4 adds requirement p5, and its approval request waits
		// on the person with no review at that version. The registration names the request, and
		// carries no approval and no approver.
		{name: "P's open request of its latest version, waiting on the person", change: func(world *statusRecorder, body map[string]any) {
			world.approvals[pSpec] = dispatch.Approval{State: "awaiting", LatestVersion: 4, Version: new(3), By: basisAlice, Seq: 21, At: basisApprovedAt,
				AskID: "ask-p4", WaitingOn: "human"}
			world.withVersion(pSpec, 4, requirementBlocks("p1", "p2", "p3", "p:4", "p5"))
			world.withVersion(rVerification, 2, checkBlockOf("k1", "dispatch://LEGION-150/spec#b-p5@v4"))
			body["basisVersion"] = 4
		}, want: func(seq int64) intake.GateRegistered {
			fact := basisFact(seq, architect.session)
			fact.Basis.Version, fact.Basis.Approval, fact.Basis.Approver = 4, record.EventMark{}, ""
			fact.Basis.Serves = served("dispatch://LEGION-150/spec#b-p5@v4")
			fact.Basis.Pending = &record.PendingBasis{Ask: "ask-p4"}
			return fact
		}, id: ":basis:" + pSpec + ":4:pending:ask-p4:view-of:LEGION-150"},
		// R's latest version holds the person's approval: the step opens on it as today, and the
		// call records no basis, only the view fact every spec registration carries.
		{name: "R's latest version approved by the person, with a basis named as well", change: func(world *statusRecorder, _ map[string]any) {
			world.approvals[rSpec] = dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), By: basisAlice, Seq: 7, At: basisApprovedAt}
			world.reviews[rSpec] = []dispatch.Review{{Version: 4, State: "approved", Kind: "person", Actor: basisAlice}}
		}, want: func(seq int64) intake.GateRegistered {
			fact := basisFact(seq, architect.session)
			fact.Basis = nil
			fact.ApprovedVersion, fact.ApprovedMark, fact.ReviewedBy = 4, record.EventMark{Seq: 7, At: basisApprovedAt}, basisAlice
			return fact
		}, id: ":view-of:LEGION-150"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := int64(200 + index)
			*statuses = basisWorld(seq)
			body := basisBody(architect.grant(t))
			if tc.change != nil {
				tc.change(statuses, body)
			}
			if tc.record != "" {
				putBasisRecord(t, h, tc.record)
			}
			before := len(facts.recorded())
			if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", body, nil); recorder.Code != http.StatusOK {
				t.Fatalf("register on the basis = %d: %s", recorder.Code, recorder.Body)
			}
			got := facts.recorded()[before:]
			if want := tc.want(seq); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("registration facts = %#v, want %#v", got, want)
			}
			if tc.id != "" {
				assertProcessedEvent(t, h, fmt.Sprintf("gate:%s:0:spec:%s:4:%d%s", basisR, rSpec, seq, tc.id))
			}
		})
	}
}

// basisFact is the registration the world every basis case starts from records, read at R's event
// sequence seq by the architect session: R's document at version 4 on P's approved version 3,
// whose approval Dispatch recorded at sequence 21 of P's log, given by alice; R's one check serves
// P2; and the view fact names P.
func basisFact(seq int64, session string) intake.GateRegistered {
	return intake.GateRegistered{Issue: basisR, Step: record.DesignStepSpec, ArtifactID: rSpec, Version: 4, DispatchVersion: 4,
		Since: record.EventMark{Seq: seq},
		Basis: &record.GateBasis{Issue: basisP, ArtifactID: pSpec, Version: 3, Approval: record.EventMark{Seq: 21, At: basisApprovedAt},
			Approver: "user:alice", Serves: []string{"dispatch://LEGION-150/spec#b-p2@v3"}, Reason: basisReason(reasonRunes), Session: session},
		ViewOf: basisP}
}

// A deployment that gates the criteria step takes, as its basis, P's Verification document a
// person approved: the step's document of the ancestor, as stepDocument binds one. R's checks are
// held to the requirement blocks of P's primary document at its latest version, which a basis of
// another document never names. A registration of the criteria step reads no view fact.
func TestTheCriteriaStepsBasisGateIsTheAncestorsVerificationDocument(t *testing.T) {
	h, facts, statuses, architect := basisHarness(t, []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec})
	*statuses = basisWorld(300)
	body := basisBody(architect.grant(t))
	body["step"], body["artifactId"], body["version"], body["basisArtifactId"], body["basisVersion"] = "criteria", rVerification, 2, pVerification, 2
	if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", body, nil); recorder.Code != http.StatusOK {
		t.Fatalf("register the criteria step on P's Verification document = %d: %s", recorder.Code, recorder.Body)
	}
	want := intake.GateRegistered{Issue: basisR, Step: record.DesignStepCriteria, ArtifactID: rVerification, Version: 2, DispatchVersion: 2,
		Since: record.EventMark{Seq: 300},
		Basis: &record.GateBasis{Issue: basisP, ArtifactID: pVerification, Version: 2, Approval: record.EventMark{Seq: 22, At: basisApprovedAt},
			Approver: "user:alice", Serves: []string{"dispatch://LEGION-150/spec#b-p2@v3"}, Reason: basisReason(reasonRunes), Session: architect.session}}
	if got := facts.recorded(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("criteria registration facts = %#v, want %#v", got, want)
	}

	// P's primary document is the basis of the spec step alone: named for the criteria step it is
	// another step's document, and refused as one.
	body = basisBody(architect.grant(t))
	body["step"], body["artifactId"], body["version"] = "criteria", rVerification, 2
	refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", body, nil), http.StatusConflict, "GATE_STEP_DOCUMENT_MISMATCH")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("the refused criteria registration applied %#v, want nothing more recorded", got[1:])
	}
}

// Every registration of the spec step records the view fact (R4), with a basis or without: the
// nearest reviewed ancestor on the root's chain, where it marks requirement blocks at its latest
// version and the root's documents read as a view on them, and "" otherwise. A read of those
// documents Dispatch fails refuses the registration with DISPATCH_FAILED and records nothing, so
// no registration records a view fact it could not read.
func TestEveryGateRegistrationOfTheSpecStepRecordsItsViewFact(t *testing.T) {
	h, facts, statuses, architect := basisHarness(t, nil)
	noBasis := func() map[string]any {
		return map[string]any{"grantId": architect.grant(t), "issue": basisR, "artifactId": rSpec, "version": 4}
	}
	for index, tc := range []struct {
		name   string
		change func(world *statusRecorder)
		viewOf string
	}{
		{name: "a view on P, whose latest version a person approved", viewOf: basisP},
		{name: "a view on P, whose latest review is a changes request", change: func(world *statusRecorder) {
			world.approvals[pSpec] = dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), By: basisBob}
		}, viewOf: basisP},
		{name: "a view on G above an unreviewed P", change: func(world *statusRecorder) {
			gUnderP(world)
			world.approvals[pSpec] = dispatch.Approval{State: "draft", LatestVersion: 3}
		}, viewOf: basisG},
		{name: "R holding a requirement of its own", change: func(world *statusRecorder) {
			world.withVersion(rSpec, 4, rViewSpec+"\n:::requirement{#r1}\nR's own.\n:::\n")
		}},
		{name: "R's check serving a requirement P's nearer review does not mark", change: func(world *statusRecorder) {
			gUnderP(world)
		}},
		{name: "P marking no requirement block", change: func(world *statusRecorder) {
			world.withVersion(pSpec, 3, requirementBlocks())
		}},
		{name: "no ancestor reviewed", change: func(world *statusRecorder) {
			world.approvals[pSpec] = dispatch.Approval{State: "draft", LatestVersion: 3}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := int64(400 + index)
			*statuses = basisWorld(seq)
			if tc.change != nil {
				tc.change(statuses)
			}
			before := len(facts.recorded())
			if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", noBasis(), nil); recorder.Code != http.StatusOK {
				t.Fatalf("register with no basis = %d: %s", recorder.Code, recorder.Body)
			}
			got := facts.recorded()[before:]
			want := intake.GateRegistered{Issue: basisR, Step: record.DesignStepSpec, ArtifactID: rSpec, Version: 4, DispatchVersion: 4,
				Since: record.EventMark{Seq: seq}, ViewOf: tc.viewOf}
			if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("registration facts = %#v, want %#v", got, want)
			}
			id := fmt.Sprintf("gate:%s:0:spec:%s:4:%d", basisR, rSpec, seq)
			if tc.viewOf != "" {
				id += ":view-of:" + tc.viewOf
			}
			assertProcessedEvent(t, h, id)
		})
	}

	t.Run("a failed read of R's documents", func(t *testing.T) {
		*statuses = basisWorld(450)
		statuses.artifactFails = map[string]error{basisR: errors.New("connection reset")}
		before := len(facts.recorded())
		refusal := refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", noBasis(), nil), http.StatusBadGateway, "DISPATCH_FAILED")
		if !strings.Contains(refusal, "the documents of LEGION-208") {
			t.Fatalf("refusal = %q, want it naming the read that failed", refusal)
		}
		if got := facts.recorded(); len(got) != before {
			t.Fatalf("the refused registration applied %#v, want nothing recorded", got[before:])
		}
	})
	t.Run("a failed read of an ancestor", func(t *testing.T) {
		*statuses = basisWorld(451)
		statuses.readFails = map[string]error{basisP: errors.New("connection reset")}
		before := len(facts.recorded())
		refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", noBasis(), nil), http.StatusBadGateway, "DISPATCH_FAILED")
		if got := facts.recorded(); len(got) != before {
			t.Fatalf("the refused registration applied %#v, want nothing recorded", got[before:])
		}
	})
}

// A registration on a basis and one with none, of one version at one sequence, are two facts: the
// fact's id names the basis document, its version and whether it is pending, so the second is not
// taken as a duplicate that answers 200 and changes nothing. A retry of the same basis stays one.
func TestARegistrationWithNoBasisAfterOneOnABasisIsItsOwnFact(t *testing.T) {
	h, facts, statuses, architect := basisHarness(t, nil)
	*statuses = basisWorld(500)
	for range 2 {
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", basisBody(architect.grant(t)), nil); recorder.Code != http.StatusOK {
			t.Fatalf("register on the basis = %d: %s", recorder.Code, recorder.Body)
		}
	}
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after a retry on one basis = %#v, want one", got)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": basisR, "artifactId": rSpec, "version": 4,
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("register with no basis = %d: %s", recorder.Code, recorder.Body)
	}
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("facts after a registration with no basis = %#v, want two", got)
	}
	if second, ok := got[1].(intake.GateRegistered); !ok || second.Basis != nil || second.ViewOf != basisP {
		t.Fatalf("the second registration = %#v, want no basis and the view fact naming %s", got[1], basisP)
	}
}
