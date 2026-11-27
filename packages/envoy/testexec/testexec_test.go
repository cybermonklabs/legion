package testexec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// writeUnder writes body to path under root, at 0o644, making the directories it needs: the one
// way the guard's tests write a fixture's files.
func writeUnder(t *testing.T, root, path, body string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAnExecutableWrittenWhileTheProcessForksRuns is c-exec's guarantee: a program WriteFile writes,
// and a test then runs, is not refused as busy (ETXTBSY) while the process forks, because WriteFile
// takes the fork lock through programfile.Write. RunsBeside forks true beside 400 writes-and-runs and
// fails on any ETXTBSY (it skips under gVisor, where the kernel refuses no such run, and fails under
// GITHUB_ACTIONS if even the probe run is not refused, a broken runner). If WriteFile ever wrote with
// os.WriteFile again, every fake command a test writes with it would be back in the flake this pull
// request exists to remove, and nothing else would catch it: the lock's other holder, programfile.Write,
// has its own guard, but WriteFile's use of it is guarded only here. This is the test c-exec names.
func TestAnExecutableWrittenWhileTheProcessForksRuns(t *testing.T) {
	dir := t.TempDir()
	RunsBeside(t, 400, func(i int) error {
		path := filepath.Join(dir, fmt.Sprintf("fake-%d", i))
		if err := WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			return err
		}
		return exec.Command(path).Run()
	})
}

