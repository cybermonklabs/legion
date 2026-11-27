package model

// keptDeliveryExpiredText and keptDeliveryMayHaveReachedText are the two errors an attempt of
// Dispatch's delivery of a post to a role's first holder is settled failed with when the copy the
// listener kept of it ended with no session taking it (KeptDeliveryExpiredError): the copy's hour
// ended, or the role's live holder does not take its delivery mode. The second is the error of a
// copy whose forward the listener began and never confirmed, which the stream may hold, so the
// role's holder may already have the message and a second delivery of it can follow a Retry.
const (
	keptDeliveryExpiredText        = "The Envoy listener kept this message for the role, and no session took it before the listener dropped it."
	keptDeliveryMayHaveReachedText = "The Envoy listener kept this message for the role, and a forward to its holder was not confirmed before the listener dropped it; the holder may already have it, so Retry can deliver a second copy."
)

// KeptDeliveryExpiredError is the error an attempt of Dispatch's delivery of a post is settled
// failed with when the copy the listener kept of it for the role ended with no session taking it:
// forwardUnconfirmed is the listener's word, in its expiry notice (bus.KeptCopyExpiry), that the
// copy's forward may have reached the role's holder. Both the outbox, which settles the attempts
// the notice names (internal/dispatch/outbox, settleKeptDelivery), and the API, which settles an
// attempt whose send reported after the notice was read (internal/dispatch/api,
// settleDeliveryAttempt), word the failure by it, so one sentence is declared once for each case.
func KeptDeliveryExpiredError(forwardUnconfirmed bool) string {
	if forwardUnconfirmed {
		return keptDeliveryMayHaveReachedText
	}
	return keptDeliveryExpiredText
}
