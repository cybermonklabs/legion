package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// b8AccessWrite is one access write of a deployment with no private project: carol writes group
// team, or adds dave to PUB's list. event is the type of the event it appends.
type b8AccessWrite struct {
	name  string
	event string
	act   func(t *testing.T, seed privateSeed)
}

// b8AccessWrites are the access writes that move nothing a reader that sees every object sees.
func b8AccessWrites(dave string) []b8AccessWrite {
	return []b8AccessWrite{
		{"a group write", accessGroupChangedEvent, func(t *testing.T, seed privateSeed) {
			seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{dave}}, http.StatusOK)
		}},
		{"an access list write", projectAccessChangedEvent, func(t *testing.T, seed privateSeed) {
			seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+dave, nil, http.StatusOK)
		}},
	}
}

// TestWithNoPrivateProjectAResumeAcrossAnAccessWriteIsComplete: in a deployment with no private
// project an open viewer sees every object (seesEveryObject), as every reader did before a project
// could be private, so no write of a group or of an access list moves what it sees, and its resume
// across one is replayed what it missed and says replay_complete true, as it did then (R9). PRIV is
// made public, so no project is private; then carol writes group team, and in the other case adds
// dave to PUB's list. The resume from before the write of each open caller - alice, carol and dave
// at a browser, carol's token that lists no project and the shared token - is complete, and its
// replay holds the write's own event.
//
// Control: the resume check asks sightMovedSQL of a reader that sees every object too; each resume
// across the group write, and dave's across his addition to PUB's list, says replay_complete false
// and is replayed nothing, and the test fails.
func TestWithNoPrivateProjectAResumeAcrossAnAccessWriteIsComplete(t *testing.T) {
	const dave = "dave@example.com"
	for _, write := range b8AccessWrites(dave) {
		t.Run(write.name, func(t *testing.T) {
			seed := seedPrivateProject(t)
			seedPeople(t, seed.database, dave)
			seed.setVisibility(t, seedPrivate, model.ProjectPublic)
			for _, login := range []string{seedAlice, dave} {
				if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(login)); err != nil || !seesEveryObject(reader) {
					t.Fatalf("with PRIV public %s reads as %+v (%v), want a reader that sees every object", login, reader.Viewer, err)
				}
			}
			since := eventHead(t, seed.database)
			write.act(t, seed)
			url := liveURLOf(t, seed.handler)
			for _, caller := range []struct {
				name   string
				header http.Header
			}{
				{string(callerAlice), asUser(seedAlice)},
				{string(callerCarol), asUser(seedCarol)},
				{dave, asUser(dave)},
				{string(callerCarolToken), withBearer(seed.bearer(t, callerCarolToken))},
				{string(callerSharedToken), withBearer(seed.bearer(t, callerSharedToken))},
			} {
				client := seed.dialLive(t, url, caller.header)
				if start := subscribeEventsStart(t, client, "feed", &since); start != liveEventsStart("feed", since, true) {
					t.Errorf("%s's resume from %d across %s = %+v, want it complete from %d, as before a project could be private",
						caller.name, since, write.name, start, since)
					continue
				}
				liveEventsThrough(t, client, "feed", func(event liveEvent) bool { return event.Type == write.event })
			}
		})
	}
}

