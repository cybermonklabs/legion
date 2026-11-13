package rescue_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rescue"
)

// c5Fixture names the copy of check c5's fixture the timing driver below runs over: a colocated jj
// repository of about 290,000 tracked files whose index carries no stat data, as a pod's does.
// The driver writes into it — new files, jj snapshots and a commit of its own — so it is never the
// fixture itself, only a full copy of it made for the run.
const c5Fixture = "RESCUE_C5_FIXTURE"

// The timing driver of check c5 (dispatch://INFRA-2848): the real route a check takes over a
// worktree jj holds (rescue.TreeTheJJRouteBuilds, which is snapshotTree itself), timed over a copy
// of the c5 fixture, on the shapes whose cost follows the paths the carry reads again: 0, 200,
// 1,600 and 5,600 files of 1 MiB new in @ that a jj snapshot recorded, which the index copy holds
// as intent-to-add and jj's tree at their content, so the carry asks git's stat comparison about
// each and adds each again (rescue.seeds, wouldRead, carried); and 200 new files of 1 MiB no jj
// command has seen, which the route's own snapshot records and the untracked half adds. Each is
// timed twice, each time over files of its own, and every git and jj command the route ran for
// 0.3 s or more is logged with the time from its start to the start of the next, the last to the
// route's end, so Go's own work between two commands is counted with the first.
//
// It runs only where RESCUE_C5_FIXTURE names the copy, so no CI run makes it: make the copy with
// `cp -a`, run one shape at a time with -run 'TestTheJJRouteOnACopyOfTheC5Fixture/<shape>', and
// delete the copy after.
func TestTheJJRouteOnACopyOfTheC5Fixture(t *testing.T) {
	fix := os.Getenv(c5Fixture)
	if fix == "" {
		t.Skipf("%s names no copy of the c5 fixture, so there is nothing to time", c5Fixture)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	c := c5Copy{dir: fix, env: os.Environ(), git: git}

	c.timed(t, "the first snapshot of the copy (not the route's)", func() { c.jj(t, "util", "snapshot") })
	if c.run(t, "jj", "--ignore-working-copy", "--no-pager", "-R", fix, "diff", "--summary", "-r", "@") != "" {
		c.timed(t, "the commit of what @ held before the run, so the clean tree is clean (not the route's)", func() {
			c.jj(t, "commit", "-m", "c5 timing baseline")
		})
	}
	t.Logf("tracked files: %d", strings.Count(c.run(t, "git", "ls-files", "-z"), "\x00"))

	stamp := time.Now().Format("150405")
	t.Run("0", func(t *testing.T) {
		for n := 1; n <= 2; n++ {
			c.route(t, fmt.Sprintf("the route over the clean tree, run %d", n))
		}
	})
	for _, count := range []int{200, 1600, 5600} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			for n := 1; n <= 2; n++ {
				dir := fmt.Sprintf("c5-reread-%d-%s-%d", count, stamp, n)
				c.write(t, dir, count)
				c.timed(t, fmt.Sprintf("the snapshot that records %d files of 1 MiB new in @ (not the route's)", count), func() {
					c.jj(t, "util", "snapshot")
				})
				c.route(t, fmt.Sprintf("the route over %d files of 1 MiB new in @, run %d", count, n))
				c.gone(t, dir)
			}
		})
	}
	t.Run("untracked-200", func(t *testing.T) {
		for n := 1; n <= 2; n++ {
			dir := fmt.Sprintf("c5-untracked-200-%s-%d", stamp, n)
			c.write(t, dir, 200)
			c.route(t, fmt.Sprintf("the route over 200 new files of 1 MiB no jj command has seen, run %d", n))
			c.gone(t, dir)
		}
	})
}

// c5Copy is the copy of the c5 fixture at dir, with the environment every command over it runs
// in and the git a jj snapshot starts.
type c5Copy struct {
	dir string
	env []string
	git string
}

// run runs name over the copy and answers its standard output, trimmed.
func (c c5Copy) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = c.dir, c.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// jj runs a jj command over the copy under the pins the check's own snapshot carries (rescue's
// runner.snapshot), so a snapshot made here records what the route's would.
func (c c5Copy) jj(t *testing.T, args ...string) string {
	t.Helper()
	pinned := []string{"--no-pager", "--color=never", "-R", c.dir}
	for _, pin := range []string{
		`fsmonitor.backend="none"`, "git.filter.enabled=false", `signing.backend="none"`,
		`signing.behavior="keep"`, `snapshot.auto-track="all()"`, "snapshot.auto-update-stale=false",
		`working-copy.eol-conversion="none"`, `working-copy.exec-bit-change="auto"`,
		`git.executable-path="` + c.git + `"`,
	} {
		pinned = append(pinned, "--config", pin)
	}
	return c.run(t, "jj", append(pinned, args...)...)
}

// timed runs do and logs how long it took, which it answers.
func (c c5Copy) timed(t *testing.T, what string, do func()) time.Duration {
	t.Helper()
	started := time.Now()
	do()
	took := time.Since(started)
	t.Logf("%s: %.1f s", what, took.Seconds())
	return took
}

// write writes count files of 1 MiB under the new directory dir of the copy, each of content of
// its own, so no two are one blob.
func (c c5Copy) write(t *testing.T, dir string, count int) {
	t.Helper()
	at := filepath.Join(c.dir, dir)
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range count {
		header := fmt.Sprintf("%s, file %d\n", dir, i)
		body := append([]byte(header), bytes.Repeat([]byte{byte('A' + i%26)}, 1<<20-len(header))...)
		if err := os.WriteFile(filepath.Join(at, fmt.Sprintf("new%04d.bin", i)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// gone takes dir out of the copy and has jj record that, so @ holds no diff again.
func (c c5Copy) gone(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(c.dir, dir)); err != nil {
		t.Fatal(err)
	}
	c.timed(t, "the snapshot that records "+dir+" gone again (not the route's)", func() { c.jj(t, "util", "snapshot") })
}

// route times the route over the copy as a check runs it, and logs each command of it that ran
// for 0.3 s or more.
func (c c5Copy) route(t *testing.T, what string) {
	t.Helper()
	head := c.run(t, "git", "rev-parse", "HEAD")
	var marks []time.Time
	var names []string
	rescue.ObserveCommands(t, func(dir, name string, args []string) {
		marks = append(marks, time.Now())
		names = append(names, name+" "+commandWords(args))
	})
	var tree string
	var err error
	took := c.timed(t, what, func() {
		tree, err = rescue.TreeTheJJRouteBuilds(context.Background(), c.env, c.dir, head)
	})
	if err != nil {
		t.Fatalf("%s: the route failed after %.1f s: %v", what, took.Seconds(), err)
	}
	marks = append(marks, marks[0].Add(took))
	for i, name := range names {
		if step := marks[i+1].Sub(marks[i]); step >= 300*time.Millisecond {
			t.Logf("    %6.1f s  %s", step.Seconds(), name)
		}
	}
	t.Logf("    tree %s, %d commands", tree, len(names))
}

// commandWords is a command's arguments as the driver's log writes them: the git pins and every
// --config left out, and a command of more than twelve words cut to its first twelve and its
// count.
func commandWords(args []string) string {
	var kept []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-replace-objects", "--literal-pathspecs":
			continue
		case "-c", "--config":
			i++
			continue
		}
		kept = append(kept, args[i])
	}
	if len(kept) > 12 {
		return fmt.Sprintf("%s … (%d words)", strings.Join(kept[:12], " "), len(kept))
	}
	return strings.Join(kept, " ")
}
