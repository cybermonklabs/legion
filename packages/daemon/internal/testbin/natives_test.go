package testbin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// standIn writes, as omp under dir, a script that embeds the natives list lines (in the format the
// real binary carries them) and runs body.
func standIn(t *testing.T, dir string, list []string, body string) string {
	t.Helper()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\n"
	for _, line := range list {
		script += "# " + line + "\n"
	}
	if err := os.WriteFile(omp, []byte(script+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return omp
}

// Of eight lookups that race for one binary's natives cache, as test binaries under `go test ./...`
// do, one runs omp and the rest find its copy; each HOME on the cache's filesystem then holds that
// copy's inode, as a hardlink and not a symlink, rather than bytes of its own, and the copy takes no
// write through a link. The staging directory of a filler that died is gone once the cache is
// filled. The omp here is a script that extracts the one natives file it embeds into its HOME, as
// the real binary does, and counts its runs.
func TestTheNativesCacheRunsOmpOnceAndEveryHomeLinksTheOneCopy(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, []string{`{ variant: "default", filename: "pi_natives.test.node", size: 6 },`},
		"echo run >>"+runs+"\nmkdir -p \"$HOME/.omp/natives/9.9.9\"\nprintf native >\"$HOME/.omp/natives/9.9.9/pi_natives.test.node\"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(root, digest+".fill-dead")
	if err := os.MkdirAll(filepath.Join(dead, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	caches, errs := make([]string, 8), make([]error, 8)
	var lookups sync.WaitGroup
	for i := range caches {
		lookups.Go(func() { caches[i], errs[i] = fillNativesCache(root, omp) })
	}
	lookups.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if caches[i] != caches[0] {
			t.Fatalf("lookups named %s and %s, want one cache", caches[0], caches[i])
		}
	}
	if got, err := os.ReadFile(runs); err != nil || string(got) != "run\n" {
		t.Fatalf("omp's runs read %q (%v), want one run", got, err)
	}
	if _, err := os.Stat(dead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the dead filler's staging directory %s stats %v, want it removed", dead, err)
	}

	cached, err := os.Stat(filepath.Join(caches[0], "9.9.9", "pi_natives.test.node"))
	if err != nil {
		t.Fatal(err)
	}
	if cached.Mode().Perm()&0o222 != 0 {
		t.Errorf("the cached natives file is %v, want it read-only", cached.Mode().Perm())
	}
	for _, home := range []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")} {
		if err := linkTree(caches[0], filepath.Join(home, ".omp", "natives"), os.Link); err != nil {
			t.Fatal(err)
		}
		linked, err := os.Lstat(filepath.Join(home, ".omp", "natives", "9.9.9", "pi_natives.test.node"))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(cached, linked) {
			t.Errorf("%s's natives file is %v and not the cache's inode, want a hardlink to the cache's", home, linked.Mode())
		}
	}
}

// A HOME on another filesystem than the cache (a TMPDIR on a data disk, the cache under $HOME)
// cannot take a hardlink: link(2) fails with EXDEV. Its native is then a symlink to the cached file,
// which the HOME reads whole with no byte of its own. The case makes link(2) fail as it does across
// filesystems, so it holds that branch on any machine; a link that fails for another reason still
// fails the HOME.
func TestANativesHomeOnAnotherFilesystemSymlinksTheCachedFile(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	cached := filepath.Join(cache, "9.9.9", "pi_natives.test.node")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("native"), 0o500); err != nil {
		t.Fatal(err)
	}
	failing := func(errno syscall.Errno) func(string, string) error {
		return func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
		}
	}

	natives := filepath.Join(dir, "home", ".omp", "natives")
	if err := linkTree(cache, natives, failing(syscall.EXDEV)); err != nil {
		t.Fatalf("linkTree across filesystems = %v, want a symlink to the cache's file", err)
	}
	home := filepath.Join(natives, "9.9.9", "pi_natives.test.node")
	if info, err := os.Lstat(home); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("the HOME's native lstats %v (%v), want a symlink", info, err)
	}
	if target, err := os.Readlink(home); err != nil || target != cached {
		t.Errorf("the HOME's native links to %q (%v), want %q", target, err, cached)
	}
	if got, err := os.ReadFile(home); err != nil || string(got) != "native" {
		t.Errorf("the HOME's native reads %q (%v), want the cached bytes", got, err)
	}

	if err := linkTree(cache, filepath.Join(dir, "other", ".omp", "natives"), failing(syscall.EACCES)); !errors.Is(err, syscall.EACCES) {
		t.Errorf("linkTree whose link fails with EACCES = %v, want that error", err)
	}
}

