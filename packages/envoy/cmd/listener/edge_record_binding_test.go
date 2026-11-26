//go:build natslive

package main

// The server writes a session record or an interest row itself when a request names that record's
// subject as its reply subject (testnats.ServerWritesKey): any user who may send a direct get of a
// bucket, or any JetStream API request, can make it. These rows hold the edge to what a session's
// own registration wrote: a copy of another session's record or row lends no session another's
// projects or topics, and a JetStream answer stored at a session's keys reads as no record and is
// replaced by the session's next registration.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

const (
	forgePrivTopic = "notifications.dispatch.issue.PRIV.>"
	forgePubTopic  = "notifications.dispatch.issue.PUB.>"
	forgePrivEvent = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	forgePubEvent  = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
)

// listedSessions is the ids GET /v1/sessions answers token.
func (r *edgeRig) listedSessions(t *testing.T, token string) []string {
	t.Helper()
	recorder := r.call(t, http.MethodGet, token, "/v1/sessions", "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/sessions")
	var rows []listenerwire.SessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SessionID)
	}
	return ids
}

// requireMissingSend fails the test unless a send from sender under token to target is answered
// as a send to a missing session is: 404, naming no last registration.
func (r *edgeRig) requireMissingSend(t *testing.T, token, sender, target, when string) {
	t.Helper()
	recorder := r.call(t, http.MethodPost, token, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, target, sender))
	var refusal listenerwire.NoLiveSessionRefusal
	_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
	if recorder.Code != http.StatusNotFound || !strings.HasPrefix(refusal.Error, "no live session "+target) || refusal.LastSeen != 0 {
		t.Errorf("%s, a send to %s: status %d (%s), want the 404 a missing session gets, naming no last registration", when, target, recorder.Code, recorder.Body.String())
	}
}

// requireHidden fails the test unless the session under token neither lists nor reads any of
// others, and its send to each is answered as to a missing session.
func (r *edgeRig) requireHidden(t *testing.T, token, sender, when string, others ...string) {
	t.Helper()
	listed := r.listedSessions(t, token)
	for _, other := range others {
		if slices.Contains(listed, other) {
			t.Errorf("%s, GET /v1/sessions lists %s: %v", when, other, listed)
		}
		if recorder := r.call(t, http.MethodGet, token, "/v1/registry/"+other, ""); recorder.Code != http.StatusNotFound {
			t.Errorf("%s, GET /v1/registry/%s: status %d (%s), want the 404 of a missing session", when, other, recorder.Code, recorder.Body.String())
		}
		r.requireMissingSend(t, token, sender, other, when)
	}
}

// awaitCachedRecordReplaced waits until the listener's session registry no longer answers the
// record placed for sessionID on machine: its watcher has applied what the server wrote over it,
// whether it kept that as a record or read it as none.
func (r *edgeRig) awaitCachedRecordReplaced(t *testing.T, sessionID, machine string) {
	t.Helper()
	testwait.Eventually(t, "the session registry to apply the write over "+sessionID+"'s record", func() bool {
		entry, _ := r.deps.sessions.Lookup(sessionID)
		return entry.MachineID != machine
	})
}

// heals registers sessionID again under token and requires a PRIV event to reach it.
func (r *edgeRig) heals(t *testing.T, token, sessionID string, standIn *sessionStandIn, after string) {
	t.Helper()
	requireStatus(t, r.subscribe(t, token, sessionID, forgePrivTopic), http.StatusOK, "the registration of "+sessionID+" after "+after)
	if listed := r.listedSessions(t, token); !slices.Contains(listed, sessionID) {
		t.Fatalf("after %s and a registration, GET /v1/sessions lists %v, want %s", after, listed, sessionID)
	}
	event, _ := r.storeEvent(t, forgePrivEvent)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), event)
}

