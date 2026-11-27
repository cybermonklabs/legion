package main

import (
	"strings"

	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// sessionEntryFromSubscribe is the session entry a registration writes: the body's fields, the
// registering listener's machine id, the projects of the token that made the registration
// (registeringProjects, [] for none), and, when delivery says the listener holds the session's
// consumer for it, consumer delivery, the plugin's epoch and the sequence that consumer starts
// at. The entry's Port is left 0: a registration naming one is refused before it reaches here
// (subscribeHandler).
func sessionEntryFromSubscribe(body listenerwire.SubscribeRequest, machineID string, delivery sessionDeliveryOutcome, projects []string) session.SessionEntry {
	if projects == nil {
		projects = []string{}
	}
	entry := session.SessionEntry{
		MachineID:        machineID,
		Dir:              body.Dir,
		Title:            body.Title,
		Capabilities:     append([]string{}, body.Capabilities...),
		SelfSubscribed:   body.SelfSubscribed,
		Host:             body.Host,
		TmuxPlace:        body.TmuxPlace,
		Plugin:           strings.TrimSpace(body.Plugin),
		PluginVersion:    strings.TrimSpace(body.PluginVersion),
		ClientInstanceID: strings.TrimSpace(body.ClientInstanceID),
		Projects:         projects,
	}
	if delivery.relayed() {
		entry.Delivery = session.DeliveryConsumer
		entry.Epoch = delivery.epoch
		// The start comes off the answer, as the mode does (sessionDeliveryOutcome.relayed), so
		// the entry the registration writes and the answer the session reads name one sequence.
		entry.ConsumerStart = delivery.answer.ConsumerStart
	}
	return entry
}
