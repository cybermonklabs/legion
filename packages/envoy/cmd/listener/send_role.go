// A send marked with the role it is sent for: whose mark the send route takes, and how it keeps a
// send whose role moved before it arrived.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// roleMarkRefusal is why a send carrying a role mark is refused, or "" for one this listener takes
// it from: a request whose identity token proved dispatchPrincipal, Dispatch's own
// (ENVOY_DISPATCH_PRINCIPAL, verifiedPrincipal). The mark decides who receives the message once
// the role moves - the listener copies a marked message to the role's next holder, and keeps one
// whose role moved before it arrived - so it is taken from Dispatch's verified identity alone, and
// never from the body's source, which is the caller's own word. Every other caller is refused: a
// session's broker token, the shared token, another service's identity token, no credential, and
// any caller of a listener that names no Dispatch principal.
func roleMarkRefusal(r *http.Request, dispatchPrincipal string) string {
	if dispatchPrincipal == "" {
		return "role is taken from Dispatch's own principal alone, and this listener names none (ENVOY_DISPATCH_PRINCIPAL)"
	}
	if principal := verifiedPrincipal(r); principal != dispatchPrincipal {
		if principal == "" {
			return "role is taken from Dispatch's own principal alone, and this request's credential proves no principal"
		}
		return fmt.Sprintf("role is taken from Dispatch's own principal alone, and this request's credential proves %s", principal)
	}
	return ""
}

// keepRoleSend keeps item, a send marked with its role whose claim names holder rather than the
// target the send names, for the role's next live holder, with the keep the publish route's role
// send takes (keepForNextHolder), which also hands the role's kept lane to whoever holds the role
// now: a live holder that took the role while the send was on its way receives it at once. It
// raises the role's wake report first, as the publish route's send to a role with no live holder
// does (roleSend.noHolder): a role released with its pod is brought back only by the wake, and the
// daemons decide what a wake of a role whose holder is live means. It answers the 202
// listenerwire.KeptSendResponse, with the copy the lane holds as the record's envelope. The wake
// costs the sender one, and so does a copy the keep stored; one the lane already held inside its
// hour costs nothing (senderCharge). A keep that stored nothing is the bus-unavailable 503, which
// the sender retries under the same key, and the lane stores that message once (bus.KeptMsgID).
func keepRoleSend(w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, target, holder string, expectsReply json.RawMessage) {
	reportRoleWake(d, d.delivery.logger, item.Role)
	charge.woke()
	kept, ok := keepForNextHolder(*d.delivery, item.Role, *item)
	if !ok {
		record.refuseBusUnavailable(w, fmt.Errorf("the kept lane of role %s did not store the message", item.Role))
		return
	}
	if kept.repeat {
		charge.forgive()
	} else {
		charge.stored()
	}
	*item = kept.item
	record.holder = holder
	record.acceptWith(w, http.StatusAccepted, listenerwire.KeptSendResponse{
		Envelope:     kept.item,
		ExpectsReply: expectsReply,
		Recipient:    target,
		Holder:       holder,
		Kept:         true,
		KeptUntil:    kept.until,
	})
}
