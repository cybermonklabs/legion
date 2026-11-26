package leakguard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Resolve looks in the order the shared check documents — PUBLIC_LEAK_CHECK, then
// public-leak-check.sh on PATH, then the pointer file under the configuration home — with the
// guard's own Check option in front of all three, and it answers nothing where none of them
// names a check that is there.
func TestResolveFindsTheCheckInTheDocumentedOrder(t *testing.T) {
	home := t.TempDir()
	onPath := leakguardtest.Write(t, t.TempDir(), passes)
	named := leakguardtest.Write(t, t.TempDir(), passes)
	pointed := leakguardtest.Write(t, t.TempDir(), passes)
	option := leakguardtest.Write(t, t.TempDir(), passes)
	pointer := filepath.Join(home, ".config", pointerFile)
	if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte(pointed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Isolated puts HOME and the configuration home under it in the environment, so the pointer
	// file above is the one Resolve reads.
	base := fixtureenv.Isolated(home)
	for _, tc := range []struct {
		name  string
		check string
		env   []string
		want  string
	}{
		{
			name:  "the guard's own option comes first",
			check: option,
			env:   []string{"PUBLIC_LEAK_CHECK=" + named, "PATH=" + filepath.Dir(onPath)},
			want:  option,
		},
		{
			name: "PUBLIC_LEAK_CHECK comes before PATH",
			env:  []string{"PUBLIC_LEAK_CHECK=" + named, "PATH=" + filepath.Dir(onPath)},
			want: named,
		},
		{
			name: "PATH comes before the pointer file",
			env:  []string{"PATH=" + filepath.Dir(onPath)},
			want: onPath,
		},
		{
			name: "the pointer file is last",
			env:  []string{"PATH=" + t.TempDir()},
			want: pointed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := New(Options{Check: tc.check, Env: append(base, tc.env...)})
			got, err := guard.Resolve()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolved %s, want %s", got, tc.want)
			}
		})
	}
	t.Run("none of the three names a check", func(t *testing.T) {
		guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir())})
		if got, err := guard.Resolve(); err == nil {
			t.Fatalf("resolved %s, want no check", got)
		}
	})
	t.Run("a name that is not there is not a check", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), checkName)
		guard := New(Options{Env: append(base, "PUBLIC_LEAK_CHECK="+gone)})
		_, err := guard.Resolve()
		if err == nil || !strings.Contains(err.Error(), gone) {
			t.Fatalf("got %v, want the missing check named", err)
		}
	})
}

// Resolve runs at every call rather than once at start: the shared check is published often, and
// a guard that kept the first answer would keep running a copy the rules have moved past.
func TestResolveReadsTheCheckAgainAtEveryCall(t *testing.T) {
	dir := t.TempDir()
	check := filepath.Join(dir, checkName)
	if err := os.WriteFile(check, []byte("#!/bin/sh\n"+passes), 0o700); err != nil {
		t.Fatal(err)
	}
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+dir)})
	if _, err := guard.Resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := os.Remove(check); err != nil {
		t.Fatal(err)
	}
	if got, err := guard.Resolve(); err == nil {
		t.Fatalf("resolved %s after the check was published away, want it read again", got)
	}
}