// G4, A1 and N1 at the edge. S_priv registers with a PRIV token; S_pub (projects [PUB]) and
// S_none (projects []) registered elsewhere. The server copies S_priv's record onto S_pub's and
// S_none's keys with a direct get: read plainly, each copy is S_priv's scope under the victim's
// key, so the edge listed S_pub and S_none to S_priv and took its sends to them. Each copy names
// S_priv's key, so the edge reads neither victim as a session at all. Then the server writes a
// stream info answer (A1) and a pull's empty status answer (N1) over S_priv's own record: each
// reads as no record, and S_priv's next registration is answered 200 and its frames flow again.
// The A1 and N1 rows pin behaviour the base already had: a record naming no registration time
// reads gone and an empty one does not decode, and a registration writes the record whole, so
// they cannot fail for the key binding; they hold that it costs no session its delivery past its
// next heartbeat. The edge's reading of a record that names no key is held by
// TestAnEdgeGrantsNoScopeToARecordNamingNoKey.
func TestAnEdgeReadsNoScopeFromAForgedSessionRecord(t *testing.T) {
	rig := newEdgeRig(t)
	const priv, pub, none = "ses_forge_priv", "ses_forge_pub", "ses_forge_none"
	token := rig.personToken(priv, "PRIV")
	standIn := startSessionStandIn(t, rig.url, priv, answerDone)
	requireStatus(t, rig.subscribe(t, token, priv, forgePrivTopic), http.StatusOK, "S_priv's registration")
	rig.placeElsewhere(t, pub, "PUB")
	rig.placeElsewhere(t, none)
	conn := rig.harness.client.Conn

	// The control: before any write of the server's, the edge already hides both, and S_priv
	// receives its own project's frames.
	rig.requireHidden(t, token, priv, "before the copies", pub, none)
	first, _ := rig.storeEvent(t, forgePrivEvent)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	t.Run("G4 a copy of S_priv's bound record onto a key no session has used", func(t *testing.T) {
		// S_priv's record was written by a conditional create, so its copy carries an expectation of
		// a key with no message (Nats-Expected-Last-Subject-Sequence 0): the server stores it only
		// on a session key no session has used. Read plainly it is S_priv's scope under that key; it
		// names S_priv's key, so the edge reads it as no session at all - listed to no one, a missing
		// session on a registry read, and a send to it answered as to a missing session.
		const phantom = "ses_forge_phantom"
		testnats.ServerCopiesKey(t, conn, bus.SessionBucket, phantom, bus.SessionBucket, priv)
		rig.awaitCachedRecordReplaced(t, phantom, "shared-listener")
		if entry, err := rig.deps.sessions.Refresh(context.Background(), phantom); err == nil {
			t.Errorf("the session registry reads the copy at %s as %+v, want no record", phantom, entry)
		}
		rig.requireHidden(t, token, priv, "after a copy onto a never-used key", phantom)
	})

	t.Run("the same copy onto S_pub's occupied key is refused by the server", func(t *testing.T) {
		// The copy's expectation of a key with no message makes the server refuse it on S_pub's key,
		// which holds S_pub's own record: a direct-get copy of a record this build wrote can never
		// overwrite a different session's live record. S_pub is unchanged. If this ever stores, the
		// edge's reading of a forged record would be reopened, and this row fails.
		testnats.ServerCopyRefused(t, conn, bus.SessionBucket, pub, bus.SessionBucket, priv)
		entry, err := rig.deps.sessions.Refresh(context.Background(), pub)
		if err != nil || entry.MachineID == "" || !slices.Contains(entry.Projects, "PUB") {
			t.Fatalf("S_pub after the refused copy reads %+v (%v), want its own PUB record", entry, err)
		}
	})

	t.Run("an earlier build's record copied onto S_pub's occupied key reads as no project's", func(t *testing.T) {
		// A record an earlier build wrote names no key and is stored by a plain put, so it carries
		// no expectation and its copy overwrites S_pub's key. The edge reads a record naming no key
		// as no project's (the no-key rule, TestAnEdgeGrantsNoScopeToARecordNamingNoKey), so S_pub
		// is listed to no one and a send to it answers as to a missing session, while every other
		// listener reads it as before.
		const legacy = "ses_forge_legacy"
		rig.placeElsewhere(t, legacy, "PRIV")
		rewriteAsEarlierBuild(t, conn, legacy)
		awaitEarlierBuildRecord(t, rig.deps.sessions, legacy)
		testnats.ServerCopiesKey(t, conn, bus.SessionBucket, pub, bus.SessionBucket, legacy)
		testwait.Eventually(t, "the session registry to read the earlier build's record copied at S_pub's key", func() bool {
			entry, state := rig.deps.sessions.Lookup(pub)
			return state == session.Live && entry.SessionID == "" && slices.Contains(entry.Projects, "PRIV")
		})
		rig.requireHidden(t, token, priv, "after an earlier build's record is copied onto S_pub", pub)
	})

	t.Run("a copy of S_pub's own interest row onto its session key", func(t *testing.T) {
		// The row was created with no previous revision, and its copy carries that expectation,
		// so the stream stores it only on a session key that holds no record yet.
		const victim = "ses_forge_pub_row"
		if _, err := rig.deps.registry.Upsert(store.Interest{SessionID: victim, MachineID: "shared-listener"}, []string{contracts.LegacyAgentSubject(victim)}); err != nil {
			t.Fatalf("write %s's interest row: %v", victim, err)
		}
		testnats.ServerCopiesKey(t, conn, bus.SessionBucket, victim, bus.InterestBucket, victim)
		// A Refresh applies what it read to the registry's copy, as the watcher would.
		if entry, err := rig.deps.sessions.Refresh(context.Background(), victim); err == nil {
			t.Errorf("Refresh(%s) reads the copy of its interest row as %+v, want no record", victim, entry)
		}
		if entry, state := rig.deps.sessions.Lookup(victim); state != session.Gone || entry.MachineID != "" {
			t.Errorf("the session registry reads the copy of %s's interest row as %+v, %s; want no record", victim, entry, state)
		}
		rig.requireHidden(t, token, priv, "after the copy of its interest row", victim)
	})

	for _, write := range []struct{ name, request string }{
		{"A1 a stream info answer onto S_priv's record", testnats.StreamInfo(bus.Stream)},
		{"N1 a pull's empty status answer onto S_priv's record", ""},
	} {
		t.Run(write.name, func(t *testing.T) {
			request, data := write.request, []byte(nil)
			if request == "" {
				request, data = testnats.NoWaitPull(t, conn, bus.SessionBucket)
			}
			testnats.ServerWritesKey(t, conn, bus.SessionBucket, priv, request, data)
			rig.awaitCachedRecordReplaced(t, priv, "test-machine")
			if entry, state := rig.deps.sessions.Lookup(priv); state != session.Gone || entry.MachineID != "" || entry.Projects != nil {
				t.Errorf("the session registry reads S_priv as %+v, %s after %s; want no record", entry, state, write.name)
			}
			if listed := rig.listedSessions(t, token); slices.Contains(listed, priv) {
				t.Errorf("after %s, GET /v1/sessions lists S_priv: %v", write.name, listed)
			}
			rig.heals(t, token, priv, standIn, write.name)
		})
	}
}

