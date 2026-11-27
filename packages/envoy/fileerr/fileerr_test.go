package fileerr

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each class of a failed read, over a path made of a made-up credential: the words name the class
// and none of the path, and Err unwraps to the read's error.
func TestClassNamesNoPath(t *testing.T) {
	const pasted = "madeUpFileCredential0123456789"
	dir := t.TempDir()
	unreadable := filepath.Join(dir, pasted+"-unreadable")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, pasted+"-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"missing", filepath.Join(dir, pasted), "it does not exist"},
		{"a directory", directory, "it is a directory"},
		{"unreadable", unreadable, "permission is denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			_, err := os.ReadFile(tc.path)
			if err == nil {
				t.Fatalf("ReadFile(%s) read it", tc.name)
			}
			wrapped := Err(err)
			if got := wrapped.Error(); got != tc.want || Class(err) != tc.want || strings.Contains(got, pasted[:10]) {
				t.Fatalf("Err(%v) = %q, want %q", err, got, tc.want)
			}
			if !errors.Is(wrapped, err) {
				t.Fatalf("Err(%v) does not unwrap to it", err)
			}
		})
	}
	if _, err := os.ReadFile(filepath.Join(dir, pasted)); !errors.Is(Err(err), fs.ErrNotExist) {
		t.Fatal("Err(a missing file's error) is not fs.ErrNotExist")
	}
}