// A check named by a relative path — by PUBLIC_LEAK_CHECK, by the pointer file, or by the guard's
// own Check option — names no one file: the guard would hash the one in its working directory and
// exec would run a bare name from PATH. Each is refused as a broken setup naming it, under a
// deployment's digest and under none, and nothing of the check's name runs, neither the copy in
// the working directory nor the one on PATH.
func TestACheckNamedByARelativePathIsABrokenSetup(t *testing.T) {
	work, bin, home := t.TempDir(), t.TempDir(), t.TempDir()
	record := filepath.Join(t.TempDir(), "record")
	leakguardtest.Write(t, work, leakguardtest.Recording(passes))
	leakguardtest.Companions(t, work, leakguardtest.Program)
	digest, err := manifestSHA256(filepath.Join(work, checkName))
	if err != nil {
		t.Fatal(err)
	}
	leakguardtest.Write(t, bin, leakguardtest.Recording(passes))
	pointer := filepath.Join(home, ".config", pointerFile)
	if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte(checkName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	withBin := "PATH=" + bin + ":/usr/bin:/bin"
	for _, source := range []struct {
		name, said, check string
		env               []string
	}{
		{name: CheckVar, said: CheckVar, env: []string{CheckVar + "=" + checkName, withBin}},
		// PATH holds no file of the check's name here, or PATH would answer before the pointer.
		{name: "the pointer file", said: "the pointer file " + pointer, env: []string{"PATH=/usr/bin:/bin"}},
		{name: "the Check option", said: "the guard's own Check option", check: checkName, env: []string{withBin}},
	} {
		for _, checker := range []string{"", digest} {
			t.Run(source.name+", digest told "+strconv.FormatBool(checker != ""), func(t *testing.T) {
				env := append(fixtureenv.Isolated(home), "LEAK_CHECK_RECORD="+record)
				env = append(env, source.env...)
				if checker != "" {
					env = append(env, CheckerVar+"="+checker)
				}
				guard := New(Options{Check: source.check, Visibility: answers(Public, nil), Env: env})
				_, resolved := guard.Resolve()
				if at, broken := BrokenAt(resolved); !broken || at != checkName {
					t.Fatalf("Resolve = %v, want a broken setup at %s", resolved, checkName)
				}
				failed := setupFailure(t, guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}))
				says(t, failed, whyNoCheck, source.said+" names "+checkName+", which is no absolute path")
				if ran, _ := os.ReadFile(record); len(ran) != 0 {
					t.Fatalf("a check of the name ran: %q", ran)
				}
			})
		}
	}
}

// The private copy of a check (Guard.sharedCheck) is made under a role's own private directory
// where a directory is there, and under the fallback only where nothing is at that path at all:
// anything else there — a file, a path through a file — refuses, so no copy is made where another
// role may reach it. That the copy is made there and not under TMPDIR is
// TestACheckUnderTheDigestRunsFromAPrivateCopyThatIsGoneAfter's.
func TestThePrivateCopyIsMadeUnderTheRolesOwnDirectory(t *testing.T) {
	own, fallback := t.TempDir(), t.TempDir()
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, base, want string
	}{
		{"a directory there", own, own},
		{"nothing there", filepath.Join(own, "absent"), fallback},
		{"a file there", file, ""},
		{"a path through a file", filepath.Join(file, "private"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := copyBase(tc.base, fallback)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("copyBase(%s) = %s, want a refusal", tc.base, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("copyBase(%s) = %q, %v; want %s", tc.base, got, err, tc.want)
			}
		})
	}
}

