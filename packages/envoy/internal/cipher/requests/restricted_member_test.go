package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestARestrictedOperatorGetsNoSecret is the request machine's half of the rule that a restricted
// member's agents get no secret. The session here acts for a person Dispatch calls a restricted
// member, and it was enrolled, and given two shared agent-tier grants, before the broker asked
// Dispatch anything: the session a broker with no project-access client made, or one made before
// this rule. Once the machine asks, that session's ask for an agent-tier shared secret is denied
// restricted-member, an ask for a name it holds a grant of is denied rather than handed the grant
// back, a human-tier ask is denied rather than put to anyone, and the release of its other grant
// is refused and the grant ended. The control is a full member's session on the same machine,
// which is given and released what it was. A session whose person Dispatch cannot be asked about
// is refused both its ask and its release (*projectaccess.MemberKindUncheckable), with no request
// written and nothing released, and is given and released both once Dispatch answers again.
//
// The airlock proxy's connection read is held to the same rule on every connection: a grant the
// proxy already read a full member's values for is refused, and ended, at the next connection
// once Dispatch calls the person a restricted member, and refused uncheckable while Dispatch
// cannot be asked.
//
// A Google group that owns a secret, or that a secret's requesters list names, gives a restricted
// member of it nothing either (groupSecrets, over a group reader that lists the person, fresh):
// once the machine asks, an ask for the group's agent-tier secret is denied and the grant of it
// the session got as a member ended, the release of its grant of a secret listed by the group is
// refused and that grant ended, a fresh ask for that secret is denied, a human-tier secret listed
// by the group is not put to its owner, and another member's approval of the group's human-tier
// secret the session left pending is the policy's denial. The control is a full member of the
// same group, which reuses, is released, is put to the owner and is granted what it was.
//
// The same holds of an Anthropic federation secret the group owns and lists, whose release the
// broker mints: the release of the grant the session got as a member before the machine asked
// mints nothing and ends the grant, another member's approval of the one it left pending is the
// policy's denial, and a fresh ask is denied. The control is a full member of the same group,
// minted the token once, granted and given its grant back.
func TestARestrictedOperatorGetsNoSecret(t *testing.T) {
	t.Parallel()
	const (
		outsider = "outsider@outside.example"
		unasked  = "uma@example.com"
	)
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	retag(t, m,
		policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"),
		policytest.Secret("SHARED_OTHER", policy.OwnerShared, policy.TierAgent, "shared-other-v1"),
		policytest.Secret("SHARED_FRESH", policy.OwnerShared, policy.TierAgent, "shared-fresh-v1"))
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		outsider:        projectaccess.MemberRestricted,
		fixtureOperator: projectaccess.MemberFull,
		unasked:         projectaccess.MemberFull,
	})
	outsiderSession, outsiderKey := newEnrollment(t, m.Store, "session", "outsider-laptop-"+t.Name(), new(outsider), nil)
	granted := func(t *testing.T, session string, key *ecdsa.PrivateKey, name string) Request {
		t.Helper()
		req, err := m.Create(ctx, session, signRequest(t, m, key, "needs "+name, name), "")
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

	t.Run("an agent-tier shared secret asked for under a restricted operator is denied", func(t *testing.T) {
		req, err := m.Create(ctx, outsiderSession, signRequest(t, m, outsiderKey, "needs it", "SHARED_FRESH"), "")
		if err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil ||
			!strings.Contains(*req.Detail, "SHARED_FRESH: "+policy.ReasonRestrictedMember) {
			t.Fatalf("Create(SHARED_FRESH) = %+v, %v; want it denied naming %s", req, err, policy.ReasonRestrictedMember)
		}
	})

	t.Run("an ask for a name it holds a grant of is denied and the grant ended", func(t *testing.T) {
		req, err := m.Create(ctx, outsiderSession, signRequest(t, m, outsiderKey, "needs it again", "SHARED_TOKEN"), "")
		if err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil ||
			!strings.Contains(*req.Detail, "SHARED_TOKEN: "+policy.ReasonRestrictedMember) {
			t.Fatalf("Create(SHARED_TOKEN) = %+v, %v; want it denied naming %s", req, err, policy.ReasonRestrictedMember)
		}
		requireGrantEnded(t, m, *heldToken.GrantID)
	})

	t.Run("a human-tier ask is denied, not put to anyone", func(t *testing.T) {
		req, err := m.Create(ctx, outsiderSession, signRequest(t, m, outsiderKey, "needs it", "SHARED_KEY"), "")
		if err != nil || req.State != "denied" || req.RecordID != nil {
			t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it denied with no approval record", req, err)
		}
	})

	t.Run("the release of a grant it holds is refused and the grant ended", func(t *testing.T) {
		if release, err := m.Values(ctx, *heldOther.GrantID, outsiderSession, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || len(release.Values) != 0 {
			t.Fatalf("Values(SHARED_OTHER) = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
		}
		requireGrantEnded(t, m, *heldOther.GrantID)
	})

	t.Run("a full member's session is given and released what it was", func(t *testing.T) {
		session, key := newEnrollment(t, m.Store, "session", "ines-box-"+t.Name(), new(fixtureOperator), nil)
		for name, value := range map[string]string{"SHARED_TOKEN": "shared-token-v1", "AUTO_TOKEN": "auto-v1"} {
			req := granted(t, session, key, name)
			if release, err := m.Values(ctx, *req.GrantID, session, ReleaseCall{}); err != nil || release.Values[name] != value {
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
		release, err := m.Values(ctx, *heldUnasked.GrantID, unaskedSession, ReleaseCall{})
		uncheckable("Values(SHARED_TOKEN)", err)
		if len(release.Values) != 0 {
			t.Fatalf("Values(SHARED_TOKEN) while Dispatch is down released %v; want nothing", release.Values)
		}
		_, err = m.Create(ctx, unaskedSession, signRequest(t, m, unaskedKey, "needs it while Dispatch is down", "SHARED_OTHER"), "")
		uncheckable("Create(SHARED_OTHER)", err)
		if n := requests(); n != before {
			t.Fatalf("the refused ask left %d requests for the session, want %d", n, before)
		}

		// The control: once Dispatch answers, the same grant is released and the same ask granted.
		// A client keeps a person's failed read 2 s and then asks again (projectaccess's own test
		// proves that on a fake clock); a fresh client stands in for that wait.
		dispatch.SetDown(false)
		m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		if release, err := m.Values(ctx, *heldUnasked.GrantID, unaskedSession, ReleaseCall{}); err != nil || release.Values["SHARED_TOKEN"] != "shared-token-v1" {
			t.Fatalf("Values(SHARED_TOKEN) once Dispatch answers = %v, %v; want it released", release.Values, err)
		}
		granted(t, unaskedSession, unaskedKey, "SHARED_OTHER")
	})

	t.Run("an airlock connection is refused once the person is restricted, and while that cannot be read", func(t *testing.T) {
		airlocked, enr, grantID, release := twoNameGrant(t, "carried by the airlock")
		stand := projectaccesstest.NewServer(t, nil, map[string]string{fixtureOperator: projectaccess.MemberFull})
		// A client of its own for each step, as a broker reads Dispatch again once its cached
		// answer has aged past projectaccess.CacheTTL.
		asking := func() { airlocked.Projects = projectaccess.New(stand.URL, stand.Account, nil) }
		placeholder := release.Airlock.ProxyPassword
		asking()
		if conn, err := airlocked.AirlockConnection(ctx, placeholder); err != nil || conn.GrantID != grantID {
			t.Fatalf("AirlockConnection for a full member = %+v, %v; want grant %s", conn, err, grantID)
		}

		stand.SetDown(true)
		asking()
		_, err := airlocked.AirlockConnection(ctx, placeholder)
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != fixtureOperator {
			t.Fatalf("AirlockConnection while Dispatch is down = %v; want MemberKindUncheckable naming %s", err, fixtureOperator)
		}
		var revoked bool
		if err := airlocked.Store.Pool.QueryRow(ctx, `select revoked_at is not null from grants where id=$1`, grantID).Scan(&revoked); err != nil || revoked {
			t.Fatalf("grant %s revoked = %t, %v after an uncheckable connection; want it left live", grantID, revoked, err)
		}
		stand.SetDown(false)

		stand.SetMemberKind(fixtureOperator, projectaccess.MemberRestricted)
		asking()
		if _, err := airlocked.AirlockConnection(ctx, placeholder); !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) {
			t.Fatalf("AirlockConnection once %s is restricted = %v; want ErrGrantNotLive for a restricted member", fixtureOperator, err)
		}
		requireGrantEnded(t, airlocked, grantID)
		if _, err := airlocked.Values(ctx, grantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values of the ended grant = %v; want ErrGrantNotLive", err)
		}
	})

	t.Run("a group that owns or lists a secret gives a restricted member of it none of it", func(t *testing.T) {
		const groupApprover = "approver@example.com"
		for _, who := range []struct {
			name       string
			person     string
			restricted bool
		}{
			{"a restricted member of the group", groupMember, true},
			{"a full member of the group", secondMember, false},
		} {
			t.Run(who.name, func(t *testing.T) {
				f := newGroupFixture(t)
				f.directory.Set(approversGroup, groupMember, secondMember, groupApprover)
				session, key := newEnrollment(t, f.m.Store, "session", "group-box-"+t.Name(), new(who.person), nil)
				// What the session holds was given as a member of the group, before the machine
				// asked Dispatch anything.
				heldTeam := f.ask(t, session, key, "TEAM_TOKEN")
				heldListed := f.ask(t, session, key, "LISTED_TOKEN")
				for _, held := range []Request{heldTeam, heldListed} {
					if held.State != "granted" || held.GrantID == nil {
						t.Fatalf("Create(%s) as a member = %+v; want it granted at once", held.Secrets[0].Name, held)
					}
				}
				pendingTeamKey := f.ask(t, session, key, "TEAM_KEY")
				if pendingTeamKey.State != "pending" || pendingTeamKey.RecordID == nil {
					t.Fatalf("Create(TEAM_KEY) as a member = %+v; want it pending with a record", pendingTeamKey)
				}
				stand := projectaccesstest.NewServer(t, nil, map[string]string{
					groupMember:   projectaccess.MemberRestricted,
					secondMember:  projectaccess.MemberFull,
					groupApprover: projectaccess.MemberFull,
				})
				f.m.Projects = projectaccess.New(stand.URL, stand.Account, nil)
				deniedRestricted := func(t *testing.T, name string, req Request) {
					t.Helper()
					if req.State != "denied" || req.GrantID != nil || req.RecordID != nil || req.Detail == nil ||
						!strings.Contains(*req.Detail, name+": "+policy.ReasonRestrictedMember) {
						t.Fatalf("Create(%s) = %+v; want it denied naming %s, with no grant and no record", name, req, policy.ReasonRestrictedMember)
					}
				}

				t.Run("an ask for the group's agent-tier secret it holds a grant of", func(t *testing.T) {
					req := f.ask(t, session, key, "TEAM_TOKEN")
					if !who.restricted {
						if req.GrantID == nil || *req.GrantID != *heldTeam.GrantID {
							t.Fatalf("Create(TEAM_TOKEN) = %+v; want grant %s reused", req, *heldTeam.GrantID)
						}
						return
					}
					deniedRestricted(t, "TEAM_TOKEN", req)
					requireGrantEnded(t, f.m, *heldTeam.GrantID)
				})

				t.Run("the release of its grant of a secret the group lists", func(t *testing.T) {
					released, err := f.m.Values(ctx, *heldListed.GrantID, session, ReleaseCall{})
					if !who.restricted {
						if err != nil || released.Values["LISTED_TOKEN"] != "listed-v1" {
							t.Fatalf("Values(LISTED_TOKEN) = %v, %v; want listed-v1", released.Values, err)
						}
						return
					}
					if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) || len(released.Values) != 0 {
						t.Fatalf("Values(LISTED_TOKEN) = %v, %v; want ErrGrantNotLive for a restricted member and no value", released.Values, err)
					}
					requireGrantEnded(t, f.m, *heldListed.GrantID)
				})

				t.Run("a fresh ask for the secret the group lists", func(t *testing.T) {
					req := f.ask(t, session, key, "LISTED_TOKEN")
					if !who.restricted {
						if req.GrantID == nil || *req.GrantID != *heldListed.GrantID {
							t.Fatalf("Create(LISTED_TOKEN) = %+v; want grant %s reused", req, *heldListed.GrantID)
						}
						return
					}
					deniedRestricted(t, "LISTED_TOKEN", req)
				})

				t.Run("an ask for a human-tier secret the group lists", func(t *testing.T) {
					req := f.ask(t, session, key, "OWNER_KEY")
					if !who.restricted {
						if req.State != "pending" || req.RecordID == nil {
							t.Fatalf("Create(OWNER_KEY) = %+v; want it pending with a record", req)
						}
						return
					}
					deniedRestricted(t, "OWNER_KEY", req)
				})

				t.Run("another member's approval of the group's secret it left pending", func(t *testing.T) {
					dec, err := f.m.ApplyDecision(ctx, *pendingTeamKey.RecordID, true, groupApprover)
					switch {
					case who.restricted && (err != nil || dec.State != "denied" || dec.GrantID != "" || !strings.Contains(dec.Detail, "TEAM_KEY: "+policy.ReasonRestrictedMember)):
						t.Fatalf("ApplyDecision(approve TEAM_KEY) by %s = %+v, %v; want it denied naming %s and no grant", groupApprover, dec, err, policy.ReasonRestrictedMember)
					case !who.restricted && (err != nil || dec.State != "granted" || dec.GrantID == ""):
						t.Fatalf("ApplyDecision(approve TEAM_KEY) by %s = %+v, %v; want it granted", groupApprover, dec, err)
					}
				})
			})
		}
	})

	t.Run("an Anthropic federation secret a group owns and lists mints a restricted member of it nothing", func(t *testing.T) {
		const groupApprover = "approver@example.com"
		for _, who := range []struct {
			name       string
			person     string
			restricted bool
		}{
			{"a restricted member of the group", groupMember, true},
			{"a full member of the group", secondMember, false},
		} {
			t.Run(who.name, func(t *testing.T) {
				f := newGroupFixture(t)
				f.directory.Set(approversGroup, groupMember, secondMember, groupApprover)
				for _, name := range []string{"FED_A", "FED_B"} {
					f.local.Put(policytest.TypeSecret(name, approversForm, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)))
				}
				if err := f.m.Policy.Refresh(ctx); err != nil {
					t.Fatalf("policy refresh: %v", err)
				}
				for _, name := range []string{"FED_A", "FED_B"} {
					if !f.m.Policy.Get().Secrets[name].PersonApprovesEveryRelease() {
						t.Fatalf("the policy does not serve %s as an Anthropic federation secret; the fixture is wrong", name)
					}
				}
				var mints atomic.Int32
				f.m.Minters = map[string]Minter{policy.TypeAnthropicFederation: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
					mints.Add(1)
					return secrets.Minted{Value: "minted-federation-token"}, nil
				}}
				session, key := newEnrollment(t, f.m.Store, "session", "fed-box-"+t.Name(), new(who.person), nil)
				ask := func(t *testing.T, name string) Request {
					t.Helper()
					// A federation secret's max-grant is an hour (policytest.FixtureMaxGrant), so
					// the request names that lifetime.
					req, err := f.m.Create(ctx, session, signRequestFor(t, f.m, key, 60*60, name), "")
					if err != nil {
						t.Fatalf("Create(%s): %v", name, err)
					}
					return req
				}
				// Before the machine asks Dispatch: FED_A asked for and approved by another member
				// of the group, and FED_B left pending on the group.
				heldA := ask(t, "FED_A")
				if heldA.State != "pending" || heldA.RecordID == nil {
					t.Fatalf("Create(FED_A) as a member = %+v; want it pending on the group", heldA)
				}
				approvedA, err := f.m.ApplyDecision(ctx, *heldA.RecordID, true, groupApprover)
				if err != nil || approvedA.GrantID == "" {
					t.Fatalf("ApplyDecision(approve FED_A) as a member = %+v, %v; want a grant", approvedA, err)
				}
				pendingB := ask(t, "FED_B")
				if pendingB.State != "pending" || pendingB.RecordID == nil {
					t.Fatalf("Create(FED_B) as a member = %+v; want it pending on the group", pendingB)
				}
				stand := projectaccesstest.NewServer(t, nil, map[string]string{
					groupMember:   projectaccess.MemberRestricted,
					secondMember:  projectaccess.MemberFull,
					groupApprover: projectaccess.MemberFull,
				})
				f.m.Projects = projectaccess.New(stand.URL, stand.Account, nil)

				t.Run("the release of its grant of the federation secret", func(t *testing.T) {
					before := mints.Load()
					released, err := f.m.Values(ctx, approvedA.GrantID, session, ReleaseCall{})
					minted := mints.Load() - before
					if !who.restricted {
						if err != nil || released.Values["FED_A"] != "minted-federation-token" || minted != 1 {
							t.Fatalf("Values(FED_A) = %v, %v with %d mints; want the minted token, minted once", released.Values, err, minted)
						}
						return
					}
					if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) || len(released.Values) != 0 || minted != 0 {
						t.Fatalf("Values(FED_A) = %v, %v with %d mints; want ErrGrantNotLive for a restricted member, nothing minted", released.Values, err, minted)
					}
					requireGrantEnded(t, f.m, approvedA.GrantID)
				})

				t.Run("another member's approval of the federation secret it left pending", func(t *testing.T) {
					before := mints.Load()
					dec, err := f.m.ApplyDecision(ctx, *pendingB.RecordID, true, groupApprover)
					switch {
					case who.restricted && (err != nil || dec.State != "denied" || dec.GrantID != "" || !strings.Contains(dec.Detail, "FED_B: "+policy.ReasonRestrictedMember)):
						t.Fatalf("ApplyDecision(approve FED_B) by %s = %+v, %v; want it denied naming %s and no grant", groupApprover, dec, err, policy.ReasonRestrictedMember)
					case !who.restricted && (err != nil || dec.State != "granted" || dec.GrantID == ""):
						t.Fatalf("ApplyDecision(approve FED_B) by %s = %+v, %v; want it granted", groupApprover, dec, err)
					}
					if minted := mints.Load() - before; minted != 0 {
						t.Fatalf("the approval minted %d tokens; want none", minted)
					}
				})

				t.Run("a fresh ask for the federation secret", func(t *testing.T) {
					req := ask(t, "FED_A")
					if !who.restricted {
						if req.GrantID == nil || *req.GrantID != approvedA.GrantID {
							t.Fatalf("Create(FED_A) = %+v; want grant %s given back", req, approvedA.GrantID)
						}
						return
					}
					if req.State != "denied" || req.GrantID != nil || req.RecordID != nil || req.Detail == nil ||
						!strings.Contains(*req.Detail, "FED_A: "+policy.ReasonRestrictedMember) {
						t.Fatalf("Create(FED_A) = %+v; want it denied naming %s, with no grant and no record", req, policy.ReasonRestrictedMember)
					}
				})
			})
		}
	})
}

