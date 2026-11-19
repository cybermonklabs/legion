// A send marked with the role it is sent for: whose mark the send route takes, and how it keeps a
// send whose role moved before it arrived.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// refuseRoleMark answers a send carrying role, before anything is read, when the mark is not
// taken: from any caller but Dispatch's own principal it is the 403 naming the field
// (roleMarkRefusal), and a role no claim can name is the 400 naming it. It reports whether it
// answered; a send with no role is never answered here.
func refuseRoleMark(w http.ResponseWriter, r *http.Request, d *listenerDeps, record *requestRecord, role string) bool {
	if role == "" {
		return false
	}
	if refusal := roleMarkRefusal(r, d.dispatchPrincipal); refusal != "" {
		record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, refusal, "role")
		return true
	}
	if !roles.Valid(role) {
		record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, fmt.Sprintf("role %q is no role a claim can name", role), "role")
		return true
	}
	return false
}

// answerRoleSend reads the claim of the role a send is marked with (item.Role) before the send's
// target, from the role bucket under the role routes' own bound (readRoleClaimHolder), never from
// this listener's watched copy of it, and reports whether it answered the send. A claim naming the
// target answers nothing, and the send goes on to its target. A claim naming another session, or
// none, is a role that moved between Dispatch's resolution and this send, and the message is the
// role's: it is kept for the role's next live holder (keepRoleSend), as a role publish that finds
// no live holder is - unless the send asked connected_only, for a session running now, which is
// the 409 a stopped target gives, with nothing kept, since a message kept for the role would reach
// another session at a later time. A claim read that failed is its 503, or the unreadable claim's
// answer.
func answerRoleSend(ctx context.Context, w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, request listenerwire.SendRequest, target string) bool {
	if item.Role == "" {
		return false
	}
	holder, err := readRoleClaimHolder(ctx, *d.delivery, item.Role)
	switch {
	case errors.Is(err, store.ErrMalformedRoleClaim):
		record.refuseRoleClaimUnreadable(w, item.Role, err)
	case err != nil:
		record.refuseBusUnavailable(w, err)
	case holder == target:
		return false
	case request.ConnectedOnly:
		record.refuse(w, http.StatusConflict, outcomeSessionStopped,
			fmt.Sprintf("the session no longer holds role %s, and a send that asks for a running session keeps nothing", item.Role))
	default:
		keepRoleSend(w, d, record, charge, item, target, holder, expectsReplyAnswer(request.ExpectsReply))
	}
	return true
}

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
// the sender retries under the same key, and the lane stores that message once (bus.KeptMsgID); a
// keep whose publish went unacknowledged may have stored the copy, so its 503 says so
// (writeBusUnavailable, listenerwire.CodeMaybeStored) and it costs the sender as a stored one.
func keepRoleSend(w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, target, holder string, expectsReply json.RawMessage) {
	reportRoleWake(d, d.delivery.logger, item.Role)
	charge.woke()
	kept, err := keepForNextHolder(*d.delivery, item.Role, *item)
	if err != nil {
		charge.published(bus.StoredAck{}, err)
		record.refuseBusUnavailable(w, fmt.Errorf("the kept lane of role %s did not store the message: %w", item.Role, err))
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