// A file of the private copy is written only where nothing is: a link at its path is neither
// followed nor written through, and a file already there is neither opened nor written. Each is
// planted at the copy's first file once the copy has made its directory and before it writes
// (beforeTheFirstWrite), and the file already there is a hard link to one outside the copy, so
// what it holds is read after the failed copy has removed its directory. A file the copy writes
// has its mode exactly, whatever the umask took off the create's: under a umask that takes the
// owner's read off, the launcher is still 0500, so it runs, and each file it reads 0400.
func TestACopyFileIsWrittenOnlyWhereNothingIsAndOfItsModeExactly(t *testing.T) {
	held := aHeldCheck()
	const kept = "a file outside the copy\n"
	for _, tc := range []struct {
		name  string
		plant func(outside, path string) error
	}{
		{"a link", func(outside, path string) error { return os.Symlink(outside, path) }},
		{"a file already there", func(outside, path string) error { return os.Link(outside, path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			base, outside := filepath.Join(root, "private"), filepath.Join(root, "outside")
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outside, []byte(kept), 0o600); err != nil {
				t.Fatal(err)
			}
			copies := filepath.Join(base, "leak-check-*")
			err := beforeTheFirstWrite(t, copies, func(dir string) (string, error) {
				path := filepath.Join(dir, ManifestFiles[0])
				return path, tc.plant(outside, path)
			}, "the private copy beside "+tc.name, func() error {
				_, err := held.privateCopy(base, base)
				return err
			})
			if !errors.Is(err, fs.ErrExist) {
				t.Errorf("the private copy over %s answered %v, want its first file refused as existing", tc.name, err)
			}
			if got, err := os.ReadFile(outside); err != nil || string(got) != kept {
				t.Errorf("%s holds %q (%v) after the refused copy, want %q", outside, got, err, kept)
			}
			if left, _ := filepath.Glob(copies); left != nil {
				t.Errorf("the refused copy left %v", left)
			}
		})
	}
	t.Run("under a umask that takes the owner's read off", func(t *testing.T) {
		base := t.TempDir()
		umask := syscall.Umask(0o400)
		dir, err := held.privateCopy(base, base)
		syscall.Umask(umask)
		if err != nil {
			t.Fatal(err)
		}
		// The umask took the owner's read off the copy's directory too (0300), and the removal of
		// the test's directory must list it.
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		for _, name := range ManifestFiles {
			want := os.FileMode(0o400)
			if name == checkName {
				want = 0o500
			}
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if body, _ := os.ReadFile(filepath.Join(dir, name)); info.Mode() != want || string(body) != string(held.files[name]) {
				t.Errorf("the copy's %s is %s holding %q, want a regular file of mode %s holding %q", name, info.Mode(), body, want, held.files[name])
			}
		}
	})
}

// aHeldCheck is a held check whose every manifest file holds the copy of its name.
func aHeldCheck() heldCheck {
	held := heldCheck{files: map[string][]byte{}}
	for _, name := range ManifestFiles {
		held.files[name] = []byte("the copy of " + name + "\n")
	}
	return held
}

// The visibility read is the one the shared check itself makes, argv for argv and variable for
// variable, so that the exemption `legion gh` makes for the check's own read covers this one too
// and a pod's gh, which is `legion gh`, cannot call the guard again.
func TestTheVisibilityReadIsTheOneTheSharedCheckMakes(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	gh := "#!/bin/sh\n{\n  echo \"argv=$*\"\n  echo \"read=${PUBLIC_LEAK_CHECK_VISIBILITY_READ}\"\n  echo \"host=${GH_HOST}\"\n} > \"" + record + "\"\necho public\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+dir)})
	visibility, err := guard.Visibility(bound(t), "invented-org/invented-repo")
	if err != nil {
		t.Fatalf("read the visibility: %v", err)
	}
	if visibility != Public {
		t.Errorf("read %q, want public", visibility)
	}
	read, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	want := "argv=api --hostname github.com repos/invented-org/invented-repo --include --jq .visibility\n" +
		"read=invented-org/invented-repo\nhost=github.com\n"
	if string(read) != want {
		t.Errorf("gh was called as\n%s\nwant\n%s", read, want)
	}
}

// A destination the exemption could not cover is read for no visibility at all, and refuses the
// write. `legion gh` lets the check's own visibility read through unchecked only for a slug
// ghrepo.Parse takes, so a read for any other slug would come back through `legion gh`, into
// the guard, into a read of the same slug, with no end to it. The guard makes no such read: a
// slug the parser refuses is a setup failure where the write is, which is where the recursion
// is broken, and the gh of the pod is never started.
func TestADestinationTheParserRefusesReadsNoVisibility(t *testing.T) {
	var asked []string
	guard := New(Options{
		Check:      leakguardtest.Write(t, t.TempDir(), passes),
		Visibility: answers(Public, &asked),
		Env:        withRules(t, fixtureenv.Isolated(t.TempDir())),
	})
	// An owner GitHub serves none under: a name beginning with a hyphen.
	err := guard.Text(bound(t), TextRequest{Repo: "-invented-org/invented-repo", Text: "a clean title\n"})
	failed := setupFailure(t, err)
	if !strings.Contains(failed.Why, "no repository GitHub serves") {
		t.Errorf("the setup failure is %q, want it to say the destination is no repository", failed.Error())
	}
	if len(asked) != 0 {
		t.Errorf("the visibility of %q was read, want no read at all", asked)
	}
}

