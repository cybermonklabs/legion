package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// heldReplay is an agent stream whose replay of one session waits, the first time it is asked,
// until the test has written what it wants written between the view's first ask of its rule and
// the replay: asked is closed when the relay asks for the replay, and the replay answers once
// release is closed, however long that takes, as a session that answers late but answers does.
type heldReplay struct {
	*agentstream.Memory
	session        string
	asked, release chan struct{}
	first          *sync.Once
}

func (h heldReplay) Replay(ctx context.Context, session string) (agentstream.Frame, error) {
	if session == h.session {
		held := false
		h.first.Do(func() { held = true })
		if held {
			close(h.asked)
			<-h.release
		}
	}
	return h.Memory.Replay(context.WithoutCancel(ctx), session)
}

// TestAnAgentViewAsksItsRuleAgainBeforeTheReplay: the relay asks a session for its replay, the
// session's whole history, after the view's first ask of its rule, and the answer can take up to
// agentStreamReplayTimeout. A write in that window can take the session from the viewer, so the
// view asks its rule again before it sends a replay, as it does before each batch of frames.
// alice, an open viewer PRIV's list does not name, opens the view of dora's session, which has no
// listener list and which only dora, on no private project's list, proved: the rule passes it, and
// the relay asks the session for its replay. While the relay waits for it, carol's credential,
// which sees PRIV, writes as the session, which ties it to carol. alice is sent the responder, and
// then the view ends (dropped): the replay never reaches her.
//
// Control: the replay sent without the re-ask; alice is sent dora's session's replay.
func TestAnAgentViewAsksItsRuleAgainBeforeTheReplay(t *testing.T) {
	t.Parallel()
	held := heldReplay{session: contentDoraUnlistedSession, asked: make(chan struct{}), release: make(chan struct{}), first: new(sync.Once)}
	rig := newContentAgentRigWith(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour},
		func(memory *agentstream.Memory) agentstream.Source {
			held.Memory = memory
			return held
		})
	release := sync.OnceFunc(func() { close(held.release) })
	t.Cleanup(release)
	client := rig.seed.dialLive(t, rig.url, rig.seed.liveHeader(t, callerAlice))
	client.send(t, liveSubscribe("v", agentView(contentDoraUnlistedSession), ""))
	select {
	case <-held.asked:
	case <-time.After(eventFeedWait):
		t.Fatal("the relay never asked dora's session for its replay: the rule refused alice before the write, and the row proves nothing")
	}
	rig.seed.asSessionOf(t, seedCarol, contentDoraUnlistedSession, seedPublic, http.MethodPost,
		"/api/v1/issues/"+rig.seed.issue[seedPublic]+"/comments", map[string]any{"body": "carol, as dora's session."}, http.StatusCreated)
	release()
	if frame, ok := client.next(t, eventFeedWait, "alice's responder of dora's session").(LiveAgentFrame); !ok || string(frame.Data) != agentViewResponding {
		sent, _ := json.Marshal(frame)
		t.Fatalf("alice's agent view of dora's session opens with %s, want its responder", sent)
	}
	frame := client.next(t, 5*time.Second, "alice's agent view of dora's session after carol's write")
	if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
		sent, _ := json.Marshal(frame)
		t.Errorf("alice's agent view of dora's session, tied to carol while its replay was asked for, sends %s, want it ended (dropped) before the replay", sent)
	}
}

