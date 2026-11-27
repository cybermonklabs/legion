//go:build natslive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// A record the server wrote reads as absent at the edge. An edge listener serves O, whose token
// lists PRIV, beside S, an in-house session of PUB another listener registered.
//
//   - A RePublish of another stream stores, at S's session key, a body its writer chose: S's key,
//     a machine, a positive time and PRIV as its projects, with that stream's headers. The session
//     cache takes it as S's record listing PRIV (the control: the cache would grant S to O), and
//     neither the watcher's entries nor a direct get carry the headers that tell it apart. O's list
//     does not show S, O's registry read of S answers 404 and O's send to S is answered as to a
//     missing session: the edge confirms a session it would grant by the record the stream stores
//     (edgeScope.showsConfirmed, session.SessionRegistry.StoredByWriter).
//   - The census row M1-R: a RePublish stores, at O's own interest key, a body reading as O's row
//     with a PUB topic beside O's own, at another stream's sequence. O's next registration answers
//     200 and O's frames resume: the write after the refusal reads the key from the leader, reads
//     the record as the server's, and replaces it at its true sequence with O's own row.
func TestAServerWrittenRecordReadsAsAbsent(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const outside, inHouse = "ses_written_outside", "ses_written_in_house"
	const privTopic = "notifications.dispatch.issue.PRIV.>"
	token := rig.personToken(outside, "PRIV")
	standIn := startSessionStandIn(t, rig.url, outside, answerDone)
	if recorder := rig.subscribe(t, token, outside, privTopic); recorder.Code != http.StatusOK {
		t.Fatalf("O's registration: status %d (%s)", recorder.Code, recorder.Body.String())
	}
	rig.placeElsewhere(t, inHouse, "PUB")
	conn := rig.harness.client.Conn

	listed := func() []string {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/sessions: status %d (%s)", recorder.Code, recorder.Body.String())
		}
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
	requireAbsent := func(when string) {
		t.Helper()
		if ids := listed(); slices.Contains(ids, inHouse) {
			t.Errorf("%s, O's GET /v1/sessions lists %s (%v), want it hidden", when, inHouse, ids)
		}
		if recorder := rig.call(t, http.MethodGet, token, "/v1/registry/"+inHouse, ""); recorder.Code != http.StatusNotFound {
			t.Errorf("%s, O's GET /v1/registry/%s: status %d (%s), want 404", when, inHouse, recorder.Code, recorder.Body.String())
		}
		send := rig.call(t, http.MethodPost, token, "/v1/messages/send",
			fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, inHouse, outside))
		var refusal listenerwire.NoLiveSessionRefusal
		_ = json.Unmarshal(send.Body.Bytes(), &refusal)
		if send.Code != http.StatusNotFound || refusal.LastSeen != 0 {
			t.Errorf("%s, O's send to %s: status %d (%s), want the 404 a missing session gets", when, inHouse, send.Code, send.Body.String())
		}
	}
	requireAbsent("before any write of the server's")

	body, err := json.Marshal(map[string]any{"session_id": inHouse, "machine_id": "shared-listener", "updated_at": time.Now().UnixMilli(),
		"projects": []string{"PRIV"}, "self_subscribed": true, "title": "a title its writer chose"})
	if err != nil {
		t.Fatalf("encode the republished record: %v", err)
	}
	testnats.RepublishesKey(t, conn, bus.SessionBucket, inHouse, body)
	testwait.Eventually(t, "the session cache to take the republished record of "+inHouse, func() bool {
		entry, _ := rig.deps.sessions.Lookup(inHouse)
		return slices.Contains(entry.Projects, "PRIV")
	})
	requireAbsent("after a RePublish of a record listing PRIV at its key")

	row, _, err := rig.deps.registry.RefreshAt(context.Background(), outside)
	if err != nil {
		t.Fatalf("read O's interest row: %v", err)
	}
	// The row carries a PUB topic beside O's own, so O's registration has a row to write: one
	// equal to the stored row, and younger than the refresh age, is skipped, and would test nothing.
	row.UpdatedAt = time.Now().UnixMilli()
	row.Topics = append(row.Topics, "notifications.dispatch.issue.PUB.>")
	rowBody, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode O's row: %v", err)
	}
	stored := testnats.RepublishesKey(t, conn, bus.InterestBucket, outside, rowBody)
	if recorder := rig.subscribe(t, token, outside, privTopic); recorder.Code != http.StatusOK {
		t.Errorf("O's registration over the row republished at its interest key: status %d (%s), want 200", recorder.Code, recorder.Body.String())
	}
	// The write after the refusal reads the key from the leader and replaces the RePublished record
	// at its true sequence with O's own row: the stream's newest message for O's key moves past the
	// RePublished one, and O's row holds none of the PUB topic that record carried. Without the
	// leader re-read the registration is a heartbeat skip over the fresh RePublished row, which keeps
	// both and reaches the base's 200 with the PUB topic standing, so these two readings are what
	// let the row fail there.
	if after := testnats.LastSequence(t, conn, bus.InterestBucket, outside); after == stored {
		t.Errorf("O's registration wrote no row: the RePublished record at sequence %d still stands", stored)
	}
	afterRow, _, err := rig.deps.registry.RefreshAt(context.Background(), outside)
	if err != nil {
		t.Fatalf("read O's row after its registration: %v", err)
	}
	if slices.Contains(afterRow.Topics, "notifications.dispatch.issue.PUB.>") {
		t.Errorf("O's row after its registration holds the PUB topic the RePublished record carried: %v", afterRow.Topics)
	}
	// A PUB event stored before O's own PRIV event: if the RePublished row's PUB topic still stood in
	// O's consumer filter, the PUB event would reach O and be its first frame.
	pub, _ := rig.storeEvent(t, "notifications.dispatch.issue.PUB.PUB-9.comment.created")
	own, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-9.comment.created")
	frames := standIn.collect(t, 1, 15*time.Second)
	if frames[0].item.EventID != own.EventID {
		t.Errorf("O's first frame after its registration is %s on %s, want its PRIV event %s (the PUB event was %s)", frames[0].item.EventID, frames[0].item.Topic, own.EventID, pub.EventID)
	}
}

