package policy_test

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// TestARestrictedMemberIsDeniedEveryProfileNameBeforeAnyProfileAnswer: a requester Dispatch calls
// a restricted member is denied every name the profiles file answers, restricted-member, as it is
// denied a stored secret, and before the file says anything of the name: a profile the set serves
// (GITHUB_READ), a source served only through its profiles (LEGION_IMPLEMENT_APP), a profile that
// fails against its source (GHOST_READ) and a github-app secret no profile names (HUMAN_APP). Each
// answer but the served profile's would otherwise name profiles, sources or why one fails, which
// is nothing a restricted member's session is told. The control is the same requester unmarked,
// who gets each name's own answer, and a name nothing in the store or the file holds is
// ErrUnknownSecret to both, as a stored name the set does not serve is.
func TestARestrictedMemberIsDeniedEveryProfileNameBeforeAnyProfileAnswer(t *testing.T) {
	const outsider = "outsider@outside.example"
	cur, _ := profiled(t, everySource(), readProfile,
		`{"name": "GHOST_READ", "source": "NO_SUCH_APP", `+acmeRead+`, `+apiProxy+`}`)
	set := cur.Get()
	unmarked := policy.Requester{Operator: outsider}
	restricted := policy.Requester{Operator: outsider, Restricted: true}

	served := servedProfile(t, set, "GITHUB_READ")
	if got, err := set.Evaluate("GITHUB_READ", unmarked); err != nil || got.Reason == policy.ReasonRestrictedMember {
		t.Errorf("Evaluate(GITHUB_READ, unmarked) = %+v, %v; want the profile's own answer", got, err)
	}
	for _, row := range []struct {
		name, source string
		unmarked     error
	}{
		{"GITHUB_READ", served.Source, nil},
		{appSourceName, "", policy.ErrServedOnlyAsProfiles},
		{"GHOST_READ", "", policy.ErrProfileRefused},
		{"HUMAN_APP", "", policy.ErrProfileRefused},
	} {
		if row.unmarked != nil {
			if _, err := set.Evaluate(row.name, unmarked); !errors.Is(err, row.unmarked) {
				t.Errorf("Evaluate(%s, unmarked) = %v; want %v", row.name, err, row.unmarked)
			}
		}
		want := policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonRestrictedMember, Source: row.source}
		if got, err := set.Evaluate(row.name, restricted); err != nil || got != want {
			t.Errorf("Evaluate(%s, a restricted member) = %+v, %v; want %+v", row.name, got, err, want)
		}
	}
	for _, r := range []policy.Requester{unmarked, restricted} {
		if _, err := set.Evaluate("NOT_A_SECRET", r); !errors.Is(err, policy.ErrUnknownSecret) || errors.Is(err, policy.ErrProfileRefused) {
			t.Errorf("Evaluate(NOT_A_SECRET, restricted %t) = %v; want ErrUnknownSecret alone", r.Restricted, err)
		}
	}
}
