//go:build linux

package leakguard

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"golang.org/x/sys/unix"
)

// The stand-in check of these rows is a launcher and a program, as the real check is: the launcher
// runs the program beside itself, found as the real launcher finds it (`readlink -f "$0"`), and the
// program is a shell script under the real program's name. swapLauncher first waits, at most 10 s,
// for the file SWAP_GO names, when one is named: that is the word of the writer that replaces the
// program (swapper), which makes the race that writer wins at once on the real check — whose
// launcher starts python before python reads its program — a certain one here, so a row counts
// writes and never timing.
const swapLauncher = `#!/bin/sh
i=0
while [ -n "${SWAP_GO:-}" ] && [ ! -e "$SWAP_GO" ] && [ "$i" -lt 1000 ]; do sleep 0.01; i=$((i+1)); done
exec /bin/sh "$(dirname "$(readlink -f "$0")")/public_leak_check.py" "$@"
`

// swapRefuses is the deployment's program: it refuses every write, as the real check refuses a
// write that names a credential. swapClears is the writer's own, which clears every write and
// says it ran in the file SWAP_RAN names.
const (
	swapRefuses = "cat >/dev/null 2>&1\necho 'LEAK CHECK: refusing: one of ours'\nexit 3\n"
	swapClears  = "cat >/dev/null 2>&1\necho ran >>\"$SWAP_RAN\"\nexit 0\n"
)

// swapInstall writes a check of launcher and program, with the README and must-pass lines its
// manifest covers, into a directory as a pod's shared install is: one every role of the pod can
// write, since they run as one user. It answers the launcher's path and the deployment's manifest
// digest of the four files.
func swapInstall(t *testing.T, launcher, program string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "leak-check")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		checkName:             launcher,
		checkProgram:          program,
		"README.md":           "# the check's readme\n",
		"must-pass-lines.txt": "a line the self-test must pass\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	check := filepath.Join(dir, checkName)
	digest, err := manifestSHA256(check)
	if err != nil {
		t.Fatal(err)
	}
	return check, digest
}

// swapPlace puts body at dir/name the way a writer replaces a file another process may be reading:
// a new file beside it, renamed over it.
func swapPlace(t *testing.T, dir, name, body string) {
	next := filepath.Join(dir, ".next")
	if err := os.WriteFile(next, []byte(body), 0o755); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(next, filepath.Join(dir, name)); err != nil {
		t.Error(err)
	}
}

// watchInstall is an inotify descriptor watching dir for every close of a file read there, in
// place before it answers, so no read made after it is unseen.
func watchInstall(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatalf("inotify watch of %s: %v", dir, err)
	}
	return fd
}

// swapper is that writer, reading the watch fd holds of the install dir. Once armed, the moment a
// reader has closed README.md and then must-pass-lines.txt — the last two files of the manifest,
// in the order the guard reads them, so the guard's digest of the install has just held — it
// renames its own program over public_leak_check.py, makes the file go names, counts the swap and
// disarms. The row restores the deployment's program and removes go before it arms the next round.
func swapper(t *testing.T, ctx context.Context, fd int, dir, evil, goFile string, armed *atomic.Bool, swaps *atomic.Int64) {
	buf := make([]byte, 64<<10)
	prev := ""
	for ctx.Err() == nil {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			time.Sleep(50 * time.Microsecond)
			continue
		}
		if err != nil {
			t.Errorf("inotify read: %v", err)
			return
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(event.Len)]
			off += unix.SizeofInotifyEvent + int(event.Len)
			name := strings.TrimRight(string(raw), "\x00")
			if name == "must-pass-lines.txt" && prev == "README.md" && armed.CompareAndSwap(true, false) {
				swapPlace(t, dir, checkProgram, evil)
				if err := os.WriteFile(goFile, nil, 0o600); err != nil {
					t.Error(err)
				}
				swaps.Add(1)
			}
			prev = name
		}
	}
}

