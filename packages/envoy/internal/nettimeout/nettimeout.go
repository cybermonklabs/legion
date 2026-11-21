// Package nettimeout reads whether a failure timed out anywhere in its error chain. net/http gives
// one read that timed out in either of two shapes, which its transport picks by a race between its
// read and write loops: the read's error directly under the client's *url.Error, or that error
// wrapped first by the read loop as "readLoopPeekFailLocked: %w". *url.Error's own Timeout asks
// only the error it directly wraps, and errors.As for a Timeout method stops at the first error
// that has one, the *url.Error, so a rule written that way reads the second shape as no timeout.
package nettimeout

import "slices"

// InChain is whether any error in err's chain, through both forms of Unwrap, has a Timeout method
// that answers true: a net.Error that timed out, at any depth and in either shape net/http gives
// it, and context.DeadlineExceeded, whose Timeout answers true. A nil err, and a chain in which
// no Timeout answers true, is false.
func InChain(err error) bool {
	for err != nil {
		if t, ok := err.(interface{ Timeout() bool }); ok && t.Timeout() {
			return true
		}
		switch wrapper := err.(type) {
		case interface{ Unwrap() error }:
			err = wrapper.Unwrap()
		case interface{ Unwrap() []error }:
			return slices.ContainsFunc(wrapper.Unwrap(), InChain)
		default:
			return false
		}
	}
	return false
}
