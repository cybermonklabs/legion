// packages/envoy/internal/cipher/requests/judge_grant_test.go
// The life of a grant the model judge decided, after the decision: what keeps releasing it — its
// key's judge tag rather than any person — what ends it, who sees it on the Live grants page, and
// who may end it there.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// judgedGrant is one grant the model judge approved for names on enrollmentID, the state every
// test in this file starts from.
func judgedGrant(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, lifetime int, names ...string) string {
	t.Helper()
	req := pendingJudgeable(t, m, enrollmentID, key, "INFRA-1353 wants it", lifetime, names...)
	dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "grant", "issue": "ACME-1353"})
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyJudgeDecision(%v) = %+v, %v; want a grant", names, dec, err)
	}
	return dec.GrantID
}

// releasedValue is what one release handed the session for name: the value itself, or, for a
// name the airlock holds back, what the command's variable of that name holds. Empty means the
// release carried nothing for it.
func releasedValue(release Release, name string) string {
	if release.Airlock != nil {
		if held, ok := release.Airlock.Names[name]; ok {
			return held.Value
		}
	}
	return release.Values[name]
}

// TestAJudgedGrantLivesByItsJudgeTagAndNotByAnOwner: the judge approves an owned human-tier
// secret in its owner's stead, so the grant is decided by no login any owner rule admits. What
// keeps such a grant releasing is the judge tag its approval rested on: the seconds the key's tag
// gives the judge must still cover the lifetime the grant holds. Every release re-verifies the
// grant's chain against the live tags (record.MayJudge, the chain verifier's JudgeSeconds), so a
// policy move that leaves the tag alone goes on releasing it, and a retag that takes the judge off
// the key or lowers it below the grant's lifetime refuses it there, as a broken chain. The owner's
// own grant of the same secret is decided by a person and is held to the person's rule
// throughout: none of this moves it.
func TestAJudgedGrantLivesByItsJudgeTagAndNotByAnOwner(t *testing.T) {
	deel := func(judge int) memory.Secret {
		s := policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1")
		if judge == 0 {
			return s
		}
		return policytest.Judged(s, judge, "")
	}
	for name, c := range map[string]struct {
		retag func(t *testing.T, m *Machine)
		live  bool
	}{
		"the tag is the one the grant was decided under": {retag: func(t *testing.T, m *Machine) {
			retag(t, m, policytest.Secret("UNRELATED_KEY", otherPerson, policy.TierAgent, "unrelated-v1"))
		}, live: true},
		"the tag's judge is exactly the grant's lifetime": {retag: func(t *testing.T, m *Machine) { retag(t, m, deel(600)) }, live: true},
		"the tag's judge is below the grant's lifetime":   {retag: func(t *testing.T, m *Machine) { retag(t, m, deel(599)) }},
		"the key carries no judge tag":                    {retag: func(t *testing.T, m *Machine) { retag(t, m, deel(0)) }},
	} {
		t.Run(name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")

			// Another session of the owner's holds a grant of the same secret that the owner
			// approved themselves, which every assertion below answers for too.
			own, ownKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			pending := pendingJudgeable(t, m, own, ownKey, "INFRA-1353 wants it on the other box", 600, "DEEL_API_KEY")
			handOver(t, m, *pending.RecordID)
			byOwner, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
			if err != nil || byOwner.GrantID == "" {
				t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
			}

			// Each retag moves the policy version, so both releases re-check every name they
			// were granted as well as re-verifying their chains.
			before := m.Policy.Get().Version
			c.retag(t, m)
			if m.Policy.Get().Version == before {
				t.Fatalf("the policy version stood still at %s, so the re-checks this test is about never ran", before)
			}

			release, err := m.Values(t.Context(), judged, session, ReleaseCall{})
			switch {
			case c.live && (err != nil || releasedValue(release, "DEEL_API_KEY") == ""):
				t.Fatalf("Values(the judge's grant) = %+v, %v; want DEEL_API_KEY released", release, err)
			case !c.live && !errors.Is(err, ErrGrantChainInvalid):
				t.Fatalf("Values(the judge's grant) = %+v, %v; want ErrGrantChainInvalid, the judge tag no longer admitting the approval", release, err)
			}
			if release, err := m.Values(t.Context(), byOwner.GrantID, own, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
				t.Fatalf("Values(the owner's own grant) = %+v, %v; want DEEL_API_KEY released whatever the judge tag now says", release, err)
			}
		})
	}
}

