package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// A Claude Code terminal keeps its registration while no mod is connected to it. The mod is the
// extension that writes a received message into the conversation, so without it the terminal
// advertises no delivery mode. Its plugin still holds the relay subject, so the connection probe
// passes, and a plain send names no mode, so the mode check passes too. The answer must not call
// that session live: it answers state no_receiver and names no recipient_state. The message is
// stored all the same, for the session's own consumer.
//
// The control is the same session, registered again with the three modes a connected mod
// advertises: the same route and the same probe then answer connected and live.
//
// The new state is a string literal, so this file compiles against a listener that predates it.
// There the first send fails, because its answer says live.
func TestPlainSendDistinguishesUnavailableConversationReceiver(t *testing.T) {
	rig := newKnownSessionRig(t)

	const terminal = "ses_terminal_without_mod"
	subject := contracts.LegacyAgentSubject(terminal)
	plugin := startSessionStandIn(t, rig.url, terminal, answerDone)
	rig.register(t, terminal, map[string]any{
		"title":          "a terminal without its mod",
		"capabilities":   []string{},
		"plugin":         "@sjawhar/claude-envoy",
		"plugin_version": "1.0.0",
	})
	entry := rig.entry(t, terminal)
	if entry.Delivery != session.DeliveryConsumer || len(entry.Capabilities) != 0 {
		t.Fatalf("the registration left the entry %+v, want consumer delivery and no delivery mode", entry)
	}

	plain := rig.send(t, terminal, nil)

	if strings.Contains(plain.raw, "live") {
		t.Fatalf("a plain send to a terminal with no mod answered %s: it says live, but nothing writes the message into the conversation now", plain.raw)
	}
	if plain.status != http.StatusOK || plain.State != "no_receiver" {
		t.Fatalf("a plain send to a terminal with no mod: status %d, body %s; want 200 state no_receiver", plain.status, plain.raw)
	}
	if strings.Contains(plain.raw, "recipient_state") {
		t.Fatalf("the no_receiver answer %s names a recipient_state, want none", plain.raw)
	}
	if plain.LastSeen != entry.UpdatedAt {
		t.Fatalf("the no_receiver answer's last_seen = %d, want the entry's updated_at %d: %s", plain.LastSeen, entry.UpdatedAt, plain.raw)
	}
	if probed := plugin.probed.Load(); probed != 1 {
		t.Fatalf("the plugin was probed %d times, want once", probed)
	}
	if held := rig.storedOn(t, subject); held != 1 {
		t.Fatalf("the stream holds %d messages on %s after the plain send, want 1", held, subject)
	}

	// The control: the mod connects, and the terminal registers again with the modes it now
	// takes. The same plain send then answers connected and live.
	rig.register(t, terminal, map[string]any{
		"title":          "a terminal with its mod",
		"capabilities":   []string{"aside", "btw", "steer"},
		"plugin":         "@sjawhar/claude-envoy",
		"plugin_version": "1.0.0",
	})
	control := rig.send(t, terminal, nil)

	control.requireAnswer(t, "a plain send to the same terminal with its mod", "connected", session.Live)
	if probed := plugin.probed.Load(); probed != 2 {
		t.Fatalf("the plugin was probed %d times after the control send, want twice", probed)
	}
	if held := rig.storedOn(t, subject); held != 2 {
		t.Fatalf("the stream holds %d messages on %s after the control send, want 2", held, subject)
	}
}
