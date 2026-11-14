package lookpath

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// On resolves an executable on the PATH it is given, skipping a relative element (as
// exec.LookPath refuses the result it would give), an empty one, a directory sharing the name,
// and a file without an execute bit; among several matches the first element wins; a name on no
// element answers false.
func TestOnReadsThePATHItIsGiven(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "notes"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	t.Chdir(workdir)
	if err := os.MkdirAll(filepath.Join(workdir, "relative", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "relative", "bin", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	searchPath := "relative/bin:" + bin
	if got, ok := On(searchPath, "tool"); !ok || got != filepath.Join(bin, "tool") {
		t.Errorf("On(tool) = %q, %t, want %s", got, ok, filepath.Join(bin, "tool"))
	}
	for _, name := range []string{"notes", "dir", "sh"} {
		if got, ok := On(searchPath, name); ok {
			t.Errorf("On(%s) = %q, want none: not an executable on the given PATH", name, got)
		}
	}
	if _, ok := On("", "tool"); ok {
		t.Error("On found tool on an empty PATH")
	}
	if got, ok := On("relative/bin", "tool"); ok {
		t.Errorf("On(relative/bin) = %q, %t, want none: a relative PATH element alone is skipped even though it resolves", got, ok)
	}
	absBin := filepath.Join(workdir, "relative", "bin")
	if got, ok := On(absBin, "tool"); !ok || got != filepath.Join(absBin, "tool") {
		t.Errorf("On(%s) = %q, %t, want %s", absBin, got, ok, filepath.Join(absBin, "tool"))
	}
}

// Among two elements both carrying the name, the first wins.
func TestOnPrefersTheFirstMatchingElement(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	searchPath := first + string(filepath.ListSeparator) + second
	if got, ok := On(searchPath, "tool"); !ok || got != filepath.Join(first, "tool") {
		t.Errorf("On(tool) = %q, %t, want the first element's %s", got, ok, filepath.Join(first, "tool"))
	}
}

// Others lists every executable of the name in PATH order with its entry's index, leaving out
// every entry that is the directory own, a symbolic link to it included, and answers own's first
// index. An empty entry, a file with no execute bit and a directory of the name are left out, and a
// relative entry is kept.
func TestOthersListsEveryExecutableOutsideOneDirectory(t *testing.T) {
	own, first, second, plain := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{own, first, second} {
		if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(plain, "tool"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "own-link")
	if err := os.Symlink(own, alias); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	t.Chdir(workdir)
	if err := os.MkdirAll(filepath.Join(workdir, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "rel", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, ownAt := Others(first+"::"+alias+":"+plain+":"+own+":rel:"+second, "tool", own)
	var got []string
	for _, f := range found {
		got = append(got, strconv.Itoa(f.Index)+" "+f.Path)
	}
	want := []string{"0 " + first + "/tool", "5 rel/tool", "6 " + second + "/tool"}
	if !slices.Equal(got, want) || ownAt != 2 {
		t.Errorf("Others = %q, own at %d; want %q, own at 2", got, ownAt, want)
	}
	if _, ownAt := Others(first, "tool", own); ownAt != -1 {
		t.Errorf("Others of a PATH without own answers own at %d, want -1", ownAt)
	}
}

// Kind reads a `#!` script, an ELF program, a file that is neither, and a file shorter than either
// mark, and refuses a file it cannot open.
func TestKindReadsTheFirstBytes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, head       string
		script, compiled bool
	}{
		{"script", "#!/bin/sh\n", true, false},
		{"elf", "\x7fELF\x02\x01\x01", false, true},
		{"mach-o", "\xcf\xfa\xed\xfe\x07", false, true},
		{"plain", "exec git \"$@\"\n", false, false},
		{"short", "#", false, false},
		{"empty", "", false, false},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte(tc.head), 0o755); err != nil {
			t.Fatal(err)
		}
		script, compiled, err := Kind(path)
		if err != nil || script != tc.script || compiled != tc.compiled {
			t.Errorf("Kind(%s) = %t, %t, %v; want %t, %t", tc.name, script, compiled, err, tc.script, tc.compiled)
		}
	}
	if _, _, err := Kind(filepath.Join(dir, "missing")); err == nil {
		t.Error("Kind of a missing file answered no error")
	}
}
