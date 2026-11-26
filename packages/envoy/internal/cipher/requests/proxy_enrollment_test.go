package requests

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestProxyGrantsOfEnrollmentAnswersEveryLiveGrantOfTheEnrollment: the proxy reads the live grants
// of a connection's enrollment that hold a name bound to the host, so a host any of them binds is
// read and a placeholder of any of them is swapped on one connection. Each grant is answered as a
// connection opened with one of its own placeholders would answer it (AirlockConnection), in grant
// id order. The controls: a grant of another enrollment, a grant that has been revoked, a grant
// that holds none of the names asked about, and a grant whose every name the session's operator
// withheld are not answered, and an enrollment with no grant, or a read naming no name, answers
// none.
func TestProxyGrantsOfEnrollmentAnswersEveryLiveGrantOfTheEnrollment(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example"), rule("SECOND_TOKEN", "example")))
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	_, first := grantOf(t, m, enr, key, "read the first for ACME-7", "AUTO_TOKEN")
	_, second := grantOf(t, m, enr, key, "read the second", "SECOND_TOKEN")
	other, otherKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	_, foreign := grantOf(t, m, other, otherKey, "read it on another box", "AUTO_TOKEN")
	passwords := map[string]string{}
	for grantID, enrollmentID := range map[string]string{first: enr, second: enr, foreign: other} {
		release, err := m.Values(t.Context(), grantID, enrollmentID, ReleaseCall{})
		if err != nil {
			t.Fatalf("Values(%s): %v", grantID, err)
		}
		passwords[grantID] = release.Airlock.ProxyPassword
	}

	both := []string{"AUTO_TOKEN", "SECOND_TOKEN"}
	live, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both)
	if err != nil {
		t.Fatalf("ProxyGrantsOfEnrollment: %v", err)
	}
	want := slices.Sorted(slices.Values([]string{first, second}))
	if got := grantIDs(live); !reflect.DeepEqual(got, want) {
		t.Fatalf("the enrollment's live grants = %v; want %v, in grant id order, and not %s of another enrollment", got, want, foreign)
	}
	for _, grant := range live {
		opened, err := m.AirlockConnection(t.Context(), passwords[grant.GrantID])
		if err != nil {
			t.Fatalf("AirlockConnection(%s's placeholder): %v", grant.GrantID, err)
		}
		if !reflect.DeepEqual(grant, opened) {
			t.Errorf("grant %s = %+v; want what a connection opened with its placeholder answers, %+v", grant.GrantID, grant, opened)
		}
	}
	if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", []string{"SECOND_TOKEN"}); err != nil || !reflect.DeepEqual(grantIDs(got), []string{second}) {
		t.Fatalf("the enrollment's live grants holding SECOND_TOKEN = %v, %v; want %s alone: %s holds no name asked about", grantIDs(got), err, second, first)
	}
	if none, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", nil); err != nil || len(none) != 0 {
		t.Fatalf("a read naming no name answered %v, %v; want none", grantIDs(none), err)
	}

	if err := m.RevokeGrant(t.Context(), second, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	live, err = m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both)
	if err != nil {
		t.Fatalf("ProxyGrantsOfEnrollment after a revoke: %v", err)
	}
	if got := grantIDs(live); !reflect.DeepEqual(got, []string{first}) {
		t.Fatalf("the enrollment's live grants after %s was revoked = %v; want %s alone", second, got, first)
	}

	// Written directly, as TestAirlockConnectionDropsAWithheldName writes it: what is read is
	// withheld_secrets, and the grant is one the session got without asking.
	if _, err := m.Store.Pool.Exec(t.Context(), `insert into withheld_secrets (enrollment_id, name) values ($1,$2)`, enr, "AUTO_TOKEN"); err != nil {
		t.Fatal(err)
	}
	live, err = m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both)
	if err != nil {
		t.Fatalf("ProxyGrantsOfEnrollment with AUTO_TOKEN withheld: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("the enrollment's live grants with every name withheld = %v; want none", grantIDs(live))
	}
	if still, err := m.ProxyGrantsOfEnrollment(t.Context(), other, "", both); err != nil || !reflect.DeepEqual(grantIDs(still), []string{foreign}) {
		t.Fatalf("the other enrollment's live grants = %v, %v; want %s, which no withhold of this enrollment reaches", grantIDs(still), err, foreign)
	}
	if others, err := m.ProxyGrantsOfEnrollment(t.Context(), other, foreign, both); err != nil || len(others) != 0 {
		t.Fatalf("the other enrollment's live grants but %s = %v, %v; want none: the grant it names is left out", foreign, grantIDs(others), err)
	}

	empty, _ := newEnrollment(t, m.Store, "session", "box-c-"+t.Name(), new(fixtureOperator), nil)
	if none, err := m.ProxyGrantsOfEnrollment(t.Context(), empty, "", both); err != nil || len(none) != 0 {
		t.Fatalf("an enrollment with no grant answered %v, %v; want none", grantIDs(none), err)
	}
}

