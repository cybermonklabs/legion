// Package stalledconnect gives a test a TCP address whose connect is neither answered nor
// refused, so the test can tell which limit ends a connect that never completes: the caller's
// context, or a fixed connect limit of the dialer it went through. Only tests import it, and only
// on Linux, where a listener whose accept queue is full drops each SYN that reaches it (Listen).
package stalledconnect
