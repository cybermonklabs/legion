// A send marked with the role it is sent for: whose mark the send route takes, and how it hands
// off or keeps a send whose role moved before it arrived.

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
// role's (handOffOrKeepRoleSend): it is kept for the session that holds the role live, and with no
// live holder it is handed to the daemon of the role's newest Legion session or kept for the
// role's next live holder, as a role publish that finds no live holder is - unless the send asked
// connected_only, for a session running now, which is the 409 a stopped target gives, with nothing
// kept, since a message kept for the role would reach another session at a later time. A claim
// read that failed is its 503, or the unreadable claim's answer (refuseRoleClaimRead). sender is
// who the request speaks for, which decides how much of a copy the stream or the kept lane already
// held the answer may carry (handOffOrKeepRoleSend, keepRoleSend).
func answerRoleSend(ctx context.Context, w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, sender requestSender, request listenerwire.SendRequest, target string) bool {
	if item.Role == "" {
		return false
	}
	holder, err := readRoleClaimHolder(ctx, *d.delivery, item.Role)
	switch {
	case err != nil:
		refuseRoleClaimRead(w, record, item.Role, err)
	case holder == target:
		return false
	case request.ConnectedOnly:
		record.refuse(w, http.StatusConflict, outcomeSessionStopped,
			fmt.Sprintf("the session no longer holds role %s, and a send that asks for a running session keeps nothing", item.Role))
	default:
		handOffOrKeepRoleSend(ctx, w, d, record, charge, item, sender, request, target, holder)
	}
	return true
}

