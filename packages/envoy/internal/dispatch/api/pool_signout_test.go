package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// poolSignOutLead leads the runbook's block that signs every user of the Cognito pool out, on the
// move from the pool to a domain.
const poolSignOutLead = "Only once that call has succeeded, sign every user of the pool out."

// awsStandIn is an aws stand-in for the pool's two calls. list-users answers three users, tab
// separated as --output text writes them, or fails when LIST_FAILS is set. admin-user-global-sign-out
// reads its stdin, as a CLI may, records the user in SIGNED_OUT, and fails for FAIL_USER.
const awsStandIn = `#!/bin/sh
case "$2" in
list-users)
  if [ -n "$LIST_FAILS" ]; then echo 'An error occurred (AccessDeniedException) when calling the ListUsers operation' >&2; exit 254; fi
  printf 'user-1\tuser-2\tuser-3\n'
  ;;
admin-user-global-sign-out)
  cat >/dev/null
  user=
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --username ]; then user=$2; fi
    shift
  done
  echo "$user" >>"$SIGNED_OUT"
  if [ "$user" = "$FAIL_USER" ]; then echo 'An error occurred (TooManyRequestsException)' >&2; exit 254; fi
  ;;
*)
  echo "unexpected aws call: $*" >&2; exit 2
  ;;
esac
`

// TestThePoolSignOutBlockReportsAnUnlistedPoolAndEveryFailedSignOut runs the runbook's sign-out of
// the pool's users with an aws stand-in. A pool it cannot list signs no one out and fails the block
// with UNKNOWN; a failed sign-out is named with NOT SIGNED OUT and fails the block after every other
// user is signed out; and the done line comes only when every call succeeded. The stand-in's
// sign-out reads stdin, so a loop that fed it the list on stdin would sign out only the first user.
func TestThePoolSignOutBlockReportsAnUnlistedPoolAndEveryFailedSignOut(t *testing.T) {
	for _, tool := range []string{"bash", "tr"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed, so the pool's sign-out block cannot be run here: %v", tool, err)
		}
	}
	const done = "every user of the pool is signed out"
	block := strings.Join(runbookBlockAfter(t, poolSignOutLead).lines, "\n") + "\n"
	all := []string{"user-1", "user-2", "user-3"}
	for _, test := range []struct {
		name, listFails, failUser string
		wantFailure               bool
		wantOutput                []string
		wantSignedOut             []string
	}{
		{name: "the pool cannot be listed", listFails: "1", wantFailure: true,
			wantOutput: []string{"UNKNOWN: the pool could not be listed, so no user was signed out"}},
		{name: "one sign-out fails", failUser: "user-2", wantFailure: true,
			wantOutput: []string{"NOT SIGNED OUT user-2"}, wantSignedOut: all},
		{name: "every sign-out succeeds", wantOutput: []string{done}, wantSignedOut: all},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			stubs := filepath.Join(dir, "bin")
			if err := os.Mkdir(stubs, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubs, "aws"), []byte(awsStandIn), 0o700); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "signout.sh")
			if err := os.WriteFile(script, []byte(block), 0o700); err != nil {
				t.Fatal(err)
			}
			signedOutFile := filepath.Join(dir, "signed-out")
			command := exec.Command("bash", script)
			command.Env = append(os.Environ(),
				"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
				"SIGNED_OUT="+signedOutFile, "LIST_FAILS="+test.listFails, "FAIL_USER="+test.failUser)
			raw, err := command.CombinedOutput()
			output := string(raw)
			if failed := err != nil; failed != test.wantFailure {
				t.Errorf("the pool's sign-out block exited with %v, want failure %v:\n%s", err, test.wantFailure, output)
			}
			for _, want := range test.wantOutput {
				if !strings.Contains(output, want) {
					t.Errorf("the pool's sign-out block printed:\n%s\nwant %q", output, want)
				}
			}
			if test.wantFailure && strings.Contains(output, done) {
				t.Errorf("the pool's sign-out block printed %q after a failure:\n%s", done, output)
			}
			for _, user := range all {
				if user != test.failUser && strings.Contains(output, "NOT SIGNED OUT "+user) {
					t.Errorf("the pool's sign-out block named %s, whose sign-out succeeded:\n%s", user, output)
				}
			}
			var signedOut []string
			if text, err := os.ReadFile(signedOutFile); err == nil {
				signedOut = strings.Fields(string(text))
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if !slices.Equal(signedOut, test.wantSignedOut) {
				t.Errorf("the pool's sign-out block signed out %q, want %q:\n%s", signedOut, test.wantSignedOut, output)
			}
		})
	}
}
