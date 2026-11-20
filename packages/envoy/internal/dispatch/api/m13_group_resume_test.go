package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// m13Team seeds group team, holding dave, on PRIV's list, and dave's token that lists PRIV, so dave
// at a browser and his token each see PRIV through the group alone; it answers the token's caller.
func m13Team(t *testing.T, seed privateSeed, dave string) privateCaller {
	t.Helper()
	const daveToken privateCaller = "dave's token that lists PRIV"
	seedPeople(t, seed.database, dave)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{dave}}, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/team", nil, http.StatusOK)
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
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	daveToken := m13Team(t, seed, dave)
	since := eventHead(t, seed.database)
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/groups/team", nil, http.StatusOK)

	url := liveURLOf(t, seed.handler)
	client := seed.dialLive(t, url, asUser(dave))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete {
		t.Errorf("dave's resume from %d across team taken off PRIV's list says its replay is complete, so his page keeps PRIV: %+v", since, got)
	}
	requireResumeComplete(t, seed, url, daveToken, since, false, "it read PRIV through team, reads it no more, and its page holds PRIV's rows")
	requireResumeComplete(t, seed, url, callerAlice, since, true, "no list names her, and she never read PRIV")
	requireResumeComplete(t, seed, url, callerCarol, since, true, "PRIV's list names her, so she reads PRIV before the change and after it")
}

// TestAResumeAcrossItsPersonLeavingAGroupOwesARefreshToAListingToken: dave sees PRIV only through
// group team, which PRIV's list names, and his token lists PRIV. carol takes dave out of team. The
// token reads PRIV no more, so its resume across the change is not complete. The group's event
// names no member, and a credential that lists projects may not read it (eventReadableSQL), so
// nothing that credential may read says whether a group write moved what it sees: its resume
// across any group write owes the refresh. dave at a browser reads the event and his resume is not
// complete either; bob, a restricted member, whom no group admits, resumes complete.
//
// Control: the resume check asks nothing of a group write a listing credential may not read; the
// token's resume is complete, and the test fails.
func TestAResumeAcrossItsPersonLeavingAGroupOwesARefreshToAListingToken(t *testing.T) {
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	daveToken := m13Team(t, seed, dave)
	since := eventHead(t, seed.database)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{}}, http.StatusOK)

	url := liveURLOf(t, seed.handler)
	requireResumeComplete(t, seed, url, daveToken, since, false, "it read PRIV through dave's place in team, reads it no more, and its page holds PRIV's rows")
	client := seed.dialLive(t, url, asUser(dave))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete {
		t.Errorf("dave's resume from %d across his leaving team says its replay is complete, so his page keeps PRIV: %+v", since, got)
	}
	requireResumeComplete(t, seed, url, callerBob, since, true, "no group admits a restricted member, so no group write moves what he sees")
}
