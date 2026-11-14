package leakguard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
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
		Env:        fixtureenv.Isolated(t.TempDir()),
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
// does, and then sleeps far past any bound these tests give it: a check whose git waits on a
// remote that never answers, as the guard sees one.
const stopsAfterItsInput = "cat >/dev/null\nsleep 30\n"

// stoppedPush is a push under ctx through a guard whose shared check is stopsAfterItsInput, as
// the setup failure it must be. The shared check runs before a push reads anything of its
// repository, so the push needs none.
func stoppedPush(t *testing.T, ctx context.Context) *SetupFailure {
	t.Helper()
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), stopsAfterItsInput), Public)
	started := time.Now()
	err := guard.Push(ctx, PushRequest{
		Dir:     t.TempDir(),
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	})
	answeredWithin10s(t, started)
	return setupFailure(t, err)
}

// answeredWithin10s fails the test where the call that began at started took 10 s or more: the
// stop has to end the call, well before the stand-in's own 30 s sleep would.
func answeredWithin10s(t *testing.T, started time.Time) {
	t.Helper()
	if took := time.Since(started); took >= 10*time.Second {
		t.Fatalf("the call answered after %s, want under 10 s: the stop did not end it", took.Round(time.Second))
	}
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
// from one that hung. The time it ran also shows that the kill reached the sleep, which bash
// started in the check's own process group: a sleep the kill missed would hold the output pipe
// open, and exec would wait its WaitDelay of a second more for it, which the time would show as
// 3 s.
func TestASharedCheckStoppedByItsBoundNamesTheCheckItsTimeAndItsBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	failed := stoppedPush(t, ctx)
	says(t, failed, "the shared public leak check", "ran 2 s", "bound of 2 s")
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// A shared check that a cancel stops, under a context with no deadline, says that a cancel
// stopped it and how long it ran: there is no bound to name.
func TestASharedCheckKilledByACancelSaysSoWithItsTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer time.AfterFunc(2*time.Second, cancel).Stop()
	failed := stoppedPush(t, ctx)
	says(t, failed, "the shared public leak check", "ran 2 s", "stopped by a cancel")
	if failed.Err != nil {
		t.Errorf("the setup failure carries %q beneath it, want the stop said once", failed.Err)
	}
}

// The gh visibility read and the git reads of a push's records keep the setup failure of their
// own, and the stop is the error beneath it, naming the program, the time it ran and its bound.
func TestAReadStoppedByItsBoundNamesItsProgramItsTimeAndItsBound(t *testing.T) {
	// sleeps is the PATH entry of a directory whose one program is name, which sleeps far past
	// the bound, in front of the test's own PATH: the guard's lookup finds it before the real
	// one, and the check's bash still finds everything else.
	sleeps := func(t *testing.T, name string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	t.Run("the gh visibility read", func(t *testing.T) {
		guard := New(Options{
			Check: leakguardtest.Write(t, t.TempDir(), passes),
			Env:   append(fixtureenv.Isolated(t.TempDir()), sleeps(t, "gh")),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		err := guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
		answeredWithin10s(t, started)
		says(t, setupFailure(t, err), "could not be read: gh ran 2 s and was stopped by its bound of 2 s")
	})
	t.Run("a git read of the push's records", func(t *testing.T) {
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, sleeps(t, "git"))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		// A local commit and a ref the destination does not hold yet: the guard asks git for the
		// commits the push would add, which is the read the stand-in git never answers.
		err := guard.Push(ctx, PushRequest{
			Dir:     t.TempDir(),
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: strings.Repeat("1", 40), RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
		})
		answeredWithin10s(t, started)
		says(t, setupFailure(t, err), "the push's commits could not be read: git ran 2 s and was stopped by its bound of 2 s")
	})
}
