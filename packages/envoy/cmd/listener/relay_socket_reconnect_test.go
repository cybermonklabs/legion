package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sjawhar/envoy/internal/contracts"
)

// R14 of dispatch://INFRA-3589 at the relay socket (#4819): the socket's replica publishes a
// session's receipt on its own NATS connection, which keeps no reconnect buffer, so nats.go
// refuses that publish at once while it reconnects. The socket closes 1013 nats_unavailable, and
// its WARN names the reconnect ("bus: NATS is reconnecting"), not nats.go's "nats: outbound buffer
// limit exceeded". Control: endNATS logging the error as nats.go gave it; the row fails naming
// that text.
func TestAReceiptARelaySocketPublishesWhileNATSReconnectsFailsByName(t *testing.T) {
	t.Parallel()
	const closed = "listener: the relay socket could not reach NATS; it is closed"
	rig := newRelaySocketRig(t, withSocketNATSProxy())
	const session = "ses_edge_receipt_reconnect"
	rig.owner.subscribe(t, consumerRegistration(session, "epoch-1"))
	client, _ := rig.open(t, session)
	client.subscribeOwn(session)
	rig.owner.storeEvent(t, contracts.LegacyAgentSubject(session))
	frame := client.next(5 * time.Second)
	if frame.Op != "msg" {
		t.Fatalf("the socket sent %s, want a msg", frame.raw)
	}

	rig.proxy.Cut()
	waitFor(t, 15*time.Second, "the socket replica's connection to start reconnecting", rig.client.Connection().IsReconnecting)
	client.pub(frame.Reply, relayReplyDone)
	if code, reason := client.closeCode(5 * time.Second); code != websocket.CloseTryAgainLater || reason != relaySocketReasonNATSUnavailable {
		t.Fatalf("the socket closed %d %q, want %d %q", code, reason, websocket.CloseTryAgainLater, relaySocketReasonNATSUnavailable)
	}

	written := 0
	for _, record := range warnLinesNaming(rig.logs.String(), session) {
		if message, _ := record["msg"].(string); message != closed {
			continue
		}
		written++
		failure, _ := record["error"].(string)
		if record["step"] != "publish a receipt" || !strings.Contains(failure, reconnectingReason) || strings.Contains(failure, reconnectBufferRefusal) {
			t.Errorf("the socket logged %q at step %v with error %q, want step %q naming %q and not %q", closed, record["step"], failure, "publish a receipt", reconnectingReason, reconnectBufferRefusal)
		}
	}
	if written != 1 {
		t.Errorf("the socket logged %q %d times, want once\n%s", closed, written, rig.logs.String())
	}
}