// refuseRoleClaimRead answers a read of a role's claim that failed, as GET /v1/roles/<role>
// answers it: a claim record no read can decode (store.ErrMalformedRoleClaim) is the 500 that
// names it, and any other failure - a read that did not answer within its cap or
// sendRequestBudget among them - the bus-unavailable 503, or the 413 or 400 a refused key gets
// (requestRecord.refuseBusUnavailable). answerRoleSend's claim read, handOffOrKeepRoleSend's
// resolution of the holder and the publish route's first resolution (roleSend.unresolved) are
// answered alike.
func refuseRoleClaimRead(w http.ResponseWriter, record *requestRecord, role string, err error) {
	if errors.Is(err, store.ErrMalformedRoleClaim) {
		record.refuseRoleClaimUnreadable(w, role, err)
		return
	}
	record.refuseBusUnavailable(w, err)
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

// handOffOrKeepRoleSend answers a send marked with its role whose claim names holder - another
// session, or none - rather than the target the send names. A claim naming a session is resolved
// first with the resolution every role route makes (roles.ResolveHolder, within the send's
// window), its claim read from the role bucket stream's leader (leaderRoleClaims), as
// answerRoleSend's own read is and as the publish route's confirmation of a miss is
// (roleSend.missConfirmed): on a replicated bucket a direct get can answer the claim as it stood
// before a takeover, naming the previous holder, which is not live, and the message would be
// handed to that holder's daemon while the session that took the role never receives it. A
// resolution that failed is answered as a claim read that failed is (refuseRoleClaimRead), with
// no wake reported and nothing handed off or kept, and the answers below name the holder the
// resolution read.
//
// A role that a session holds live is that holder's, whether its issue is finished or not: the
// session that took the role over - a person's resumed or forked session among them (#4387's
// takeover) - reads the message, so it is kept (keepRoleSend), whose keep hands the role's kept
// lane to that holder at once, and nothing is handed off.
//
// A role with no live holder - no claim, or a claim whose holder is not live - is answered as the
// publish route answers a send to such a role (roleSend.noHolder). A Legion role whose newest
// session's daemon answers for it, whether its issue has left the workflow or not and whether or
// not the daemon released that session's idle issue pod, takes the message through that daemon
// (handOffToLegion on the role token, under the conditions its comment names), answered 200 as
// the send route answers a send it handed off: the envelope handed off, that session as
// recipient, and for a repeat whose hand-off the stream already held, that hand-off's envelope,
// with duplicate where it is another request's (answerHandedOff). Dispatch marks a role's message
// it sends to that stopped session whenever its recheck of the session pinned on the attempt
// finds the listener's own record of it, a known-session record among them, which names no
// registry redirect (mentionResolutionFor, resolveDeliveryTarget in internal/dispatch/api), so
// this is where such a message reaches the daemon. A body that carried "handoff": false is never
// handed off. A hand-off whose publish failed is the bus-unavailable 503, which the sender retries
// under the same key, and the hand-off's MsgId stores that message once. The hand-off costs the
// sender what its publish stored (senderCharge.published), and raises no wake report: the daemon
// decides what wakes the agent. Every other role's message - a role the registry holds no entry
// for, a project that does not answer - is kept (keepRoleSend).
//
// Every message that is kept raises the role's wake report first: a role released with its pod is
// brought back only by the wake, and the daemons decide what a wake of a role whose holder is live
// means, acting on it only for a released claim of their own. The wake costs the sender one
// (senderCharge).
func handOffOrKeepRoleSend(ctx context.Context, w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, sender requestSender, request listenerwire.SendRequest, target, holder string) {
	live := false
	if holder != "" {
		resolved, err := roles.ResolveHolder(ctx, leaderRoleClaims{d.registry}, roleHolderSessions{d.sessions}, item.Role)
		if err != nil {
			refuseRoleClaimRead(w, record, item.Role, err)
			return
		}
		holder, live = resolved.Holder, resolved.State == roles.Live
	}
	if !live {
		handed, err := d.handoff.handOffToLegion(ctx, legionHandoffTarget{
			envelope: *item, roleToken: item.Role, stampSender: true,
			optedOut: optedOutOfHandoff(request.MessageBody),
		})
		if err != nil {
			charge.published(bus.StoredAck{}, err)
			record.refuseBusUnavailable(w, err)
			return
		}
		if handed != nil {
			charge.published(handed.ack, nil)
			duplicate, err := answerHandedOff(ctx, d.client, record, sender, handed, item)
			if err != nil {
				record.refuseBusUnavailable(w, err)
				return
			}
			record.accept(w, listenerwire.SendResponse{
				Envelope:     *item,
				ExpectsReply: expectsReplyAnswer(request.ExpectsReply, item.ExpectsReply),
				Recipient:    handed.TargetSession,
				Duplicate:    duplicate,
			})
			return
		}
	}
	reportRoleWake(d, d.delivery.logger, item.Role)
	charge.woke()
	keepRoleSend(w, d, record, charge, item, sender, target, holder, request.ExpectsReply)
}

// keepRoleSend keeps item, a send marked with its role whose claim names holder rather than the
// target the send names, for the role's next live holder, with the keep the publish route's role
// send takes (keepForNextHolder), which also hands the role's kept lane to whoever holds the role
// now: a live holder that took the role while the send was on its way receives it at once. It
// answers the 202 listenerwire.KeptSendResponse with the copy the lane holds, which the record's
// line names too, as the publish route's kept answer does (roleSend.kept): whole for its own
// sender, and for anyone else the key-taken answer or the envelope this send built
// (requestSender.answer), with duplicate where it is another request's copy, and its expects_reply
// the answered envelope's (expectsReplyAnswer). A copy the keep stored costs the sender one; one
// the lane already held inside its hour costs nothing (senderCharge). A keep that stored nothing is
// the bus-unavailable 503, which the sender retries under the same key, and the lane stores that
// message once (bus.KeptMsgID); a keep whose publish went unacknowledged may have stored the copy,
// so its 503 says so (writeBusUnavailable, listenerwire.CodeMaybeStored) and it costs the sender
// as a stored one.
func keepRoleSend(w http.ResponseWriter, d *listenerDeps, record *requestRecord, charge *senderCharge, item *contracts.Envelope, sender requestSender, target, holder string, expectsReply json.RawMessage) {
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
	duplicate := record.answerStored(item, kept.item, sender)
	record.holder = holder
	record.acceptWith(w, http.StatusAccepted, listenerwire.KeptSendResponse{
		Envelope:     *item,
		ExpectsReply: expectsReplyAnswer(expectsReply, item.ExpectsReply),
		Recipient:    target,
		Holder:       holder,
		Kept:         true,
		KeptUntil:    kept.until,
		Duplicate:    duplicate,
	})
}