// Unguarded names a write of a file with an execute bit by os.WriteFile or os.OpenFile in any file
// that is no test file, in a test file of a package whose tests call Parallel, and in any file of a
// test-support package, by whatever name the file imports os and however the mode is spelled, and
// a mode it cannot read as holding no execute bit, a mask that leaves one included. It names an
// open by os.WriteFile, os.OpenFile, os.Create or os.CreateTemp whose file the same function gives
// an execute bit by os.Chmod of its path or of its file's Name(), or by the file's Chmod method, in
// a closure too. It follows the file through calls that wrap the open, one or two deep, a Chmod or
// a Name() called on such a call, a variable or a field the file is assigned to, by itself or
// beside other values, a copy of one, assigned or declared, a field a keyed composite literal or
// its address sets, in a nested literal too, a field written or read through (*h).f, and an
// os.Rename, which moves to its new path the opens made before it. It names a use of one of those
// functions as a value, and a dot import of os. It leaves out a mode with no execute bit, a mode
// masked so it cannot hold one (mode&^0o111, mode&0o644), a Chmod with none or of a path no open
// of the function names, a Chmod of a copy made before the open or of a variable that never held
// one, a Chmod on a call that opens no file for writing, the old path of an os.Rename that moved
// another file onto the open's path, a test file of a package whose tests do not run beside each
// other, a write by testexec.WriteFile, another module below root, and programfile.Write itself,
// the Write function of github.com/sjawhar/envoy/programfile in a file that is no test file:
// another function of that package is named, and so are a Write in a test file of that package
// and a Write of a package of that name elsewhere. The code a deployment runs is
// planted in daemonish: the shapes the daemon's own program files had before programfile.Write,
// the git helper's os.WriteFile at 0o700 and workerbin's os.CreateTemp with a Chmod of the file to
// 0o700, and a copy that keeps a file's mode. Its test file is left out: daemonish's tests run one
// at a time, so the guard reads that package's code and not its tests.
func TestUnguardedNamesEveryExecutableWriteThatMayRunBesideAFork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		writeUnder(t, root, path, body)
	}
	const parallel = "func TestParallel(t *testing.T) { t.Parallel() }\n"
	write("go.mod", "module github.com/sjawhar/envoy\n")
	write("beside/beside.go", "package beside\n")
	write("beside/a_test.go", `package beside

import (
	"io/fs"
	"os"
	"testing"
)

`+parallel+`
func writes(t *testing.T) {
	_ = os.WriteFile("a", nil, 0o755)
	_ = os.WriteFile("b", nil, 0o644)
	_ = os.WriteFile("c", nil, 0700)
	_ = os.WriteFile("d", nil, fs.FileMode(0o500))
	_ = os.WriteFile("e", nil, 0o600|0o100)
	f, _ := os.OpenFile("f", os.O_CREATE|os.O_WRONLY, 0o755)
	_ = f
	var mode fs.FileMode = 0o755
	_ = os.WriteFile("g", nil, mode)
}
`)
	write("beside/b_test.go", `package beside_test

import (
	osx "os"
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

func writesToo(t *testing.T) {
	_ = osx.WriteFile("h", nil, 0o755)
	_ = testexec.WriteFile("i", nil, 0o755)
}
`)
	write("alone/alone_test.go", `package alone

import (
	"os"
	"testing"
)

func TestAlone(t *testing.T) { _ = os.WriteFile("j", nil, 0o755) }
`)
	write("fakestest/fakestest.go", `package fakestest

import "os"

func Fake() { _ = os.WriteFile("k", nil, 0o755) }
`)
	write("programfile/programfile.go", `package programfile

import "os"

func Write(name string, flag int, perm os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|flag, perm)
	if err != nil {
		return err
	}
	return file.Close()
}

func Other(name string) error { return os.WriteFile(name, nil, 0o755) }
`)
	write("programfile/write_test.go", `package programfile

import (
	"os"
	"testing"
)

`+parallel+`
func Write(name string) error { return os.WriteFile(name, nil, 0o755) }
`)
	write("daemonish/programfile/programfile.go", `package programfile

import "os"

func Write(name string, perm os.FileMode) error { return os.WriteFile(name, nil, perm) }
`)
	write("daemonish/install.go", `package daemonish

import "os"

func helper(dir string) error { return os.WriteFile(dir+"/helper", []byte("#!/bin/sh\n"), 0o700) }

func script(dir string) error {
	temporary, err := os.CreateTemp(dir, ".script-")
	if err != nil {
		return err
	}
	_ = temporary.Chmod(0o700)
	return temporary.Close()
}

func named(dir string) {
	temporary, _ := os.CreateTemp(dir, ".named-")
	_ = os.Chmod(temporary.Name(), 0o755)
}

func data(dir string) {
	temporary, _ := os.CreateTemp(dir, ".data-")
	_ = temporary.Chmod(0o600)
	_ = os.WriteFile(dir+"/data", nil, 0o644)
}

func copied(path string, mode os.FileMode) error { return os.WriteFile(path, nil, mode) }

var create = os.CreateTemp

func masked(path string, mode os.FileMode) {
	_ = os.WriteFile(path, nil, mode&^0o111)
	_ = os.WriteFile(path, nil, mode&0o644)
	_ = os.WriteFile(path, nil, mode&0o755)
	_ = os.WriteFile(path, nil, mode&^0o100)
}
`)
	write("daemonish/install_test.go", `package daemonish

import (
	"os"
	"testing"
)

func TestAlone(t *testing.T) { _ = os.WriteFile("v", nil, 0o755) }
`)
	write("other/go.mod", "module other\n")
	write("other/other_test.go", `package other

import (
	"os"
	"testing"
)

`+parallel+`
func writes() { _ = os.WriteFile("l", nil, 0o755) }
`)
	write("beside/chmod_test.go", `package beside

import (
	"os"
	"testing"
)

func chmods(t *testing.T, mode os.FileMode) {
	_ = os.WriteFile("m", nil, 0o644)
	_ = os.Chmod("m", 0o755)
	f, _ := os.Create("n")
	_ = f.Chmod(0o700)
	g, _ := os.OpenFile("o", os.O_CREATE|os.O_WRONLY, 0o600)
	_ = g.Chmod(0o755)
	_ = os.WriteFile("p", nil, 0o600)
	t.Cleanup(func() { _ = os.Chmod("p", mode) })
	h, _ := os.Create("q")
	_ = h.Chmod(0o600)
	_ = os.WriteFile("r", nil, 0o644)
	_ = os.Chmod("r", 0o640)
	_ = os.Chmod(t.TempDir(), 0o755)
	var i, _ = os.Create("s")
	_ = i.Chmod(0o755)
}

func values() {
	write := os.WriteFile
	_ = write("t", nil, 0o644)
}
`)
	write("beside/dot_test.go", `package beside

import (
	. "os"
	"testing"
)

func dotted(t *testing.T) { _ = WriteFile("u", nil, 0o755) }
`)
	write("beside/createtemp_test.go", `package beside

import (
	"os"
	"testing"
)

func temporaries(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "fake-")
	_ = f.Chmod(0o700)
	g, _ := os.CreateTemp(t.TempDir(), "fake-")
	_ = os.Chmod(g.Name(), 0o755)
	h, _ := os.CreateTemp(t.TempDir(), "log-")
	_ = h.Chmod(0o600)
	create := os.CreateTemp
	_, _ = create("", "")
}
`)
	write("beside/held_test.go", `package beside

import (
	"os"
	"path/filepath"
	"testing"
)

type holder struct{ f *os.File }

func must(f *os.File, err error) *os.File { return f }

func held(t *testing.T, dir string) {
	a, _ := os.Create(filepath.Join(dir, "a"))
	_ = os.Chmod(a.Name(), 0o755)
	b, _ := os.OpenFile(filepath.Join(dir, "b"), os.O_CREATE|os.O_WRONLY, 0o600)
	_ = os.Chmod(b.Name(), 0o700)
	var h holder
	h.f, _ = os.CreateTemp(dir, "d")
	_ = h.f.Chmod(0o755)
	c := must(os.CreateTemp(dir, "c"))
	_ = c.Chmod(0o755)
	e, _ := os.CreateTemp(dir, "e")
	_ = os.Rename(e.Name(), filepath.Join(dir, "run"))
	_ = os.Chmod(filepath.Join(dir, "run"), 0o755)
	k, _ := os.CreateTemp(dir, "k")
	_ = os.Rename(filepath.Join(dir, "old"), k.Name())
	_ = os.Chmod(filepath.Join(dir, "old"), 0o755)
}
`)
	write("beside/followed_test.go", `package beside

import (
	"os"
	"path/filepath"
	"testing"
)

type nest struct{ h holder }

func wrap(f *os.File, err error) (*os.File, error) { return f, err }

func followed(t *testing.T, dir string) {
	f, _ := os.CreateTemp(dir, "f")
	g := f
	_ = g.Chmod(0o755)
	a, _ := os.Create(filepath.Join(dir, "a"))
	var b = a
	_ = os.Chmod(b.Name(), 0o755)
	c, d := must(os.CreateTemp(dir, "c")),
		must(os.CreateTemp(dir, "d"))
	_ = d.Chmod(0o755)
	h := holder{f: must(os.CreateTemp(dir, "h"))}
	_ = h.f.Chmod(0o755)
	p := &holder{f: must(os.CreateTemp(dir, "p"))}
	_ = p.f.Chmod(0o755)
	e, _ := os.CreateTemp(dir, "e")
	r := holder{f: e}
	_ = r.f.Chmod(0o755)
	n := nest{h: holder{f: must(os.CreateTemp(dir, "n"))}}
	_ = n.h.f.Chmod(0o755)
	v := holder{f: must(os.CreateTemp(dir, "v"))}
	y := v.f
	_ = y.Chmod(0o755)
	s := &holder{f: must(os.CreateTemp(dir, "s"))}
	_ = (*s).f.Chmod(0o755)
	u := &holder{f: must(os.CreateTemp(dir, "u"))}
	_ = os.Chmod((*u).f.Name(), 0o755)
	w := &holder{}
	(*w).f, _ = os.CreateTemp(dir, "w")
	_ = w.f.Chmod(0o755)
	_ = must(os.CreateTemp(dir, "chained")).Chmod(0o755)
	_ = os.Chmod(must(os.CreateTemp(dir, "named")).Name(), 0o755)
	_ = os.Rename(must(os.CreateTemp(dir, "moved")).Name(), filepath.Join(dir, "moved"))
	_ = os.Chmod(filepath.Join(dir, "moved"), 0o755)
	x := must(wrap(os.CreateTemp(dir, "x")))
	_ = x.Chmod(0o755)
	var none *os.File
	copied := none
	_ = copied.Chmod(0o755)
	var late *os.File
	early := late
	late, _ = os.CreateTemp(dir, "late")
	_ = early.Chmod(0o755)
	_ = must(os.Open(filepath.Join(dir, "read"))).Chmod(0o755)
	_ = c
}
`)
	got, err := Unguarded(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"beside/a_test.go:12", "beside/a_test.go:14", "beside/a_test.go:15", "beside/a_test.go:16",
		"beside/a_test.go:17", "beside/a_test.go:20", "beside/b_test.go:11",
		"beside/chmod_test.go:11", "beside/chmod_test.go:13", "beside/chmod_test.go:15",
		"beside/chmod_test.go:22", "beside/chmod_test.go:27", "beside/chmod_test.go:9",
		"beside/createtemp_test.go:11", "beside/createtemp_test.go:15", "beside/createtemp_test.go:9",
		"beside/dot_test.go:4",
		"beside/followed_test.go:14", "beside/followed_test.go:17", "beside/followed_test.go:21",
		"beside/followed_test.go:23", "beside/followed_test.go:25", "beside/followed_test.go:27",
		"beside/followed_test.go:30", "beside/followed_test.go:32", "beside/followed_test.go:35",
		"beside/followed_test.go:37", "beside/followed_test.go:40", "beside/followed_test.go:42",
		"beside/followed_test.go:43", "beside/followed_test.go:44", "beside/followed_test.go:46",
		"beside/held_test.go:14", "beside/held_test.go:16", "beside/held_test.go:19", "beside/held_test.go:21",
		"beside/held_test.go:23",
		"daemonish/install.go:17", "daemonish/install.go:27", "daemonish/install.go:29",
		"daemonish/install.go:34", "daemonish/install.go:35",
		"daemonish/install.go:5", "daemonish/install.go:8",
		"daemonish/programfile/programfile.go:5",
		"fakestest/fakestest.go:5",
		"programfile/programfile.go:13", "programfile/write_test.go:10",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Unguarded named\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// Unguarded refuses, naming the root and what it could not read, a root that is no module's: the
// parent of a module's directory, or a directory inside one, holds no go.mod. It refuses a root
// whose go.mod declares no module, or cannot be read, saying that rather than that it holds none.
// It refuses a module root under which it read no test file, because the module holds none or
// because each one is in another module below it. A check of such a root would name no write and
// pass while it read nothing. A module root with one test file and no write is named nothing and
// no error.
func TestUnguardedRefusesARootUnderWhichItReadsNoTestFile(t *testing.T) {
	t.Parallel()
	const test = "package beside\n\nimport \"testing\"\n\nfunc TestParallel(t *testing.T) { t.Parallel() }\n"
	parent := t.TempDir()
	writeUnder(t, parent, "module/go.mod", "module example.com/checked\n")
	writeUnder(t, parent, "module/beside/beside_test.go", test)
	noTests := t.TempDir()
	writeUnder(t, noTests, "go.mod", "module example.com/no-tests\n")
	writeUnder(t, noTests, "beside/beside.go", "package beside\n")
	nested := t.TempDir()
	writeUnder(t, nested, "go.mod", "module example.com/outer\n")
	writeUnder(t, nested, "inner/go.mod", "module example.com/inner\n")
	writeUnder(t, nested, "inner/beside_test.go", test)
	noModule := t.TempDir()
	writeUnder(t, noModule, "go.mod", "go 1.27\n")
	writeUnder(t, noModule, "beside/beside_test.go", test)
	unreadable := t.TempDir()
	writeUnder(t, unreadable, "go.mod/README", "a directory where the go.mod should be\n")
	writeUnder(t, unreadable, "beside/beside_test.go", test)
	for _, row := range []struct{ what, root, want, notWant string }{
		{"the parent of a module's directory", parent, "holds no go.mod", ""},
		{"a directory inside a module", filepath.Join(parent, "module", "beside"), "holds no go.mod", ""},
		{"a root whose go.mod declares no module", noModule, "its go.mod declares no module", "holds no go.mod"},
		{"a root whose go.mod is a directory", unreadable, "its go.mod could not be read", "holds no go.mod"},
		{"a module with no test file", noTests, "read no Go test file under", ""},
		{"a module whose one test file is in another module below it", nested, "read no Go test file under", ""},
	} {
		got, err := Unguarded(row.root)
		if got != nil || err == nil || !strings.Contains(err.Error(), row.want) || !strings.Contains(err.Error(), row.root) ||
			row.notWant != "" && strings.Contains(err.Error(), row.notWant) {
			t.Errorf("Unguarded on %s (%s) = %v, %v; want no names and an error naming the root and saying %q, not %q", row.what, row.root, got, err, row.want, row.notWant)
		}
	}
	if got, err := Unguarded(filepath.Join(parent, "module")); err != nil || len(got) != 0 {
		t.Errorf("Unguarded on a module root with one test file and no write = %v, %v; want nothing and no error", got, err)
	}
}

// Unguarded reads the module path a go.mod declares as Go does: past a comment after it, and out
// of quotes. Under either spelling of its module's path programfile.Write is left out, and the
// package's other write is named.
func TestUnguardedReadsTheModulePathAsGoDoes(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"module github.com/sjawhar/envoy // the writer's module",
		`module "github.com/sjawhar/envoy"`,
	} {
		root := t.TempDir()
		writeUnder(t, root, "go.mod", line+"\n")
		writeUnder(t, root, "programfile/programfile.go", "package programfile\n\nimport \"os\"\n\n"+
			"func Write(name string, perm os.FileMode) error { return os.WriteFile(name, nil, perm) }\n\n"+
			"func Other(name string) error { return os.WriteFile(name, nil, 0o755) }\n")
		writeUnder(t, root, "programfile/programfile_test.go", "package programfile\n\nimport \"testing\"\n\nfunc TestParallel(t *testing.T) { t.Parallel() }\n")
		if got, err := Unguarded(root); err != nil || !slices.Equal(got, []string{"programfile/programfile.go:7"}) {
			t.Errorf("Unguarded under a go.mod of %q = %v, %v; want only programfile/programfile.go:7, Other's write", line, got, err)
		}
	}
}

