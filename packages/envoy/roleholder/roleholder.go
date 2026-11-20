// Package roleholder names the reasons the Envoy listener gives when it refuses a role publish
// after keeping nothing. A no-holder publish answers 202 when the listener keeps it; it answers
// 404 with one of them only when the publish sent `keep: false`, its role token is one no claim
// can name (`roles.Valid`), or its keep failed for another reason. A stream that lacks the kept
// lane's subject answers 503 instead. The listener raises one role-lane exception carrying the
// same 404 `reason` (cmd/listener, publishRefusalException). The Legion daemon reads them on
// both: its notice publisher takes a 404 naming one as "no holder yet" and keeps the notice to
// retry (packages/daemon/internal/notify), and its exception consumer
// re-holds nothing for an exception naming one, since that publisher still holds the notice
// (packages/daemon/internal/daemon, notice_exceptions.go). Both modules read this one list so they
// cannot drift apart: a reason the listener gives that the daemon does not know reads as a broken
// publish path on the first and as a malformed exception on the second, which logs an ERROR for
// every refused publish.
//
// A no-holder state the listener names in a 404 is listed here before the listener gives it: the
// listener raises the refusal's exception only for a reason IsNoHolderReason accepts.
package roleholder

// Unclaimed is the refusal reason for a publish to a role no claim names when the listener keeps
// nothing for it.
const Unclaimed = "unclaimed"

// HolderLapsed is the refusal of a publish to a role whose claim names a session that is no longer
// live, when the listener kept nothing for it: the publish sent keep: false, or the keep failed
// for a reason other than the stream lacking the kept lane's subject. The listener's 202 that
// keeps such a publish names the same word as its kept_reason.
const HolderLapsed = "holder_lapsed"

// IsNoHolderReason reports whether reason is one the listener gives when it refuses a role publish
// because no live session holds the role.
func IsNoHolderReason(reason string) bool {
	return reason == Unclaimed || reason == HolderLapsed
}