// The environment every child runs in makes a credential prompt fail closed: git asks nothing of
// a terminal, and the askpass programs both name one that answers nothing, so a check whose git
// cannot find a credential fails rather than waiting for a person who is not there.
func TestAChildCannotBeAskedForACredential(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	reports := `{
  echo "prompt=${GIT_TERMINAL_PROMPT}"
  echo "askpass=${GIT_ASKPASS}"
  echo "ssh=${SSH_ASKPASS}"
  echo "require=${SSH_ASKPASS_REQUIRE}"
} > "${STANDIN_RECORD}"
exit 0
`
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), reports), Public, "STANDIN_RECORD="+record)
	if err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}); err != nil {
		t.Fatalf("a clean text: %v", err)
	}
	read, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(read)), "\n")
	if len(lines) != 4 || lines[0] != "prompt=0" || lines[3] != "require=force" {
		t.Fatalf("the child's environment is %q", read)
	}
	for _, line := range lines[1:3] {
		program := strings.SplitN(line, "=", 2)[1]
		if program == "" {
			t.Fatalf("the child's environment names no askpass program: %q", read)
		}
		status, _, err := guard.run(bound(t), child{argv: []string{program}})
		if err != nil {
			t.Fatalf("run %s: %v", program, err)
		}
		if status == 0 {
			t.Errorf("%s answered a credential (exit 0), want a program that cannot", program)
		}
	}
}

// stopsAfterItsInput is a stand-in shared check that reads its standard input, as the real one
// does, and then waits on a sleep that outlasts any test binary's deadline, so it never answers on
// its own: a check whose git waits on a remote that never answers, as the guard sees one. The sleep
// is bash's child in the check's own process group, and the check writes its pid to pids.
func stopsAfterItsInput(pids string) string {
	return "cat >/dev/null\nsleep 86400 &\necho $! >" + shQuote(pids) + "\nwait\n"
}

// stoppedPush is a push under ctx through a guard whose shared check is stopsAfterItsInput, as
// the setup failure it must be, and how long the push took, once the stop the guard made of the
// check has also ended the check's sleep (sleepEnded). onceSleeping, when it is not nil, runs as
// soon as the check has written its sleep's pid, and the row then requires that sleep's end. The
// shared check runs before a push reads anything of its repository, so the push needs none. It
// also answers the path of the file the check wrote its sleep's pid to, whose time of writing
// bounds when the check had started (TestASharedCheckStoppedByItsBoundNamesTheCheckItsTimeAndItsBound).
func stoppedPush(t *testing.T, ctx context.Context, onceSleeping func()) (*SetupFailure, time.Duration, string) {
	t.Helper()
	pids := filepath.Join(t.TempDir(), "sleep")
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), stopsAfterItsInput(pids)), Public)
	req := PushRequest{
		Dir:     t.TempDir(),
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	}
	pushed, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for onceSleeping != nil {
			if text, _ := os.ReadFile(pids); strings.TrimSpace(string(text)) != "" {
				onceSleeping()
				return
			}
			select {
			case <-pushed:
				return
			case <-time.After(testwait.Poll):
			}
		}
	}()
	var err error
	var took time.Duration
	answered(t, "the push whose shared check never answers", func() {
		began := time.Now()
		err = guard.Push(ctx, req)
		took = time.Since(began)
	})
	close(pushed)
	<-watched
	failed := setupFailure(t, err)
	sleepEnded(t, pids, onceSleeping != nil)
	return failed, took, pids
}

