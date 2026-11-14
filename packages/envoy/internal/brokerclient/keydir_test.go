// packages/envoy/internal/brokerclient/keydir_test.go
//
// What a drop of a key directory's machine session takes and what it leaves. `legion controller
// start` drops the one agent-secrets kept in the controller's key directory whenever a helper
// socket is set or present (R6), so it must take every file of that session and nothing else, and
// never touch a key that is not agent-secrets's machine session.

package brokerclient

import (
	"os"
	"path/filepath"
	"testing"
)

// keyDirWith writes files into a key dir of this test's own, each holding its own name, and
// answers the dir.
func keyDirWith(t *testing.T, files ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDropMachineSessionTakesEveryFileOfAKeptSessionAndNothingElse: a key dir holding a machine
// session agent-secrets kept — its key, its enrollment, a refused enrollment's error and the
// marker — loses all four and answers true, so the signer no longer takes the key and the next
// read goes to the helper; a file beside them that is no part of the session stays as it was.
func TestDropMachineSessionTakesEveryFileOfAKeptSessionAndNothingElse(t *testing.T) {
	dir := keyDirWith(t, KeyFile, EnrollmentFile, EnrollmentErrorFile, MachineSessionMarker, "notes.txt")

	dropped, err := DropMachineSession(dir)
	if err != nil || !dropped {
		t.Fatalf("DropMachineSession = %v, %v; want true, nil", dropped, err)
	}
	for _, name := range []string{KeyFile, EnrollmentFile, EnrollmentErrorFile, MachineSessionMarker} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still in the key dir (%v); want it dropped", name, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "notes.txt")); err != nil || string(got) != "notes.txt\n" {
		t.Errorf("notes.txt = %q (%v); want it left as it was", got, err)
	}
}

// TestDropMachineSessionLeavesAKeyWithNoMarker: a key and enrollment with no marker beside them
// are a launcher's, or the ones `agent-secrets enroll --pod` wrote, not a machine session
// agent-secrets kept, so the drop answers false and leaves both byte for byte.
func TestDropMachineSessionLeavesAKeyWithNoMarker(t *testing.T) {
	dir := keyDirWith(t, KeyFile, EnrollmentFile)

	dropped, err := DropMachineSession(dir)
	if err != nil || dropped {
		t.Fatalf("DropMachineSession = %v, %v; want false, nil", dropped, err)
	}
	for _, name := range []string{KeyFile, EnrollmentFile} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != name+"\n" {
			t.Errorf("%s = %q (%v); want it left as it was", name, got, err)
		}
	}
}

// TestDropMachineSessionAnswersFalseWhereThereIsNothing: an empty key dir, and one that does not
// exist, hold no session to drop, which is no failure; the empty one stays empty and the missing
// one is not made.
func TestDropMachineSessionAnswersFalseWhereThereIsNothing(t *testing.T) {
	empty, missing := keyDirWith(t), filepath.Join(t.TempDir(), "absent")
	for name, dir := range map[string]string{"empty": empty, "missing": missing} {
		if dropped, err := DropMachineSession(dir); err != nil || dropped {
			t.Errorf("DropMachineSession on a %s key dir = %v, %v; want false, nil", name, dropped, err)
		}
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Errorf("the empty key dir holds %v (%v); want nothing", entries, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("the drop made the missing key dir (%v); want it still missing", err)
	}
}
