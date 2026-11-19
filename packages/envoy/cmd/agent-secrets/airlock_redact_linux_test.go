// packages/envoy/cmd/agent-secrets/airlock_redact_linux_test.go
//
// A write of a grant's airlock files that fails, made to fail the one way an unprivileged test can
// count on: Linux refuses a path of PATH_MAX (4096) bytes or more with ENAMETOOLONG, so a root
// whose build directory still fits, and whose files do not, fails the first file write and
// nothing before it.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnAirlockFileThatCannotBeWrittenNamesItsPathButNotWGETRCs: when the trust bundle cannot be
// written, the refusal names the file's path under the airlock root and the system's reason: the
// root is XDG_RUNTIME_DIR and names this CLI made below it, none of which a person sets to a
// secret's file. The WGETRC the command inherits names a file that can hold http_password, so
// neither its path nor what it holds prints. A root whose files fit still takes them: the
// control.
func TestAnAirlockFileThatCannotBeWrittenNamesItsPathButNotWGETRCs(t *testing.T) {
	wgetrc, secrets := inheritedWgetrc(t)
	// The root is base/agent-secrets/airlock; its build directory, root/.building-<1 to 10
	// digits>, is at most len(root)+21 bytes, and the trust bundle's temporary file in it at least
	// len(root)+29. A root of 4070 bytes keeps the first under 4096 and puts the second over it.
	const rootLen = 4070
	base := filepath.Join(t.TempDir(), "runtime")
	for remaining := rootLen - len("/agent-secrets/airlock") - len(base); remaining > 0; remaining = rootLen - len("/agent-secrets/airlock") - len(base) {
		n := remaining - 1
		if remaining > 201 {
			n = 150
		}
		base += "/" + strings.Repeat("d", n)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + base, wgetrc}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
	if err == nil {
		t.Fatal("prepareAirlock wrote the trust bundle at a path longer than PATH_MAX")
	}
	building := filepath.Join(base, "agent-secrets", "airlock", airlockBuildPrefix)
	wantNamedNotPrinted(t, "the refusal", err.Error(), []string{"write the trust bundle", building, "file name too long"}, secrets...)

	t.Run("control: a root whose files fit", func(t *testing.T) {
		fits := filepath.Join(t.TempDir(), "runtime")
		if err := os.MkdirAll(fits, 0o700); err != nil {
			t.Fatal(err)
		}
		files, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + fits, wgetrc}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", nil)
		if err != nil {
			t.Fatalf("prepareAirlock: %v", err)
		}
		if _, err := os.Stat(files.bundle); err != nil {
			t.Fatalf("the trust bundle: %v", err)
		}
	})
}