// A role claim's refresh of the claimant's registration (roleSetHandler) writes back the stored
// record's projects. A RePublish of another stream stores, at the claimant's own session key, a
// record that names that key (so its binding holds and the session cache takes it) and lists a
// project the claimant's token does not. Before the leader-read confirmation the refresh keeps
// those projects and writes them back as a writer's record, laundering a project the server wrote
// into one the edge then grants; the fix confirms the stored record by the stream's leader
// (session.SessionRegistry.StoredByWriter), finds the server's message, and refreshes the entry
// with the projects of the token this claim was made with (registeringProjects).
func TestAnEdgeRoleClaimWritesNoForgedProjectsBack(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	if !rig.edgeOn() {
		t.Skip("the listener serves no project, so no edge confirmation applies")
	}
	project := rig.projects[0]
	const forged = "ses_role_forge_other_project"
	const outside, role = "ses_role_forge_outside", "legion-priv-priv-1-tester"
	token := rig.personToken(outside, project)
	if recorder := rig.subscribe(t, token, outside); recorder.Code != http.StatusOK {
		t.Fatalf("O's registration: status %d (%s)", recorder.Code, recorder.Body.String())
	}
	conn := rig.harness.client.Conn
	body, err := json.Marshal(map[string]any{"session_id": outside, "machine_id": "shared-listener", "updated_at": time.Now().UnixMilli(),
		"projects": []string{project, forged}, "self_subscribed": true})
	if err != nil {
		t.Fatalf("encode the republished record: %v", err)
	}
	testnats.RepublishesKey(t, conn, bus.SessionBucket, outside, body)
	testwait.Eventually(t, "the session cache to take the republished record of "+outside, func() bool {
		entry, _ := rig.deps.sessions.Lookup(outside)
		return slices.Contains(entry.Projects, forged)
	})
	if recorder := rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, outside, role)); recorder.Code != http.StatusOK {
		t.Fatalf("O's role claim: status %d (%s)", recorder.Code, recorder.Body.String())
	}
	stored, err := rig.deps.sessions.Refresh(context.Background(), outside)
	if err != nil {
		t.Fatalf("read O's record after the claim: %v", err)
	}
	if slices.Contains(stored.Projects, forged) {
		t.Errorf("O's record after the role claim lists %v, which holds the forged project %q: the server-written record's projects were written back as O's own registration", stored.Projects, forged)
	}
	if !slices.Equal(stored.Projects, []string{project}) {
		t.Errorf("O's record after the role claim lists %v, want only its token's project [%s]", stored.Projects, project)
	}
	if _, own, err := rig.deps.sessions.StoredByWriter(context.Background(), outside); err != nil || !own {
		t.Errorf("StoredByWriter(O) after the claim = %t, %v; want true (the refresh wrote a writer's record)", own, err)
	}
}
