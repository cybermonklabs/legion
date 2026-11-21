package requests

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// reasonNaming is a reason whose issue keys name n distinct projects, P00 to P(n-1), none of
// which Dispatch holds.
func reasonNaming(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("P%02d-1", i)
	}
	return strings.Join(keys, " ")
}

// storedBeforeTheCap writes the request a broker from before record.CheckReasonProjects stored for
// reason: the request object verified as every read verifies it, then Create past its creation
// rules (createChecked), so the record and the rows are exactly those Create writes.
func storedBeforeTheCap(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, reason string, names ...string) Request {
	t.Helper()
	obj, err := record.VerifyRequestObject(signRequest(t, m, key, reason, names...), m.Audience, m.Skew, time.Now())
	if err != nil {
		t.Fatalf("VerifyRequestObject: %v", err)
	}
	enr, err := m.enrollment(t.Context(), enrollmentID)
	if err != nil {
		t.Fatalf("read the enrollment: %v", err)
	}
	req, err := m.createChecked(t.Context(), enr, enrollmentID, "", obj, names, nil)
	if err != nil {
		t.Fatalf("createChecked(%v): %v", names, err)
	}
	return req
}

// pendingRecords is the record ids PendingForApprover lists for login.
func pendingRecords(t *testing.T, m *Machine, login string) []string {
	t.Helper()
	pending, err := m.PendingForApprover(t.Context(), login, PendingStates{})
	if err != nil {
		t.Fatalf("PendingForApprover(%s): %v", login, err)
	}
	var ids []string
	for _, p := range pending {
		ids = append(ids, p.RecordID)
	}
	return ids
}

// approverGrants is the grant ids GrantsForApprover lists for login.
func approverGrants(t *testing.T, m *Machine, login string) []string {
	t.Helper()
	grants, err := m.GrantsForApprover(t.Context(), login)
	if err != nil {
		t.Fatalf("GrantsForApprover(%s): %v", login, err)
	}
	var ids []string
	for _, g := range grants {
		ids = append(ids, g.GrantID)
	}
	return ids
}