// answered runs call and waits for it to return with the test binary's deadline as its only bound
// (testwait.Receive): a call that never returns fails the row by name before the package's
// timeout, and one a loaded machine makes slow passes.
func answered(t *testing.T, what string, call func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()
	testwait.Receive(t, what, done)
}

// sleepEnded waits, with the test binary's deadline as its only bound (testwait.Receive), for the
// sleep whose pid the stand-in check wrote to pids to be gone: the kill the guard sends the
// check's process group reaches it. A sleep the kill missed would run on for a day, past any
// deadline, so the wait fails the row by name; it is killed when the test ends, whatever the wait
// read. A check the stop reached before it wrote the pid, which a loaded machine can make it, fails
// a row that required the sleep; any other row says in its log that it tested that case, and that
// the kill reaching the sleep went untested in this run.
func sleepEnded(t *testing.T, pids string, required bool) {
	t.Helper()
	text, err := os.ReadFile(pids)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read the stand-in check's pid of its sleep: %v", err)
	}
	if strings.TrimSpace(string(text)) == "" {
		if required {
			t.Fatalf("the stand-in check wrote no pid of its sleep to %s, though its stop was to come after it", pids)
		}
		t.Logf("this run tested a stop that came before the stand-in check wrote its sleep's pid to %s; the kill reaching that sleep went untested", pids)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatalf("the stand-in check wrote %q as its sleep's pid: %v", text, err)
	}
	t.Cleanup(func() {
		if sleepRunning(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for sleepRunning(pid) {
			time.Sleep(testwait.Poll)
		}
	}()
	testwait.Receive(t, fmt.Sprintf("the stand-in check's sleep (pid %d) to end with the check's process group", pid), gone)
}

// sleepRunning is whether pid is a sleep that has not exited: its /proc entry names sleep and its
// state is not a zombie's.
func sleepRunning(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The fields after the command's closing parenthesis begin with the state.
	name, rest, ok := strings.Cut(string(stat), ") ")
	return ok && strings.HasSuffix(name, "(sleep") && !strings.HasPrefix(rest, "Z")
}

// says fails the test for each of wants that the setup failure's line does not hold.
func says(t *testing.T, failed *SetupFailure, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(failed.Error(), want) {
			t.Errorf("the setup failure says %q, want it to hold %q", failed.Error(), want)
		}
	}
}

// stopSaid is the sentence the stop beneath err says of program, in the whole seconds the guard
// counted, once that stop is held to the deadline of ctx, the caller's context. The guard counts
// from the moment it started program (Guard.run), so the start it counted from is the deadline
// less the bound, and the moment of the stop is that start plus the run. The start must come no
// earlier than called, when the test made the call, and before the deadline, since the bound is
// what was left of it; the stop no earlier than the deadline, which is what ended program, and no
// later than now, after the call answered. So the bound is more than 0 and no more than the
// caller's, and the run no shorter than the bound. No comparison rounds to a second, so however
// long the guard took before it started program, the verdict is the same.
func stopSaid(t *testing.T, ctx context.Context, err error, program string, called time.Time) string {
	t.Helper()
	answered := time.Now()
	var stopped *stoppedError
	if !errors.As(err, &stopped) {
		t.Fatalf("%v carries no stop of %s beneath it", err, program)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the caller's context has no deadline to hold the stop to")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the caller's context ended with %v, want its deadline to have stopped %s", ctx.Err(), program)
	}
	began := deadline.Add(-stopped.bound)
	ended := began.Add(stopped.ran)
	if stopped.program != program || stopped.cancelled || began.Before(called) || !began.Before(deadline) ||
		ended.Before(deadline) || ended.After(answered) {
		t.Fatalf("the stop beneath %q is of %s (by a cancel: %v), begun %s after the call and %s before the deadline, "+
			"ended %s after the deadline and %s before the call answered; want a stop of %s by the deadline, begun after the "+
			"call and before the deadline, ended at or after the deadline and before the call answered",
			err, stopped.program, stopped.cancelled, began.Sub(called), deadline.Sub(began), ended.Sub(deadline), answered.Sub(ended), program)
	}
	return fmt.Sprintf("%s ran %d s and was stopped by its bound of %d s",
		program, int64(stopped.ran.Round(time.Second)/time.Second), int64(stopped.bound.Round(time.Second)/time.Second))
}

