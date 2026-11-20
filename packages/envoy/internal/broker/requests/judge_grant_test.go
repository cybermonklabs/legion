// packages/envoy/internal/broker/requests/judge_grant_test.go
// The life of a grant the model judge decided, after the decision: what keeps releasing it — its
// airlock entry rather than any person — what ends it, who sees it on the Live grants page, and
// who may end it there.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// judgedGrant is one grant the model judge approved for names on enrollmentID, the state every
// test in this file starts from.
func judgedGrant(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, lifetime int, names ...string) string {
	t.Helper()
	req := pendingJudgeable(t, m, enrollmentID, key, "the release job wants it", lifetime, names...)
	dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "grant", "issue": "ACME-1353"})
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyJudgeDecision(%v) = %+v, %v; want a grant", names, dec, err)
	}
	return dec.GrantID
}

// releasedValue is what one release handed the session for name: the value itself, or, for a
// name the airlock holds back, what the command's variable of that name holds. Empty means the
// release carried nothing for it. A judged name is always an airlock entry's — the rules are
// what let the judge decide it — so the same grant answers through the other half of a release
// once that entry is gone.
func releasedValue(release Release, name string) string {
	if release.Airlock != nil {
		if held, ok := release.Airlock.Names[name]; ok {
			return held.Value
		}
	}
	return release.Values[name]
}

// withJudge answers the rules file an operator rewrote: name's entry giving the judge seconds,
// or, for a nil seconds, an entry with no judge at all. Every other entry is left as it was.
func withJudge(name string, seconds *uint32) func(airlock.Rules) airlock.Rules {
	return func(rules airlock.Rules) airlock.Rules {
		changed := airlock.Rules{Secrets: slices.Clone(rules.Secrets)}
		for i := range changed.Secrets {
			if changed.Secrets[i].Name == name {
				changed.Secrets[i].Judge = seconds
			}
		}
		return changed
	}
}

// withoutEntry answers the rules file with name's entry gone from it altogether.
func withoutEntry(name string) func(airlock.Rules) airlock.Rules {
	return func(rules airlock.Rules) airlock.Rules {
		var changed airlock.Rules
		for _, s := range rules.Secrets {
			if s.Name != name {
				changed.Secrets = append(changed.Secrets, s)
			}
		}
		return changed
	}
}

// asDecided is the rules file the grant was decided under, left alone.
func asDecided(rules airlock.Rules) airlock.Rules { return rules }

// TestAJudgedGrantLivesByItsRulesEntryAndNotByAnOwner: the judge approves an owned human-tier
// secret in its owner's stead, so the grant is decided by no login any owner rule admits. Once
// the policy version moves, every release re-checks what it was granted, and what is asked of
// such a grant is the rules entry its approval rested on: the judge the entry gives this name
// must still cover the lifetime the grant holds. So a policy move that leaves the entry alone
// goes on releasing it, and a file that takes the judge off the name, lowers it below the grant's
// lifetime, or drops the name altogether ends it. The owner's own grant of the same secret is
// decided by a person and is held to the person's rule throughout: none of this moves it.
func TestAJudgedGrantLivesByItsRulesEntryAndNotByAnOwner(t *testing.T) {
	for name, c := range map[string]struct {
		rules func(airlock.Rules) airlock.Rules
		live  bool
	}{
		"the entry is the one the grant was decided under":  {rules: asDecided, live: true},
		"the entry's judge is exactly the grant's lifetime": {rules: withJudge("DEEL_API_KEY", new(uint32(600))), live: true},
		"the entry's judge is below the grant's lifetime":   {rules: withJudge("DEEL_API_KEY", new(uint32(599)))},
		"the entry has no judge at all":                     {rules: withJudge("DEEL_API_KEY", nil)},
		"the rules no longer name the secret":               {rules: withoutEntry("DEEL_API_KEY")},
	} {
		t.Run(name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")

			// Another session of the owner's holds a grant of the same secret that the owner
			// approved themselves, which every assertion below answers for too.
			own, ownKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			pending := pendingJudgeable(t, m, own, ownKey, "the release job wants it on the other box", 600, "DEEL_API_KEY")
			handOver(t, m, *pending.RecordID)
			byOwner, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
			if err != nil || byOwner.GrantID == "" {
				t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
			}

			// A secret neither grant holds is retagged, which moves the policy version and so
			// makes both releases re-check every name they were granted. DEEL_API_KEY's own
			// tags, owner and tier are untouched.
			before := m.Policy.Get().Version
			retag(t, m, policytest.Secret("UNRELATED_KEY", otherPerson, policy.TierAgent, "unrelated-v1"))
			if m.Policy.Get().Version == before {
				t.Fatalf("the policy version stood still at %s, so the re-checks this test is about never ran", before)
			}
			// The rules the release reads. The chain verifier keeps the bound the fixture built
			// it with, so what answers below is the release's own re-check of the entry and not
			// the chain's re-verification of the approval; a pod that booted on the rewritten
			// file refuses such a grant at both.
			m.Airlock = c.rules(m.Airlock)

			release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{})
			switch {
			case c.live && (err != nil || releasedValue(release, "DEEL_API_KEY") == ""):
				t.Fatalf("Values(the judge's grant) = %+v, %v; want DEEL_API_KEY released", release, err)
			case !c.live && !errors.Is(err, ErrGrantNotLive):
				t.Fatalf("Values(the judge's grant) = %+v, %v; want ErrGrantNotLive", release, err)
			}
			if release, err := m.Values(t.Context(), byOwner.GrantID, own, nil, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
				t.Fatalf("Values(the owner's own grant) = %+v, %v; want DEEL_API_KEY released whatever the rules now say of the judge", release, err)
			}
		})
	}
}

