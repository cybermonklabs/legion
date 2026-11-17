package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// processRegistration is the registration body one process of sessionID sends: consumer
// delivery at epoch, the client instance the process minted when it started, and topics.
func processRegistration(t *testing.T, sessionID, instance, epoch string, topics ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"self_subscribed":    true,
		"client_instance_id": instance,
		"topics":             append([]string{}, topics...),
		"delivery":           map[string]any{"consumer": true, "epoch": epoch},
	})
	if err != nil {
		t.Fatalf("encode the registration of %s: %v", sessionID, err)
	}
	return string(body)
}

// listedTopics is what envoy_list reads for sessionID through the routes: the topics of
// GET /v1/interests/<session_id>, the row pi-envoy's listInterests merges with what the process
// itself follows. A resumed process that remembers nothing follows nothing of its own, so for it
// this row is the whole list.
func (rig *sessionConsumerRig) listedTopics(t *testing.T, sessionID string) []string {
	t.Helper()
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/interests/"+sessionID, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/interests/%s: status %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	var row struct {
		Topics []string `json:"topics"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode %s's interest row %s: %v", sessionID, recorder.Body.String(), err)
	}
	return row.Topics
}

// A Legion role session follows two pull requests of other repositories (what envoy_subscribe
// registers), and its pod is then released for idling: the daemon's clean stop deletes the
// session's registration and its consumer (DELETE /v1/sessions/<id>?if_not_registered_after=…),
// and the release lasts past the interest reaper's allowance (store.InterestReapAllowance of
// store.InterestReapGrace, 15 minutes), which another listener's reaper pass reaches. The wake
// starts a new process of the same session, which remembers nothing it followed: its first
// registration names its own agent subject alone, under a client instance of its own, and it
// claims its role.
//
// envoy_list in that process must show both pull requests beside its agent subject and its role
// lane, the consumer the registration made must filter on both, and the message stored on one of
// them during the release, and one published on each after the wake, must reach the session from
// that consumer.
func TestAReleasedSessionKeepsItsSubscriptionsPastTheReapAllowanceAndGetsThemBackWhenWoken(t *testing.T) {
	rig := newSessionConsumerRig(t)
	ctx := context.Background()
	const (
		sessionID = "ses_released_follower"
		role      = "released-follower-worker"
		widgets   = "notifications.github.acme.widgets.pr.3370.>"
		tools     = "notifications.github.acme.tools.pr.205.>"
	)
	agent := contracts.LegacyAgentSubject(sessionID)
	roleLane := contracts.RoleTopicPrefix + role

	rig.subscribe(t, processRegistration(t, sessionID, "process-1", "epoch-1", agent, widgets, tools))
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)

	// The release: the daemon's clean stop of the session, at the moment its probe judged the
	// process gone.
	rig.deregister(t, sessionID, fmt.Sprintf("?if_not_registered_after=%d", time.Now().Add(time.Second).UnixMilli()))
	rig.awaitNoConsumer(t, sessionID)
	rig.deps.consumers.deletes.Wait()

	// Another listener's reaper pass, one minute past the allowance after the row's last write.
	reapAt := time.Now().Add(store.InterestReapAllowance(store.InterestReapGrace) + time.Minute)
	reaper, err := store.Open(t.Context(), rig.harness.client.Conn, store.WithReplicas(1), store.WithClock(func() time.Time { return reapAt }))
	if err != nil {
		t.Fatalf("open the reaping listener's registry: %v", err)
	}
	waitForRegistry(t, reaper)
	testwait.Eventually(t, "the reaping listener to hold "+sessionID+"'s row", func() bool {
		_, cached := reaper.Cached(sessionID)
		return cached
	})
	if _, err := reaper.Reap(func(id string) (bool, error) { return sessionMayBeLive(rig.harness.sessions, id) }, store.InterestReapGrace); err != nil {
		t.Fatalf("the reaper's pass: %v", err)
	}
	// This listener follows what the pass did to the row before the session comes back, so the
	// registration below plans over the row the bucket holds.
	testwait.Eventually(t, "this listener to follow the reaper's pass over "+sessionID+"'s row", func() bool {
		row, cached := rig.harness.registry.Cached(sessionID)
		return !cached || row.MachineID == ""
	})

	// A review on one of the pull requests while the pod is released.
	duringRelease := rig.store(t, "notifications.github.acme.widgets.pr.3370.review")

	// The wake: a new process of the same session registers as a resumed process does, and
	// claims its role.
	rig.subscribe(t, processRegistration(t, sessionID, "process-2", "epoch-2", agent))
	rig.post(t, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"client_instance_id":"process-2"}`, sessionID, role))

	listed := rig.listedTopics(t, sessionID)
	for _, want := range []string{agent, roleLane, widgets, tools} {
		if !slices.Contains(listed, want) {
			t.Errorf("envoy_list of the woken session shows %v, want %s among them", listed, want)
		}
	}
	filters := rig.filters(t, sessionID)
	for _, want := range []string{widgets, tools} {
		if !slices.Contains(filters, want) {
			t.Errorf("the woken session's consumer filters on %v, want %s among them", filters, want)
		}
	}

	afterWake := []uint64{
		rig.store(t, "notifications.github.acme.widgets.pr.3370.comment"),
		rig.store(t, "notifications.github.acme.tools.pr.205.comment"),
	}
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pull, err := rig.consumers.Pull(pullCtx, sessionID)
	if err != nil {
		t.Fatalf("pull the woken session's consumer: %v", err)
	}
	defer pull.Stop()
	for _, want := range append([]uint64{duringRelease}, afterWake...) {
		msg, err := pull.Next(pullCtx)
		if err != nil || msg.StreamSeq != want {
			t.Fatalf("the woken session's consumer handed over seq %d, %v; want the message stored at %d on a pull request it followed before the release", msg.StreamSeq, err, want)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", want, err)
		}
	}
}
