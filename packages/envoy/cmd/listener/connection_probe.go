// The listener's check of whether a session's plugin still holds a NATS connection: the three
// words a send's answer gives for a session, and the one core request the send route makes for
// it, on the session's own relay subject.

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The three words the send route's state gives for a session; GET
// /v1/registry/<session_id>?include=…,stopped gives the second. Connected is a session whose relay
// subject a subscriber holds - its probe was answered or met silence - one whose entry records no
// consumer delivery, and one this listener could not ask about; stopped is a session Envoy knows
// whose relay subject nobody holds, or one it knows only by its known-session record. no_receiver
// is a session whose probe passed but whose entry advertises no delivery mode, as a Claude Code
// terminal does while no mod is connected to it: the session runs, but nothing writes a message
// into its conversation now. None of the three is a session.State: StateOf, the reapers, role
// resolution and the session list never see them, and a session neither store knows is the 404
// both routes answer.
const (
	sessionStateConnected  = "connected"
	sessionStateStopped    = "stopped"
	sessionStateNoReceiver = "no_receiver"
)

// connectionProbeTimeout bounds the one core request the send route makes on a target's relay
// subject. A second is the whole wait: a plugin holding the subscription answers from memory in
// well under it, and silence through it reads connected, so the wait decides nothing that a
// no-responders answer does not decide at once.
//
// The probe and the sender stamp share one second of sendRequestBudget: the probe runs on a
// goroutine of its own while the route reads the stamp, each bounded at a second
// (senderStampReadTimeout), so a send that both stamps and probes spends one second on the pair
// rather than two. The budget is therefore 1 s for the target's read (sendSessionReadTimeout), 1 s
// for the stamp and the probe together, and 1.5 s for the publish.
const connectionProbeTimeout = 1 * time.Second

// probeSessionConnection asks whether anything subscribes sessionID's relay subject
// (bus.DeliverSubject), the subject both plugins subscribe before they register and on which the
// session's owning listener relays its consumer's messages, and reports whether the session is
// connected. The request is one core NATS request bounded by connectionProbeTimeout within ctx
// (bus.Client.RequestCore), carrying bus.ConnectionProbeHeader and no body, on a reply inbox of
// its own, so its answer never reaches the relay's reply handler.
//
// Only NATS's no-responders status (nats.ErrNoResponders) is stopped: no subscription on this
// server's cluster holds the subject, which is what a plugin whose connection closed leaves. A
// reply is connected - a plugin answers done, and so does each stand-in for a session in the
// tests, the load test and the end-to-end scripts - and so is a second of silence, which a
// subscriber that holds the subject and is no session leaves: an operator's natstail on
// notifications.>, say, passes the probe over unanswered and uncounted. Every other error is
// connected too, and logged: a publish this listener's own grant denies (bus.ErrPublishDenied) and
// a connection that is reconnecting say something about this listener, not about the session, and
// a listener that cannot ask must not call every session of the deployment stopped.
func probeSessionConnection(ctx context.Context, client *bus.Client, sessionID string, logger *logging.Logger) bool {
	probeCtx, cancel := context.WithTimeout(ctx, connectionProbeTimeout)
	defer cancel()
	_, err := client.RequestCore(probeCtx, bus.DeliverSubject(sessionID), nats.Header{bus.ConnectionProbeHeader: []string{"1"}})
	switch {
	case err == nil, errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.Is(err, nats.ErrNoResponders):
		return false
	default:
		logger.Warn("listener connection probe did not complete; the session counts as connected",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return true
	}
}
