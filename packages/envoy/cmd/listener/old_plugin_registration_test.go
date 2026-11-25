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
	"github.com/sjawhar/envoy/internal/session"
)

// oldPluginRegistration is the registration body a published @sjawhar/pi-envoy 16.x sends for a
// new session on a pass that asks for no consumer, read from the package's own bundle
// (dist/envoy.js of `npm pack @sjawhar/pi-envoy@16.49.2` and `@16.36.1`). Such a build asks only
// when (16.49.2 line 49904, 16.36.1 line 49378):
//
//	const asking = relay?.sessionID === registeringFor && relay.subscription !== undefined && replay?.startPending() !== true ? relay : undefined;
//
// A first pass that cannot reach NATS opens no relay subject, and its POST /v1/replay, which this
// listener does not serve, fails, so the session's own agent subject is marked from_start
// (16.49.2 line 38415, 16.36.1 line 38988):
//
//	cursors.set(own, { from_start: now() });
//
// Only an answer of that route clears the mark, so every later pass asks for none too. The body
// carries a delivery field only when it asks (16.49.2 line 49897, 16.36.1 line 49371):
//
//	...asking === undefined ? {} : { delivery: asking.delivery.registration() },
//
// and the transport posts it as (16.49.2 lines 37710-37725, 16.36.1 lines 38288-38303):
//
//	session_id: input.sessionID,
//	dir: input.directory,
//	topics: expandSubscriptionTopics(input.topics),
//	port: input.port,
//	title: input.title,
//	...input.capabilities === undefined ? {} : { capabilities: input.capabilities },
//	driving: input.driving,
//	...input.selfSubscribed === undefined ? {} : { self_subscribed: input.selfSubscribed },
//	...input.delivery === undefined ? {} : { delivery: input.delivery },
//	...input.clientInstanceID === undefined ? {} : { client_instance_id: input.clientInstanceID },
//	host: machineID(),
//	...input.place === undefined ? {} : { tmux_place: input.place },
//	plugin: input.plugin.name,
//	plugin_version: input.plugin.version
//
// with topics the session's own agent subject and nothing else for a new session, port 0, the
// title empty until the host names the session, the capabilities DELIVERY_CAPABILITIES, driving
// false and self_subscribed true (registrationInput, 16.49.2 lines 49877-49899), and no tmux_place
// for a session in no pane.
func oldPluginRegistration(sessionID, plugin, version string) string {
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"dir":                "/home/agent/project",
		"topics":             []string{contracts.LegacyAgentSubject(sessionID)},
		"port":               0,
		"title":              "",
		"capabilities":       []string{"aside", "btw", "steer"},
		"driving":            false,
		"self_subscribed":    true,
		"client_instance_id": "instance-" + sessionID,
		"host":               "agent-pod-0",
		"plugin":             plugin,
		"plugin_version":     version,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// A new session on pi-envoy 16.49.2 or 16.36.1 whose first pass could not reach NATS asks for no
// consumer at that pass and at every pass after it. It registers at its first pass all the same,
// with a consumer, and a message stored for it while its relay subject was closed reaches it once
// that subject opens. A client that reads no relay frame it did not ask for is still refused, and
// the refusal names what that build needs (dispatch://INFRA-3589, R21).
func TestANewOldPluginSessionRegistersAfterAFailedFirstPass(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"16.49.2", "16.36.1"} {
		t.Run("@sjawhar/pi-envoy "+version+" is given a consumer", func(t *testing.T) {
			rig := newRelayRig(t)
			sessionID := "ses_pi_envoy_" + strings.ReplaceAll(version, ".", "_")
			registration := oldPluginRegistration(sessionID, "@sjawhar/pi-envoy", version)
			requireConsumer := func(t *testing.T, answer subscribeAnswer) {
				t.Helper()
				if answer.Delivery == nil || answer.Delivery.Mode != "consumer" || answer.Delivery.Subject != bus.DeliverSubject(sessionID) {
					t.Fatalf("answer %s, want delivery mode consumer on %s", answer.raw, bus.DeliverSubject(sessionID))
				}
				if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
					t.Fatalf("session entry %+v, want consumer delivery", entry)
				}
			}

			// The first pass: NATS is down, so nothing subscribes the relay subject yet.
			requireConsumer(t, rig.subscribe(t, registration))
			stored, seq := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))

			// NATS is back: the next pass opens the relay subject and registers again, still with
			// no delivery field.
			standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
			requireConsumer(t, rig.subscribe(t, registration))

			requireFrames(t, standIn.collect(t, 1, 15*time.Second), stored)
			awaitAcked(t, rig.consumers, sessionID, seq)
			// The heartbeat after it sends the same body, and the message does not come again.
			requireConsumer(t, rig.subscribe(t, registration))
			standIn.none(t, 2*time.Second)
		})
	}

	for _, c := range []struct {
		name, plugin, version string
		want                  []string
	}{
		{
			name:    "@sjawhar/pi-envoy 14.0.2, the release before consumer delivery, is told to update",
			plugin:  "@sjawhar/pi-envoy",
			version: "14.0.2",
			want:    []string{"update @sjawhar/pi-envoy to 14.1.0 or later", "this registration came from @sjawhar/pi-envoy 14.0.2"},
		},
		{
			name:    "another plugin is told to send the field",
			plugin:  "@sjawhar/claude-envoy",
			version: "68de3e90c155",
			want:    []string{"update this client to a build that sends that field", "this registration came from @sjawhar/claude-envoy 68de3e90c155"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newSessionConsumerRig(t)
			const sessionID = "ses_no_relay_reader"

			refusal := rig.refusedSubscribe(t, oldPluginRegistration(sessionID, c.plugin, c.version))

			refusal.requireRefused(t, http.StatusConflict, "not_requested")
			for _, want := range c.want {
				if !strings.Contains(refusal.Error, want) {
					t.Fatalf("refusal %q names no %q", refusal.Error, want)
				}
			}
			if _, err := rig.consumers.Info(context.Background(), sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
				t.Fatalf("consumer of %s: %v, want none", sessionID, err)
			}
			rig.requireNothingWritten(t, sessionID)
		})
	}
}
