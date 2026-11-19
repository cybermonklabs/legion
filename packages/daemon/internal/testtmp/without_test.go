package testtmp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fatalRecorder is a test whose Fatalf is recorded rather than run, so a test can read what
// without says when it refuses; everything else is the real test's.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }

// A base holding one of the characters is passed over for the next, and the directory made there
// holds none of them. Both bases sit under a directory Without made, since the run's own temporary
// directory may hold one of the characters itself.
func TestWithoutPassesOverABaseHoldingOneOfTheCharacters(t *testing.T) {
	root := Without(t, "", " '?")
	for _, name := range []string{"tmp with space", "tmp-q?x", "tmp-sq'x"} {
		t.Run(name, func(t *testing.T) {
			held, plain := filepath.Join(root, name), filepath.Join(root, "plain")
			if err := os.MkdirAll(held, 0o700); err != nil {
				t.Fatal(err)
			}
			dir := without(t, "", " '?", nil, nil, held, plain)
			if !strings.HasPrefix(dir, plain+string(filepath.Separator)) || strings.ContainsAny(dir, " '?") {
				t.Errorf("without(%s, %s) = %q, want a directory under %s", held, plain, dir, plain)
			}
		})
	}
}

// A base whose directory leaves no room for longest within a socket path is passed over too.
func TestWithoutPassesOverABaseTooLongForTheSocket(t *testing.T) {
	root := Without(t, "", " '?")
	long := filepath.Join(root, strings.Repeat("d", maxSocketPath))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if dir := without(t, "s.sock", " '?", nil, nil, long, root); !strings.HasPrefix(dir, root+string(filepath.Separator)) || strings.HasPrefix(dir, long) {
		t.Errorf("without(a %d-byte base) = %q, want a directory under %s", len(long), dir, root)
	}
}

// A base whose directory the caller's accept check refuses is passed over too, and the directory
// it made there is removed; the refusal is named when no base passes.
func TestWithoutPassesOverABaseTheAcceptCheckRefuses(t *testing.T) {
	root := Without(t, "", " '?")
	refused, plain := filepath.Join(root, "refused"), filepath.Join(root, "plain")
	accept := func(dir string) error {
		if strings.HasPrefix(dir, refused) {
			return errors.New("no hardlink lands here")
		}
		return nil
	}
	if dir := without(t, "", " '?", accept, nil, refused, plain); !strings.HasPrefix(dir, plain+string(filepath.Separator)) {
		t.Errorf("without(refused, plain) = %q, want a directory under %s", dir, plain)
	}
	if left, _ := os.ReadDir(refused); len(left) != 0 {
		t.Errorf("the refused base keeps %d entries, want the directory made there removed", len(left))
	}
	recorder := &fatalRecorder{TB: t}
	if dir := without(recorder, "", " '?", accept, nil, refused); dir != "" || !strings.Contains(recorder.fatal, refused+" made a directory that will not do (no hardlink lands here)") {
		t.Errorf("without(refused) = %q, failing with %q, want no directory and the refusal named", dir, recorder.fatal)
	}
}

// When every base holds one of the characters, the test fails, naming each base and the reason.
func TestWithoutFailsWhenEveryBaseHoldsOneOfTheCharacters(t *testing.T) {
	recorder := &fatalRecorder{TB: t}
	dir := without(recorder, "", " '?", nil, nil, "/a/tmp-q?x", "/b/legion's checkout/.cache/testtmp", "/c/tmp with space")
	if dir != "" {
		t.Errorf("without = %q, want none", dir)
	}
	for _, want := range []string{
		`no base gives a directory whose path holds none of " '?"`,
		`/a/tmp-q?x holds one of " '?"`, `/b/legion's checkout/.cache/testtmp holds one of " '?"`, `/c/tmp with space holds one of " '?"`,
	} {
		if !strings.Contains(recorder.fatal, want) {
			t.Errorf("the failure %q does not say %q", recorder.fatal, want)
		}
	}
}

// The base after a TMPDIR holding one of the characters is its closest ancestor holding none,
// which shares its filesystem as often as anything can; none is named for the root alone, or for
// a TMPDIR that needs no other base.
func TestCleanAncestorIsTheClosestDirectoryAboveTMPDIRHoldingNoneOfTheCharacters(t *testing.T) {
	for _, testCase := range []struct{ dir, want string }{
		{"/mnt/scratch/run/tmp-q?x", "/mnt/scratch/run"},
		{"/mnt/scratch/a b/tmp-q?x/deeper", "/mnt/scratch"},
		{"/mnt/scratch/run/plain", ""},
		{"/tmp-q?x", ""},
	} {
		if got := cleanAncestor(testCase.dir, " '?"); got != testCase.want {
			t.Errorf("cleanAncestor(%q) = %q, want %q", testCase.dir, got, testCase.want)
		}
	}
}

// WithoutAccepting itself, under a TMPDIR holding a '?', makes its directory under that TMPDIR's
// closest clean ancestor, ahead of the module's own .cache/testtmp.
func TestWithoutTakesTheClosestCleanAncestorOfTMPDIRNext(t *testing.T) {
	root := Without(t, "", "?")
	question := filepath.Join(root, "tmp-q?x")
	if err := os.Mkdir(question, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", question)
	if dir := Without(t, "", "?"); filepath.Dir(dir) != root {
		t.Errorf("Without under TMPDIR %s = %q, want a directory directly under %s", question, dir, root)
	}
}