// bootedJudgeFixture is newJudgeFixture wired as cmd/broker wires its one rules file: the policy
// loader fingerprints each entry of the rules the broker booted on (policy.Loader.AirlockDigest,
// whose fingerprints are in the policy version), and the chain verifier holds a judge's approval
// to that same file. boot is a pod booting on rules, which the loader, m.Airlock and the chain
// all read from then on.
func bootedJudgeFixture(t *testing.T) (m *Machine, session string, key *ecdsa.PrivateKey, boot func(airlock.Rules)) {
	t.Helper()
	m, session, key = newJudgeFixture(t)
	booted := m.Airlock
	loader := policytest.Loader(fixtureStore(m), fixtureService)
	loader.AirlockDigest = func(name string) string { return booted.Digest(name) }
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	m.Policy = current
	return m, session, key, func(rules airlock.Rules) {
		t.Helper()
		booted, m.Airlock = rules, rules
		m.Chain = NewChainVerifier(m.Store, testAudience, time.Minute, rules)
		if err := m.Policy.Refresh(t.Context()); err != nil {
			t.Fatalf("policy refresh: %v", err)
		}
	}
}

// TestARulesEditUnderAJudgedGrantEndsItForGood: an edit to the rules entry of the key the judge
// decided — its judge taken off, lowered below the grant's lifetime, or the entry dropped — ends
// a judged grant for good, as every edit to an entry ends the grants that entry affects, at the
// release and at the next request that would reuse it. The grant also holds AUTO_TOKEN, which its
// session got at once. The pod boots on the edited file, so its chain, which holds a judge's
// approval to the rules in force, refuses the grant as well, and so does stillAllowed's judge arm;
// either refusal alone would leave the grant live, and booting the decided file again would
// release it. The chain's own line (GrantChainInvalidMessage) is still written beside the end,
// once, so an operator who reads those lines finds this grant there as before. No request reuses
// the grant whose judge was lowered: a request for its 600 seconds is refused when it is written,
// the judge now granting at most 599.
func TestARulesEditUnderAJudgedGrantEndsItForGood(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN"}
	for _, c := range []struct {
		name  string
		edit  func(airlock.Rules) airlock.Rules
		reuse bool
	}{
		{name: "the entry has no judge at all", edit: withJudge("DEEL_API_KEY", nil)},
		{name: "the entry has no judge at all", edit: withJudge("DEEL_API_KEY", nil), reuse: true},
		{name: "the entry's judge is below the grant's lifetime", edit: withJudge("DEEL_API_KEY", new(uint32(599)))},
		{name: "the rules no longer name the secret", edit: withoutEntry("DEEL_API_KEY")},
		{name: "the rules no longer name the secret", edit: withoutEntry("DEEL_API_KEY"), reuse: true},
	} {
		path := map[bool]string{false: "the release", true: "the next request"}[c.reuse]
		at := map[bool]string{false: "values", true: "reuse"}[c.reuse]
		t.Run(c.name+", at "+path, func(t *testing.T) {
			m, session, key, boot := bootedJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, names...)
			decided := m.Airlock
			boot(c.edit(decided))
			capture := captureRecords(t)

			if c.reuse {
				again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", 600, names...), "", nil)
				if err != nil || again.GrantID != nil || again.State != "pending" {
					t.Fatalf("Create under the edited rules = %+v, %v; want no grant handed back and a new pending request", again, err)
				}
			} else if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "" {
				t.Fatalf("Values under the edited rules = %+v, %v; want ErrGrantNotLive and nothing released", release, err)
			}
			if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
				t.Fatalf("after %s under the edited rules: revoked_by %q with %d grant.revoked rows by %s; want the grant ended once for good", path, by, audits, GrantEndedActor)
			}
			lines := chainLines(t, capture)
			if len(lines) != 1 || lines[0]["grant"] != judged || lines[0]["at"] != at {
				t.Fatalf("the %q lines after %s are %v; want one naming grant %s at=%s", GrantChainInvalidMessage, path, lines, judged, at)
			}
			if reason, _ := lines[0]["reason"].(string); !strings.HasPrefix(reason, ErrGrantChainInvalid.Error()) {
				t.Fatalf("the line's reason is %q; want ErrGrantChainInvalid's text", reason)
			}

			boot(decided)
			if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "" {
				t.Fatalf("Values once the decided rules are booted again = %+v, %v; want the grant still ended", release, err)
			}
		})
	}
}

