package requests

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// withheldCount is how many names are withheld from enrollment id.
func withheldCount(t *testing.T, m *Machine, id string) int {
	t.Helper()
	var n int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from withheld_secrets where enrollment_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnOperatorsRevokeWithholdsFromEveryLiveBoxOfTheSession: the session ends its own AUTO_TOKEN
// grant and moves to a second box of the same session name under the same principal. The first
// box held nothing live when the session moved, so no adoption takes its grants: the ended grant
// stays on it. The operator then revokes that grant from a Live grants page loaded while it was
// live, which withholds AUTO_TOKEN (RevokeByApprover's already-ended case). The name must be
// withheld where the session runs, so the session's next AUTO_TOKEN request from the second box
// waits for a person. Rows: the first box ended for good, and the first box still live. The
// control is a second box of another session of the same person: a withhold binds a session, so
// that box gets nothing, and its request is granted at once.
func TestAnOperatorsRevokeWithholdsFromEveryLiveBoxOfTheSession(t *testing.T) {
	for _, c := range []struct {
		name         string
		secondName   string
		endFirst     bool
		wantWithheld int
	}{
		{name: "the box the session left ended for good", secondName: "sess-moved", endFirst: true, wantWithheld: 1},
		{name: "the box the session left still live", secondName: "sess-moved", wantWithheld: 1},
		{name: "a box of another session of the same person (the control)", secondName: "sess-other", wantWithheld: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			principal := exchange.PersonPrincipal(fixtureOperator)
			nameTheSession(t, m, enr, "sess-moved", principal)
			_, grantID := grantOf(t, m, enr, key, "deploy the worker", "AUTO_TOKEN")
			if err := m.RevokeGrant(t.Context(), grantID, enr); err != nil {
				t.Fatalf("the session's own revoke: %v", err)
			}
			if c.endFirst {
				if _, err := m.Store.Pool.Exec(t.Context(), `update enrollments set revoked_at=now() where id=$1`, enr); err != nil {
					t.Fatal(err)
				}
			}
			second, secondKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			nameTheSession(t, m, second, c.secondName, principal)

			if err := m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
				t.Fatalf("the operator's revoke of the ended grant = %v; want it to withhold", err)
			}
			if got := withheldCount(t, m, enr); got != 1 {
				t.Errorf("names withheld on the box that holds the grant = %d; want 1", got)
			}
			if got := withheldCount(t, m, second); got != c.wantWithheld {
				t.Errorf("names withheld on the second box = %d; want %d", got, c.wantWithheld)
			}
			again, err := m.Create(t.Context(), second, signRequest(t, m, secondKey, "deploy again", "AUTO_TOKEN"), "", nil)
			if err != nil {
				t.Fatalf("the second box's AUTO_TOKEN request: %v", err)
			}
			if granted := again.GrantID != nil; granted != (c.wantWithheld == 0) {
				t.Errorf("the second box's AUTO_TOKEN request granted at once = %v; want %v", granted, c.wantWithheld == 0)
			}
		})
	}
}

// TestARevokeWaitsForAnAdoptionOfTheSessionInFlight: box A ended its own AUTO_TOKEN grant and
// holds nothing live. Box N of the same session name and principal is being enrolled: its
// transaction holds the session's lock (store.LockSession, as enroll.adopt takes it before it
// reads its predecessors), has written N's session, and found nothing on A to take, since A holds
// nothing live and no withheld name yet. It has not committed. The operator then revokes the ended
// grant, which withholds AUTO_TOKEN. The revoke must wait for N, so its lock statement finds N
// among the session's live boxes once N commits, and withholds the name there too: N's next
// AUTO_TOKEN request waits for a person. The row fails when the revoke takes no session lock:
// it commits without N, and N, which read its predecessors before the name was withheld, is
// granted at once.
func TestARevokeWaitsForAnAdoptionOfTheSessionInFlight(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	principal := exchange.PersonPrincipal(fixtureOperator)
	nameTheSession(t, m, enr, "sess-in-flight", principal)
	_, grantID := grantOf(t, m, enr, key, "deploy the worker", "AUTO_TOKEN")
	if err := m.RevokeGrant(t.Context(), grantID, enr); err != nil {
		t.Fatalf("the session's own revoke: %v", err)
	}
	n, nKey := newEnrollment(t, m.Store, "session", "box-n-"+t.Name(), new(fixtureOperator), nil)
	adopting, err := m.Store.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adopting.Rollback(context.Background()) })
	if err := store.LockSession(t.Context(), adopting, "sess-in-flight", principal); err != nil {
		t.Fatal(err)
	}
	if _, err := adopting.Exec(t.Context(), `update enrollments set session_id=$2, carry_principal=$3 where id=$1`, n, "sess-in-flight", principal); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator))
	}()
	waitBlockedBy(t, m, adopting, done)
	if err := adopting.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the operator's revoke = %v; want it to withhold", err)
	}
	if got := withheldCount(t, m, n); got != 1 {
		t.Errorf("names withheld on box N, whose adoption the revoke waited for = %d; want 1", got)
	}
	again, err := m.Create(t.Context(), n, signRequest(t, m, nKey, "deploy again", "AUTO_TOKEN"), "", nil)
	if err != nil || again.GrantID != nil {
		t.Errorf("box N's AUTO_TOKEN request = %+v, %v; want it waiting for a person", again, err)
	}
}