// RunsBeside runs its test only where a run of a busy program was refused with ETXTBSY: each row
// is an answer such a run can give, and the verdict busyRunVerdict's doc gives it.
func TestRunsBesideRunsOnlyWhereABusyRunIsRefused(t *testing.T) {
	t.Parallel()
	refused := &os.PathError{Op: "fork/exec", Path: "/tmp/held-open", Err: syscall.ETXTBSY}
	noexec := &os.PathError{Op: "fork/exec", Path: "/tmp/held-open", Err: syscall.EACCES}
	noShell := &os.PathError{Op: "fork/exec", Path: "/tmp/held-open", Err: syscall.ENOENT}
	for _, row := range []struct {
		what       string
		answer     error
		ci         bool
		skip, fail string
	}{
		{"refused", refused, false, "", ""},
		{"refused, in CI", refused, true, "", ""},
		{"ran", nil, false, "this kernel ran a program this process held open for writing rather than refuse it with ETXTBSY, as gVisor does", ""},
		{"ran, in CI", nil, true, "", "under GITHUB_ACTIONS that is a broken runner"},
		{"noexec", noexec, false, "", "answered fork/exec /tmp/held-open: permission denied, neither ETXTBSY nor a run"},
		{"no shell, in CI", noShell, true, "", "answered fork/exec /tmp/held-open: no such file or directory, neither ETXTBSY nor a run"},
	} {
		skip, fail := busyRunVerdict(row.answer, row.ci)
		if (skip == "") != (row.skip == "") || !strings.Contains(skip, row.skip) || (fail == "") != (row.fail == "") || !strings.Contains(fail, row.fail) {
			t.Errorf("%s: busyRunVerdict = skip %q, fail %q; want skip with %q, fail with %q", row.what, skip, fail, row.skip, row.fail)
		}
	}
}

