package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// M4: a role claim through a replica whose relay run for the session had ended for want of an
// admission. The claim records the admission and renews the entry, but the entry's change is a
// relayCheck, which starts no ended run (sessionRelays.apply), so without a resume the session's
// messages wait until its next registration. The run is made to end as a replica's restart ends
// it: the admission is forgotten and the relays lose and regain the durable binding, so the run
// started again finds no admission. The refresh then sends the resume a registration sends
// (api.go, the roleSetHandler write), which starts the ended run. Controls: the relay delivers the
// session's first message before the admission is lost, and, when the resume is missing, a
// registration after the probe's window delivers the message the probe waited for.
func TestAnEdgeRoleClaimResumesARelayRunThatEndedForWantOfAnAdmission(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const sessionID, role = "ses_claim_resume", "legion-priv-priv-31-tester"
	const held = "edge relay holds the session's messages in its consumer until the session registers or claims a role through this listener"
	const stopped = "session relay stopped"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the session's registration")
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), first)

	count := func(level, prefix string) int {
		n := 0
		for _, line := range edgeLogLines(rig.harness.logs.String(), level, prefix) {
			if line["session_id"] == sessionID {
				n++
			}
		}
		return n
	}

	rig.deps.consumers.admissions.prune(time.Now().Add(time.Hour), func(string) bool { return true })
	rig.relays.setOwned(false)
	testwait.Eventually(t, "the session's relay to stop", func() bool { return count("INFO", stopped)+count("WARN", stopped) > 0 })
	rig.relays.setOwned(true)
	testwait.Eventually(t, "the session's relay to end for want of an admission", func() bool { return count("INFO", held) > 0 })

	claim := rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))
	requireStatus(t, claim, http.StatusOK, "the claim of "+role)
	if _, admitted := rig.deps.consumers.admissions.of(sessionID); !admitted {
		t.Fatalf("UNKNOWN: the claim recorded no admission, so the test does not reach the ended run")
	}
	second, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	deadline := time.After(30 * time.Second)
wait:
	for {
		select {
		case frame := <-standIn.frames:
			if frame.item.EventID == second.EventID {
				t.Logf("after the claim: the session's message %s reached it", second.EventID)
				return
			}
			t.Logf("after the claim: an earlier frame %s reached the session (want %s); still waiting", frame.item.EventID, second.EventID)
		case <-deadline:
			break wait
		}
	}
	t.Errorf("after a role claim through the replica, the session's message %s reached it in no 30 s: the ended run was not resumed", second.EventID)
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the control registration")
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), second)
	t.Logf("CONTROL: the registration after the window delivered %s", second.EventID)
}

// The repository-gap-sent residue: frame F carried mark X; F is nak'd and its re-mark runs before
// the relay loop's clear of X (the N7 ordering), and the re-mark's write fails, so it notes a fresh
// mark Y in this process alone (reMarkWithheldGap). The loop's clear then runs with the bus
// answering again: repositoryGapSent forgets only the mark the frame carried, so Y stays owed in
// memory and this listener's next relay run sends it, and X is cleared from the entry. The failing
// write is the re-mark's registry on a closed connection; the clear runs on the relay's working
// registry. Control: the same steps with the re-mark's write working keep a mark owed.
func TestARepositoryGapSentForgetsOnlyTheMarkTheFrameCarried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	open := func(name string) (*bus.Client, *session.SessionRegistry) {
		t.Helper()
		client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		t.Cleanup(client.Close)
		reg, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
		if err != nil {
			t.Fatalf("open %s's registry: %v", name, err)
		}
		return client, reg
	}
	_, reader := open("reader")
	run := func(sessionID string, fail bool) (owed, stored string) {
		_, reg := open(sessionID)
		failingClient, failing := open(sessionID + "-failing")
		m := &sessionRelays{sessions: reg, logger: logging.New("test")}
		if err := reg.Put(sessionID, session.SessionEntry{MachineID: "test-machine", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
		mark, err := m.noteRepositoryGap(ctx, sessionID)
		if err != nil {
			t.Fatalf("the loop's mark: %v", err)
		}
		testwait.Eventually(t, "the relay's cache to show the loop's mark", func() bool {
			entry, _ := reg.Lookup(sessionID)
			return entry.RepositoryGap == mark
		})
		if fail {
			failingClient.Close()
			m.sessions = failing
		}
		m.reMarkWithheldGap(sessionID)
		m.sessions = reg
		m.repositoryGapSent(ctx, sessionID, mark)
		got, _, err := reader.StoredByWriter(ctx, sessionID)
		if err != nil {
			t.Fatalf("read %s: %v", sessionID, err)
		}
		testwait.Eventually(t, "the relay's cache to show the stored mark", func() bool {
			entry, _ := reg.Lookup(sessionID)
			return entry.RepositoryGap == got.RepositoryGap
		})
		return m.owedRepositoryGap(sessionID), got.RepositoryGap
	}
	if owed, stored := run("ses_gap_sent_ok", false); owed == "" && stored == "" {
		t.Fatalf("CONTROL: a working re-mark before the loop's clear kept no mark (owed=%q stored=%q)", owed, stored)
	} else {
		t.Logf("CONTROL working re-mark: owed=%q stored=%q", owed, stored)
	}
	owed, stored := run("ses_gap_sent_fail", true)
	t.Logf("failed re-mark before the loop's clear: owed=%q stored=%q", owed, stored)
	if owed == "" && stored == "" {
		t.Errorf("a failed re-mark that ran before the loop's clear left the notice owed nowhere")
	}
}
