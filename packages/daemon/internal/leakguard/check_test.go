package leakguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
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
	visibility, err := guard.Visibility(testwait.Context(t), "invented-org/invented-repo")
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
		Env:        fixtureenv.Isolated(t.TempDir()),
	})
	// An owner GitHub serves none under: a name beginning with a hyphen.
	err := guard.Text(testwait.Context(t), TextRequest{Repo: "-invented-org/invented-repo", Text: "a clean title\n"})
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
	if err := guard.Text(testwait.Context(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}); err != nil {
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
		status, _, err := guard.run(testwait.Context(t), child{argv: []string{program}})
		if err != nil {
			t.Fatalf("run %s: %v", program, err)
		}
		if status == 0 {
			t.Errorf("%s answered a credential (exit 0), want a program that cannot", program)
		}
	}
}

// stopsAfterItsInput is a stand-in shared check that reads its standard input, as the real one
// does, and then sleeps far past any bound these tests give it: a check whose git waits on a
// remote that never answers, as the guard sees one. The sleep is a child of the check's bash, in
// the check's process group, and the stand-in writes its process id to pidFile, which says the
// check runs and lets the test read whether the stop reached the sleep.
func stopsAfterItsInput(pidFile string) string {
	return fmt.Sprintf("cat >/dev/null\nsleep 30 &\necho $! >%q\nwait\n", pidFile)
}

// boundOn is a caller's context bounded at d from now on clock: it ends, with
// context.DeadlineExceeded, once the test moves clock to its deadline, and its Deadline reports
// that deadline, as a caller's deadline context does in production. A clocktest.Fake's own
// deadline context reports none, so that no fake deadline reaches a library that would compare it
// with the wall clock; the guard reads it only to name the bound it stopped a child at.
func boundOn(t *testing.T, clock *clocktest.Fake, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := clock.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return reportsDeadline{Context: ctx, at: clock.Now().Add(d)}
}

// reportsDeadline is a context that reports at as its deadline (boundOn).
type reportsDeadline struct {
	context.Context
	at time.Time
}

func (c reportsDeadline) Deadline() (time.Time, bool) { return c.at, true }

// stoppedPush is a push under ctx through a guard on clock whose shared check is
// stopsAfterItsInput, as the setup failure it must be. The shared check runs before a push reads
// anything of its repository, so the push needs none. stop ends ctx once the check runs. A push
// the stop did not end answers only when the stand-in's sleep ends on its own, as a pass, which is
// no setup failure; and once a stopped push answers, the stop has reached the check's sleep too,
// since a sleep it missed would still be running and holding the check's output pipe.
func stoppedPush(t *testing.T, ctx context.Context, clock *clocktest.Fake, stop func()) *SetupFailure {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	guard := New(Options{
		Check:      leakguardtest.Write(t, t.TempDir(), stopsAfterItsInput(pidFile)),
		Visibility: answers(Public, nil),
		Env:        fixtureenv.Isolated(t.TempDir()),
		Clock:      clock,
	})
	answered := make(chan error, 1)
	go func() {
		answered <- guard.Push(ctx, PushRequest{
			Dir:     t.TempDir(),
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
		})
	}()
	sleep := testwait.Until(t, "the shared check to run and write its sleep's process id", func() (int, bool) {
		select {
		case err := <-answered:
			t.Fatalf("the push answered before the shared check started its sleep: %v", err)
		default:
		}
		raw, _ := os.ReadFile(pidFile)
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		return pid, err == nil && strings.HasSuffix(string(raw), "\n")
	})
	stop()
	err := testwait.Receive(t, "the push's answer once its stop came", answered)
	// /proc/<pid>/cmdline is empty for a process that has exited, whether it was reaped or not.
	cmdline, read := os.ReadFile(filepath.Join("/proc", strconv.Itoa(sleep), "cmdline"))
	switch {
	case read == nil && len(cmdline) > 0:
		t.Errorf("the check's sleep %d still runs once the push answered: the stop did not reach the check's process group", sleep)
	case read != nil && !errors.Is(read, os.ErrNotExist):
		t.Fatalf("read whether the check's sleep %d still runs: %v", sleep, read)
	}
	return setupFailure(t, err)
}

// stoppedAtItsBound is call's answer under a bound of 2 s on clock, which the test moves to the
// bound once the stand-in program that writes started has started, and not before: the guard
// reads the time a child starts at before it starts it (stoppedOnceStarted).
func stoppedAtItsBound(t *testing.T, clock *clocktest.Fake, started string, call func(context.Context) error) error {
	t.Helper()
	ctx := boundOn(t, clock, 2*time.Second)
	return stoppedOnceStarted(t, started, func() { clock.Advance(2 * time.Second) }, func() error { return call(ctx) })
}