// recorder is a testing.TB that keeps what a helper of this package reports rather than failing
// the test that runs it. Its Fatalf ends the goroutine it is called on, as testing.T's does. It
// sets no other method of testing.TB, so a helper that calls one panics.
type recorder struct {
	testing.TB
	errors, fatals []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// record runs report with a recorder on a goroutine of its own, which the recorder's Fatalf ends,
// and answers the recorder once report has returned or ended.
func record(report func(testing.TB)) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		report(r)
	}()
	<-done
	return r
}

// RunsBeside's trials go on past each run refused as busy (an error that is ETXTBSY, wrapped or
// not), and fail the test at the end, naming how many runs were refused of how many and the first
// refusal's error. Any other error fails the test at once, naming the run and the error, and no
// run follows it. Trials of which none was refused pass.
func TestRunsBesideCountsTheRunsRefusedAsBusy(t *testing.T) {
	t.Parallel()
	busy := func(i int) error {
		return &os.PathError{Op: "fork/exec", Path: fmt.Sprintf("/tmp/program-%d", i), Err: syscall.ETXTBSY}
	}
	for _, row := range []struct {
		what           string
		runs           int
		answer         func(i int) error
		ran            int
		errors, fatals []string
	}{
		{"no run refused", 5, func(int) error { return nil }, 5, nil, nil},
		{"two runs refused", 6, func(i int) error {
			switch i {
			case 1:
				return busy(1)
			case 4:
				return fmt.Errorf("install the check: %w", busy(4))
			}
			return nil
		}, 6, []string{"2 of 6 runs were refused as busy (ETXTBSY) while the process forked; the first: fork/exec /tmp/program-1: text file busy"}, nil},
		{"another error after a refusal", 6, func(i int) error {
			switch i {
			case 1:
				return busy(1)
			case 3:
				return &os.PathError{Op: "fork/exec", Path: "/tmp/program-3", Err: syscall.EACCES}
			}
			return nil
		}, 4, nil, []string{"run 4 of 6: fork/exec /tmp/program-3: permission denied"}},
	} {
		var ran []int
		got := record(func(t testing.TB) {
			countBusyRuns(t, row.runs, func(i int) error {
				ran = append(ran, i)
				return row.answer(i)
			})
		})
		var want []int
		for i := range row.ran {
			want = append(want, i)
		}
		if !slices.Equal(ran, want) || !slices.Equal(got.errors, row.errors) || !slices.Equal(got.fatals, row.fatals) {
			t.Errorf("%s: ran %v, errors %q, fatals %q; want ran %v, errors %q, fatals %q", row.what, ran, got.errors, got.fatals, want, row.errors, row.fatals)
		}
	}
}