// TestAnAdministratorsTagEndsAJudgedGrantAndLeavesAPersonsAlone: a person approves every release
// of a secret its owner tags as an administrator's (policy.TagAdmin). The model judge may have
// decided a grant of it before the tag was written — the rules file gave the name judge seconds,
// and the judge read a secret that was not yet an administrator's — and the tag moves the policy
// version, so every release of that grant and every request for exactly its names re-reads what
// it was granted (checkRelease, and Create's check of the live grants, endMatchingLiveGrants).
// Both refuse it and end the grant for good, rather than let a model's approval go on releasing
// an administrator's credential for the rest of the seconds the rules gave it. Taking the tag
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
		pending := pendingJudgeable(t, m, own, ownKey, "the release job wants it on the other box", 600, "DEEL_API_KEY")
		handOver(t, m, *pending.RecordID)
		byOwner, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
		if err != nil || byOwner.GrantID == "" {
			t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
		}

		before := m.Policy.Get().Version
		retag(t, m, adminSecret("DEEL_API_KEY", "deel-v1"))
		if m.Policy.Get().Version == before {
			t.Fatalf("the policy version stood still at %s, so the re-checks this test is about never ran", before)
		}

		release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{})
		if !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" {
			t.Fatalf("Values(the judge's grant of what is now an administrator's secret) = %+v, %v; want ErrGrantNotLive and nothing released", release, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the judged grant's revoked_by = %q with %d grant.revoked rows by %s; want it ended once for good", by, audits, GrantEndedActor)
		}
		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" {
			t.Fatalf("a second Values of the judged grant = %+v, %v; want it refused as ended, not merely passed over once", release, err)
		}
		if release, err := m.Values(t.Context(), byOwner.GrantID, own, nil, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" {
			t.Fatalf("Values(the owner's own grant) = %+v, %v; want DEEL_API_KEY released: a person approved it, which is all the tag asks", release, err)
		}

		// The tag taken off again does not bring the judged grant back.
		retag(t, m, policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"))
		if _, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values of the judged grant once the tag is gone = %v; want it still ended", err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("once the tag is gone: revoked_by %q with %d grant.revoked rows; want the one end", by, audits)
		}
	})

	t.Run("the next request ends it rather than handing it back", func(t *testing.T) {
		m, session, key := newJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, "DEEL_API_KEY")
		reused, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "the release job wants it again", 600, "DEEL_API_KEY"), "", nil)
		if err != nil || reused.GrantID == nil || *reused.GrantID != judged {
			t.Fatalf("a request for the same name and lifetime while the secret is an ordinary one = %+v, %v; want the judged grant %s handed back", reused, err, judged)
		}

		retag(t, m, adminSecret("DEEL_API_KEY", "deel-v1"))
		again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "the release job wants it once more", 600, "DEEL_API_KEY"), "", nil)
		if err != nil || again.GrantID != nil || again.State != "pending" {
			t.Fatalf("Create once the secret is an administrator's = %+v, %v; want no grant handed back and a card its person answers", again, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the grant the request found = revoked_by %q with %d grant.revoked rows by %s; want the request to have ended it once", by, audits, GrantEndedActor)
		}
	})
}

