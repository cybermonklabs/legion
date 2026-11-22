package testbin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtmp"
)

// OMPDir is a fresh directory of the test's own, removed when the test ends, for every path a test
// hands the real Oh My Pi omp: its HOME, the plugin roots, a repository, a working directory. It
// also points TMPDIR at the directory for the rest of the test, so a file the code under test makes
// for Oh My Pi with os.MkdirTemp("", …), such as the boot gate's load probe, lands there too.
//
// It works around a limit of Bun, which Oh My Pi runs on: Bun loads no module from a path that
// holds a '?', in any form, so under such a path the pinned Oh My Pi loads neither its natives nor
// an extension (cybermonklabs/oh-my-pi#46 fixes the natives; an extension still cannot load). The
// directory is
// testtmp.WithoutAccepting's for that one character: under os.TempDir() when it holds no '?', else
// under its closest ancestor that holds none, else the module's own .cache/testtmp, else /tmp. Each
// base must also take a hardlink from omp's natives cache, since OMPHome links the natives into a
// HOME made under the directory and a link cannot cross filesystems. The test fails, naming what
// each base answered, when none qualifies; it never skips.
func OMPDir(t testing.TB, omp string) string {
	t.Helper()
	dir := testtmp.WithoutAccepting(t, "", "?", func(dir string) error { return nativesLinkInto(omp, dir) })
	t.Setenv("TMPDIR", dir)
	return dir
}

// nativesLinkInto answers whether OMPHome can link omp's natives into a HOME under dir, by linking
// one file of the natives cache there and removing the link.
func nativesLinkInto(omp, dir string) error {
	cache, err := nativesCache(omp)
	if err != nil {
		return fmt.Errorf("the natives cache for %s: %w", omp, err)
	}
	var file string
	if err := filepath.WalkDir(cache, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			file = path
			return fs.SkipAll
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the natives cache %s: %w", cache, err)
	}
	if file == "" {
		return fmt.Errorf("the natives cache %s holds no file", cache)
	}
	probe := filepath.Join(dir, ".natives-link-probe")
	if err := os.Link(file, probe); err != nil {
		return fmt.Errorf("no link from the natives cache %s lands here: %w", cache, err)
	}
	return os.Remove(probe)
}
