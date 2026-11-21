// Ordinary, always-visible unit tests for the Stage 4a agent-secrets checks' pure logic that
// live_secrets_pure_test.go defines with no e2e build tag: shellJoin's shell quoting. The
// e2e-tagged checks it backs (checkSecretsTwoPodsEnrolled and friends) need a real cluster and a
// real broker and stay untestable here.
package sandbox

import "testing"

func TestShellJoin(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no args":              {nil, ""},
		"plain args":           {[]string{"self", "--json"}, "'self' '--json'"},
		"single quote in arg":  {[]string{"it's"}, `'it'\''s'`},
		"arg with a space":     {[]string{"s4a cross-issue probe"}, "'s4a cross-issue probe'"},
		"empty string arg":     {[]string{""}, "''"},
		"shell metacharacters": {[]string{"$(rm -rf /)", "a;b"}, `'$(rm -rf /)' 'a;b'`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shellJoin(tc.args); got != tc.want {
				t.Fatalf("shellJoin(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