// TestAnAdministratorsTagOnAKeyGrantedAtOnceEndsTheJudgedGrantBesideIt: a judged grant may hold,
// beside the key the judge decided, a key its session got at once, which needed no decision
// (TestTheJudgesGrantBesideAKeyGrantedAtOnceReleasesBoth). When that key's owner tags it as an
// administrator's, the grant holds an administrator's secret that no person approved, exactly as
// when the judged key itself is tagged: the release and the next request that would reuse the
// grant both refuse it and end it for good, and taking the tag off again does not bring it back.
// A grant of the same two keys that the owner approved is not ended: its key granted at once now
// wants an approval nobody gave, so it releases nothing while the tag stands, and it releases
// again once the tag is off.
func TestAnAdministratorsTagOnAKeyGrantedAtOnceEndsTheJudgedGrantBesideIt(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN"}
	released := func(release Release) bool {
		return releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != ""
	}
	untag := func(t *testing.T, m *Machine) {
		t.Helper()
		retag(t, m, policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"))
	}

	t.Run("a release refuses it and ends it for good", func(t *testing.T) {
		m, session, key := newJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, names...)
		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" || releasedValue(release, "AUTO_TOKEN") == "" {
			t.Fatalf("Values(the judge's grant) before any tag = %+v, %v; want both keys released", release, err)
		}

		// Another session of the owner's holds a grant of the same two keys that the owner
		// approved themselves.
		own, ownKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
		pending := pendingJudgeable(t, m, own, ownKey, "post the release notes from the other box", 600, names...)
		handOver(t, m, *pending.RecordID)
		byOwner, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
		if err != nil || byOwner.GrantID == "" {
			t.Fatalf("ApplyDecision by the owner = %+v, %v; want a grant", byOwner, err)
		}

		before := m.Policy.Get().Version
		retag(t, m, adminSecret("AUTO_TOKEN", "auto-v1"))
		if m.Policy.Get().Version == before {
			t.Fatalf("the policy version stood still at %s, so the re-checks this test is about never ran", before)
		}

		release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{})
		if !errors.Is(err, ErrGrantNotLive) || released(release) {
			t.Fatalf("Values(the judge's grant of a key now an administrator's) = %+v, %v; want ErrGrantNotLive and nothing released", release, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the judged grant's revoked_by = %q with %d grant.revoked rows by %s; want it ended once for good", by, audits, GrantEndedActor)
		}
		if release, err := m.Values(t.Context(), byOwner.GrantID, own, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
			t.Fatalf("Values(the owner's grant) while AUTO_TOKEN is an administrator's = %+v, %v; want ErrGrantNotLive: nobody approved AUTO_TOKEN", release, err)
		}
		if by, audits := endedForGood(t, m, byOwner.GrantID); by != "" || audits != 0 {
			t.Fatalf("the owner's grant: revoked_by %q with %d grant.revoked rows by %s; want it not ended", by, audits, GrantEndedActor)
		}

		// The tag taken off again does not bring the judged grant back, and the owner's grant
		// releases both keys again.
		untag(t, m)
		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
			t.Fatalf("Values of the judged grant once the tag is gone = %+v, %v; want it still ended", release, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("once the tag is gone: revoked_by %q with %d grant.revoked rows; want the one end", by, audits)
		}
		if release, err := m.Values(t.Context(), byOwner.GrantID, own, nil, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" || releasedValue(release, "AUTO_TOKEN") == "" {
			t.Fatalf("Values(the owner's grant) once the tag is gone = %+v, %v; want both keys released", release, err)
		}
	})

	t.Run("the next request ends it rather than handing it back", func(t *testing.T) {
		m, session, key := newJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, names...)
		reused, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", 600, names...), "", nil)
		if err != nil || reused.GrantID == nil || *reused.GrantID != judged {
			t.Fatalf("a request for the same names and lifetime while AUTO_TOKEN is an ordinary key = %+v, %v; want the judged grant %s handed back", reused, err, judged)
		}

		retag(t, m, adminSecret("AUTO_TOKEN", "auto-v1"))
		again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes once more", 600, names...), "", nil)
		if err != nil || again.GrantID != nil || again.State != "pending" {
			t.Fatalf("Create once AUTO_TOKEN is an administrator's = %+v, %v; want no grant handed back and a card its person answers", again, err)
		}
		if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
			t.Fatalf("the grant the request found = revoked_by %q with %d grant.revoked rows by %s; want the request to have ended it once", by, audits, GrantEndedActor)
		}

		untag(t, m)
		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
			t.Fatalf("Values of the judged grant once the tag is gone = %+v, %v; want it still ended", release, err)
		}
	})
}

