package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// c-one-leader-read (dispatch://INFRA-5459). The NATS server stores a message under a role's key
// on its own - here a RePublish of another stream - whose body is a claim naming a session that is
// live. No writer wrote it, and a direct get cannot tell it from a claim: it reads the message as
// that session's claim (the control). Every reader that decides which session a role's message
// reaches reads the role's claim from the role bucket stream's leader, with one decoding
// (store.Registry.RoleClaimFromLeader), which answers such a message store.ErrMalformedRoleClaim.
// So the controller notice lane's read gives the lane to nobody (store.Registry.ReadLaneHolders,
// behind a publish's recipient count, a stored row's filter and the interest writes), the publish
// route answers the unreadable claim's 500 and forwards nothing (sendToRole), and the role lane
// keeps the message under roleClaimUnread and forwards nothing (resolveCoreRoleHolder).
func TestAServerWrittenClaimOfALiveSessionReachesThatSessionThroughNoReader(t *testing.T) {
	const (
		project = "leaderread"
		holder  = "ses_server_written_claim"
	)
	role := contracts.ControllerRoleToken(project)
	topic := contracts.RoleTopicPrefix + role
	lane, isLane := bus.ControllerNoticeLane(role)
	if !isLane || !roles.Valid(role) {
		t.Fatalf("%s is no role token a claim can name with a controller notice lane (lane %q)", role, lane)
	}
	ctx := context.Background()
	rig := newRoleActRig(t)
	rig.register(t, holder)
	body, err := json.Marshal(store.RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the claim the RePublish stores: %v", err)
	}
	testnats.RepublishesKey(t, rig.harness.client.Conn, bus.RoleClaimBucket, role, body)
	if direct, err := rig.harness.registry.RoleClaim(ctx, role); err != nil || direct.HolderSessionID != holder {
		t.Fatalf("a direct get of %s's claim reads %+v, %v; want %s's claim, or the control shows nothing a reader could take for one", role, direct, err, holder)
	}

	t.Run("the leader read", func(t *testing.T) {
		if claim, err := rig.harness.registry.RoleClaimFromLeader(ctx, role); !errors.Is(err, store.ErrMalformedRoleClaim) {
			t.Fatalf("RoleClaimFromLeader(%s) = %+v, %v; want store.ErrMalformedRoleClaim", role, claim, err)
		}
	})

	t.Run("the controller notice lane's read", func(t *testing.T) {
		holders, err := rig.harness.registry.ReadLaneHolders(ctx, []string{lane})
		if err != nil {
			t.Fatalf("ReadLaneHolders(%s): %v", lane, err)
		}
		if named, weighed := holders[role]; !weighed || named != "" {
			t.Fatalf("ReadLaneHolders(%s) gives the lane to %q (weighed %t), want it weighed and given to nobody", lane, named, weighed)
		}
	})

	t.Run("the publish route", func(t *testing.T) {
		deps := &listenerDeps{client: rig.harness.client, registry: rig.harness.registry, sessions: rig.harness.sessions,
			delivery: &rig.cfg, confirmed: newConfirmedRecord(t)}
		recorder, record := serveRecorded(t,
			func(logger *logging.Logger) http.HandlerFunc { return publishHandler(deps, logger) },
			postJSON("/v1/messages/publish", sendBody(map[string]any{
				"topic":          topic,
				"source":         "agent",
				"source_session": "ses_server_written_publisher",
				"message":        "for the controller",
			})))
		rig.requireNoForward(t, 0)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("the publish to %s answered %d %s, want 500", topic, recorder.Code, recorder.Body.String())
		}
		var refusal listenerwire.APIError
		if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
		}
		head := "envoy: the claim record of role " + role + " is unreadable: "
		if !strings.HasPrefix(refusal.Error, head) || !strings.Contains(refusal.Error, "a message the server wrote") {
			t.Fatalf("the refusal reads %q, want it to begin %q and name the message the server wrote", refusal.Error, head)
		}
		assertRecordFields(t, record, map[string]any{
			"msg":     "listener publish",
			"outcome": "role_claim_unreadable",
			"status":  float64(http.StatusInternalServerError),
			"topic":   topic,
		})
	})

	t.Run("the role lane", func(t *testing.T) {
		item := rig.deliver(t, role, "server-written-claim-lane")
		rig.requireNoForward(t, 0)
		waitForKeptRoleMessage(t, rig.harness.client, role, item.EventID)
		rig.requireNoForward(t, time.Second)
		logs := rig.harness.logs.String()
		if kept := logRecordsNamed(logs, "listener kept role message for the role's next holder", role); len(kept) != 1 || kept[0]["reason"] != string(roleClaimUnread) {
			t.Fatalf("the kept lines for role %s are %v, want one naming the reason %s:\n%s", role, kept, roleClaimUnread, logs)
		}
	})
}
