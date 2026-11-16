package identity

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

// The person a Cognito username carries is its email with the ASCII letters lowered and nothing
// else folded: Unicode lowers U+212A KELVIN SIGN to k, so a Unicode fold would sign the owner of
// "\u212aate@d.example" in as kate@d.example, a person that address does not name.
func TestPersonLowersTheUsernamesASCIILettersAlone(t *testing.T) {
	for _, tc := range []struct{ name, username, want string }{
		{"ASCII capitals are lowered", "ExampleIdP_Kate@D.example", "kate@d.example"},
		{"a Kelvin sign is not the k of the listed address", "ExampleIdP_\u212aate@d.example", "\u212aate@d.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			person, err := dispatchMembers.Person(context.Background(), oidc.Claims{
				Username:  tc.username,
				Groups:    []string{"dispatch-members"},
				Providers: []string{"ExampleIdP"},
			})
			if err != nil || person.Email != tc.want {
				t.Fatalf("Person(%q) = %q, %v; want %q", tc.username, person.Email, err, tc.want)
			}
		})
	}
}