// TestARestrictedMembersGrantEndsWhateverItsNames: a restricted member's grant is ended at its
// next release before any of its names is re-checked, so a grant whose first name the policy no
// longer serves, which on its own is a pause and not an end (noLongerServed), still ends, and ends
// as a restricted member's (errRestrictedMember). The control is the same grant shape under a full
// member, which is refused for the name that is gone and left live.
//
// An approval of a request a restricted member's session left pending writes no grant whatever
// its names either: when the policy no longer serves any of them, so that no name is left for the
// policy to deny, the approval is still the policy's denial, restricted-member. The control is the
// same approval of a full member's request for the same name, which is granted, as an approval of
// a name the policy no longer serves is (its release is then refused for the name).
func TestARestrictedMembersGrantEndsWhateverItsNames(t *testing.T) {
	t.Parallel()
	const outsider = "outsider@outside.example"
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	gone := policytest.Secret("GONE_SECRET", policy.OwnerShared, policy.TierAgent, "gone-v1")
	goneKey := policytest.Secret("GONE_KEY", policy.OwnerShared, policy.TierHuman, "gone-key-v1")
	retag(t, m, gone, goneKey, policytest.Secret("KEPT_SECRET", policy.OwnerShared, policy.TierAgent, "kept-v1"))
	twoNames := func(t *testing.T, person string) (string, string) {
		t.Helper()
		session, key := newEnrollment(t, m.Store, "session", person+"-box-"+t.Name(), new(person), nil)
		req, err := m.Create(ctx, session, signRequest(t, m, key, "needs both", "GONE_SECRET", "KEPT_SECRET"), "")
		if err != nil || req.State != "granted" || req.GrantID == nil {
			t.Fatalf("Create(GONE_SECRET, KEPT_SECRET) for %s = %+v, %v; want it granted at once", person, req, err)
		}
		return session, *req.GrantID
	}
	outsiderSession, outsiderGrant := twoNames(t, outsider)
	fullSession, fullGrant := twoNames(t, fixtureOperator)
	// Each person's session also leaves a request for GONE_KEY pending before the policy drops it.
	pendingGoneKey := func(t *testing.T, person string) Request {
		t.Helper()
		session, key := newEnrollment(t, m.Store, "session", person+"-asks-"+t.Name(), new(person), nil)
		req, err := m.Create(ctx, session, signRequest(t, m, key, "needs the key", "GONE_KEY"), "")
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(GONE_KEY) for %s = %+v, %v; want pending with a record", person, req, err)
		}
		return req
	}
	outsiderPending := pendingGoneKey(t, outsider)
	fullPending := pendingGoneKey(t, fixtureOperator)
	fixtureStore(m).Delete(gone.Name)
	fixtureStore(m).Delete(goneKey.Name)
	if err := m.Policy.Refresh(ctx); err != nil {
		t.Fatalf("policy refresh: %v", err)
	}
	for _, name := range []string{"GONE_SECRET", "GONE_KEY"} {
		if _, served := m.Policy.Get().Secrets[name]; served {
			t.Fatalf("the policy still serves %s; the fixture is wrong", name)
		}
	}
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{outsider: projectaccess.MemberRestricted, fixtureOperator: projectaccess.MemberFull})
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)

	release, err := m.Values(ctx, outsiderGrant, outsiderSession, ReleaseCall{})
	if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) || len(release.Values) != 0 {
		t.Fatalf("Values of %s's grant whose first name is gone = %v, %v; want ErrGrantNotLive for a restricted member and no value", outsider, release.Values, err)
	}
	requireGrantEnded(t, m, outsiderGrant)

	// The control: a full member's grant of the same names is refused for the name that is gone,
	// a pause, and stays live.
	if _, err := m.Values(ctx, fullGrant, fullSession, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || errors.Is(err, errRestrictedMember) {
		t.Fatalf("Values of %s's grant whose first name is gone = %v; want ErrGrantNotLive for the name, not a restricted member's end", fixtureOperator, err)
	}
	var revoked bool
	if err := m.Store.Pool.QueryRow(ctx, `select revoked_at is not null from grants where id=$1`, fullGrant).Scan(&revoked); err != nil || revoked {
		t.Fatalf("%s's grant %s revoked = %t, %v; want it left live", fixtureOperator, fullGrant, revoked, err)
	}

	// The approval of the restricted member's request whose one name the policy no longer serves
	// is the policy's denial, restricted-member, and writes no grant.
	dec, err := m.ApplyDecision(ctx, *outsiderPending.RecordID, true, "bob@example.com")
	if err != nil || dec.State != "denied" || dec.GrantID != "" || !strings.Contains(dec.Detail, "GONE_KEY: "+policy.ReasonRestrictedMember) {
		t.Fatalf("ApplyDecision(approve %s's GONE_KEY, no longer served) = %+v, %v; want it denied naming %s and no grant", outsider, dec, err, policy.ReasonRestrictedMember)
	}
	var grants int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, outsiderPending.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("%s's GONE_KEY request holds %d grants, %v; want none", outsider, grants, err)
	}

	// The control: the same approval of the full member's request is granted.
	if dec, err := m.ApplyDecision(ctx, *fullPending.RecordID, true, "bob@example.com"); err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(approve %s's GONE_KEY, no longer served) = %+v, %v; want it granted", fixtureOperator, dec, err)
	}
}