// A shared check its bound stops is a setup failure that names the check, the time it ran and
// the bound it had, once, so a person reading the line can tell a check given too little time
// from one that hung. The seconds are read against the caller's bound (stoppedByItsBound), never
// for an exact count: the bound is what was left of the caller's deadline when the check started,
// and on a loaded machine the guard's own work before the check takes longer. Where the stand-in
// wrote its sleep's process id before the bound ended, the bound also has a floor: the check
// started before that file was written, so the bound is no shorter than what was left of the
// deadline when the file was written, less 50 ms for the file's coarser clock. Rounding to whole
// seconds keeps that order, so the floor needs no more slack. The floor is what a wrong bound
// fails, so the caller's bound is 10 s, long enough that the stand-in has written its process id
// before it ends: at a load average of 1,500, a bound of 2 s left the file unwritten in some runs.
// Where there is no file there is no floor to read; a file written after the deadline, before the
// stop reached the stand-in, gives a floor of 0 s or less, which any bound meets, so the test asks
// for no floor either way. A run with no file says in its log that it read no floor, and any other
// failure to read the file fails the test, since it says nothing of when the stand-in wrote. The
// kill reaches the sleep bash started in the check's own process group (stoppedPush): a sleep the
// kill missed would hold the output pipe open, and exec would wait its WaitDelay for it.
func TestASharedCheckStoppedByItsBoundNamesTheCheckItsTimeAndItsBound(t *testing.T) {
	const theBound = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), theBound)
	defer cancel()
	failed, _, pids := stoppedPush(t, ctx, nil)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the push answered %q inside the caller's %s: the caller's deadline did not stop the check", failed.Error(), theBound)
	}
	stoppedByItsBound(t, failed, theBound)
	written, err := os.Stat(pids)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.Logf("the stand-in wrote no process id before its bound ended: no floor read")
	case err != nil:
		t.Fatal(err)
	default:
		deadline, _ := ctx.Deadline()
		floor := wholeSeconds(deadline.Sub(written.ModTime()) - 50*time.Millisecond)
		if _, bound, _ := sharedStop(t, failed); bound < floor {
			t.Errorf("the setup failure says %q: a bound of %d s, want one no shorter than the %d s left of the deadline when the stand-in wrote its sleep's process id, after the check had started",
				failed.Error(), bound, floor)
		}
	}
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// A shared check that a cancel stops, under a context with no deadline, says that a cancel
// stopped it and how long it ran: there is no bound to name. The cancel comes 2 s after the check
// started its sleep (stoppedPush), so the check has run at least that long however late it started,
// and the row reads the kill reaching that sleep in every run. The run is read between that floor
// (stoppedByACancel) and the push's own time, never for an exact count.
func TestASharedCheckKilledByACancelSaysSoWithItsTime(t *testing.T) {
	const after = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed, took, _ := stoppedPush(t, ctx, func() {
		time.Sleep(after)
		cancel()
	})
	stoppedByACancel(t, failed, after)
	if ran, _, _ := sharedStop(t, failed); ran > wholeSeconds(took) {
		t.Errorf("the setup failure says %q: a run of %d s, want one no longer than the push's own %s", failed.Error(), ran, took.Round(time.Millisecond))
	}
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// A shared check a signal ended (the kernel's out-of-memory kill) is a setup failure that it could
// not be run, with the signal and what the check printed before it beneath: the check may have said
// why first. Read as a status, it was "exited -1, which it does not document", with that output.
func TestASharedCheckASignalEndedKeepsWhatItPrinted(t *testing.T) {
	killsItself := "cat >/dev/null\necho 'LEAK CHECK ERROR: out of memory reading the history' >&2\nkill -KILL $$\n"
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), killsItself), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     t.TempDir(),
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	})
	failed := setupFailure(t, err)
	says(t, failed, "the shared public leak check could not be run")
	if want := "signal: killed\nLEAK CHECK ERROR: out of memory reading the history"; failed.Err == nil || failed.Err.Error() != want {
		t.Errorf("the setup failure carries %v beneath it, want %q", failed.Err, want)
	}
}