// TestAGrantHoldingNoBoundNameIsNotCheckedWhileDispatchIsDown: the proxy reads the session's other
// grants for one host, and a grant that holds no name bound to it is left out before its project
// check, which asks Dispatch. So while Dispatch cannot answer, a read for AUTO_TOKEN's host answers
// the AUTO_TOKEN grant beside a live TEAM_TOKEN grant of the same session, where reading every
// grant would fail the whole read for TEAM's sake - a CONNECT to a host the TEAM grant has no part
// in answered 503. The control: a read whose names include TEAM_TOKEN still fails closed,
// ErrProjectAccessUnavailable, and once Dispatch answers it answers both grants.
func TestAGrantHoldingNoBoundNameIsNotCheckedWhileDispatchIsDown(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, bound(rule("AUTO_TOKEN", "example"), rule("TEAM_TOKEN", "example")), publicTeam)
	_, untagged := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	_, tagged := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	for _, grantID := range []string{untagged, tagged} {
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
			t.Fatalf("Values(%s): %v", grantID, err)
		}
	}
	dispatch := answerFrom(t, m, dispatchRows(publicTeam))
	dispatch.SetProjectsDown(true)

	if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", []string{"AUTO_TOKEN"}); err != nil || !reflect.DeepEqual(grantIDs(got), []string{untagged}) {
		t.Fatalf("the grants holding AUTO_TOKEN while Dispatch is down = %v, %v; want %s alone: the TEAM grant holds no name asked about, so its project is not asked", grantIDs(got), err, untagged)
	}
	if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", []string{"AUTO_TOKEN", "TEAM_TOKEN"}); !errors.Is(err, ErrProjectAccessUnavailable) {
		t.Fatalf("the grants holding AUTO_TOKEN or TEAM_TOKEN while Dispatch is down = %v, %v; want ErrProjectAccessUnavailable: the TEAM grant could take the call", grantIDs(got), err)
	}
	dispatch.SetProjectsDown(false)
	want := slices.Sorted(slices.Values([]string{untagged, tagged}))
	if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", []string{"AUTO_TOKEN", "TEAM_TOKEN"}); err != nil || !reflect.DeepEqual(grantIDs(got), want) {
		t.Fatalf("the grants holding AUTO_TOKEN or TEAM_TOKEN once Dispatch answers = %v, %v; want %v", grantIDs(got), err, want)
	}
}

// TestARestrictedMembersEnrollmentGrantsAreEndedNotAnswered: the read of a session's other grants
// makes AirlockConnection's restricted-member check. Once Dispatch calls the session's person a
// restricted member, the read answers no grant and is refused ErrGrantNotLive for a restricted
// member (errRestrictedMember), and every grant it listed is ended, as a connection opened with
// each grant's own placeholder would end it. The control: the same read while Dispatch calls the
// person a full member answers both grants and ends neither.
func TestARestrictedMembersEnrollmentGrantsAreEndedNotAnswered(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example"), rule("SECOND_TOKEN", "example")))
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	_, first := grantOf(t, m, enr, key, "read the first", "AUTO_TOKEN")
	_, second := grantOf(t, m, enr, key, "read the second", "SECOND_TOKEN")
	listed := slices.Sorted(slices.Values([]string{first, second}))
	for _, grantID := range listed {
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
			t.Fatalf("Values(%s): %v", grantID, err)
		}
	}
	both := []string{"AUTO_TOKEN", "SECOND_TOKEN"}
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{fixtureOperator: projectaccess.MemberFull})
	// A client of its own for each read, as a broker reads Dispatch again once its cached answer
	// has aged past projectaccess.CacheTTL.
	asking := func() { m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil) }

	asking()
	if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both); err != nil || !reflect.DeepEqual(grantIDs(got), listed) {
		t.Fatalf("the enrollment's grants while %s is a full member = %v, %v; want %v", fixtureOperator, grantIDs(got), err, listed)
	}
	for _, grantID := range listed {
		requireGrantLive(t, m, grantID)
	}

	dispatch.SetMemberKind(fixtureOperator, projectaccess.MemberRestricted)
	asking()
	got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both)
	if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, errRestrictedMember) || len(got) != 0 {
		t.Fatalf("the enrollment's grants once %s is restricted = %v, %v; want none, refused ErrGrantNotLive for a restricted member", fixtureOperator, grantIDs(got), err)
	}
	for _, grantID := range listed {
		requireGrantEnded(t, m, grantID)
	}
}