// RequireGuarded fails the test once where Unguarded names a write, saying how many it named, each
// as file:line, and the writer to write each with; where Unguarded refuses the root, saying what it
// could not read under which root; and passes a module root where Unguarded names nothing.
func TestRequireGuardedFailsNamingEachWrite(t *testing.T) {
	t.Parallel()
	const test = "package beside\n\nimport \"testing\"\n\nfunc TestParallel(t *testing.T) { t.Parallel() }\n"
	written := t.TempDir()
	writeUnder(t, written, "go.mod", "module example.com/written\n")
	writeUnder(t, written, "beside/beside.go", "package beside\n\nimport \"os\"\n\n"+
		"func Install(name string) error { return os.WriteFile(name, nil, 0o755) }\n")
	writeUnder(t, written, "beside/beside_test.go", test)
	clean := t.TempDir()
	writeUnder(t, clean, "go.mod", "module example.com/clean\n")
	writeUnder(t, clean, "beside/beside_test.go", test)
	noModule := t.TempDir()
	writeUnder(t, noModule, "beside/beside_test.go", test)
	for _, row := range []struct {
		what, root string
		fatal      []string
	}{
		{"a module with one write", written, []string{"1 writes of a file that runs can meet a fork beside it:\n\tbeside/beside.go:5\n",
			"write each with programfile.Write, or with testexec.WriteFile in a test", `"A file a test runs"`}},
		{"a root with no go.mod", noModule, []string{"read the module's Go files: ", noModule, "holds no go.mod"}},
		{"a module with no write", clean, nil},
	} {
		got := record(func(t testing.TB) { RequireGuarded(t, row.root) })
		if len(got.errors) != 0 || len(got.fatals) != min(len(row.fatal), 1) ||
			len(got.fatals) == 1 && slices.ContainsFunc(row.fatal, func(part string) bool { return !strings.Contains(got.fatals[0], part) }) {
			t.Errorf("%s: RequireGuarded reported errors %q, fatals %q; want one fatal holding each of %q, or none if that is empty", row.what, got.errors, got.fatals, row.fatal)
		}
	}
}