// TestAnEndOnOneKeyOfAJudgedGrantIsNotHiddenByAnotherKeysPause: a judged grant of DEEL_API_KEY,
// which the judge decided, and of AUTO_TOKEN and ACME_TOKEN, which its session got at once.
// ACME_TOKEN sorts first. Its owner first changes it: most changes only pause the grant (it wants
// an approval, or the policy no longer serves it through a tag the loader refuses or a value that
// is gone); one lowers its max-grant to 10 minutes, which leaves the 600-second grant releasing
// but refuses a next request that asks for 15 minutes, after Create's check of the live grants.
// Then a later key comes to end the grant for good: DEEL_API_KEY or AUTO_TOKEN tagged as an
// administrator's, DEEL_API_KEY's requesters list dropping the session's person, DEEL_API_KEY
// served from another source, or DEEL_API_KEY's max-grant lowered below the grant's 600 seconds;
// or ACME_TOKEN's own max-grant is lowered to 5 minutes while it still wants an approval. The
// release, or the next request for the grant's names, must end it whatever ACME_TOKEN answers,
// and once every change is taken back the grant still releases nothing. In each case one change
// alone ends the grant: the 10-minute cap ends nothing, so its case needs the administrator tag's
// end, and ACME_TOKEN's own 5-minute cap is the only end of its cases.
func TestAnEndOnOneKeyOfAJudgedGrantIsNotHiddenByAnotherKeysPause(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN", "ACME_TOKEN"}
	released := func(release Release) bool {
		return slices.ContainsFunc(names, func(name string) bool { return releasedValue(release, name) != "" })
	}
	capped := func(s secrets.LocalSecret, limit string) secrets.LocalSecret {
		s.Tags[policy.TagMaxGrant] = limit
		return s
	}
	// A setup change pauses the grant's release or, with asks set, leaves the grant releasing and
	// only caps the lifetime the next request may ask for: that request asks for asks seconds, over
	// the cap, so Create's own max-grant refusal comes after its check of the live grants.
	type setupChange struct {
		apply func(*testing.T, *Machine)
		asks  int
	}
	setups := map[string]setupChange{
		"ACME_TOKEN wants an approval": {apply: func(t *testing.T, m *Machine) {
			retag(t, m, policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierHuman, "acme-v1"))
		}},
		"ACME_TOKEN wants another person's approval": {apply: func(t *testing.T, m *Machine) {
			retag(t, m, policytest.Secret("ACME_TOKEN", otherPerson, policy.TierHuman, "acme-v1"))
		}},
		"ACME_TOKEN is not served under a tag the loader refuses": {apply: func(t *testing.T, m *Machine) {
			refused := policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1")
			refused.Tags[policy.TagAdmin] = "yes"
			retag(t, m, refused)
		}},
		"ACME_TOKEN is not served, its value gone": {apply: func(t *testing.T, m *Machine) {
			fixtureStore(m).Delete(policytest.ID("ACME_TOKEN"))
			retag(t, m)
		}},
		"ACME_TOKEN allows at most 10 minutes, and the request asks for 15": {apply: func(t *testing.T, m *Machine) {
			retag(t, m, capped(policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1"), "10m"))
		}, asks: 900},
	}
	ends := map[string]struct {
		name   string
		secret func() secrets.LocalSecret
	}{
		"DEEL_API_KEY is tagged as an administrator's": {"DEEL_API_KEY", func() secrets.LocalSecret { return adminSecret("DEEL_API_KEY", "deel-v1") }},
		"AUTO_TOKEN is tagged as an administrator's":   {"AUTO_TOKEN", func() secrets.LocalSecret { return adminSecret("AUTO_TOKEN", "auto-v1") }},
		"DEEL_API_KEY no longer lists the person": {"DEEL_API_KEY", func() secrets.LocalSecret {
			return policytest.ListedSecret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, otherPerson, "deel-v1")
		}},
		"DEEL_API_KEY is served from another source": {"DEEL_API_KEY", func() secrets.LocalSecret {
			return policytest.TypeSecret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin))
		}},
		"DEEL_API_KEY allows at most 5 minutes": {"DEEL_API_KEY", func() secrets.LocalSecret {
			return capped(policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"), "5m")
		}},
		"ACME_TOKEN itself allows at most 5 minutes": {"ACME_TOKEN", func() secrets.LocalSecret {
			return capped(policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierHuman, "acme-v1"), "5m")
		}},
	}
	type combination struct {
		setup, end string
		reuse      bool
	}
	var combinations []combination
	for _, end := range []string{"DEEL_API_KEY is tagged as an administrator's", "AUTO_TOKEN is tagged as an administrator's", "DEEL_API_KEY no longer lists the person", "DEEL_API_KEY is served from another source"} {
		for _, reuse := range []bool{false, true} {
			combinations = append(combinations, combination{"ACME_TOKEN wants an approval", end, reuse})
		}
	}
	combinations = append(combinations,
		combination{"ACME_TOKEN wants an approval", "DEEL_API_KEY allows at most 5 minutes", false},
		combination{"ACME_TOKEN wants an approval", "ACME_TOKEN itself allows at most 5 minutes", false},
		combination{"ACME_TOKEN wants an approval", "ACME_TOKEN itself allows at most 5 minutes", true},
		combination{"ACME_TOKEN is not served under a tag the loader refuses", "DEEL_API_KEY is tagged as an administrator's", false},
		combination{"ACME_TOKEN is not served under a tag the loader refuses", "DEEL_API_KEY is tagged as an administrator's", true},
		combination{"ACME_TOKEN is not served, its value gone", "DEEL_API_KEY is tagged as an administrator's", false},
		combination{"ACME_TOKEN is not served, its value gone", "DEEL_API_KEY is tagged as an administrator's", true},
		combination{"ACME_TOKEN is not served, its value gone", "AUTO_TOKEN is tagged as an administrator's", true},
		combination{"ACME_TOKEN is not served under a tag the loader refuses", "AUTO_TOKEN is tagged as an administrator's", true},
		combination{"ACME_TOKEN allows at most 10 minutes, and the request asks for 15", "AUTO_TOKEN is tagged as an administrator's", true},
		combination{"ACME_TOKEN wants another person's approval", "AUTO_TOKEN is tagged as an administrator's", true},
		combination{"ACME_TOKEN is not served, its value gone", "DEEL_API_KEY no longer lists the person", true},
		combination{"ACME_TOKEN is not served, its value gone", "DEEL_API_KEY is served from another source", true},
	)
	for _, c := range combinations {
		path := map[bool]string{false: "the release", true: "the next request"}[c.reuse]
		t.Run(c.setup+", then "+c.end+", at "+path, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			retag(t, m, policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1"))
			judged := judgedGrant(t, m, session, key, 600, names...)
			if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); err != nil || slices.ContainsFunc(names, func(name string) bool { return releasedValue(release, name) == "" }) {
				t.Fatalf("Values before any change = %+v, %v; want all three keys released", release, err)
			}

			// The setup's own effect is data, not its label: most changes only pause the release,
			// while a cap the grant is inside leaves it releasing and refuses only the next request.
			setup := setups[c.setup]
			setup.apply(t, m)
			if setup.asks == 0 {
				if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
					t.Fatalf("Values while %s = %+v, %v; want ErrGrantNotLive and nothing released", c.setup, release, err)
				}
				if by, audits := endedForGood(t, m, judged); by != "" || audits != 0 {
					t.Fatalf("after the pause alone: revoked_by %q with %d grant.revoked rows; want the grant paused, not ended", by, audits)
				}
			} else if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); err != nil || slices.ContainsFunc(names, func(name string) bool { return releasedValue(release, name) == "" }) {
				t.Fatalf("Values while %s = %+v, %v; want all three keys released: the grant is inside the cap", c.setup, release, err)
			}

			// The end, while ACME_TOKEN still pauses the grant or caps the next request.
			end := ends[c.end]
			retag(t, m, end.secret())
			if _, served := m.Policy.Get().Secrets[end.name]; !served {
				t.Fatalf("the policy does not serve %s once %s; the test sets up nothing", end.name, c.end)
			}
			if c.reuse {
				asks := 600
				if setup.asks != 0 {
					asks = setup.asks
				}
				again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", asks, names...), "", nil)
				if again.GrantID != nil {
					t.Fatalf("Create once %s = %+v, %v; want no grant handed back", c.end, again, err)
				}
				if setup.asks != 0 && !errors.Is(err, ErrGrantRequiresLifetime) {
					t.Fatalf("Create for %d seconds once %s = %+v, %v; want the max-grant refusal, which comes after the end", asks, c.end, again, err)
				}
			} else if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
				t.Fatalf("Values once %s = %+v, %v; want ErrGrantNotLive and nothing released", c.end, release, err)
			}
			if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
				t.Errorf("after %s once %s while %s: revoked_by %q with %d grant.revoked rows by %s; want the grant ended once for good", path, c.end, c.setup, by, audits, GrantEndedActor)
			}

			// Every change taken back.
			retag(t, m,
				policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"),
				policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"),
				policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1"))
			if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || released(release) {
				t.Errorf("Values once every change is taken back = DEEL_API_KEY %q, AUTO_TOKEN %q, ACME_TOKEN %q, %v; want ErrGrantNotLive and nothing released", releasedValue(release, "DEEL_API_KEY"), releasedValue(release, "AUTO_TOKEN"), releasedValue(release, "ACME_TOKEN"), err)
			}
		})
	}
}