// TestAnApprovalOfARestrictedMembersOldRequestGrantsNothing: a request a restricted member's
// session left pending from before the broker asked Dispatch is not granted by an approval: the
// approval asks Dispatch first and records the request as the policy's denial, restricted-member,
// with no grant written; and the model judge's read of the policy (JudgePolicy) answers deny,
// restricted-member, before it would spend a model call. The control is a full member's pending
// request on the same machine, which the same approval grants.
func TestAnApprovalOfARestrictedMembersOldRequestGrantsNothing(t *testing.T) {
	t.Parallel()
	const outsider = "outsider@outside.example"
	ctx := context.Background()
	m, _, _, _ := newFixture(t)
	pending := func(t *testing.T, person string) (string, Request) {
		t.Helper()
		session, key := newEnrollment(t, m.Store, "session", person+"-box-"+t.Name(), new(person), nil)
		req, err := m.Create(ctx, session, signRequest(t, m, key, "needs the shared key", "SHARED_KEY"), "")
		if err != nil || req.State != "pending" || req.RecordID == nil {
			t.Fatalf("Create(SHARED_KEY) for %s = %+v, %v; want pending with a record", person, req, err)
		}
		return session, req
	}
	outsiderSession, outsiderReq := pending(t, outsider)
	_, fullReq := pending(t, fixtureOperator)
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{outsider: projectaccess.MemberRestricted, fixtureOperator: projectaccess.MemberFull})
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)

	decision, verdict, _, err := m.JudgePolicy(ctx, outsiderSession, "SHARED_KEY")
	if err != nil || verdict || decision.Outcome != policy.Deny || decision.Reason != policy.ReasonRestrictedMember {
		t.Fatalf("JudgePolicy(%s's session, SHARED_KEY) = %+v, %v; want deny %s", outsider, decision, err, policy.ReasonRestrictedMember)
	}
	dec, err := m.ApplyDecision(ctx, *outsiderReq.RecordID, true, "bob@example.com")
	if err != nil || dec.State != "denied" || dec.GrantID != "" || !strings.Contains(dec.Detail, "SHARED_KEY: "+policy.ReasonRestrictedMember) {
		t.Fatalf("ApplyDecision(approve %s's old request) = %+v, %v; want it denied naming %s and no grant", outsider, dec, err, policy.ReasonRestrictedMember)
	}
	var grants int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, outsiderReq.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("%s's old request holds %d grants, %v; want none", outsider, grants, err)
	}

	// While Dispatch cannot say what kind of member the full member is, the same approval is
	// refused, *projectaccess.MemberKindUncheckable, and decides nothing: the request stays
	// pending with no grant. A fresh client, so no answer the steps above kept
	// (projectaccess.CacheTTL) stands in for the outage.
	dispatch.SetDown(true)
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	dec, err = m.ApplyDecision(ctx, *fullReq.RecordID, true, "bob@example.com")
	var uncheckable *projectaccess.MemberKindUncheckable
	if !errors.As(err, &uncheckable) || uncheckable.Person != fixtureOperator {
		t.Fatalf("ApplyDecision(approve %s's request) while Dispatch is down = %+v, %v; want *projectaccess.MemberKindUncheckable naming %s", fixtureOperator, dec, err, fixtureOperator)
	}
	var state string
	if err := m.Store.Pool.QueryRow(ctx, `select state, (select count(*) from grants where request_id=$1) from requests where id=$1`, fullReq.ID).Scan(&state, &grants); err != nil || state != "pending" || grants != 0 {
		t.Fatalf("%s's request after the refused approval: state %q with %d grants, %v; want pending and none", fixtureOperator, state, grants, err)
	}

	// The control: once Dispatch answers, the same approval of the full member's request grants it.
	// A client keeps a person's failed read 2 s; a fresh client stands in for that wait.
	dispatch.SetDown(false)
	m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	if dec, err := m.ApplyDecision(ctx, *fullReq.RecordID, true, "bob@example.com"); err != nil || dec.State != "granted" || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(approve %s's request) once Dispatch answers = %+v, %v; want it granted", fixtureOperator, dec, err)
	}
}