// A writer of the shared install replaces public_leak_check.py the moment the guard's manifest
// digest of the install has held, and the write is still judged by the program that digest
// covered: the guard runs a private copy of the bytes it hashed, never the install it read them
// from. Each shape — a text, a push and a gh call — is asked rounds times to clear a write the
// deployment's program refuses, with the writer armed for every call; every call is refused by that
// program and the writer's own never runs. The writer's swap count is the row's control that the
// instrument is live: a row where it swapped fewer times than it was asked proves nothing.
func TestAProgramSwappedInAfterTheDigestHeldNeverJudgesTheWrite(t *testing.T) {
	const rounds = 10
	for _, shape := range []struct {
		name  string
		write func(t *testing.T, g *Guard) error
	}{
		{"a text", func(t *testing.T, g *Guard) error {
			return g.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title\n"})
		}},
		{"a push", func(t *testing.T, g *Guard) error {
			return g.Push(bound(t), PushRequest{Dir: t.TempDir(), Remote: "origin", URL: "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/x", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/x", RemoteSHA: noSHA}}})
		}},
		{"a gh call", func(t *testing.T, g *Guard) error {
			_, err := g.GhCall(bound(t), GhRequest{Args: []string{"pr", "comment", "1", "--body", "a body"}, Repo: "invented-org/invented-repo", Files: []GhFile{}})
			return err
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			check, digest := swapInstall(t, swapLauncher, swapRefuses)
			dir := filepath.Dir(check)
			marks := t.TempDir()
			goFile, ranFile := filepath.Join(marks, "go"), filepath.Join(marks, "ran")
			env := append(fixtureenv.Isolated(t.TempDir()), CheckVar+"="+check, CheckerVar+"="+digest, "SWAP_GO="+goFile, "SWAP_RAN="+ranFile)
			// Private, so a write the check clears is cleared whole: no rule pass, and no git read
			// of a push's commits, stands between the check's answer and the call's.
			guard := func() *Guard { return New(Options{Visibility: answers(Private, nil), Env: env}) }

			fd := watchInstall(t, dir)
			ctx, stop := context.WithCancel(context.Background())
			var armed atomic.Bool
			var swaps atomic.Int64
			done := make(chan struct{})
			go func() { defer close(done); swapper(t, ctx, fd, dir, swapClears, goFile, &armed, &swaps) }()

			cleared, refused, other := 0, 0, []string{}
			for round := range rounds {
				armed.Store(true)
				err := shape.write(t, guard())
				var refusal *Refusal
				switch {
				case err == nil:
					cleared++
				case errors.As(err, &refusal):
					refused++
				default:
					other = append(other, err.Error())
				}
				if swaps.Load() != int64(round+1) {
					stop()
					<-done
					t.Fatalf("round %d: the writer swapped %d times in %d rounds, so this row proves nothing (inotify saw no read of the install)", round, swaps.Load(), round+1)
				}
				swapPlace(t, dir, checkProgram, swapRefuses)
				if err := os.Remove(goFile); err != nil {
					t.Fatal(err)
				}
			}
			stop()
			<-done
			ran, _ := os.ReadFile(ranFile)
			t.Logf("%s: %d rounds, %d swaps: %d cleared, %d refused, %d other %q; the writer's program ran %d times",
				shape.name, rounds, swaps.Load(), cleared, refused, len(other), other, strings.Count(string(ran), "ran"))
			if cleared != 0 || refused != rounds || len(ran) != 0 {
				t.Fatalf("%d of %d writes the deployment's program refuses were cleared, %d refused, by a program swapped in after %s=%s held",
					cleared, rounds, refused, CheckerVar, digest)
			}
		})
	}
}

// `legion gh`'s checked-only read is decided by the program the digest covered, hashed from the
// bytes the guard read before the check ran: a writer that changes the install's program while the
// check runs changes neither the program that runs nor the one the read was proven against.
func TestAReadIsProvenByTheProgramTheDigestCovered(t *testing.T) {
	// The launcher rewrites the install's program, then runs its own: from the private copy, the
	// program the digest covered, which clears the read.
	launcher := "#!/bin/sh\nprintf '# rewritten while the check ran\\nexit 0\\n' >\"$SWAP_INSTALL/public_leak_check.py\"\n" +
		"exec /bin/sh \"$(dirname \"$(readlink -f \"$0\")\")/public_leak_check.py\" \"$@\"\n"
	program := "# stands for the shared check a read rule was proven against\ncat >/dev/null 2>&1\nexit 0\n"
	check, digest := swapInstall(t, launcher, program)
	env := append(fixtureenv.Isolated(t.TempDir()), CheckVar+"="+check, CheckerVar+"="+digest, "SWAP_INSTALL="+filepath.Dir(check))
	var asked []string
	guard := New(Options{Visibility: answers(Public, &asked), Env: env})
	answer, err := guard.GhCall(bound(t), GhRequest{Args: []string{"search", "code", "x"}, Repo: "invented-org/invented-repo", Files: []GhFile{},
		WritesNothing: true, ProvenCheck: hexSHA256(program)})
	if now, _ := os.ReadFile(filepath.Join(filepath.Dir(check), checkProgram)); string(now) == program {
		t.Fatalf("the launcher did not rewrite the install's program, so this row proves nothing")
	}
	if err != nil || !answer.CheckedOnly || len(asked) != 0 {
		t.Fatalf("GhCall = %+v, %v, visibility asked of %q; want the read cleared by the check alone, proven by the program the digest covered", answer, err, asked)
	}
}

// recordingLauncher records, in the file LEAK_CHECK_RECORD names, the directory it runs from
// (`from <dir>`), the mode of that directory and of each file in it (`<mode> <path>`), and on a
// gh call's second go the file its buffered standard input is read from (`stdin <path>`), then
// runs the program beside it.
const recordingLauncher = `#!/bin/sh
here=$(dirname "$(readlink -f "$0")")
{
  echo "from $here"
  stat -c '%a %n' "$here" "$here"/*
  if [ -n "${PUBLIC_LEAK_CHECK_STDIN:-}" ]; then echo "stdin $(readlink "$PUBLIC_LEAK_CHECK_STDIN")"; fi
} >>"$LEAK_CHECK_RECORD"
exec /bin/sh "$here/public_leak_check.py" "$@"
`

// runRecord is what recordingLauncher recorded over a call: the directory of each run, in order,
// the mode of each path it listed, and the file each second go read its standard input from.
type runRecord struct {
	from  []string
	modes map[string]string
	stdin []string
}

func readRunRecord(t *testing.T, path string) runRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the check recorded nothing: %v", err)
	}
	ran := runRecord{modes: map[string]string{}}
	for line := range strings.Lines(string(raw)) {
		head, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch head {
		case "from":
			ran.from = append(ran.from, rest)
		case "stdin":
			ran.stdin = append(ran.stdin, rest)
		case "checked":
		default:
			ran.modes[rest] = head
		}
	}
	return ran
}