// TestAChainFailureDoesNotHideAnotherKeysEnd: an unserved key's changed airlock entry makes the
// judged grant's approval chain fail at the release. The release still writes that chain's
// warning, but an administrator tag or a requester drop ends the grant before the chain refusal
// can leave it live.
func TestAChainFailureDoesNotHideAnotherKeysEnd(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN"}
	for _, c := range []struct {
		name string
		edit func(airlock.Rules) airlock.Rules
		end  func() secrets.LocalSecret
	}{
		{"an entry is dropped beside an administrator tag", withoutEntry("DEEL_API_KEY"), func() secrets.LocalSecret { return adminSecret("AUTO_TOKEN", "auto-v1") }},
		{"a judge is removed beside an administrator tag", withJudge("DEEL_API_KEY", nil), func() secrets.LocalSecret { return adminSecret("AUTO_TOKEN", "auto-v1") }},
		{"a judge is lowered beside an administrator tag", withJudge("DEEL_API_KEY", new(uint32(599))), func() secrets.LocalSecret { return adminSecret("AUTO_TOKEN", "auto-v1") }},
		{"an entry is dropped beside a requester drop", withoutEntry("DEEL_API_KEY"), func() secrets.LocalSecret {
			return policytest.ListedSecret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, otherPerson, "auto-v1")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, session, key, boot := bootedJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, names...)
			fixtureStore(m).Delete(policytest.ID("DEEL_API_KEY"))
			decided := m.Airlock
			boot(c.edit(decided))
			retag(t, m, c.end())
			capture := captureRecords(t)

			if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "" {
				t.Fatalf("Values after %s = %+v, %v; want the grant ended without a release", c.name, release, err)
			}
			if by, audits := endedForGood(t, m, judged); by != GrantEndedActor || audits != 1 {
				t.Fatalf("after %s: revoked_by %q with %d grant.revoked rows; want one broker end", c.name, by, audits)
			}
			if lines := chainLines(t, capture); len(lines) != 1 {
				t.Fatalf("after %s: %d failed-chain lines; want the one warning beside the end", c.name, len(lines))
			}

			boot(decided)
			retag(t, m,
				policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"),
				policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"))
			if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "" {
				t.Fatalf("Values after every change is restored = %+v, %v; want the ended grant to stay unavailable", release, err)
			}
		})
	}

	t.Run("the failed chain alone remains live and writes its warning", func(t *testing.T) {
		m, session, key, boot := bootedJudgeFixture(t)
		judged := judgedGrant(t, m, session, key, 600, names...)
		fixtureStore(m).Delete(policytest.ID("DEEL_API_KEY"))
		decided := m.Airlock
		boot(withoutEntry("DEEL_API_KEY")(decided))
		capture := captureRecords(t)

		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); !errors.Is(err, ErrGrantChainInvalid) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "" {
			t.Fatalf("Values of the chain-invalid grant with no ending policy change = %+v, %v; want only its chain refusal", release, err)
		}
		if by, audits := endedForGood(t, m, judged); by != "" || audits != 0 {
			t.Fatalf("the chain-invalid grant alone: revoked_by %q with %d grant.revoked rows; want it live", by, audits)
		}
		if lines := chainLines(t, capture); len(lines) != 1 {
			t.Fatalf("the chain-invalid grant alone: %d failed-chain lines; want one", len(lines))
		}

		boot(decided)
		retag(t, m, policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"))
		if release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{}); err != nil || releasedValue(release, "DEEL_API_KEY") == "" || releasedValue(release, "AUTO_TOKEN") == "" {
			t.Fatalf("Values after the chain becomes valid = %+v, %v; want the still-live grant released", release, err)
		}
	})
}