// A read of the guard's that a signal ended after its program printed (the kernel's out-of-memory
// kill) fails naming the signal, with what it printed beneath, as the same read's failed exit
// carries it: the gh visibility read, its GraphQL read, the curl read, the read with no credential,
// a git read of a push's records, which quotes only git's standard error (gitSaid), never the
// push's content on its standard output, and the token read, which quotes only its standard error,
// never the password on its standard output. Each row's stand-in is the one program it names, first
// on PATH, printing and killing itself; the curl read runs Guard.curl, never PATH's, so it is
// pointed at the stand-in.
func TestAReadASignalEndedKeepsWhatItPrinted(t *testing.T) {
	const slug = "invented-org/invented-repo"
	killsItself := func(t *testing.T, name, script string, env ...string) *Guard {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\nkill -KILL $$\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		guard := New(Options{Env: append(append(fixtureenv.Isolated(t.TempDir()), env...), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))})
		guard.curl = filepath.Join(dir, "curl")
		return guard
	}
	said := "error connecting to api.github.com for " + slug
	toStderr := "echo '" + said + "' >&2"
	for _, row := range []struct {
		name, program, script string
		env                   []string
		read                  func(g *Guard) error
	}{
		{"the gh visibility read", "gh", toStderr, nil, func(g *Guard) error { _, err := g.ghVisibility(bound(t), slug); return err }},
		{"the GraphQL read", "gh", toStderr, nil, func(g *Guard) error { _, err := g.graphQLVisibility(bound(t), slug); return err }},
		{"the curl read", "curl", toStderr, []string{"GH_TOKEN=" + theToken}, func(g *Guard) error { _, err := g.curlVisibility(bound(t), slug); return err }},
		{"the read with no credential", "git", toStderr, nil, func(g *Guard) error { _, err := g.lsRemote(bound(t), githubOrigin, slug); return err }},
		{"a git read of a push's records", "git", "echo '+the pushed line'\n" + toStderr, nil, func(g *Guard) error { _, err := g.git(bound(t), t.TempDir(), "rev-list", "HEAD"); return err }},
		{"the token read", "gh", "echo 'password=" + theToken + "'\n" + toStderr, nil, func(g *Guard) error { _, err := g.ghToken(bound(t), slug); return err }},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := row.read(killsItself(t, row.program, row.script, row.env...))
			if want := "signal: killed\n" + said; err == nil || err.Error() != want {
				t.Errorf("read = %v, want %q", err, want)
			}
		})
	}
}