// Under a deployment's digest the check runs from a copy of the bytes the digest held, in a new
// directory under the role's own private one (copyBase) that only this user reaches — 0700, the
// launcher 0500, the three files it only reads 0400 — and never from the install it was read
// from; a gh call's buffered standard input is made in that directory too, not in TMPDIR. The copy
// is gone once the call answers, whatever it answered: a text cleared, one refused, a check that
// could not finish, a push cleared and one refused, and a gh call the check asks twice, whose
// second go runs from the same copy.
// With no digest told the check runs where Resolve found it, as it did before the setting.
func TestACheckUnderTheDigestRunsFromAPrivateCopyThatIsGoneAfter(t *testing.T) {
	// A TMPDIR of its own, in this process and in the guard's children, so a copy or a buffer made
	// there would be told apart from one made where the copy belongs.
	tmpdir := t.TempDir()
	t.Setenv("TMPDIR", tmpdir)
	base, err := copyBase(PrivateCopyBase, privateCopyFallback)
	if err != nil {
		t.Fatal(err)
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	if base == tmpdir {
		t.Fatalf("the copy's base is the TMPDIR of this test, so this row proves nothing")
	}
	text := func(t *testing.T, g *Guard) error {
		return g.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title\n"})
	}
	push := func(t *testing.T, g *Guard) error {
		return g.Push(bound(t), PushRequest{Dir: t.TempDir(), Remote: "origin", URL: "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: "refs/heads/x", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/x", RemoteSHA: noSHA}}})
	}
	cleared := func(t *testing.T, err error) {
		if err != nil {
			t.Fatalf("got %v, want the write cleared", err)
		}
	}
	rows := []struct {
		name    string
		program string
		write   func(t *testing.T, g *Guard) error
		want    func(t *testing.T, err error)
		runs    int
	}{
		{"a write the check clears", passes, text, cleared, 1},
		{"a write the check refuses", refuses, text, func(t *testing.T, err error) { refusal(t, err) }, 1},
		{"a check that cannot finish", cannotFinish, text, func(t *testing.T, err error) { setupFailure(t, err) }, 1},
		{"a push the check clears", passes, push, cleared, 1},
		{"a push the check refuses", refuses, push, func(t *testing.T, err error) { refusal(t, err) }, 1},
		{"a gh call the check asks twice", leakguardtest.Reading(0, ""), func(t *testing.T, g *Guard) error {
			_, err := g.GhCall(bound(t), GhRequest{Args: []string{"pr", "comment", "1", "--body-file", "-"}, Stdin: strings.NewReader("a body\n"),
				Repo: "invented-org/invented-repo", Files: []GhFile{}})
			return err
		}, cleared, 2},
	}
	guard := func(t *testing.T, check, record string, extra ...string) *Guard {
		env := append(fixtureenv.Isolated(t.TempDir()), CheckVar+"="+check, "LEAK_CHECK_RECORD="+record, "TMPDIR="+tmpdir)
		return New(Options{Visibility: answers(Private, nil), Env: append(env, extra...)})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			check, digest := swapInstall(t, recordingLauncher, tc.program)
			record := filepath.Join(t.TempDir(), "record")
			tc.want(t, tc.write(t, guard(t, check, record, CheckerVar+"="+digest)))
			ran := readRunRecord(t, record)
			if len(ran.from) != tc.runs {
				t.Fatalf("the check ran %d times (%q), want %d", len(ran.from), ran.from, tc.runs)
			}
			copied := ran.from[0]
			for _, from := range ran.from[1:] {
				if from != copied {
					t.Errorf("a later go ran from %s, want the first go's copy %s", from, copied)
				}
			}
			if filepath.Dir(copied) != base || copied == filepath.Dir(check) {
				t.Errorf("the check ran from %s, want a directory of its own under %s, not the install %s", copied, base, filepath.Dir(check))
			}
			want := map[string]string{copied: "700", filepath.Join(copied, checkName): "500"}
			for _, name := range ManifestFiles[1:] {
				want[filepath.Join(copied, name)] = "400"
			}
			for path, mode := range want {
				if ran.modes[path] != mode {
					t.Errorf("%s ran with the mode %q, want %s", path, ran.modes[path], mode)
				}
			}
			for _, stdin := range ran.stdin {
				if filepath.Dir(stdin) != copied {
					t.Errorf("the second go read its standard input from %s, want a file of the copy %s", stdin, copied)
				}
			}
			if _, err := os.Lstat(copied); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the copy %s is still there once the call answered (%v)", copied, err)
			}
		})
	}
	t.Run("no digest told", func(t *testing.T) {
		check, _ := swapInstall(t, recordingLauncher, passes)
		record := filepath.Join(t.TempDir(), "record")
		cleared(t, text(t, guard(t, check, record)))
		install, err := filepath.EvalSymlinks(filepath.Dir(check))
		if err != nil {
			t.Fatal(err)
		}
		if ran := readRunRecord(t, record); len(ran.from) != 1 || ran.from[0] != install {
			t.Fatalf("the check ran from %q, want once from where it was found, %s", ran.from, install)
		}
	})
}