// TestAPendingValueDoesNotHideAnEndAtTheNextRequest: Create still reports a key waiting for
// its value, but must end a matching live grant first when another key now requires approval.
func TestAPendingValueDoesNotHideAnEndAtTheNextRequest(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN"}
	for _, c := range []struct {
		name string
		end  bool
	}{
		{"an administrator tag ends the grant", true},
		{"a pending value alone leaves the grant live", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			judged := judgedGrant(t, m, session, key, 600, names...)
			waiting := policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "")
			waiting.Tags[secrets.CreateLabel] = "0123456789abcdef0123456789abcdef"
			created := time.Now()
			waiting.CreatedAt = &created
			fixtureStore(m).Delete(policytest.ID("AUTO_TOKEN"))
			if c.end {
				retag(t, m, waiting, adminSecret("DEEL_API_KEY", "deel-v1"))
			} else {
				retag(t, m, waiting)
			}
			if _, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", 600, names...), "", nil); !errors.Is(err, policy.ErrPendingValue) {
				t.Fatalf("Create while AUTO_TOKEN waits for its value = %v; want the pending-value refusal", err)
			}
			if by, audits := endedForGood(t, m, judged); c.end && (by != GrantEndedActor || audits != 1) {
				t.Fatalf("the pending-value request with an administrator tag: revoked_by %q with %d grant.revoked rows; want one broker end", by, audits)
			} else if !c.end && (by != "" || audits != 0) {
				t.Fatalf("the pending-value request with no ending change: revoked_by %q with %d grant.revoked rows; want it live", by, audits)
			}

			retag(t, m,
				policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"),
				policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"))
			release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{})
			if c.end && (!errors.Is(err, ErrGrantNotLive) || releasedValue(release, "DEEL_API_KEY") != "" || releasedValue(release, "AUTO_TOKEN") != "") {
				t.Fatalf("Values after the pending-value end is restored = %+v, %v; want the grant permanently ended", release, err)
			}
			if !c.end && (err != nil || releasedValue(release, "DEEL_API_KEY") == "" || releasedValue(release, "AUTO_TOKEN") == "") {
				t.Fatalf("Values after the pending-value control is restored = %+v, %v; want the live grant released", release, err)
			}
		})
	}
}

