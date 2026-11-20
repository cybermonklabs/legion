package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/projectaccess/projectaccesstest"
)

// TestARestrictedMembersSessionIsReleasedNoSecret is the request machine's half of the rule that
// a restricted member's agents get no secret. The session here acts for a person Dispatch calls a
// restricted member, and it was enrolled, and given two shared agent-tier grants, before the
// broker asked Dispatch anything: the session a broker with no project-access client made, or one
// made before this rule. Once the machine asks, that session's ask for a name it holds a grant of
// is denied restricted-member rather than handed the grant back, a human-tier ask is denied
// rather than put to anyone, and the release of its other grant is refused and the grant ended.
// The control is a full member's session on the same machine, which is given and released what it
// was. A session whose person Dispatch cannot be asked about is refused both its ask and its
// release (*projectaccess.MemberKindUncheckable), with no request written and nothing released,
// and is given and released both once Dispatch answers again.
func TestARestrictedMembersSessionIsReleasedNoSecret(t *testing.T) {
	const (
		outsider = "outsider@outside.example"
		unasked  = "uma@example.com"
	)
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	retag(t, m,
		policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"),
		policytest.Secret("SHARED_OTHER", policy.OwnerShared, policy.TierAgent, "shared-other-v1"))
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		outsider:        projectaccess.MemberRestricted,
		fixtureOperator: projectaccess.MemberFull,
		unasked:         projectaccess.MemberFull,
	})
	outsiderSession, outsiderKey := newEnrollment(t, m.Store, "session", "outsider-laptop-"+t.Name(), new(outsider), nil)
	granted := func(t *testing.T, session string, key *ecdsa.PrivateKey, name string) Request {
		t.Helper()
		req, err := m.Create(ctx, session, signRequest(t, m, key, "needs "+name, name), "", nil)
		if err != nil || req.State != "granted" || req.GrantID == nil {
			t.Fatalf("Create(%s) = %+v, %v; want it granted at once", name, req, err)
		}
		return req
	}
	heldToken := granted(t, outsiderSession, outsiderKey, "SHARED_TOKEN")
	heldOther := granted(t, outsiderSession, outsiderKey, "SHARED_OTHER")
	// A full member's session with a grant made before the machine asked, so no answer about its
	// person is kept when Dispatch goes down.
	unaskedSession, unaskedKey := newEnrollment(t, m.Store, "session", "uma-box-"+t.Name(), new(unasked), nil)
	heldUnasked := granted(t, unaskedSession, unaskedKey, "SHARED_TOKEN")
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)

	t.Run("an ask for a name it holds a grant of is denied and the grant ended", func(t *testing.T) {
		req, err := m.Create(ctx, outsiderSession, signRequest(t, m, outsiderKey, "needs it again", "SHARED_TOKEN"), "", nil)
		if err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil ||
			!strings.Contains(*req.Detail, "SHARED_TOKEN: "+policy.ReasonRestrictedMember) {
			t.Fatalf("Create(SHARED_TOKEN) = %+v, %v; want it denied naming %s", req, err, policy.ReasonRestrictedMember)
		}
		requireGrantEnded(t, m, *heldToken.GrantID)
	})

	t.Run("a human-tier ask is denied, not put to anyone", func(t *testing.T) {
		req, err := m.Create(ctx, outsiderSession, signRequest(t, m, outsiderKey, "needs it", "SHARED_KEY"), "", nil)
		if err != nil || req.State != "denied" || req.RecordID != nil {
			t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it denied with no approval record", req, err)
		}
	})

	t.Run("the release of a grant it holds is refused and the grant ended", func(t *testing.T) {
		if release, err := m.Values(ctx, *heldOther.GrantID, outsiderSession, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || len(release.Values) != 0 {
			t.Fatalf("Values(SHARED_OTHER) = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
		}
		requireGrantEnded(t, m, *heldOther.GrantID)
	})

	t.Run("a full member's session is given and released what it was", func(t *testing.T) {
		session, key := newEnrollment(t, m.Store, "session", "ines-box-"+t.Name(), new(fixtureOperator), nil)
		for name, value := range map[string]string{"SHARED_TOKEN": "shared-token-v1", "AUTO_TOKEN": "auto-v1"} {
			req := granted(t, session, key, name)
			if release, err := m.Values(ctx, *req.GrantID, session, nil, ReleaseCall{}); err != nil || release.Values[name] != value {
				t.Fatalf("Values(%s) = %v, %v; want %q", name, release.Values, err, value)
			}
		}
	})

	t.Run("a session whose person Dispatch cannot be asked about is refused its ask and its release", func(t *testing.T) {
		requests := func() int {
			var n int
			if err := m.Store.Pool.QueryRow(ctx, `select count(*) from requests where enrollment_id=$1`, unaskedSession).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		uncheckable := func(what string, err error) {
			t.Helper()
			var refused *projectaccess.MemberKindUncheckable
			if !errors.As(err, &refused) || refused.Person != unasked {
				t.Fatalf("%s while Dispatch is down = %v; want MemberKindUncheckable naming %s", what, err, unasked)
			}
		}
		before := requests()
		dispatch.SetDown(true)
		t.Cleanup(func() { dispatch.SetDown(false) })
		release, err := m.Values(ctx, *heldUnasked.GrantID, unaskedSession, nil, ReleaseCall{})
		uncheckable("Values(SHARED_TOKEN)", err)
		if len(release.Values) != 0 {
			t.Fatalf("Values(SHARED_TOKEN) while Dispatch is down released %v; want nothing", release.Values)
		}
		_, err = m.Create(ctx, unaskedSession, signRequest(t, m, unaskedKey, "needs it while Dispatch is down", "SHARED_OTHER"), "", nil)
		uncheckable("Create(SHARED_OTHER)", err)
		if n := requests(); n != before {
			t.Fatalf("the refused ask left %d requests for the session, want %d", n, before)
		}

		// The control: once Dispatch answers, the same grant is released and the same ask granted.
		dispatch.SetDown(false)
		if release, err := m.Values(ctx, *heldUnasked.GrantID, unaskedSession, nil, ReleaseCall{}); err != nil || release.Values["SHARED_TOKEN"] != "shared-token-v1" {
			t.Fatalf("Values(SHARED_TOKEN) once Dispatch answers = %v, %v; want it released", release.Values, err)
		}
		granted(t, unaskedSession, unaskedKey, "SHARED_OTHER")
	})
}

// requireGrantEnded fails t unless grant is revoked.
func requireGrantEnded(t *testing.T, m *Machine, grant string) {
	t.Helper()
	var revoked bool
	if err := m.Store.Pool.QueryRow(context.Background(), `select revoked_at is not null from grants where id=$1`, grant).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("grant %s revoked = %t, %v; want it ended", grant, revoked, err)
	}
}