// A fill holding a file that is not what the binary embeds at its embedded size is refused and
// caches nothing; the next whole fill is cached. A binary that writes only one of the variants it
// embeds, which the pin does, is a whole fill. The stand-in writes its native short on the first
// run and whole after.
func TestANativesFillShortOfAnEmbeddedSizeCachesNothing(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, []string{
		`{ variant: "modern", filename: "pi_natives.test-modern.node", size: 6 },`,
		`{ variant: "baseline", filename: "pi_natives.test-baseline.node", size: 8 },`,
	}, "natives=\"$HOME/.omp/natives/9.9.9\"\nmkdir -p \"$natives\"\n"+
		"if [ -e "+runs+" ]; then printf modern >\"$natives/pi_natives.test-modern.node\"; "+
		"else printf mod >\"$natives/pi_natives.test-modern.node\"; fi\necho run >>"+runs+"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fillNativesCache(root, omp); err == nil || !strings.Contains(err.Error(), "embeds pi_natives.test-modern.node at 6 bytes, but extracted it at 3") {
		t.Fatalf("the fill whose extraction is short of the modern file's size = %v, want it refused naming that file and both sizes", err)
	}
	if _, err := os.Stat(filepath.Join(root, digest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the refused fill the cache stats %v, want no cache", err)
	}
	cache, err := fillNativesCache(root, omp)
	if err != nil {
		t.Fatalf("the fill whose extraction holds the modern file whole = %v, want it cached", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "9.9.9", "pi_natives.test-modern.node")); err != nil {
		t.Errorf("the cache lacks the variant the binary extracted: %v", err)
	}
}

// A file under natives that is not a native the binary embeds is refused by its name. omp writes
// each native to a `.tmp.<pid>.<uuid>` file and renames it once it is whole, so a write that failed
// and whose cleanup failed too leaves that file in place of its native.
func TestANativesFillHoldingALeftoverTemporaryFileCachesNothing(t *testing.T) {
	dir := t.TempDir()
	omp := standIn(t, dir, []string{
		`{ variant: "default", filename: "pi_natives.test.node", size: 6 },`,
	}, "natives=\"$HOME/.omp/natives/9.9.9\"\nmkdir -p \"$natives\"\n"+
		"printf nat >\"$natives/pi_natives.test.node.tmp.1.2\"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fillNativesCache(root, omp); err == nil || !strings.Contains(err.Error(), "extracted pi_natives.test.node.tmp.1.2 under") {
		t.Fatalf("the fill holding a leftover temporary file = %v, want it refused naming that file", err)
	}
	if _, err := os.Stat(filepath.Join(root, digest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the refused fill the cache stats %v, want no cache", err)
	}
}

// A fill that writes no native at all, as a failed write and its cleanup leave it.
func TestANativesFillThatExtractsNothingCachesNothing(t *testing.T) {
	dir := t.TempDir()
	omp := standIn(t, dir, []string{
		`{ variant: "default", filename: "pi_natives.test.node", size: 6 },`,
	}, "mkdir -p \"$HOME/.omp/natives/9.9.9\"\n")
	root := filepath.Join(dir, "cache")
	digest, _, err := readBinary(omp)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fillNativesCache(root, omp); err == nil || !strings.Contains(err.Error(), "extracted no native under") {
		t.Fatalf("the fill whose extraction wrote nothing = %v, want it refused", err)
	}
	if _, err := os.Stat(filepath.Join(root, digest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the refused fill the cache stats %v, want no cache", err)
	}
}

// A binary that lists no embedded natives is refused before it runs: with nothing to check an
// extraction against, every file it wrote would be refused by name anyway, so refusing first fails
// with a precise message and never runs omp to extract a native of about 125 MiB.
func TestANativesFillRefusesABinaryThatListsNoEmbeddedNatives(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	omp := standIn(t, dir, nil, "echo run >>"+runs+"\nmkdir -p \"$HOME/.omp/natives/9.9.9\"\nprintf native >\"$HOME/.omp/natives/9.9.9/pi_natives.test.node\"\n")
	if _, err := fillNativesCache(filepath.Join(dir, "cache"), omp); err == nil || !strings.Contains(err.Error(), "lists no embedded natives") {
		t.Fatalf("the fill of a binary that lists no natives = %v, want it refused", err)
	}
	if _, err := os.Stat(runs); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused binary ran (%v), want it never run", err)
	}
}