// TestAJudgesApprovalIsRefusedWhileTheMemberKindCannotBeRead: the model judge's approval asks what
// kind of member the session's person is, afresh and under its bound, and while Dispatch cannot
// say, the approval is refused, *projectaccess.MemberKindUncheckable, and nothing is granted; once
// Dispatch answers, the same approval grants.
func TestAJudgesApprovalIsRefusedWhileTheMemberKindCannotBeRead(t *testing.T) {
	t.Parallel()
	m, _, req := judgeHeldTeamRequest(t)
	dispatch := answerFrom(t, m, dispatchRows(publicTeam))
	dispatch.SetDown(true)
	dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
	var uncheckable *projectaccess.MemberKindUncheckable
	if !errors.As(err, &uncheckable) || uncheckable.Person != fixtureOperator {
		t.Fatalf("ApplyJudgeDecision(approve) while Dispatch is down = %+v, %v; want *projectaccess.MemberKindUncheckable naming %s", dec, err, fixtureOperator)
	}
	var grants int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from grants where request_id=$1`, req.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("the judge's refused approval left %d grants, %v; want none", grants, err)
	}
	dispatch.SetDown(false)
	if dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"}); err != nil || dec.State != "granted" {
		t.Fatalf("ApplyJudgeDecision(approve) once Dispatch answers = %+v, %v; want granted: the judge asks afresh", dec, err)
	}
}

// TestARestrictedMembersPodGetsNoSecretAfterTheSettingChanges: a person's pod enrolled for a
// restricted member before the broker asked Dispatch, whose account BROKER_POD_OPERATORS then
// drops or maps to another person, is still held to the person it was enrolled for. The setting
// decides whom the pod acts for, and so whose owned secrets it gets (operatorOf). It does not
// change whose launcher credential enrolled the pod, and that person's member kind is what decides
// that the pod gets no secret at all. So once the machine asks Dispatch, the pod's ask for a
// shared agent-tier secret is denied restricted-member, an ask for a name it holds a grant of is
// denied and the grant ended rather than reused, the release of its other grant is refused and the
// grant ended, the airlock proxy's next connection for its airlocked grant is refused and the
// grant ended, the model judge's read of the policy answers deny, and an approval of the request
// it left pending is the policy's denial. While Dispatch cannot be asked about that person, the
// release is refused *projectaccess.MemberKindUncheckable naming the person, and the grant is
// left live.
//
// The control is a full member's pod under the same change. Dispatch is asked about the person
// it was enrolled for, as the outage step shows, and the pod is given, reuses and is released
// what it was (as TestPersonPodGrantsFollowTheSetting has it), its airlock connection is carried,
// and its pending request is granted.
func TestARestrictedMembersPodGetsNoSecretAfterTheSettingChanges(t *testing.T) {
	t.Parallel()
	const outsider = "outsider@outside.example"
	outsidersPod := personPod{person: outsider, service: "person-outsider", account: "system:serviceaccount:people:outsider"}
	for _, change := range []struct {
		name  string
		apply func(operators map[string]string, account string)
	}{
		{"the setting drops the account", func(operators map[string]string, account string) { delete(operators, account) }},
		{"the setting maps the account to another person", func(operators map[string]string, account string) { operators[account] = otherPerson }},
	} {
		for _, pod := range []struct {
			name       string
			of         personPod
			restricted bool
		}{
			{"a restricted member's pod", outsidersPod, true},
			{"a full member's pod", samisPod, false},
		} {
			t.Run(change.name+", "+pod.name, func(t *testing.T) {
				ctx := t.Context()
				m, _, _, _ := newPlaceholderFixture(t, bound(rule("AWS_KEY", "aws")))
				retag(t, m,
					policytest.Secret("AWS_KEY", policy.OwnerShared, policy.TierAgent, `{"acc`+`ess_key_id":"AKI`+`AONE","sec`+`ret_acc`+`ess_key":"real-secret"}`),
					policytest.Secret("SHARED_TOKEN", policy.OwnerShared, policy.TierAgent, "shared-token-v1"),
					policytest.Secret("SHARED_OTHER", policy.OwnerShared, policy.TierAgent, "shared-other-v1"),
					policytest.Secret("SHARED_FRESH", policy.OwnerShared, policy.TierAgent, "shared-fresh-v1"))
				m.PodOperators = map[string]string{pod.of.account: pod.of.person}
				enr, key := pod.of.enroll(t, m.Store, "pod-"+t.Name())

				// What the pod holds was given while the setting mapped its account to the person and
				// before the machine asked Dispatch anything: a pod a broker with no project-access
				// client enrolled, or one enrolled before the member-kind rule.
				_, heldToken := grantOf(t, m, enr, key, "needs the token", "SHARED_TOKEN")
				_, heldOther := grantOf(t, m, enr, key, "needs the other", "SHARED_OTHER")
				_, airlocked := grantOf(t, m, enr, key, "carried by the airlock", "AWS_KEY")
				release, err := m.Values(ctx, airlocked, enr, ReleaseCall{})
				if err != nil || release.Airlock == nil || release.Airlock.ProxyPassword == "" {
					t.Fatalf("Values(AWS_KEY) = %+v, %v; want a placeholder release", release, err)
				}
				placeholder := release.Airlock.ProxyPassword
				pending, err := m.Create(ctx, enr, signRequest(t, m, key, "needs the shared key", "SHARED_KEY"), "")
				if err != nil || pending.State != "pending" || pending.RecordID == nil {
					t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending with a record", pending, err)
				}

				change.apply(m.PodOperators, pod.of.account)
				// The case under test: the pod acts for no person now, and its row keeps the person
				// it was enrolled for.
				if row, err := m.enrollment(ctx, enr); err != nil || m.operatorOf(row) != nil || deref(row.Operator) != pod.of.person {
					t.Fatalf("after %s, enrollment %s = %+v, %v, acting for %q; want it acting for no person and recording %s", change.name, enr, row, err, deref(m.operatorOf(row)), pod.of.person)
				}
				dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
					outsider:        projectaccess.MemberRestricted,
					fixtureOperator: projectaccess.MemberFull,
					otherPerson:     projectaccess.MemberFull,
				})
				// A client of its own for each step, as a broker reads Dispatch again once a kept
				// answer, or a kept failure, has aged out.
				asking := func() { m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil) }

				t.Run("a release while Dispatch cannot say what the person is", func(t *testing.T) {
					dispatch.SetDown(true)
					asking()
					defer func() { dispatch.SetDown(false); asking() }()
					released, err := m.Values(ctx, heldOther, enr, ReleaseCall{})
					var uncheckable *projectaccess.MemberKindUncheckable
					if !errors.As(err, &uncheckable) || uncheckable.Person != pod.of.person || len(released.Values) != 0 {
						t.Fatalf("Values(SHARED_OTHER) while Dispatch is down = %v, %v; want *projectaccess.MemberKindUncheckable naming %s and no value", released.Values, err, pod.of.person)
					}
					var revoked bool
					if err := m.Store.Pool.QueryRow(ctx, `select revoked_at is not null from grants where id=$1`, heldOther).Scan(&revoked); err != nil || revoked {
						t.Fatalf("grant %s revoked = %t, %v after an uncheckable release; want it left live", heldOther, revoked, err)
					}
				})

				t.Run("an ask for a shared agent-tier secret", func(t *testing.T) {
					req, err := m.Create(ctx, enr, signRequest(t, m, key, "needs it", "SHARED_FRESH"), "")
					switch {
					case pod.restricted && (err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil || !strings.Contains(*req.Detail, "SHARED_FRESH: "+policy.ReasonRestrictedMember)):
						t.Fatalf("Create(SHARED_FRESH) = %+v, %v; want it denied naming %s", req, err, policy.ReasonRestrictedMember)
					case !pod.restricted && (err != nil || req.State != "granted" || req.GrantID == nil):
						t.Fatalf("Create(SHARED_FRESH) = %+v, %v; want it granted at once", req, err)
					}
				})

				t.Run("an ask for a name it holds a grant of", func(t *testing.T) {
					req, err := m.Create(ctx, enr, signRequest(t, m, key, "needs the token again", "SHARED_TOKEN"), "")
					if !pod.restricted {
						if err != nil || req.GrantID == nil || *req.GrantID != heldToken {
							t.Fatalf("Create(SHARED_TOKEN) = %+v, %v; want grant %s reused", req, err, heldToken)
						}
						return
					}
					if err != nil || req.State != "denied" || req.GrantID != nil || req.Detail == nil || !strings.Contains(*req.Detail, "SHARED_TOKEN: "+policy.ReasonRestrictedMember) {
						t.Fatalf("Create(SHARED_TOKEN) = %+v, %v; want it denied naming %s, not grant %s reused", req, err, policy.ReasonRestrictedMember, heldToken)
					}
					requireGrantEnded(t, m, heldToken)
				})

				t.Run("the release of a grant it holds", func(t *testing.T) {
					released, err := m.Values(ctx, heldOther, enr, ReleaseCall{})
					if !pod.restricted {
						if err != nil || released.Values["SHARED_OTHER"] != "shared-other-v1" {
							t.Fatalf("Values(SHARED_OTHER) = %v, %v; want shared-other-v1", released.Values, err)
						}
						return
					}
					if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) || len(released.Values) != 0 {
						t.Fatalf("Values(SHARED_OTHER) = %v, %v; want ErrGrantNotLive for a restricted member and no value", released.Values, err)
					}
					requireGrantEnded(t, m, heldOther)
				})

				t.Run("the airlock proxy's next connection", func(t *testing.T) {
					conn, err := m.AirlockConnection(ctx, placeholder)
					if !pod.restricted {
						if err != nil || conn.GrantID != airlocked {
							t.Fatalf("AirlockConnection = %+v, %v; want grant %s", conn, err, airlocked)
						}
						return
					}
					if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) {
						t.Fatalf("AirlockConnection = %+v, %v; want ErrGrantNotLive for a restricted member", conn, err)
					}
					requireGrantEnded(t, m, airlocked)
				})

				t.Run("the model judge's read of the policy", func(t *testing.T) {
					decision, _, _, err := m.JudgePolicy(ctx, enr, "SHARED_KEY")
					switch {
					case pod.restricted && (err != nil || decision.Outcome != policy.Deny || decision.Reason != policy.ReasonRestrictedMember):
						t.Fatalf("JudgePolicy(SHARED_KEY) = %+v, %v; want deny %s", decision, err, policy.ReasonRestrictedMember)
					case !pod.restricted && (err != nil || decision.Outcome != policy.Approval):
						t.Fatalf("JudgePolicy(SHARED_KEY) = %+v, %v; want an approval", decision, err)
					}
				})

				t.Run("an approval of the request it left pending", func(t *testing.T) {
					dec, err := m.ApplyDecision(ctx, *pending.RecordID, true, "bob@example.com")
					switch {
					case pod.restricted && (err != nil || dec.State != "denied" || dec.GrantID != "" || !strings.Contains(dec.Detail, "SHARED_KEY: "+policy.ReasonRestrictedMember)):
						t.Fatalf("ApplyDecision(approve SHARED_KEY) = %+v, %v; want it denied naming %s and no grant", dec, err, policy.ReasonRestrictedMember)
					case !pod.restricted && (err != nil || dec.State != "granted" || dec.GrantID == ""):
						t.Fatalf("ApplyDecision(approve SHARED_KEY) = %+v, %v; want it granted", dec, err)
					}
				})
			})
		}
	}
}

// requireGrantEnded fails t unless grant is revoked.
func requireGrantEnded(t *testing.T, m *Machine, grant string) {
	t.Helper()
	var revoked bool
	if err := m.Store.Pool.QueryRow(context.Background(), `select revoked_at is not null from grants where id=$1`, grant).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("grant %s revoked = %t, %v; want it ended", grant, revoked, err)
	}
}
