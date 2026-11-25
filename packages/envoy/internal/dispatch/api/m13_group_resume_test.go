package api

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// m13Team seeds group team, holding dave, on PRIV's list, and dave's token that lists PRIV, so dave
// at a browser and his token each see PRIV through the group alone; it answers the token's caller.
func m13Team(t *testing.T, seed privateSeed, dave string) privateCaller {
	t.Helper()
	const daveToken privateCaller = "dave's token that lists PRIV"
	seedPeople(t, seed.database, dave)
	seed.putGroup(t, "team", dave)
	seed.addPrincipal(t, seedPrivate, access.PrincipalGroup, "team")
	mintListingToken(t, seed, daveToken, dave, []string{seedPrivate})
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(dave)); err != nil || !slices.Contains(reader.Viewer.Sees, seedPrivate) {
		t.Fatalf("dave, in team on PRIV's list, reads as %+v (%v), want a reader that sees PRIV", reader.Viewer, err)
	}
	return daveToken
}

// TestAResumeAcrossAGroupTakenOffAListOwesARefreshToItsMembers: dave sees PRIV only through group
// team, which PRIV's list names, and so does his token that lists PRIV. carol takes team off the
// list. dave and his token read PRIV no more, so the resume of each across the change is not
// complete, though neither may read the change itself: its page holds PRIV's rows. alice, whom no
// list names, never read PRIV, and carol, whom PRIV's list names, reads it before the change and
// after; each resumes complete.
//
// Control: lostSightSQL leaves out a group taken off a list; the resumes of dave and his token are
// complete, and the test fails.
func TestAResumeAcrossAGroupTakenOffAListOwesARefreshToItsMembers(t *testing.T) {
	t.Parallel()
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	daveToken := m13Team(t, seed, dave)
	since := eventHead(t, seed.database)
	seed.removePrincipal(t, seedPrivate, access.PrincipalGroup, "team")

	url := liveURLOf(t, seed.handler)
	client := seed.dialLive(t, url, asUser(dave))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete {
		t.Errorf("dave's resume from %d across team taken off PRIV's list says its replay is complete, so his page keeps PRIV: %+v", since, got)
	}
	requireResumeComplete(t, seed, url, daveToken, since, false, "it read PRIV through team, reads it no more, and its page holds PRIV's rows")
	requireResumeComplete(t, seed, url, callerAlice, since, true, "no list names her, and she never read PRIV")
	requireResumeComplete(t, seed, url, callerCarol, since, true, "PRIV's list names her, so she reads PRIV before the change and after it")
}

// TestAResumeAcrossAGroupTakenOffAListOwesNothingToARestrictedMemberInIt: erin's address was put in
// group team, which PRIV's list names, before anybody signed in as her, and she then signed in
// from outside, a restricted member; dave, a full member, is in team too. A group admits only its
// full members (store.PersonSeesProjectSQL), so erin never saw PRIV. carol takes team off PRIV's
// list: dave's resume across the change owes his page a refresh, and erin's is complete, since
// the change took nothing from her and tells her nothing of a project she never read.
//
// Control: lostSightSQL's group arm counting every member of the removed group, a restricted one
// included (the rule before); erin's resume is not complete, and the test fails naming her.
func TestAResumeAcrossAGroupTakenOffAListOwesNothingToARestrictedMemberInIt(t *testing.T) {
	t.Parallel()
	const dave, erin = "dave@example.com", "erin@outside.example"
	seed := seedPrivateProject(t)
	seedPeople(t, seed.database, dave)
	seed.putGroup(t, "team", dave, erin)
	seed.addPrincipal(t, seedPrivate, access.PrincipalGroup, "team")
	seedPeople(t, seed.database, erin)
	markRestricted(t, seed.database, erin)
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(erin)); err != nil || slices.Contains(reader.Viewer.Sees, seedPrivate) {
		t.Fatalf("erin, restricted and in team on PRIV's list, reads as %+v (%v), want a reader that does not see PRIV", reader.Viewer, err)
	}
	since := eventHead(t, seed.database)
	seed.removePrincipal(t, seedPrivate, access.PrincipalGroup, "team")

	url := liveURLOf(t, seed.handler)
	for _, member := range []struct {
		login    string
		complete bool
		why      string
	}{
		{dave, false, "he read PRIV through team, reads it no more, and his page holds PRIV's rows"},
		{erin, true, "team never admitted her, a restricted member, so its removal took nothing from her"},
	} {
		client := seed.dialLive(t, url, asUser(member.login))
		if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete != member.complete {
			t.Errorf("%s's resume from %d across team taken off PRIV's list answers replay complete %v, want %v: %s",
				member.login, since, got.ReplayComplete, member.complete, member.why)
		}
	}
}

// TestAResumeAcrossItsPersonLeavingAGroupOwesARefreshToAListingToken: dave sees PRIV only through
// group team, which PRIV's list names, and his token lists PRIV. carol takes dave out of team. The
// token reads PRIV no more, so its resume across the change is not complete. The group's event
// names no member, and an open viewer reads it (eventReadableSQL), dave's token among them, since a
// full member's credential is open whatever it lists: its resume across a group write owes the
// refresh (sightMovedSQL). dave at a browser reads the event and his resume is not complete either.
// bob, a restricted member, whom no group admits, resumes complete, and so does his token that lists
// PRIV: no group write moves what a restricted member's credential sees, and it reads none.
//
// Controls: sightMovedSQL leaving out a group write, under which the token's resume is complete;
// and the resume's gap holding every group write for a credential that lists projects (the rule
// before, unreadGroupWriteSQL), under which bob's token's resume is not complete. Each fails the
// test naming that caller.
func TestAResumeAcrossItsPersonLeavingAGroupOwesARefreshToAListingToken(t *testing.T) {
	t.Parallel()
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	daveToken := m13Team(t, seed, dave)
	since := eventHead(t, seed.database)
	seed.putGroup(t, "team")

	url := liveURLOf(t, seed.handler)
	requireResumeComplete(t, seed, url, daveToken, since, false, "it read PRIV through dave's place in team, reads it no more, and its page holds PRIV's rows")
	client := seed.dialLive(t, url, asUser(dave))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete {
		t.Errorf("dave's resume from %d across his leaving team says its replay is complete, so his page keeps PRIV: %+v", since, got)
	}
	requireResumeComplete(t, seed, url, callerBob, since, true, "no group admits a restricted member, so no group write moves what he sees")
	requireResumeComplete(t, seed, url, callerBobPrivToken, since, true, "no group admits a restricted member, so no group write moves what his token sees")
}
