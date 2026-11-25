package main

import (
	"testing"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
)

// TestAClaimLineNamesATokenOnlyInAShapeTheDaemonMakes: a line names a claim token of the daemon's
// answer only where it is in a shape the daemon makes one in (legionclaim.Token.Shaped): a role's
// token and the controller's print as they are, and a token that ends in a role but names no
// issue, which no token the daemon makes is, prints as words saying it has no claim token's shape
// (issue-less), where the command's own copy of the grammar took it.
func TestAClaimLineNamesATokenOnlyInAShapeTheDaemonMakes(t *testing.T) {
	const unshaped = "a claim whose token has no claim token's shape"
	for _, c := range []struct {
		name  string
		token legionclaim.Token
		want  string
	}{
		{"role", architectClaim, string(architectClaim)},
		{"controller", legionclaim.ControllerToken("legion"), "legion-legion-controller"},
		{"issue-less", "legion-legion-architect", unshaped},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := claimWord(c.token); got != c.want {
				t.Errorf("claimWord(%q) = %q, want %q", c.token, got, c.want)
			}
		})
	}
}