// TestAnUnmappedPersonsPodsAutomaticGrantNamesNoOperatorOnLiveGrants: dave's pod gets SLACK_TOKEN
// without asking while BROKER_POD_OPERATORS maps it to dave, then the setting drops the account.
// The pod acts for no person, so the revoke counts nobody as the grant's operator (operatorOf),
// and the list says the same: dave's Live grants still holds the grant, which it selects by the
// enrollment's record, but names no operator and no approver on it, so the page reads "Ends with
// its machine login" (GrantsSection.tsx's unrevocable) rather than offering a Revoke the broker
// refuses. The control, in the same row: once the setting maps the account to dave again, the
// row names him as the operator. That the revoke answers the same is
// TestAnUnmappedPersonsPodsFormerPersonEndsAGrantTheySeeOnlyAsAnyoneMay's, and
// TestAnUnmappedPersonsPodNamesNoOperatorOnItsGrant's control.
func TestAnUnmappedPersonsPodsAutomaticGrantNamesNoOperatorOnLiveGrants(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	dave := personPod{person: "dave@example.com", service: "person-dave", account: "system:serviceaccount:people:dave"}
	set, err := exchange.NewSet([]exchange.AccountEntry{{Account: dave.account, Project: teamProject, Person: insider}}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	m.Exchange = set
	m.PodOperators = map[string]string{dave.account: dave.person}
	pod, key := dave.enroll(t, m.Store, "pod-dave-automatic")
	_, grantID := grantOf(t, m, pod, key, "post to the team channel", "SLACK_TOKEN")
	delete(m.PodOperators, dave.account)

	row := func() (ApproverGrant, bool) {
		t.Helper()
		grants, err := m.GrantsForApprover(t.Context(), dave.person)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", dave.person, err)
		}
		i := slices.IndexFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID })
		if i < 0 {
			return ApproverGrant{}, false
		}
		return grants[i], true
	}
	g, ok := row()
	if !ok {
		t.Fatalf("dave's Live grants does not hold grant %s of the pod made for him", grantID)
	}
	if g.Approver != "" || g.Enrollment.Operator != "" {
		t.Errorf("dave's row of grant %s names approver %q and operator %q; want neither, so the page offers no Revoke the broker refuses", grantID, g.Approver, g.Enrollment.Operator)
	}
	m.PodOperators[dave.account] = dave.person
	if g, ok := row(); !ok || g.Enrollment.Operator != dave.person {
		t.Errorf("once the setting maps the pod to dave again, his row of grant %s = %+v, %v; want it naming him as the operator", grantID, g, ok)
	}
}

// TestAnOperatorsRevokeEndsADormantBoxsAutomaticGrantOfTheName: box A of a session holds an
// automatic AUTO_TOKEN grant G1; box D of the same session and principal holds its own automatic
// AUTO_TOKEN grant G2 and then goes dormant, its box ended while holding G2. The operator revokes
// G1. The revoke locks D with A (a dormant box of the session is among them), withholds AUTO_TOKEN
// on it and ends G2. The session then enrols on box N, and the adoption moves D's rows and A's
// withheld name to N (moveSession and the withheld clause): G2 releases nothing on N, and N's next
// AUTO_TOKEN request waits for a person. The row fails when the revoke takes live boxes only: G2
// stays live, moves to N, releases there, and is handed back to N's request.
func TestAnOperatorsRevokeEndsADormantBoxsAutomaticGrantOfTheName(t *testing.T) {
	m, a, keyA, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	principal := exchange.PersonPrincipal(fixtureOperator)
	nameTheSession(t, m, a, "sess-dormant", principal)
	_, g1 := grantOf(t, m, a, keyA, "deploy the worker", "AUTO_TOKEN")
	d, keyD := newEnrollment(t, m.Store, "session", "box-d-"+t.Name(), new(fixtureOperator), nil)
	nameTheSession(t, m, d, "sess-dormant", principal)
	_, g2 := grantOf(t, m, d, keyD, "deploy the worker on d", "AUTO_TOKEN")
	if _, err := m.Store.Pool.Exec(t.Context(), `update enrollments set revoked_at=now(), dormant_since=now() where id=$1`, d); err != nil {
		t.Fatal(err)
	}
	if err := m.RevokeByApprover(t.Context(), g1, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
		t.Fatalf("the operator's revoke of grant %s: %v", g1, err)
	}
	if n := withheldCount(t, m, d); n != 1 {
		t.Errorf("the dormant box withholds %d names after the operator's revoke; want 1, AUTO_TOKEN", n)
	}
	var g2Live bool
	if err := m.Store.Pool.QueryRow(t.Context(), `select revoked_at is null from grants where id=$1`, g2).Scan(&g2Live); err != nil {
		t.Fatal(err)
	}
	if g2Live {
		t.Errorf("the dormant box's automatic AUTO_TOKEN grant %s is live after the operator withheld AUTO_TOKEN from the session; want it ended", g2)
	}

	n, keyN := newEnrollment(t, m.Store, "session", "box-n-"+t.Name(), new(fixtureOperator), nil)
	nameTheSession(t, m, n, "sess-dormant", principal)
	moveSession(t, m, d, n)
	if _, err := m.Store.Pool.Exec(t.Context(), `with moved as (delete from withheld_secrets where enrollment_id=$1 returning name, created_at)
		insert into withheld_secrets (enrollment_id, name, created_at) select $2, name, created_at from moved on conflict do nothing`, a, n); err != nil {
		t.Fatal(err)
	}
	if release, err := m.Values(t.Context(), g2, n, nil, ReleaseCall{}); err == nil || release.Values != nil {
		t.Errorf("G2 on the box the session moved to released %v, %v; want it refused", release.Values, err)
	}
	again, err := m.Create(t.Context(), n, signRequest(t, m, keyN, "deploy again", "AUTO_TOKEN"), "", nil)
	if err != nil || again.GrantID != nil {
		t.Errorf("box N's AUTO_TOKEN request = %+v, %v; want it waiting for a person", again, err)
	}
}