// TestWithNoPrivateProjectAnAccessWriteBehindTheHorizonEndsNoFeed: in a deployment with no private
// project an access write moves nothing a reader that sees every object sees, so it reaches that
// reader's feed as any other event does and the feed goes on past it, as every feed did before a
// project could be private (R9), also where the feed cannot tell whether the reader it holds was
// resolved before the write or after it. The event horizon is held while carol writes, so the
// write's event stands above the cursor alice's subscription hands over at and at or below the
// head it read just after resolving her (eventsReader.through), and reaches her feed live once the
// horizon is let go. The shared token's feed is open first, so alice's subscription joins the
// relay's generation at the cursor it stands at rather than waiting for a horizon of its own.
//
// Control: resolveAgain takes every access change at or below through as one that moved what the
// reader sees, whatever the reader; alice's feed ends (behind) at the write, and the test fails.
func TestWithNoPrivateProjectAnAccessWriteBehindTheHorizonEndsNoFeed(t *testing.T) {
	const dave = "dave@example.com"
	for _, write := range b8AccessWrites(dave) {
		t.Run(write.name, func(t *testing.T) {
			seed := seedPrivateProject(t)
			seedPeople(t, seed.database, dave)
			seed.setVisibility(t, seedPrivate, model.ProjectPublic)
			url := liveURLOf(t, seed.handler)
			subscribeEvents(t, seed.dialLive(t, url, seed.liveHeader(t, callerSharedToken)), "first", nil)

			release := holdClusterTransactionID(t, eventFeedWait)
			write.act(t, seed)
			client := seed.dialLive(t, url, asUser(seedAlice))
			subscribeEvents(t, client, "feed", nil)
			release()
			liveEventsThrough(t, client, "feed", func(event liveEvent) bool { return event.Type == write.event })
		})
	}
}

// TestAResumeAcrossTheLastPrivateProjectMadePublicIsNotComplete: PRIV, the one private project, is
// made public. Each open caller that read PUB alone now sees every object (seesEveryObject), PRIV
// among them, which its page, read while PRIV was private, lacks; so its resume from before the
// change says replay_complete false, the frame on which the page reads everything again. A project
// made public is the one access write a reader that sees every object owes a refresh for.
//
// Control: the resume check asks nothing of a reader that sees every object; each resume is
// complete and the test fails.
func TestAResumeAcrossTheLastPrivateProjectMadePublicIsNotComplete(t *testing.T) {
	seed := seedPrivateProject(t)
	since := eventHead(t, seed.database)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice)); err != nil || !seesEveryObject(reader) {
		t.Fatalf("with PRIV public alice reads as %+v (%v), want a reader that sees every object", reader.Viewer, err)
	}
	url := liveURLOf(t, seed.handler)
	for _, caller := range []privateCaller{callerAlice, callerCarolToken, callerSharedToken} {
		requireResumeComplete(t, seed, url, caller, since, false, "it now sees PRIV, which its page, read while PRIV was private, lacks")
	}
}

// TestAFeedBehindTheHorizonOfTheLastPrivateProjectMadePublicEnds: the one access write a reader that
// sees every object owes a refresh for, a project made public, still ends its feed where the feed
// cannot tell whether the reader it holds was resolved before the write or after it. The horizon
// is held while PRIV is made public, so alice's subscription resolves her as a reader that sees
// every object, PRIV among them, while the change stands above the cursor it hands over at: her
// page, which never read PRIV, owes the refresh. Her feed ends (behind) at the change, and her
// resubscribe from where it started says replay_complete false.
//
// Control: resolveAgain exempts a reader that sees every object from every access change at or
// below through, a project made public among them; alice's feed goes on past the change, and the
// test fails.
func TestAFeedBehindTheHorizonOfTheLastPrivateProjectMadePublicEnds(t *testing.T) {
	seed := seedPrivateProject(t)
	url := liveURLOf(t, seed.handler)
	subscribeEvents(t, seed.dialLive(t, url, seed.liveHeader(t, callerSharedToken)), "first", nil)

	release := holdClusterTransactionID(t, eventFeedWait)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	client := seed.dialLive(t, url, asUser(seedAlice))
	started := subscribeEvents(t, client, "feed", nil)
	release()
	if frame := eventFeedFrame(t, client, "feed's end at PRIV made public"); frame != liveEnded("feed", liveEndedBehind) {
		t.Fatalf("after PRIV was made public behind the horizon alice's feed sent %+v, want its end, behind: her page never read PRIV", frame)
	}
	if start := subscribeEventsStart(t, client, "feed", &started); start.ReplayComplete {
		t.Errorf("alice's resubscribe from %d across PRIV made public says its replay is complete, so her page never reads PRIV: %+v", started, start)
	}
}
