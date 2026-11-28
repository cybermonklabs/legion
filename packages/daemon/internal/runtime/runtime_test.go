package runtime_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The table of every role's identity reads back as it was written: one JSON object, keys sorted,
// each value the role's name and email. The reader passes over a field of an identity it does not
// know, since a role's image may run a `legion` of another release than the daemon's, and refuses
// every other value it cannot use with a reason that names none of the value's names or emails.
func TestRoleIdentitiesReadBackAndPassOverAnUnknownField(t *testing.T) {
	identities := map[claim.Role]runtime.GitIdentity{
		claim.RoleReviewer:    {Name: "rev", Email: "rev@x"},
		claim.RoleImplementer: {Name: "impl", Email: "impl@x"},
	}
	const written = `{"implementer":{"name":"impl","email":"impl@x"},"reviewer":{"name":"rev","email":"rev@x"}}`
	if got := runtime.RoleIdentities(identities); got != written {
		t.Fatalf("RoleIdentities = %s, want %s", got, written)
	}
	if read, why := runtime.ReadRoleIdentities(written); why != "" || !maps.Equal(read, identities) {
		t.Fatalf("ReadRoleIdentities(%s) = %v, %q; want %v", written, read, why, identities)
	}
	const later = `{"implementer":{"name":"a","email":"b","login":"c"}}`
	want := map[claim.Role]runtime.GitIdentity{claim.RoleImplementer: {Name: "a", Email: "b"}}
	if read, why := runtime.ReadRoleIdentities(later); why != "" || !maps.Equal(read, want) {
		t.Fatalf("ReadRoleIdentities(%s) = %v, %q; want %v: a field it does not know is passed over", later, read, why, want)
	}

	const notTable = "not a JSON object of each role's name and email"
	for _, tc := range []struct{ value, want string }{
		{"", "empty"},
		{"implementer=legion-implementer[bot]", notTable},
		{"null", notTable},
		{`["implementer"]`, notTable},
		{`{"implementer":{"name":"secret-name","email":"secret@x"}} {}`, notTable},
		{`{"implementer":"secret-name"}`, notTable},
		{`{"wizard":{"name":"secret-name","email":"secret@x"}}`, `a table with "wizard", which is no role`},
		{`{"implementer":null}`, "a table with an empty name for implementer"},
		{`{"implementer":{"name":"","email":"secret@x"}}`, "a table with an empty name for implementer"},
		{`{"implementer":{"name":"secret-name","email":""}}`, "a table with an empty email for implementer"},
		{`{"implementer":{"name":"secret-name\nLegion-Role: reviewer","email":"secret@x"}}`, "a table with a line break in the name of implementer"},
		{`{"implementer":{"name":"secret-name","email":"secret@x\r"}}`, "a table with a line break in the email of implementer"},
	} {
		read, why := runtime.ReadRoleIdentities(tc.value)
		if why != tc.want || read != nil {
			t.Errorf("ReadRoleIdentities(%q) = %v, %q; want no table and %q", tc.value, read, why, tc.want)
		}
		if strings.Contains(why, "secret") {
			t.Errorf("ReadRoleIdentities(%q)'s reason %q names a value of the table", tc.value, why)
		}
	}
}

// The account drift's reason reads back through its reader as it was written
// (runtime.AccountDriftReason, runtime.AccountDrift): out of the reason alone, and out of a
// round's recorded reasons (runtime.RoundReasons), where the mark puts it first and another reason
// follows it. The text is pinned whole, since an operator reads it in a round's lines and notices.
// A reason with no mark that displays the same words, and a round with no account drift, read as
// none.
func TestTheAccountDriftReasonReadsBackFromARoundsReasons(t *testing.T) {
	reason := runtime.AccountDriftReason("default", "legion-worker")
	if want := "[breaks the pod] account default, now legion-worker"; reason != want {
		t.Fatalf("AccountDriftReason = %q, want %q", reason, want)
	}
	if !runtime.Breaks(reason) {
		t.Fatalf("Breaks(%q) = false, want true: a pod on an account the operator replaced breaks", reason)
	}
	for name, reasons := range map[string]string{
		"the reason alone":           reason,
		"a round's recorded reasons": runtime.RoundReasons([]string{"image legion:1, now legion:2", reason, "leak check old, now new"}),
		"the reason behind another":  runtime.RoundReasons([]string{runtime.BreakingDrift + "agent-secrets-exchange (unset), now set", reason}),
	} {
		if old, next, ok := runtime.AccountDrift(reasons); !ok || old != "default" || next != "legion-worker" {
			t.Errorf("%s: AccountDrift(%q) = %q, %q, %t, want default, legion-worker, true", name, reasons, old, next, ok)
		}
	}
	for name, reasons := range map[string]string{
		"no drift":                        "",
		"no account drift":                runtime.RoundReasons([]string{"image legion:1, now legion:2"}),
		"the words with no mark in front": "image account default, now legion-worker",
	} {
		if old, next, ok := runtime.AccountDrift(reasons); ok {
			t.Errorf("%s: AccountDrift(%q) = %q, %q, true, want no account drift", name, reasons, old, next)
		}
	}
}
