// Package projectscope is the bus's rule for a broker token's list of projects: which projects
// the list covers. It imports nothing, so every reader of the rule takes it without linking what
// another reader links: Dispatch reads it through internal/dispatch/access, and the Envoy
// listener, which must link no package of Dispatch's store, reads it here.
package projectscope

import "slices"

// BrokerTokenAdmits is the bus's rule for a broker token: a listed project, or, with no list, any
// project that is not private. It is valid only for a broker token, because the broker checks
// every listed key against the person through the membership answer at mint and at every trade
// (no refresh token exists), and never mints an empty list for a restricted member, so a lost
// project is dropped within BROKER_ACCESS_TOKEN_SECONDS and the answer's cache lifetime.
func BrokerTokenAdmits(listed []string, project string, private bool) bool {
	if len(listed) > 0 {
		return slices.Contains(listed, project)
	}
	return !private
}