// TestAnAgentViewOfAnAllPublicDeploymentReadsWhetherAProjectIsPrivateBeforeEachBatch: an open
// viewer resolved while no project is private is passed by the content rule before it reads a row
// (R9), so before each batch of frames its view reads only whether a project is private now, and
// where one is it resolves the viewer again and asks the rule. The switch is an access change the
// event relay announces, but a frame can arrive first; here PRIV is made private by an SQL
// statement alone, which announces nothing, so only the read before the batch can see it. PRIV is
// public, and alice watches carol's pod session, which wrote in PRIV: she is relayed its replay.
// PRIV is then made private, and the next frame the session publishes must not reach her: her view
// ends (dropped).
//
// Control: the view trusting the viewer it resolved while no project was private (no read before
// the batch); the frame reaches alice.
func TestAnAgentViewOfAnAllPublicDeploymentReadsWhetherAProjectIsPrivateBeforeEachBatch(t *testing.T) {
	t.Parallel()
	rig := newContentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
	rig.seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	client, got := agentVerdict(t, rig.seed, rig.url, callerAlice, contentCarolPodSession)
	if got != agentViewResponding {
		t.Fatalf("with no private project alice's agent view of carol's pod session answers %s, want its responder: the row proves nothing", got)
	}
	_ = client.next(t, eventFeedWait, "alice's replay of carol's pod session")
	if _, err := rig.seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = $1`, seedPrivate); err != nil {
		t.Fatalf("make PRIV private by SQL alone: %v", err)
	}
	after := `{"v":1,"kind":"turn","session":"` + contentCarolPodSession + `","after":"PRIV made private"}`
	rig.stream.Publish(contentCarolPodSession, agentstream.Frame(after))
	frame := client.next(t, 5*time.Second, "alice's agent view of carol's pod session after PRIV was made private")
	if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
		sent, _ := json.Marshal(frame)
		t.Errorf("alice's agent view of carol's pod session, which wrote in PRIV, sends %s once PRIV is private, want it ended (dropped) before the frame", sent)
	}
}

// rosterListedSession is a session the listener lists with PUB and no person is tied to.
const rosterListedSession = "6a3f8b6d-0e7c-4fc0-9b8d-6c7d8e9fa0b1"

// TestAnAgentViewOfAListedSessionOutlivesItsStoredListenerAnswer: the content rule reads a
// session's listener list only from a stored listener answer that has not passed its expires_at,
// and the stored answer expires five seconds after the listener's answer, so the view refreshes
// it (readRouteRoster) before each ask of the rule. alice, an open viewer PRIV's list does not
// name, watches a session the listener lists with PUB alone, which no person is tied to: the
// listed arm passes it. The stored answer then passes its expires_at with the listener still
// answering, and the session publishes a frame: the view fetches the listener's answer again, the
// rule reads its list, and the frame reaches alice.
//
// Control: the ask reading the stored answer as it is (no refresh); the expired answer lists
// nothing, the untied session passes no open viewer, and alice's view ends (dropped).
func TestAnAgentViewOfAListedSessionOutlivesItsStoredListenerAnswer(t *testing.T) {
	t.Parallel()
	rig := newReachedRigOf(t, `[
		{"session_id":"`+rosterListedSession+`","title":"Lists PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]}
	]`, rosterListedSession)
	if _, err := rig.seed.database.Pool.Exec(t.Context(), `insert into session_projects (session_id, project_key) values ($1, $2)`,
		rosterListedSession, seedPublic); err != nil {
		t.Fatalf("record the listed session's history in PUB: %v", err)
	}
	client, got := agentVerdict(t, rig.seed, rig.url, callerAlice, rosterListedSession)
	if got != agentViewResponding {
		t.Fatalf("alice's agent view of the listed session answers %s, want its responder: the row proves nothing", got)
	}
	_ = client.next(t, eventFeedWait, "alice's replay of the listed session")
	if _, err := rig.seed.database.Pool.Exec(t.Context(), `update route_roster set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("let the stored listener answer pass its expires_at: %v", err)
	}
	after := `{"v":1,"kind":"turn","session":"` + rosterListedSession + `","after":"the stored answer expired"}`
	rig.stream.Publish(rosterListedSession, agentstream.Frame(after))
	frame := client.next(t, 5*time.Second, "alice's agent view of the listed session after its stored answer expired")
	if agent, ok := frame.(LiveAgentFrame); !ok || string(agent.Data) != after {
		sent, _ := json.Marshal(frame)
		t.Errorf("alice's agent view of a session the listener still lists with PUB sends %s once the stored answer expired, want the session's frame", sent)
	}
}