// TestANewRequestsReasonNamingMoreThanSixteenProjectsIsRefused is round 4's B9 on
// cybermonklabs/legion#4135. On a broker that holds a project-access client, a request whose
// reason names the issues of more than record.MaxReasonProjects projects is refused when it is
// created, as ErrRequestInvalid naming the bound — the API's 400 REQUEST_INVALID — and writes
// nothing, so such a broker makes no request or grant whose every reader asks Dispatch about more
// projects than that. The session's corrected retry is a request of its own that its approver
// sees, never one joined to a hidden request. At the bound, an agent-tier secret is granted at
// once and its person sees the grant, as with any other reason.
func TestANewRequestsReasonNamingMoreThanSixteenProjectsIsRefused(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)

	refused, err := m.Create(t.Context(), enr, signRequest(t, m, key, reasonNaming(record.MaxReasonProjects+1), "DEEL_API_KEY"), "", nil)
	if !errors.Is(err, record.ErrRequestInvalid) || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", record.MaxReasonProjects)) {
		t.Fatalf("Create(a reason naming %d projects) = %+v, %v; want ErrRequestInvalid naming the bound of %d", record.MaxReasonProjects+1, refused, err, record.MaxReasonProjects)
	}
	var written int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from requests where enrollment_id=$1`, enr).Scan(&written); err != nil || written != 0 {
		t.Fatalf("requests written by the refused Create = %d, %v; want none", written, err)
	}

	retry, err := m.Create(t.Context(), enr, signRequest(t, m, key, "INFRA-12: rotate the deploy key", "DEEL_API_KEY"), "", nil)
	if err != nil || retry.State != "pending" || retry.RecordID == nil || retry.Coalesced {
		t.Fatalf("the corrected retry = %+v, %v; want a pending request of its own", retry, err)
	}
	if listed := pendingRecords(t, m, fixtureOperator); !slices.Contains(listed, *retry.RecordID) {
		t.Errorf("PendingForApprover(%s) = %v; want the retry %s", fixtureOperator, listed, *retry.RecordID)
	}

	atTheCap, err := m.Create(t.Context(), enr, signRequest(t, m, key, reasonNaming(record.MaxReasonProjects), "AUTO_TOKEN"), "", nil)
	if err != nil || atTheCap.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, a reason naming %d projects) = %+v, %v; want granted at once", record.MaxReasonProjects, atTheCap, err)
	}
	if listed := approverGrants(t, m, fixtureOperator); !slices.Contains(listed, *atTheCap.GrantID) {
		t.Errorf("GrantsForApprover(%s) = %v; want the grant %s its reason's %d projects do not hide", fixtureOperator, listed, *atTheCap.GrantID, record.MaxReasonProjects)
	}
}

// TestABrokerWithNoProjectAccessTakesAReasonNamingAnyNumberOfProjects: a broker that holds no
// project-access client reads no reason's projects, so the bound on them is not its rule (R9). A
// request whose reason names the issues of more than record.MaxReasonProjects projects is decided
// as before the bound: an agent-tier secret granted at once, and a human-tier one pending on its
// approver's list and page.
func TestABrokerWithNoProjectAccessTakesAReasonNamingAnyNumberOfProjects(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	over := reasonNaming(record.MaxReasonProjects + 1)
	granted, err := m.Create(t.Context(), enr, signRequest(t, m, key, over, "AUTO_TOKEN"), "", nil)
	if err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, a reason naming %d projects) with no project-access client = %+v, %v; want granted at once, as before the bound", record.MaxReasonProjects+1, granted, err)
	}
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, over, "DEEL_API_KEY"), "", nil)
	if err != nil || pending.State != "pending" || pending.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY, a reason naming %d projects) with no project-access client = %+v, %v; want pending", record.MaxReasonProjects+1, pending, err)
	}
	if listed := pendingRecords(t, m, approver); !slices.Contains(listed, *pending.RecordID) {
		t.Errorf("PendingForApprover(%s) = %v; want the request %s", approver, listed, *pending.RecordID)
	}
	if detail, err := m.ReadRecordFor(t.Context(), *pending.RecordID, approver); err != nil || detail.Reason != over {
		t.Errorf("ReadRecordFor(the request, %s) = %+v, %v; want it read", approver, detail, err)
	}
}

// overTheCapNaming is a reason whose issue keys name record.MaxReasonProjects projects Dispatch
// does not hold and then project, one more than a new request may name.
func overTheCapNaming(project string) string {
	return reasonNaming(record.MaxReasonProjects) + " " + project + "-1"
}

// TestARequestStoredOverTheCapOfNoPrivateProjectStaysVisible: a request stored before the cap,
// with a reason naming more than record.MaxReasonProjects projects none of which is private,
// verifies as it always did, and its person and approvers see it — the pending request in their
// list and on its page, the automatic grant in theirs. Every project its reason names is asked
// (maySee), and none hides it.
func TestARequestStoredOverTheCapOfNoPrivateProjectStaysVisible(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	over := overTheCapNaming(teamProject)
	pending := storedBeforeTheCap(t, m, enr, key, over, "DEEL_API_KEY")
	granted := storedBeforeTheCap(t, m, enr, key, over, "AUTO_TOKEN")
	if pending.RecordID == nil || granted.GrantID == nil {
		t.Fatalf("the stored requests = %+v and %+v; want one pending and one granted", pending, granted)
	}

	detail, err := m.ReadRecordFor(t.Context(), *pending.RecordID, fixtureOperator)
	if err != nil || detail.Reason != over {
		t.Fatalf("ReadRecordFor(the stored request, %s) = %+v, %v; want it read and verified", fixtureOperator, detail, err)
	}
	if err := m.HideUnseen(t.Context(), *pending.RecordID, fixtureOperator); err != nil {
		t.Errorf("HideUnseen(the stored request, %s) = %v; want its approver to see it", fixtureOperator, err)
	}
	if listed := pendingRecords(t, m, fixtureOperator); !slices.Contains(listed, *pending.RecordID) {
		t.Errorf("PendingForApprover(%s) = %v; want the stored request %s", fixtureOperator, listed, *pending.RecordID)
	}
	if listed := approverGrants(t, m, fixtureOperator); !slices.Contains(listed, *granted.GrantID) {
		t.Errorf("GrantsForApprover(%s) = %v; want the stored grant %s", fixtureOperator, listed, *granted.GrantID)
	}
}

// TestARequestStoredOverTheCapNamingAPrivateProjectIsHidden is the post-merge review's first
// finding on cybermonklabs/legion#4135: a request stored before the cap whose reason names more
// than record.MaxReasonProjects projects, the last of them PRIV, is PRIV's request, though nothing
// else of it touches PRIV. A person PRIV does not list finds it in no list, and reading it answers
// them as an id no record has; a person PRIV lists sees it.
func TestARequestStoredOverTheCapNamingAPrivateProjectIsHidden(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	// A pod that names no person, so every signed-in person's list reads its request
	// (everyonesPod).
	pod, podKey := everyonesPod(t, m, "pod-c-stored-private", "")
	stored := storedBeforeTheCap(t, m, pod, podKey, overTheCapNaming(privProject), "SHARED_KEY")
	if stored.RecordID == nil {
		t.Fatalf("the stored request = %+v; want it pending", stored)
	}

	if listed := pendingRecords(t, m, outsider); slices.Contains(listed, *stored.RecordID) {
		t.Errorf("PendingForApprover(%s) = %v; want no request whose reason names PRIV for a person PRIV does not list", outsider, listed)
	}
	if err := m.HideUnseen(t.Context(), *stored.RecordID, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("HideUnseen(the stored request, %s) = %v; want pgx.ErrNoRows", outsider, err)
	}
	if detail, err := m.ReadRecordFor(t.Context(), *stored.RecordID, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ReadRecordFor(the stored request, %s) = reason %q, %v; want pgx.ErrNoRows and nothing of it", outsider, detail.Reason, err)
	}
	if listed := pendingRecords(t, m, insider); !slices.Contains(listed, *stored.RecordID) {
		t.Errorf("PendingForApprover(%s) = %v; want the stored request %s, shown to a person PRIV lists", insider, listed, *stored.RecordID)
	}
	if _, err := m.ReadRecordFor(t.Context(), *stored.RecordID, insider); err != nil {
		t.Errorf("ReadRecordFor(the stored request, %s) = %v; want it read", insider, err)
	}
}

// TestARequestStoredOverTheCapOfASecretSinceTaggedPrivateStaysHidden: a secret a stored request
// names can have been tagged with a private project since it was stored, and the record's tagged
// secrets come of the live policy, so the request is the private project's from then on: an
// outsider no longer sees it, and a person the project lists still does, whatever its reason
// names.
func TestARequestStoredOverTheCapOfASecretSinceTaggedPrivateStaysHidden(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	// A pod that names no person, so every signed-in person's list reads its request
	// (everyonesPod).
	pod, podKey := everyonesPod(t, m, "pod-c-stored", "")
	stored := storedBeforeTheCap(t, m, pod, podKey, reasonNaming(record.MaxReasonProjects+1), "SHARED_KEY")
	if stored.RecordID == nil {
		t.Fatalf("the stored request = %+v; want it pending", stored)
	}
	if listed := pendingRecords(t, m, outsider); !slices.Contains(listed, *stored.RecordID) {
		t.Fatalf("PendingForApprover(%s) before the tag = %v; want the shared request %s, which anyone may approve", outsider, listed, *stored.RecordID)
	}
	retag(t, m, taggedSecret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, privProject, "shared-v1"))

	if listed := pendingRecords(t, m, outsider); slices.Contains(listed, *stored.RecordID) {
		t.Errorf("PendingForApprover(%s) once SHARED_KEY is PRIV's = %v; want the request hidden from a person PRIV does not list", outsider, listed)
	}
	if listed := pendingRecords(t, m, insider); !slices.Contains(listed, *stored.RecordID) {
		t.Errorf("PendingForApprover(%s) once SHARED_KEY is PRIV's = %v; want the request shown to a person PRIV lists", insider, listed)
	}
}

// TestTheDecisionTheRevokeAndTheJudgeReadEveryProjectOfAStoredReason: the readers that act on a
// row read a stored reason past the cap whole, as the lists and the page do. A pending request of a
// pod that names no person, whose reason names 16 projects Dispatch does not hold and then PRIV,
// is decided by no outsider: their approval answers as an id no record has. The fixture session's
// grant with that reason is revoked by no outsider, answered the same way. The model judge leaves
// such a request to a person. The same rows with the reason's last project TEAM, which is public,
// are refused the outsider only as someone who is not the grant's approver, and the judge keeps
// them: the reason's projects, not its length, decide.
func TestTheDecisionTheRevokeAndTheJudgeReadEveryProjectOfAStoredReason(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	pod, podKey := everyonesPod(t, m, "pod-over-cap-readers", "")
	private, public := overTheCapNaming(privProject), overTheCapNaming(teamProject)

	pending := storedBeforeTheCap(t, m, pod, podKey, private, "SHARED_KEY")
	if pending.RecordID == nil {
		t.Fatalf("the stored request = %+v; want it pending", pending)
	}
	if _, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, outsider); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ApplyDecision(a request whose stored reason names PRIV, %s) = %v; want pgx.ErrNoRows", outsider, err)
	}
	if state := requestState(t, m, pending.ID); state != "pending" {
		t.Errorf("the request after %s's approval is %s; want still pending", outsider, state)
	}

	granted := storedBeforeTheCap(t, m, enr, key, private, "AUTO_TOKEN")
	if granted.GrantID == nil {
		t.Fatalf("the stored grant = %+v; want it granted", granted)
	}
	if err := m.RevokeByApprover(t.Context(), *granted.GrantID, outsider, record.HumanActor(outsider)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("RevokeByApprover(a grant whose stored reason names PRIV, %s) = %v; want pgx.ErrNoRows", outsider, err)
	}
	second, secondKey := newEnrollment(t, m.Store, "session", "box-over-cap-"+t.Name(), new(fixtureOperator), nil)
	publicGrant := storedBeforeTheCap(t, m, second, secondKey, public, "AUTO_TOKEN")
	if publicGrant.GrantID == nil {
		t.Fatalf("the stored grant = %+v; want it granted", publicGrant)
	}
	if err := m.RevokeByApprover(t.Context(), *publicGrant.GrantID, outsider, record.HumanActor(outsider)); err == nil || errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("RevokeByApprover(a grant whose stored reason names TEAM, %s) = %v; want a refusal other than pgx.ErrNoRows", outsider, err)
	}

	if referred, err := m.JudgeProject(t.Context(), enr, "AUTO_TOKEN", private); err != nil || !referred {
		t.Errorf("JudgeProject(a reason naming 16 projects and then PRIV) = %v, %v; want true: the judge leaves it to a person", referred, err)
	}
	if referred, err := m.JudgeProject(t.Context(), enr, "AUTO_TOKEN", public); err != nil || referred {
		t.Errorf("JudgeProject(a reason naming 16 projects and then TEAM) = %v, %v; want false", referred, err)
	}
}

// TestASessionReadsARequestStoredOverTheCapByEveryProjectItsReasonNames: the session rule a
// session's own routes read (hideFromSession) joins every project a row's reason names, as the
// person rule does. A request and a grant stored before the cap, with a reason naming more than
// record.MaxReasonProjects projects, are read by another session of the same person, which does
// not hold them. Where none of those projects is private the rule shows both; where the last is
// PRIV it hides both from a session that does not name PRIV and shows both to one that does.
func TestASessionReadsARequestStoredOverTheCapByEveryProjectItsReasonNames(t *testing.T) {
	for _, c := range []struct {
		name    string
		project string
		named   []string
		hidden  bool
	}{
		{"a reason naming no private project", teamProject, nil, false},
		{"a reason naming PRIV, read by a session naming none", privProject, nil, true},
		{"a reason naming PRIV, read by a session naming PRIV", privProject, []string{privProject}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			over := overTheCapNaming(c.project)
			pending := storedBeforeTheCap(t, m, enr, key, over, "DEEL_API_KEY")
			granted := storedBeforeTheCap(t, m, enr, key, over, "AUTO_TOKEN")
			if pending.RecordID == nil || granted.GrantID == nil {
				t.Fatalf("the stored requests = %+v and %+v; want one pending and one granted", pending, granted)
			}
			other, _ := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			nameProjects(t, m, other, c.named...)

			var want error
			if c.hidden {
				want = pgx.ErrNoRows
			}
			if err := m.HideRequestFromSession(t.Context(), pending.ID, other); !errors.Is(err, want) {
				t.Errorf("HideRequestFromSession(the stored request, another session of %s naming %v) = %v; want %v", fixtureOperator, c.named, err, want)
			}
			if err := m.hideGrantFromSession(t.Context(), *granted.GrantID, other); !errors.Is(err, want) {
				t.Errorf("hideGrantFromSession(the stored grant, another session of %s naming %v) = %v; want %v", fixtureOperator, c.named, err, want)
			}
		})
	}
}
