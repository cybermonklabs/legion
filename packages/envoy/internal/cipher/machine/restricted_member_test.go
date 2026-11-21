package machine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARestrictedMemberDeniesNoServicesLogin: a service's login a broker from before token logins
// opened, still pending, is denied by anyone signed in (TestAPendingServiceLoginIsNeverApprovedButMayBeDenied),
// but not by a person Dispatch calls a restricted member, who is no approver of a secret: their
// deny is refused record.ErrNotApprover and the login stays pending, while a full member's deny,
// the control, denies it. While Dispatch cannot say what kind of member the denying person is, the
// deny is refused *projectaccess.MemberKindUncheckable naming them, and the login stays pending; a
// read Dispatch does not answer at all ends before Dispatch stops waiting for the route it relays,
// and the deny is refused the same way. A restricted member's own machine login is theirs alone to
// decide: they deny it, asking Dispatch nothing.
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

	t.Run("while Dispatch does not answer, the deny is refused within the wait of the route Dispatch relays", func(t *testing.T) {
		svc := newFixture(t)
		recordID, code, _ := insertServiceRecord(t, svc, record.AnyoneApprover)
		stalled := &stalledKind{}
		svc.Enroll.Projects = stalled
		_, _, err := svc.ApplyDecision(ctx, recordID, false, member, code)
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != member {
			t.Fatalf("ApplyDecision(deny) by %s while Dispatch does not answer = %v; want MemberKindUncheckable naming them", member, err)
		}
		if n := decisions(t, svc, recordID); n != 0 {
			t.Fatalf("the refused deny wrote %d decisions; want none", n)
		}
		stalled.requireBounded(t)
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

// dispatchRelayWait is how long Dispatch's server waits for the broker's answer to a route it
// relays (internal/dispatch/cipher's defaultTimeout): a read of Dispatch that such a route makes
// must end before it, or the route answers nobody.
const dispatchRelayWait = 5 * time.Second

// stalledKind is a projectaccess.Access whose member-kind reads Dispatch never answers: each waits
// until its caller's context ends, and records how long that context had left. A read whose
// context has no deadline would wait as long as its caller, so it is answered at once with an
// error and counted as unbounded, which the row reads as the fault. It answers no project.
type stalledKind struct {
	mu        sync.Mutex
	unbounded int
	bounds    []time.Duration
}

func (s *stalledKind) Access(context.Context, string, string) (projectaccess.Answer, error) {
	return projectaccess.Answer{}, errors.New("stalledKind answers no project")
}

func (s *stalledKind) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errors.New("stalledKind answers no project")
}

func (s *stalledKind) MemberKind(ctx context.Context, _ string) (string, error) {
	deadline, bounded := ctx.Deadline()
	s.mu.Lock()
	if !bounded {
		s.unbounded++
		s.mu.Unlock()
		return "", errors.New("stalledKind: a member-kind read with no deadline")
	}
	s.bounds = append(s.bounds, time.Until(deadline))
	s.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

// requireBounded fails t unless s was asked at least one member-kind read, and every read had a
// deadline short of dispatchRelayWait.
func (s *stalledKind) requireBounded(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unbounded != 0 || len(s.bounds) == 0 {
		t.Fatalf("the deny asked %d member-kind reads with no deadline and %d with one; want at least one read, each bounded", s.unbounded, len(s.bounds))
	}
	for _, bound := range s.bounds {
		if bound >= dispatchRelayWait {
			t.Fatalf("the deny read a member kind with %s to run; want less than the %s Dispatch waits for the route", bound, dispatchRelayWait)
		}
	}
}
