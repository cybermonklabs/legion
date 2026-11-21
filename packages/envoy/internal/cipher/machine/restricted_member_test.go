package machine

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARestrictedMemberDeniesNoServicesLogin: a service's login a broker from before token logins
// opened, still pending, is denied by anyone signed in (TestAPendingServiceLoginIsNeverApprovedButMayBeDenied),
// but not by a person Dispatch calls a restricted member, who is no approver of a secret: their
// deny is refused record.ErrNotApprover and the login stays pending, while a full member's deny,
// the control, denies it. While Dispatch cannot say what kind of member the denying person is, the
// deny is refused *projectaccess.MemberKindUncheckable naming them, and the login stays pending. A
// restricted member's own machine login is theirs alone to decide: they deny it, asking Dispatch
// nothing.
func TestARestrictedMemberDeniesNoServicesLogin(t *testing.T) {
	const (
		guest  = "guest@outside.example"
		member = "bob@example.com"
	)
	kinds := map[string]string{guest: projectaccess.MemberRestricted, member: projectaccess.MemberFull}
	ctx := context.Background()
	decisions := func(t *testing.T, svc *Service, recordID string) int {
		t.Helper()
		var n int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from credential_request_events where record_id=$1`, recordID).Scan(&n); err != nil {
			t.Fatalf("count the decisions of %s: %v", recordID, err)
		}
		return n
	}
	asking := func(svc *Service, dispatch *projectaccesstest.Server) {
		svc.Enroll.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	}

	t.Run("a restricted member's deny is refused", func(t *testing.T) {
		svc := newFixture(t)
		recordID, code, _ := insertServiceRecord(t, svc, record.AnyoneApprover)
		asking(svc, projectaccesstest.NewServer(t, nil, kinds))
		if state, _, err := svc.ApplyDecision(ctx, recordID, false, guest, code); !errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("ApplyDecision(deny) by %s = %q, %v; want record.ErrNotApprover", guest, state, err)
		}
		if n := decisions(t, svc, recordID); n != 0 {
			t.Fatalf("the refused deny wrote %d decisions; want none, the login still pending", n)
		}
	})

	t.Run("a full member's deny, the control, denies it", func(t *testing.T) {
		svc := newFixture(t)
		recordID, code, _ := insertServiceRecord(t, svc, record.AnyoneApprover)
		asking(svc, projectaccesstest.NewServer(t, nil, kinds))
		if state, credentialID, err := svc.ApplyDecision(ctx, recordID, false, member, code); err != nil || state != "denied" || credentialID != "" {
			t.Fatalf("ApplyDecision(deny) by %s = %q, %q, %v; want denied", member, state, credentialID, err)
		}
	})

	t.Run("while Dispatch cannot say, the deny is refused", func(t *testing.T) {
		svc := newFixture(t)
		recordID, code, _ := insertServiceRecord(t, svc, record.AnyoneApprover)
		down := projectaccesstest.NewServer(t, nil, kinds)
		down.SetDown(true)
		asking(svc, down)
		_, _, err := svc.ApplyDecision(ctx, recordID, false, member, code)
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != member {
			t.Fatalf("ApplyDecision(deny) by %s while Dispatch is down = %v; want MemberKindUncheckable naming them", member, err)
		}
		if n := decisions(t, svc, recordID); n != 0 {
			t.Fatalf("the refused deny wrote %d decisions; want none", n)
		}
		if down.Calls() == 0 {
			t.Fatal("the down Dispatch was never asked: the row is blind")
		}
	})

	t.Run("a restricted member's own machine login is theirs to deny, asking nothing", func(t *testing.T) {
		svc := newFixture(t)
		code, err := openLogin(t, svc, signMachineLogin(t, guest, "guest-host", ""))
		if err != nil {
			t.Fatalf("LoginDevice for %s: %v", guest, err)
		}
		view, err := svc.LookupByCode(ctx, code)
		if err != nil {
			t.Fatalf("LookupByCode: %v", err)
		}
		down := projectaccesstest.NewServer(t, nil, kinds)
		down.SetDown(true)
		asking(svc, down)
		if state, _, err := svc.ApplyDecision(ctx, view.RecordID, false, guest, code); err != nil || state != "denied" {
			t.Fatalf("ApplyDecision(deny) of their own machine login by %s = %q, %v; want denied", guest, state, err)
		}
		if n := down.Calls(); n != 0 {
			t.Fatalf("the deny of their own machine login asked Dispatch %d times; want none", n)
		}
	})
}
