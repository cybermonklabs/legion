package ompsessions

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadDSN's refusals name neither the path nor what the file holds: a made-up database URL
// pasted where the file's path goes is a file that does not exist, refused by why alone and still
// errors.Is fs.ErrNotExist, and an empty file is refused as one.
func TestReadDSNNamesNoPath(t *testing.T) {
	const pasted = "postgres:madeUpDsnPassword0123456789"
	_, err := ReadDSN(filepath.Join(t.TempDir(), pasted))
	if want := "read the database URL file: it does not exist"; err == nil || err.Error() != want {
		t.Fatalf("ReadDSN(a missing file) = %v, want %q", err, want)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDSN(a missing file) = %v, which is not fs.ErrNotExist", err)
	}
	empty := filepath.Join(t.TempDir(), pasted+"-empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadDSN(empty)
	if want := "the database URL file is empty"; err == nil || err.Error() != want {
		t.Fatalf("ReadDSN(an empty file) = %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), pasted[:10]) {
		t.Fatalf("ReadDSN = %v, which names the path", err)
	}
}

// A session database URL whose made-up password leaves part of itself after pgx's user part (a
// password holding an unencoded "@", one holding an "@" and then a "?", or a "/" after digits) is
// refused by the file alone, before any connection names that part.
func TestReadDSNRefusesAURLWhosePasswordSpills(t *testing.T) {
	for name, tc := range map[string]struct{ dsn, spilled string }{
		"a password holding an @":             {"postgres://sessions:madeuphead0123" + "@madeuptail0123@db.invalid/sessions", "madeuptail"},
		"a password holding an @ and then ?":  {"postgres://sessions:madeuphead0123" + "@madeuptail0123?zqtwo@db.invalid/sessions", "madeuptail"},
		"a password holding a / after digits": {"postgres://sessions:48213579/madeuptail0123" + "@db.invalid/sessions", "48213579"},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "session-dsn")
			if err := os.WriteFile(file, []byte(tc.dsn+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadDSN(file)
			if err == nil || !strings.HasPrefix(err.Error(), `the database URL file cannot be read: it holds an "@" after its user name and password`) {
				t.Fatalf("ReadDSN = %v, want the file refused by its name", err)
			}
			for _, part := range []string{tc.spilled, "madeuphead", "madeuptail"} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("ReadDSN = %v, which holds %q", err, part)
				}
			}
		})
	}
}
