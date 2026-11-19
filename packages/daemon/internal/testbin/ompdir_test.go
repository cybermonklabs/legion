package testbin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// Under a TMPDIR that holds a '?', OMPDir makes its directory where the path holds none, and
// TMPDIR names that directory for the rest of the test, so the files the code under test makes for
// Oh My Pi land there too.
func TestOMPDirPassesOverATemporaryDirectoryHoldingAQuestionMark(t *testing.T) {
	question := filepath.Join(testtmp.Without(t, "", "?"), "tmp-q?x")
	if err := os.Mkdir(question, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", question)
	dir := OMPDir(t, OMP(t))
	if strings.Contains(dir, "?") {
		t.Errorf("OMPDir under TMPDIR %s = %q, which holds a '?'", question, dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("OMPDir = %q, which is no directory: %v", dir, err)
	}
	if got := os.Getenv("TMPDIR"); got != dir {
		t.Errorf("TMPDIR = %q, want the directory %s", got, dir)
	}
}