// TestJudgeStillDecidesReadsTheJudgeTag pins stillAllowed's arm for a judged grant on its own,
// since every release re-verifies the chain first and refuses there what this arm also refuses:
// a key whose judge tag still covers the grant's lifetime passes, and one whose tag is gone, is
// lower than that lifetime, or whose grant names no lifetime is no longer live.
func TestJudgeStillDecidesReadsTheJudgeTag(t *testing.T) {
	store := memory.New(
		policytest.Judged(policytest.Secret("JUDGED_KEY", fixtureOperator, policy.TierHuman, "v"), 600, ""),
		policytest.Secret("UNJUDGED_KEY", fixtureOperator, policy.TierHuman, "v"),
	)
	set := policytest.Current(t, store).Get()
	for _, c := range []struct {
		name     string
		lifetime *int
		live     bool
	}{
		{"JUDGED_KEY", new(600), true},
		{"JUDGED_KEY", new(601), false},
		{"JUDGED_KEY", nil, false},
		{"UNJUDGED_KEY", new(60), false},
	} {
		err := judgeStillDecides(set, c.name, c.lifetime)
		if live := err == nil; live != c.live || (!live && !errors.Is(err, ErrGrantNotLive)) {
			t.Errorf("judgeStillDecides(%s, %v) = %v; want live %v, else ErrGrantNotLive", c.name, c.lifetime, err, c.live)
		}
	}
}

// deelAsAdmin is the fixture's own DEEL_API_KEY tagged as an administrator's secret: the same
// owner, tier, value and judge tag, now carrying admin: "true" and, beside it, the two things
// policy.TagRules demands of such a secret — a requesters list naming the person every session
// below is signed in as, and a max-grant, an hour here (policytest.FixtureMaxGrant), which is
// longer than any grant this file makes. Nothing else about the secret moves, so what answers
// below is the admin tag and not a secret that changed hands, shrank its cap or lost its judge.
func deelAsAdmin() memory.Secret {
	admin := policytest.ListedSecret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, fixtureOperator, "deel-v1")
	admin.Tags[policy.TagAdmin] = policy.AdminTrue
	return policytest.Judged(admin, 900, "")
}

// TestAnAdministratorsTagEndsAJudgedGrantAndLeavesAPersonsAlone: a person approves every release
// of a secret its owner tags as an administrator's (policy.TagAdmin). The model judge may have
// decided a grant of it before the tag was written — its judge tag gave the judge seconds of it,
// and the judge read a secret that was not yet an administrator's — and the tag moves the policy
// version, so every release of that grant and every reuse of it re-reads what it was granted.
// Both refuse it and end the grant for good, rather than let a model's approval go on releasing
// an administrator's credential for the rest of the seconds its judge tag gave it. Taking the tag
// off again leaves the grant ended: the session asks afresh, as a new request. A grant of the
// same secret that its owner approved releases throughout, a person's approval being all the
// tag asks.
func TestAnAdministratorsTagEndsAJudgedGrantAndLeavesAPersonsAlone(t *testing.T) {
	t.Run("a release refuses it and ends it for good", func(t *testing.T) {
		m, session, key := newJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")

		// Another session of the owner's holds a grant of the same secret that the owner
		// approved themselves, which every assertion below answers for too.
		own, ownKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
		pending := pendingJudgeable(t, m, own, ownKey, "INFRA-1353 wants it on the other box", 600, "DEEL_API_KEY")
		handOver(t, m, *pending.RecordID)
		byOwner, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
		if err != nil || byOwner.GrantID == "" {
			t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
		}

		before := m.Policy.Get().Version
		retag(t, m, deelAsAdmin())
		if m.Policy.Get().Version == before {
			t.Fatalf("the policy version stood still at %s, so the re-checks this test is about never ran", before)
		}

		release, err := m.Values(t.Context(), judged, session, ReleaseCall{})
		if !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" {
			t.Fatalf("Values(the judge's grant of what is now an administrator's secret) = %+v, %v; want ErrGrantNotLive and nothing released", release, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the judged grant's revoked_by = %q with %d grant.revoked rows by %s; want it ended once for good", by, audits, GrantEndedActor)
		}
		if release, err := m.Values(t.Context(), judged, session, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" {
			t.Fatalf("a second Values of the judged grant = %+v, %v; want it refused as ended, not merely passed over once", release, err)
		}
		if release, err := m.Values(t.Context(), byOwner.GrantID, own, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
			t.Fatalf("Values(the owner's own grant) = %+v, %v; want DEEL_API_KEY released: a person approved it, which is all the tag asks", release, err)
		}

		// The admin tag taken off again does not bring the judged grant back.
		retag(t, m, policytest.Judged(policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"), 900, ""))
		if _, err := m.Values(t.Context(), judged, session, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values of the judged grant once the tag is gone = %v; want it still ended", err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("once the tag is gone: revoked_by %q with %d grant.revoked rows; want the one end", by, audits)
		}
	})

	t.Run("the next request ends it rather than handing it back", func(t *testing.T) {
		m, session, key := newJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")
		reused, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "INFRA-1353 wants it again", 600, "DEEL_API_KEY"), "")
		if err != nil || reused.GrantID == nil || *reused.GrantID != judged {
			t.Fatalf("a request for the same name and lifetime while the secret is an ordinary one = %+v, %v; want the judged grant %s handed back", reused, err, judged)
		}

		retag(t, m, deelAsAdmin())
		again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "INFRA-1353 wants it once more", 600, "DEEL_API_KEY"), "")
		if err != nil || again.GrantID != nil || again.State != "pending" {
			t.Fatalf("Create once the secret is an administrator's = %+v, %v; want no grant handed back and a card its person answers", again, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the grant the reuse found = revoked_by %q with %d grant.revoked rows by %s; want the reuse to have ended it once", by, audits, GrantEndedActor)
		}
	})
}