// A caller that is no role names the directory the copy is made under (Options.PrivateCopyBase),
// and a guard under a deployment's digest makes its copy there and runs the check, though the
// role's private directory is there and cannot be written: the daemon's own container, run from
// the worker image, holds that directory root-owned and empty, with nothing bound over it. The
// package's constant cannot be pointed elsewhere, so the row stands for it by setting the guard's
// own copyBase and copyFallback, which New fills from the constant, to a 0555 directory of its own
// after New; with no Options.PrivateCopyBase the same guard is refused for the copy it cannot make.
func TestACallersPrivateCopyBaseIsWhereTheCopyIsMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a 0555 directory, so it cannot stand for one the daemon's user cannot write")
	}
	unwritable := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(unwritable, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) })
	text := func(g *Guard) error {
		return g.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title\n"})
	}
	guard := func(t *testing.T, check, digest, record, base string) *Guard {
		env := append(fixtureenv.Isolated(t.TempDir()), CheckVar+"="+check, CheckerVar+"="+digest, "LEAK_CHECK_RECORD="+record)
		g := New(Options{Visibility: answers(Private, nil), Env: env, PrivateCopyBase: base})
		if base == "" {
			g.copyBase, g.copyFallback = unwritable, unwritable
		}
		return g
	}

	t.Run("the caller's directory", func(t *testing.T) {
		mine := t.TempDir()
		if err := os.Chmod(mine, 0o700); err != nil {
			t.Fatal(err)
		}
		check, digest := swapInstall(t, recordingLauncher, passes)
		record := filepath.Join(t.TempDir(), "record")
		g := guard(t, check, digest, record, mine)
		if g.copyBase != mine || g.copyFallback != mine {
			t.Fatalf("New made the copy's base %s and its fallback %s, want %s for both", g.copyBase, g.copyFallback, mine)
		}
		if err := text(g); err != nil {
			t.Fatalf("the write under the caller's directory = %v, want it cleared", err)
		}
		ran := readRunRecord(t, record)
		resolved, err := filepath.EvalSymlinks(mine)
		if err != nil {
			t.Fatal(err)
		}
		if len(ran.from) != 1 || filepath.Dir(ran.from[0]) != resolved {
			t.Fatalf("the check ran from %q, want once from a copy under %s", ran.from, resolved)
		}
	})
	t.Run("no directory of the caller's", func(t *testing.T) {
		check, digest := swapInstall(t, recordingLauncher, passes)
		record := filepath.Join(t.TempDir(), "record")
		failed := setupFailure(t, text(guard(t, check, digest, record, "")))
		if !strings.Contains(failed.Error(), "could not be copied where only this role can reach it") || !strings.Contains(failed.Error(), unwritable) {
			t.Fatalf("the write with no directory of the caller's = %q, want the copy under %s refused", failed.Error(), unwritable)
		}
		if _, err := os.Stat(record); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the check ran with no copy made (%v)", err)
		}
	})
}

