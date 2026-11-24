package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A slot holds the App of the role its caller passes SlotRole — the role the slot's process was
// launched as, or the claim's stored role where no record of that launch stands — here tester,
// which key a's claim `…-implementer` took for its tester step, and not the slot's name. Where the
// runtime is given an ActsAs, the role it gives the token wins; one that gives none leaves the
// role passed, and one that fails is that failure.
func TestASlotsRoleIsTheRoleOfTheStepThatItsClaimDoesNow(t *testing.T) {
	ctx := context.Background()
	const token claim.Token = "legion-legion-infra-12-implementer"
	refused := errors.New("the record could not be read")
	for _, tc := range []struct {
		name    string
		actsAs  ActsAs
		want    claim.Role
		wantErr error
	}{
		{"no ActsAs", nil, claim.RoleTester, nil},
		{"an ActsAs that gives a role", func(_ context.Context, got claim.Token) (claim.Role, bool, error) {
			if got != token {
				t.Errorf("ActsAs asked of %s, want %s", got, token)
			}
			return claim.RoleImplementer, true, nil
		}, claim.RoleImplementer, nil},
		{"an ActsAs that gives none", func(context.Context, claim.Token) (claim.Role, bool, error) {
			return "", false, nil
		}, claim.RoleTester, nil},
		{"an ActsAs that fails", func(context.Context, claim.Token) (claim.Role, bool, error) {
			return "", false, refused
		}, "", refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SlotRole(ctx, tc.actsAs, token, claim.RoleTester)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("SlotRole = %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
