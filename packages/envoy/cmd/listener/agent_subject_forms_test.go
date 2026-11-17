package main

import "testing"

// A session's agent subject has two forms while every reader takes both (dispatch://INFRA-3862):
// the session alone, and the session under a project (`_` for none). The listener's durable path
// reads the session a stored agent message is for (durableNeedsDelivery, agentSubjectDelivery),
// and must read it from either form: a reader that cut the prefix off the new form would look up a
// session named INFRA.s1, which none is, and send the message down the unplaceable path (deferred,
// then given up with a no_holder exception and a wake for a session that does not exist) although
// s1's own consumer delivers it.
func TestAnAgentSubjectNamesItsSessionInEitherForm(t *testing.T) {
	for subject, want := range map[string]string{
		"notifications.agent.s1":       "s1",
		"notifications.agent._.s1":     "s1",
		"notifications.agent.INFRA.s1": "s1",
		"notifications.agent.ses-a-1":  "ses-a-1",
	} {
		if got := agentSubjectSession(subject); got != want {
			t.Errorf("agentSubjectSession(%q) = %q, want %q", subject, got, want)
		}
	}
}
