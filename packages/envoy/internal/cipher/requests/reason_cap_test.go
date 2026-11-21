package requests

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
// cybermonklabs/legion#4135. A request whose reason names the issues of more than
// record.MaxReasonProjects projects is refused when it is created, as ErrRequestInvalid naming the
// bound — the API's 400 REQUEST_INVALID — and writes nothing, so no request or grant exists whose
// reason no rule could read cheaply. The session's corrected retry is a request of its own that
// its approver sees, never one joined to a hidden request. At the bound, an agent-tier secret is
// granted at once and its person sees the grant, as with any other reason.
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

// TestARequestStoredOverTheCapStaysVisibleAndAsksDispatchNothing: a request stored before the
// cap with a reason naming more than record.MaxReasonProjects projects verifies as it always did,
// its person and approvers see it — the pending request in their list and on its page, the
// automatic grant in theirs — and reading it asks Dispatch nothing: past the bound the reason's
// keys are not read, so they count as naming no project, which is how every reader saw the
// record when it was written.
func TestARequestStoredOverTheCapStaysVisibleAndAsksDispatchNothing(t *testing.T) {
	m, enr, key, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	over := reasonNaming(record.MaxReasonProjects + 1)
	pending := storedBeforeTheCap(t, m, enr, key, over, "DEEL_API_KEY")
	granted := storedBeforeTheCap(t, m, enr, key, over, "AUTO_TOKEN")
	if pending.RecordID == nil || granted.GrantID == nil {
		t.Fatalf("the stored requests = %+v and %+v; want one pending and one granted", pending, granted)
	}

	asked := dispatch.Calls()
	detail, err := m.ReadRecord(t.Context(), *pending.RecordID)
	if err != nil || detail.Reason != over {
		t.Fatalf("ReadRecord(the stored request) = %+v, %v; want it read and verified", detail, err)
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
	if calls := dispatch.Calls() - asked; calls != 0 {
		t.Errorf("reading the two stored requests asked Dispatch %d times; want none: past the bound the reason is not read", calls)
	}
}

// TestARequestStoredOverTheCapOfASecretSinceTaggedPrivateStaysHidden: past the bound only the
// reason's keys go unread. A secret a stored request names can have been tagged with a private
// project since it was stored, and the record's tagged secrets come of the live policy, so the
// request is the private project's from then on: an outsider no longer sees it, and a person the
// project lists still does, whatever its reason names.
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

// TestASessionReadsARequestStoredOverTheCapAskingDispatchNothing: the session rule a session's
// own routes read (hideFromSession) joins a row's reason projects through the same cap as the
// person rule (reasonProjects). A request and a grant stored before the cap, with a reason naming
// more than record.MaxReasonProjects projects, are read by another session of the same person,
// which does not hold them: the rule shows both, and asks Dispatch nothing, since past the bound
// the reason's keys count as naming no project, as every reader saw the row when it was written.
func TestASessionReadsARequestStoredOverTheCapAskingDispatchNothing(t *testing.T) {
	m, enr, key, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	over := reasonNaming(record.MaxReasonProjects + 1)
	pending := storedBeforeTheCap(t, m, enr, key, over, "DEEL_API_KEY")
	granted := storedBeforeTheCap(t, m, enr, key, over, "AUTO_TOKEN")
	if pending.RecordID == nil || granted.GrantID == nil {
		t.Fatalf("the stored requests = %+v and %+v; want one pending and one granted", pending, granted)
	}
	other, _ := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)

	asked := dispatch.Calls()
	if err := m.HideRequestFromSession(t.Context(), pending.ID, other); err != nil {
		t.Errorf("HideRequestFromSession(the stored request, another session of %s) = %v; want it shown", fixtureOperator, err)
	}
	if err := m.hideGrantFromSession(t.Context(), *granted.GrantID, other); err != nil {
		t.Errorf("hideGrantFromSession(the stored grant, another session of %s) = %v; want it shown", fixtureOperator, err)
	}
	if calls := dispatch.Calls() - asked; calls != 0 {
		t.Errorf("the session rule's two reads of the stored rows asked Dispatch %d times; want none: past the bound the reason is not read", calls)
	}
}
