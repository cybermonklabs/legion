// Package roleholder names the reasons the Envoy listener gives when it refuses a role publish
// because no live session holds the role. The listener answers such a publish 404 with one of them
// in the body's `reason` and raises one role-lane exception carrying the same `reason`
// (cmd/listener, publishRefusalException). The Legion daemon reads them on both: its notice
// publisher takes a 404 naming one as "no holder yet" and keeps the notice to retry
// (packages/daemon/internal/notify), and its exception consumer re-holds nothing for an exception
// naming one, since that publisher still holds the notice (packages/daemon/internal/daemon,
// notice_exceptions.go). Both modules read this one list so they cannot drift apart: a reason the
// listener gives that the daemon does not know reads as a broken publish path on the first and as
// a malformed exception on the second, which logs an ERROR for every refused publish.
//
// A state the listener refuses a role publish for, because no live session holds the role, is
// listed here before the listener gives it: the listener raises the refusal's exception only for a
// reason IsNoHolderReason accepts.
package roleholder

import "slices"

// Unclaimed is the refusal of a publish to a role no claim names.
const Unclaimed = "unclaimed"

// HolderLapsed is the refusal of a publish to a role whose claim names a session that is no longer
// live, when the listener kept nothing for it: the publish sent keep: false, or the keep failed.
// The listener's 202 that keeps such a publish names the same word as its kept_reason.
const HolderLapsed = "holder_lapsed"

// NoHolderReasons is every reason the listener gives when it refuses a role publish because no
// live session holds the role: the one list IsNoHolderReason reads, and a reader that holds the
// listener's reason as the listener's words (redact.RemoteText) compares against.
var NoHolderReasons = []string{Unclaimed, HolderLapsed}

// IsNoHolderReason reports whether reason is one the listener gives when it refuses a role publish
// because no live session holds the role (NoHolderReasons).
func IsNoHolderReason(reason string) bool {
	return slices.Contains(NoHolderReasons, reason)
}
