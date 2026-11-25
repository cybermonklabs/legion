package main

import (
	"strings"
	"testing"
)

// Dev sign-in signs in whoever a loopback client names, as header identity takes whoever a header
// names, so an outside origin beside it would hand the address people from outside reach to a
// sign-in that proves nobody (R6). Boot refuses the pair naming DISPATCH_OUTSIDE_ORIGIN, as it
// refuses it beside header identity (TestOutsideSignInOriginIsRefusedBesideHeaderIdentity). The
// control: the same dev sign-in with no outside origin still boots.
func TestOutsideSignInOriginIsRefusedBesideDevSignIn(t *testing.T) {
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_TEST_HOOKS":     "1",
		"DISPATCH_OUTSIDE_ORIGIN": "http://localhost:9031",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_OUTSIDE_ORIGIN") {
		t.Fatalf("resolveBootConfig with DISPATCH_OUTSIDE_ORIGIN beside dev sign-in: err = %v, want a refusal naming DISPATCH_OUTSIDE_ORIGIN", err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_TEST_HOOKS": "1"})); err != nil {
		t.Fatalf("resolveBootConfig with dev sign-in and no outside origin: %v", err)
	}
}