// stoppedOnceStarted is call's answer once the test has run stop, which ends the call's context
// (a cancel, or a move of the test clock to a deadline on it). stop runs once the stand-in
// program that writes started has started, and not before: an end that came before the program
// started would stop nothing it is meant to. A call that answers before the program starts fails
// the test.
func stoppedOnceStarted(t *testing.T, started string, stop func(), call func() error) error {
	t.Helper()
	answered := make(chan error, 1)
	go func() { answered <- call() }()
	testwait.Eventually(t, "the stand-in program to start", func() bool {
		select {
		case err := <-answered:
			t.Fatalf("the call answered before the stand-in program started: %v", err)
		default:
		}
		_, err := os.Stat(started)
		return err == nil
	})
	stop()
	return testwait.Receive(t, "the call's answer once its stop came", answered)
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

// A shared check its bound stops is a setup failure that names the check, the time it ran and
// the bound it had, once, so a person reading the line can tell a check given too little time
// from one that hung. The bound runs on the guard's clock, which the test moves to it, so the
// check ran exactly its bound; that the kill reached the sleep bash started in the check's own
// process group is read from the sleep itself (stoppedPush).
func TestASharedCheckStoppedByItsBoundNamesTheCheckItsTimeAndItsBound(t *testing.T) {
	clock := clocktest.New(time.Now())
	failed := stoppedPush(t, boundOn(t, clock, 2*time.Second), clock, func() { clock.Advance(2 * time.Second) })
	says(t, failed, "the shared public leak check", "ran 2 s", "bound of 2 s")
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// A shared check that a cancel stops, under a context with no deadline, says that a cancel
// stopped it and how long it ran: there is no bound to name. The test cancels once the guard's
// clock has moved 2 s past the check's start.
func TestASharedCheckKilledByACancelSaysSoWithItsTime(t *testing.T) {
	clock := clocktest.New(time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := stoppedPush(t, ctx, clock, func() {
		clock.Advance(2 * time.Second)
		cancel()
	})
	says(t, failed, "the shared public leak check", "ran 2 s", "stopped by a cancel")
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// The gh visibility read and the git reads of a push's records keep the setup failure of their
// own, and the stop is the error beneath it, naming the program, the time it ran and its bound.
func TestAReadStoppedByItsBoundNamesItsProgramItsTimeAndItsBound(t *testing.T) {
	// sleeps is the PATH entry of a directory whose one program is name, which says it started in
	// the file started names and then sleeps far past the bound, in front of the test's own PATH:
	// the guard's lookup finds it before the real one, and the check's bash still finds
	// everything else.
	sleeps := func(t *testing.T, name string) (path, started string) {
		dir := t.TempDir()
		started = filepath.Join(dir, name+".started")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("#!/bin/sh\n: >%q\nsleep 30\n", started)), 0o700); err != nil {
			t.Fatal(err)
		}
		return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), started
	}
	t.Run("the gh visibility read", func(t *testing.T) {
		clock := clocktest.New(time.Now())
		path, started := sleeps(t, "gh")
		guard := New(Options{
			Check: leakguardtest.Write(t, t.TempDir(), passes),
			Env:   append(fixtureenv.Isolated(t.TempDir()), path),
			Clock: clock,
		})
		err := stoppedAtItsBound(t, clock, started, func(ctx context.Context) error {
			return guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
		})
		says(t, setupFailure(t, err), "could not be read: gh ran 2 s and was stopped by its bound of 2 s")
	})
	t.Run("a git read of the push's records", func(t *testing.T) {
		clock := clocktest.New(time.Now())
		path, started := sleeps(t, "git")
		guard := New(Options{
			Check:      leakguardtest.Write(t, t.TempDir(), passes),
			Visibility: answers(Public, nil),
			Env:        append(fixtureenv.Isolated(t.TempDir()), path),
			Clock:      clock,
		})
		// A local commit and a ref the destination does not hold yet: the guard asks git for the
		// commits the push would add, which is the read the stand-in git never answers.
		err := stoppedAtItsBound(t, clock, started, func(ctx context.Context) error {
			return guard.Push(ctx, PushRequest{
				Dir:     t.TempDir(),
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
			})
		})
		says(t, setupFailure(t, err), "the push's commits could not be read: git ran 2 s and was stopped by its bound of 2 s")
	})
}