// TestAJudgedGrantOfASharedSecretIsEverySignedInPersonsToSeeAndEnd: the judge approves a shared
// human-tier secret in anyone's stead, so the grant names anyone as its approver and no person at
// all, and it sits on a service's pod, which acts for no person either. Without this nobody could
// see what the judge had handed a pod, which is the reason every signed-in person already sees a
// grant a service's pod took without asking. Each of them lists both pods' grants, and either of
// them ends one; the sentinel is no signed-in person and lists nothing. Neither revoke is the
// session's operator's — a service's pod has none — so neither withholds anything from the pod it
// ended a grant on.
func TestAJudgedGrantOfASharedSecretIsEverySignedInPersonsToSeeAndEnd(t *testing.T) {
	m, _, _ := newJudgeFixture(t)
	first, firstKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-one-"+t.Name())
	second, secondKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-two-"+t.Name())
	one := judgedGrant(t, m, first, firstKey, 600, "SHARED_KEY")
	two := judgedGrant(t, m, second, secondKey, 600, "SHARED_KEY")

	for _, viewer := range []string{otherPerson, carol} {
		listed, err := m.GrantsForApprover(t.Context(), viewer)
		if err != nil || len(listed) != 2 {
			t.Fatalf("GrantsForApprover(%s) = %+v, %v; want both pods' judged grants of the shared secret", viewer, listed, err)
		}
		for _, g := range listed {
			if g.Approver != record.AnyoneApprover || g.Granted != policy.Approval || g.RecordID == nil {
				t.Errorf("GrantsForApprover(%s) listed %+v; want it granted by approval, under the %q the judge decided in the stead of, on the record it rests on", viewer, g, record.AnyoneApprover)
			}
		}
	}
	if listed, err := m.GrantsForApprover(t.Context(), record.AnyoneApprover); err != nil || len(listed) != 0 {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want nothing: the sentinel is no signed-in person", record.AnyoneApprover, listed, err)
	}

	for _, revoke := range []struct{ grant, login string }{{one, otherPerson}, {two, carol}} {
		if err := m.RevokeByApprover(t.Context(), revoke.grant, revoke.login, record.HumanActor(revoke.login)); err != nil {
			t.Fatalf("RevokeByApprover(%s, %s) = %v; want the grant ended", revoke.grant, revoke.login, err)
		}
	}
	for _, viewer := range []string{otherPerson, carol} {
		if listed, err := m.GrantsForApprover(t.Context(), viewer); err != nil || len(listed) != 0 {
			t.Fatalf("GrantsForApprover(%s) after both revokes = %+v, %v; want nothing left", viewer, listed, err)
		}
	}
	var withheld int
	if err := m.Store.Pool.QueryRow(t.Context(), `select count(*) from withheld_secrets where enrollment_id in ($1,$2)`, first, second).Scan(&withheld); err != nil {
		t.Fatalf("read withheld_secrets: %v", err)
	}
	if withheld != 0 {
		t.Errorf("withheld_secrets rows = %d; want none: whoever ends such a grant stands in for its approver, not for the session", withheld)
	}
}

// TestAJudgedGrantOfAnOwnedSecretIsItsOwnersAlone: the judge approves an owned secret in its
// owner's stead, so that grant records the owner as its approver and is the owner's to see and to
// end, exactly as one the owner approved. It is on no other person's page, and no other person
// ends it, though it too sits on a pod that acts for nobody.
func TestAJudgedGrantOfAnOwnedSecretIsItsOwnersAlone(t *testing.T) {
	m, _, _ := newJudgeFixture(t)
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
	grantID := judgedGrant(t, m, pod, podKey, 600, "DEEL_API_KEY")

	listed, err := m.GrantsForApprover(t.Context(), fixtureOperator)
	if err != nil || len(listed) != 1 || listed[0].GrantID != grantID || listed[0].Approver != fixtureOperator {
		t.Fatalf("GrantsForApprover(%s) = %+v, %v; want the one grant the judge approved in their stead", fixtureOperator, listed, err)
	}
	for _, viewer := range []string{otherPerson, carol} {
		if listed, err := m.GrantsForApprover(t.Context(), viewer); err != nil || len(listed) != 0 {
			t.Fatalf("GrantsForApprover(%s) = %+v, %v; want nothing: the grant is the owner's", viewer, listed, err)
		}
		if err := m.RevokeByApprover(t.Context(), grantID, viewer, record.HumanActor(viewer)); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("RevokeByApprover(%s) = %v; want ErrNotApprover", viewer, err)
		}
	}
	if err := m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
		t.Fatalf("RevokeByApprover(%s) = %v; want the owner's own revoke to end it", fixtureOperator, err)
	}
	if listed, err := m.GrantsForApprover(t.Context(), fixtureOperator); err != nil || len(listed) != 0 {
		t.Fatalf("GrantsForApprover(%s) after the revoke = %+v, %v; want nothing left", fixtureOperator, listed, err)
	}
}