// TestAGrantTheProjectRuleEndsIsLeftOutBesideAHealthyGrant: the read of a session's other grants
// makes AirlockConnection's project rule for each grant. A grant the rule ends - a name of it is
// tagged with TEAM, which turned private, and the session names no project - is ended for good and
// left out of the answer rather than failing the read, since no connection names it, and the
// healthy grant of the same session beside it is still answered. The ended grant is the one that
// sorts first, so an answer that stopped at it rather than going on past it would leave the
// healthy grant out as well. The controls: before the tag, and with the tag while TEAM is public,
// the same read answers both grants and ends neither.
func TestAGrantTheProjectRuleEndsIsLeftOutBesideAHealthyGrant(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, bound(rule("AUTO_TOKEN", "example"), rule("SECOND_TOKEN", "example")), publicTeam)
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	both := []string{"AUTO_TOKEN", "SECOND_TOKEN"}
	nameOf := map[string]string{}
	for _, name := range both {
		_, grantID := grantOf(t, m, enr, key, "read it", name)
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
			t.Fatalf("Values(%s): %v", grantID, err)
		}
		nameOf[grantID] = name
	}
	listed := slices.Sorted(maps.Keys(nameOf))
	ended, healthy := listed[0], listed[1]
	read := func(what string, want []string) {
		t.Helper()
		if got, err := m.ProxyGrantsOfEnrollment(t.Context(), enr, "", both); err != nil || !reflect.DeepEqual(grantIDs(got), want) {
			t.Fatalf("the enrollment's grants %s = %v, %v; want %v", what, grantIDs(got), err, want)
		}
	}

	read("before any tag", listed)
	retag(t, m, taggedSecret(nameOf[ended], fixtureOperator, policy.TierAgent, teamProject, "tagged-v1"))
	read("with "+nameOf[ended]+" tagged TEAM while TEAM is public", listed)
	for _, grantID := range listed {
		requireGrantLive(t, m, grantID)
	}

	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	read("once TEAM is private and the session names no project", []string{healthy})
	if by, _ := endedForGood(t, m, ended); by != GrantEndedActor {
		t.Fatalf("grant %s of %s, which the project rule refused, revoked by %q; want it ended for good by %q", ended, nameOf[ended], by, GrantEndedActor)
	}
	requireGrantLive(t, m, healthy)
}

// TestProxyPlaceholderGrantNamesTheGrantOfAPlaceholderLiveOrEnded: the proxy names, in the refusal
// of a request that still carries a placeholder after the swap, the grant that placeholder was
// minted for. A live grant's placeholder names its grant, and so does an ended grant's until the
// sweep deletes its rows; a placeholder the broker never minted names none, and so does an ended
// grant's once its rows are gone.
func TestProxyPlaceholderGrantNamesTheGrantOfAPlaceholderLiveOrEnded(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example")))
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	held := release.Airlock.ProxyPassword
	named := func(what, text, want string) {
		t.Helper()
		if got, err := m.ProxyPlaceholderGrant(t.Context(), text); err != nil || got != want {
			t.Fatalf("ProxyPlaceholderGrant(%s) = %q, %v; want %q", what, got, err, want)
		}
	}
	named("a live grant's placeholder", held, grantID)
	named("a placeholder the broker never minted", placeholder.Prefix+"neverMintedByThisBrokerAtAll0000", "")
	if err := m.RevokeGrant(t.Context(), grantID, enr); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	named("an ended grant's placeholder before the sweep", held, grantID)
	if _, err := m.deleteSpentPlaceholders(t.Context()); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	named("an ended grant's placeholder after the sweep", held, "")
}

// TestProxyPlaceholderGrantNamesAMintedGrantWhileItLives: a minted capability's placeholder is
// read as AirlockConnection reads it (mintedConnection), so the refusal of a request that still
// carries one after the swap names the grant, by its kind's word and its id, exactly when a
// connection of it would be opened. Its grant past its end names none, nor does a grant made
// under another entry digest than this broker's, nor a minted placeholder of a kind this broker
// does not serve.
func TestProxyPlaceholderGrantNamesAMintedGrantWhileItLives(t *testing.T) {
	m, _, _, _ := newPlaceholderFixture(t, bound(rule("AUTO_TOKEN", "example")))
	grants := &memoryGrants{}
	withMintedKind(m, grants, &mintedTokens{})
	live, grant := grants.grant(t, testEntryDigest, time.Now().Add(time.Hour))
	ended, _ := grants.grant(t, testEntryDigest, time.Now().Add(-time.Second))
	moved, _ := grants.grant(t, otherEntryDigest, time.Now().Add(time.Hour))
	for _, c := range []struct{ what, text, want string }{
		{"a live minted grant's placeholder", live, testMintedWord + ":" + grant.ID},
		{"an ended minted grant's placeholder", ended, ""},
		{"a live grant made under another entry digest", moved, ""},
		{"a minted placeholder of a kind no one serves", placeholder.MintedPrefix("otherkind") + "OtherOtherOtherOtherOtherOtherOt", ""},
	} {
		if got, err := m.ProxyPlaceholderGrant(t.Context(), c.text); err != nil || got != c.want {
			t.Fatalf("ProxyPlaceholderGrant(%s) = %q, %v; want %q", c.what, got, err, c.want)
		}
	}
}

// grantIDs is each grant's id, in the order given.
func grantIDs(grants []AirlockConnection) []string {
	ids := make([]string, len(grants))
	for i, grant := range grants {
		ids[i] = grant.GrantID
	}
	return ids
}