// placeFollowing writes sessionID's entry as another listener's registration under a token listing
// projects would, and its interest row following topics, in one write: the row's first, which
// the bucket stores with no previous revision.
func (r *edgeRig) placeFollowing(t *testing.T, sessionID string, projects []string, topics ...string) {
	t.Helper()
	if err := r.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "shared-listener", SelfSubscribed: true, Title: sessionID, Projects: projects}); err != nil {
		t.Fatalf("register %s elsewhere: %v", sessionID, err)
	}
	if _, err := r.deps.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "shared-listener"}, append([]string{contracts.LegacyAgentSubject(sessionID)}, topics...)); err != nil {
		t.Fatalf("write %s's interest row: %v", sessionID, err)
	}
}

// requireOwnAnswer fails the test unless a registration's answer names sessionID and no PUB topic.
func requireOwnAnswer(t *testing.T, recorder *httptest.ResponseRecorder, sessionID, when string) {
	t.Helper()
	requireStatus(t, recorder, http.StatusOK, when)
	var answer listenerwire.SubscribeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if answer.SessionID != sessionID || slices.Contains(answer.Topics, forgePubTopic) || !slices.Contains(answer.Topics, forgePrivTopic) {
		t.Errorf("%s is answered %s, want %s's own row, holding %s and not %s", when, recorder.Body.String(), sessionID, forgePrivTopic, forgePubTopic)
	}
}