// The gh visibility read and the git reads of a push's records keep the setup failure of their
// own, and the stop is the error beneath it, naming the program, the time it ran and its bound.
// The bound is what was left of the caller's deadline when the child started (Guard.run), so the
// row reads the figures the guard made, never a count of seconds the test supposes. The caller's
// deadline is due 2 s after the call, and the guard must have started the program by then
// (startedBeforeDue), as a deadline of 2 s requires of the guard's own work before the child, the
// shared check among it. The deadline passes when due, but not before the stand-in program has
// started (deadlinePassedOnceStarted): a program the guard started in time may, on a loaded
// machine, run its first line later, and a guard that started it late is named as one rather than
// read as a stop of something else. The line is held to the stop beneath it, and the stop to the
// caller's deadline (stopSaid), never to the two seconds whole.
func TestAReadStoppedByItsBoundNamesItsProgramItsTimeAndItsBound(t *testing.T) {
	// neverAnswers is the PATH entry of a directory whose one program is name, which marks started
	// and sleeps past any test binary's deadline, in front of the test's own PATH: the guard's
	// lookup finds it before the real one, and the check's bash still finds everything else.
	neverAnswers := func(t *testing.T, name, started string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n: >"+shQuote(started)+"\nexec sleep 86400\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	t.Run("the gh visibility read", func(t *testing.T) {
		started := filepath.Join(t.TempDir(), "started")
		guard := New(Options{
			Check: leakguardtest.Write(t, t.TempDir(), passes),
			Env:   withRules(t, append(fixtureenv.Isolated(t.TempDir()), neverAnswers(t, "gh", started))),
		})
		ctx := deadlinePassedOnceStarted(t, 2*time.Second, started)
		var err error
		began := time.Now()
		answered(t, "the text whose gh visibility read never answers", func() {
			err = guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
		})
		says(t, setupFailure(t, err), "could not be read: "+stopSaid(t, ctx, err, "gh", began))
		startedBeforeDue(t, ctx, err, "gh")
	})
	t.Run("a git read of the push's records", func(t *testing.T) {
		started := filepath.Join(t.TempDir(), "started")
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, neverAnswers(t, "git", started))
		ctx := deadlinePassedOnceStarted(t, 2*time.Second, started)
		// A local commit and a ref the destination does not hold yet: the guard asks git for the
		// repository's remotes and then for the commits the push would add, which are reads the
		// stand-in git never answers.
		req := PushRequest{
			Dir:     t.TempDir(),
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
		}
		var err error
		began := time.Now()
		answered(t, "the push whose git read never answers", func() { err = guard.Push(ctx, req) })
		says(t, setupFailure(t, err), "the push's commits could not be read: "+stopSaid(t, ctx, err, "git", began))
		startedBeforeDue(t, ctx, err, "git")
	})
}

// deadlinePassedOnceStarted is a caller's context whose deadline is due bound after the call and
// passes then, but not before the file at started is there, which the stand-in program writes as
// its first act. It ends as a deadline does (testwait.StandInDeadline: its Err and its cause
// context.DeadlineExceeded). Its Deadline is the moment it is due until it passes, and the moment
// it passed from then on, so a program that started after the due moment still reads a bound
// that is what was left of the deadline when it started.
func deadlinePassedOnceStarted(t *testing.T, bound time.Duration, started string) dueDeadline {
	t.Helper()
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	due := time.Now().Add(bound)
	whenWritten(ctx, started, func() {
		time.Sleep(time.Until(due))
		pass(context.DeadlineExceeded)
	})
	return dueDeadline{Context: ctx, due: due}
}

// startedBeforeDue fails the test unless the stop beneath err is of program and the guard started
// program before ctx's deadline was due: the bound a deadline that passes when due sets on the
// guard's own work before its child, which deadlinePassedOnceStarted, passing no sooner than the
// child's first line, does not set by itself. The start is the one the guard counted (Guard.run),
// as stopSaid reads it: the deadline the guard read, less the bound it gave program.
func startedBeforeDue(t *testing.T, ctx dueDeadline, err error, program string) {
	t.Helper()
	var stopped *stoppedError
	if !errors.As(err, &stopped) || stopped.program != program {
		t.Fatalf("%v carries no stop of %s beneath it", err, program)
	}
	deadline, _ := ctx.Deadline()
	if began := deadline.Add(-stopped.bound); !began.Before(ctx.due) {
		t.Errorf("the guard started %s %s after the caller's deadline was due, want it started before: its own work before %s took longer than the bound",
			program, began.Sub(ctx.due).Round(time.Millisecond), program)
	}
}

// dueDeadline is a context whose deadline reads as due until the context it wraps has a deadline
// of its own: the moment a stand-in deadline passed.
type dueDeadline struct {
	context.Context
	due time.Time
}

func (d dueDeadline) Deadline() (time.Time, bool) {
	if at, ok := d.Context.Deadline(); ok {
		return at, true
	}
	return d.due, true
}