// A private copy that fails removes the directory it made as sharedRun.remove removes the copy
// (rolefile.RemoveTree): without a plain open of the directory's parent, which would wait on a fifo
// there until a writer came. os.RemoveAll makes that open whenever it cannot unlink its path
// outright. In a pod the parent is the role's private directory, a mount point nothing can rename,
// so this row's parent is a directory of its own, swapped for a fifo once the copy has made its
// directory and before it writes the first file: that write then fails (ENOTDIR), and the removal
// meets the fifo. writeCopyFile holds syscall.ForkLock for reading while it writes, so the row
// holds it for writing until the swap is made, which is only how the row times the swap.
func TestAFailedPrivateCopyAnswersAParentSwappedForAFifo(t *testing.T) {
	root := t.TempDir()
	base, out := filepath.Join(root, "private"), filepath.Join(root, "out")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	held := heldCheck{files: map[string][]byte{}}
	for _, name := range ManifestFiles {
		held.files[name] = []byte("the copy of " + name + "\n")
	}
	swapped := make(chan error, 1)
	syscall.ForkLock.Lock()
	go func() {
		defer syscall.ForkLock.Unlock()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if made, _ := filepath.Glob(filepath.Join(base, "leak-check-*")); len(made) != 0 {
				if err := os.Rename(base, out); err != nil {
					swapped <- err
					return
				}
				swapped <- unix.Mkfifo(base, 0o600)
				return
			}
		}
		swapped <- errors.New("the copy made no directory within 10s")
	}()
	err := returnsWithoutAWriter(t, base, "the failed private copy", func() error {
		_, err := held.privateCopy(base, base)
		return err
	})
	if swap := <-swapped; swap != nil {
		t.Fatalf("the swap: %v", swap)
	}
	if !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("the private copy answered %v, want its first file refused once the parent is no directory (ENOTDIR)", err)
	}
	stillFifo(t, base)
}