// G3pre and A2 at the edge. S_pub, registered elsewhere, follows a PUB topic. The server copies
// S_pub's row onto the key of S_priv, which has never registered: read plainly, S_priv's first
// registration merged into that row and wrote it under S_pub's key, so it was answered S_pub's id
// and topic, relayed PUB frames, and wrote PRIV's topics into S_pub's row. The copy names S_pub's
// key, so the registration replaces it with S_priv's own row and S_pub's row is not touched. Then
// the server writes a stream info answer over another PRIV session's row (A2): read plainly that
// row named the session id "", and every later registration was refused naming key "". The next
// registration replaces it.
func TestAnEdgeRegistrationTakesNoForgedInterestRow(t *testing.T) {
	rig := newEdgeRig(t)
	conn := rig.harness.client.Conn
	ctx := context.Background()

	t.Run("G3pre a copy of S_pub's row onto S_priv's key before its first registration", func(t *testing.T) {
		const priv, pub = "ses_forge_row_priv", "ses_forge_row_pub"
		token := rig.personToken(priv, "PRIV")
		standIn := startSessionStandIn(t, rig.url, priv, answerDone)
		rig.placeFollowing(t, pub, []string{"PUB"}, forgePubTopic)
		pubRow, pubRevision, err := rig.deps.registry.RefreshAt(ctx, pub)
		if err != nil {
			t.Fatalf("read S_pub's row: %v", err)
		}
		testnats.ServerCopiesKey(t, conn, bus.InterestBucket, priv, bus.InterestBucket, pub)

		requireOwnAnswer(t, rig.subscribe(t, token, priv, forgePrivTopic), priv, "S_priv's first registration over the copy")
		after, revision, err := rig.deps.registry.RefreshAt(ctx, pub)
		if err != nil || revision != pubRevision || after.MachineID != pubRow.MachineID || !slices.Equal(after.Topics, pubRow.Topics) {
			t.Errorf("S_pub's row after S_priv's registration is %+v at revision %d (%v), want it unchanged: %+v at revision %d", after, revision, err, pubRow, pubRevision)
		}
		own, _, err := rig.deps.registry.RefreshAt(ctx, priv)
		if err != nil || own.SessionID != priv || slices.Contains(own.Topics, forgePubTopic) {
			t.Errorf("S_priv's row is %+v (%v), want its own, holding no PUB topic", own, err)
		}

		rig.storeEvent(t, forgePubEvent)
		privEvent, _ := rig.storeEvent(t, forgePrivEvent)
		frames := standIn.collect(t, 1, 10*time.Second)
		if frames[0].item.EventID != privEvent.EventID {
			t.Fatalf("S_priv's first frame is %s on %s, want the PRIV event: the PUB event reached it", frames[0].item.EventID, frames[0].item.Topic)
		}
		standIn.none(t, time.Second)
	})

	t.Run("A2 a stream info answer onto S_priv's row", func(t *testing.T) {
		const priv = "ses_forge_row_info"
		token := rig.personToken(priv, "PRIV")
		standIn := startSessionStandIn(t, rig.url, priv, answerDone)
		requireOwnAnswer(t, rig.subscribe(t, token, priv, forgePrivTopic), priv, "S_priv's registration")
		stored := testnats.ServerWritesKey(t, conn, bus.InterestBucket, priv, testnats.StreamInfo(bus.Stream), nil)

		requireOwnAnswer(t, rig.subscribe(t, token, priv, forgePrivTopic), priv, "S_priv's registration over the stream info answer")
		own, revision, err := rig.deps.registry.RefreshAt(ctx, priv)
		if err != nil || own.SessionID != priv || revision <= stored {
			t.Errorf("S_priv's row is %+v at revision %d (%v), want its own, written after the answer stored at %d", own, revision, err, stored)
		}
		event, _ := rig.storeEvent(t, forgePrivEvent)
		requireFrames(t, standIn.collect(t, 1, 10*time.Second), event)
	})
}