// TestCreateRechecksEachLiveGrantUnderItsOwnLifetime: Create checks every live grant for exactly
// the requested names before its own refusals, but it checks each one under the lifetime that
// grant's request signed, not the new request's. So a request that cannot reuse the grant still
// ends it for an administrator tag or a cap the grant is over, and a request over a cap does not
// end a grant that is still inside that cap.
func TestCreateRechecksEachLiveGrantUnderItsOwnLifetime(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN", "ACME_TOKEN"}
	released := func(release Release) bool {
		return slices.ContainsFunc(names, func(name string) bool { return releasedValue(release, name) != "" })
	}
	acmeUnserved := func(t *testing.T, m *Machine) {
		fixtureStore(m).Delete(policytest.ID("ACME_TOKEN"))
		retag(t, m)
	}
	capped := func(name, value, limit string) func(*testing.T, *Machine) {
		return func(t *testing.T, m *Machine) {
			s := policytest.Secret(name, fixtureOperator, policy.TierAgent, value)
			s.Tags[policy.TagMaxGrant] = limit
			retag(t, m, s)
		}
	}
	for _, c := range []struct {
		name          string
		grantSeconds  int
		askedSeconds  int
		change        func(*testing.T, *Machine)
		adminTag      bool
		createRefuses bool
		endsGrant     bool
	}{
		{"an unserved key refuses a shorter request and leaves the grant live", 600, 300, acmeUnserved, false, true, false},
		{"an unserved key refuses a shorter request but an administrator tag ends the grant", 600, 300, acmeUnserved, true, true, true},
		{"a longer request over a cap leaves a grant inside it live", 600, 900, capped("ACME_TOKEN", "acme-v1", "10m"), false, true, false},
		{"a longer request over a cap still ends a grant an administrator tag ends", 600, 900, capped("ACME_TOKEN", "acme-v1", "10m"), true, true, true},
		{"a shorter request ends a grant over a lowered cap", 600, 300, capped("AUTO_TOKEN", "auto-v1", "5m"), false, false, true},
		{"a longer request over a cap leaves a shorter grant inside it live", 300, 900, capped("AUTO_TOKEN", "auto-v1", "10m"), false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, session, key := newJudgeFixture(t)
			retag(t, m, policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1"))
			judged := judgedGrant(t, m, session, key, c.grantSeconds, names...)
			c.change(t, m)
			if c.adminTag {
				retag(t, m, adminSecret("AUTO_TOKEN", "auto-v1"))
			}

			again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", c.askedSeconds, names...), "", nil)
			if c.createRefuses && err == nil {
				t.Fatalf("Create(%d seconds) = %+v; want its own refusal before any reuse", c.askedSeconds, again)
			}
			if !c.createRefuses && err != nil {
				t.Fatalf("Create(%d seconds) = %v; want no refusal of its own", c.askedSeconds, err)
			}
			by, audits := endedForGood(t, m, judged)
			if c.endsGrant && (by != GrantEndedActor || audits != 1) {
				t.Fatalf("the %d-second grant after a %d-second request: revoked_by %q with %d grant.revoked rows; want one broker end", c.grantSeconds, c.askedSeconds, by, audits)
			}
			if !c.endsGrant && (by != "" || audits != 0) {
				t.Fatalf("the %d-second grant after a %d-second request: revoked_by %q with %d grant.revoked rows; want it live", c.grantSeconds, c.askedSeconds, by, audits)
			}

			retag(t, m,
				policytest.Secret("DEEL_API_KEY", fixtureOperator, policy.TierHuman, "deel-v1"),
				policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"),
				policytest.Secret("ACME_TOKEN", fixtureOperator, policy.TierAgent, "acme-v1"))
			release, err := m.Values(t.Context(), judged, session, nil, ReleaseCall{})
			if c.endsGrant && (!errors.Is(err, ErrGrantNotLive) || released(release)) {
				t.Fatalf("Values once every change is taken back = %+v, %v; want the grant still ended", release, err)
			}
			if !c.endsGrant && (err != nil || !released(release)) {
				t.Fatalf("Values once every change is taken back = %+v, %v; want the live grant released", release, err)
			}
		})
	}
}

// TestCreateRechecksEveryLiveGrantForTheNames: two live grants of the same names, the older over
// AUTO_TOKEN's lowered cap and the newer inside it. A request for the newer grant's lifetime must
// end the older grant before it hands the newer one back.
func TestCreateRechecksEveryLiveGrantForTheNames(t *testing.T) {
	names := []string{"DEEL_API_KEY", "AUTO_TOKEN"}
	m, session, key := newJudgeFixture(t)
	older := judgedGrant(t, m, session, key, 600, names...)
	newer := judgedGrant(t, m, session, key, 300, names...)
	if older == newer {
		t.Fatalf("setup made one grant %s; want two", older)
	}
	capped := policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1")
	capped.Tags[policy.TagMaxGrant] = "5m"
	retag(t, m, capped)

	again, err := m.Create(t.Context(), session, signJudgeable(t, m, key, "post the release notes again", 300, names...), "", nil)
	if err != nil || again.GrantID == nil || *again.GrantID != newer {
		t.Fatalf("Create(300 seconds) = %+v, %v; want the newer grant %s handed back", again, err, newer)
	}
	if by, audits := endedForGood(t, m, older); by != GrantEndedActor || audits != 1 {
		t.Fatalf("the older grant over the cap: revoked_by %q with %d grant.revoked rows; want one broker end", by, audits)
	}
	if by, audits := endedForGood(t, m, newer); by != "" || audits != 0 {
		t.Fatalf("the newer grant inside the cap: revoked_by %q with %d grant.revoked rows; want it live", by, audits)
	}
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
