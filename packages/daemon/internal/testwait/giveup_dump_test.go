package testwait

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// A wait that gives up shows a reader where the test was waiting and what it last read: after its
// failure come the goroutine stacks as it gave up, the waiting goroutine's first, naming the
// function the wait was called from, and then the goroutine the test started that holds the fact
// back; and its last read prints the time a pointer field holds, and <nil>, not their addresses.
// A wait that holds prints no stacks, and a later wait begun past the margin says it prints none.
// The child plants each wait's deadline (plantedDeadline) on a stepped clock, so the margin passes
// at a step of the child. This row's file uses only what the package had before the stacks, so the
// same row run at a commit without them fails by name
// (dispatch://INFRA-3904/comment/e4db89bf-b6a8-4eab-89ea-c4bac7158cef). It logs the child's whole
// output, which is what a reader of a give-up sees.
func TestAGiveUpPrintsTheStacksAndWhatItsReadingHolds(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestAGiveUpDumpChild$", "-test.timeout="+Remaining(t).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	t.Logf("the child's output (%v):\n%s\nits stderr:\n%s", err, &stdout, &stderr)
	if err == nil {
		t.Fatal("the child was meant to fail; it passed")
	}
	if strings.Contains(stdout.String()+stderr.String(), "panic: test timed out") {
		t.Fatal("the child's wait ran into the binary's deadline rather than giving up before it")
	}
	output := strings.Split(stdout.String(), "\n")
	name := "TestAGiveUpDumpChild/"
	gaveUp := "testwait: " + name + "until: gave up waiting for the fact that never comes 30s before the test binary's deadline; last read: " +
		"{heard:2026-11-25 10:18:06 +0000 UTC missing:<nil>}"
	if !slices.Contains(strings.Split(stderr.String(), "\n"), gaveUp) || !strings.Contains(stdout.String(), gaveUp+"\n") {
		t.Errorf("the child wrote no give-up line, on stderr and as its failure, whose last read holds the time and the nil:\n%s", gaveUp)
	}
	if !strings.Contains(stdout.String(), "forcing fired: the wait's deadline is planted 50ms of a stepped clock away, and the fact never comes") {
		t.Error("the child logged no line that its forcing fired")
	}
	if !strings.Contains(stdout.String(), "--- PASS: "+name+"holds (") {
		t.Error("the child's wait that holds did not pass")
	}
	header := "testwait: " + name + "until: the goroutine stacks as the wait gave up: "
	if n := strings.Count(stdout.String(), "the goroutine stacks as the wait gave up: "); n != 1 {
		t.Errorf("the child printed the goroutine stacks %d times, want once: for the wait that gave up, and not for the wait that held or the one begun past the margin", n)
	}
	start := slices.IndexFunc(output, func(line string) bool { return strings.Contains(line, header) })
	end := slices.IndexFunc(output, func(line string) bool {
		return strings.Contains(line, "testwait: "+name+"until: end of the goroutine stacks: ")
	})
	failed := slices.IndexFunc(output, func(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "--- FAIL: "+name+"until (") })
	failure := slices.IndexFunc(output, func(line string) bool { return strings.HasSuffix(line, gaveUp) })
	if start < 0 || end < start || failure > start || failed < end {
		t.Fatalf("the child's output holds no goroutine stacks after the wait's failure (line %d) and before its --- FAIL line (line %d): their first line at %d, their last at %d", failure, failed, start, end)
	}
	stacks := output[start:end]
	waiting, _, _ := strings.Cut(strings.Join(stacks[1:], "\n"), "\n    \n")
	for _, frame := range []struct{ in, function, why string }{
		{waiting, ".awaitTheFactThatNeverComes(", "the waiting goroutine's stack, first, names the function the wait was called from"},
		{strings.Join(stacks, "\n"), ".holdsTheFactBack(", "the stacks name the goroutine the test started that holds the fact back"},
	} {
		if !strings.Contains(frame.in, frame.function) {
			t.Errorf("%s: no frame %s in:\n%s", frame.why, frame.function, frame.in)
		}
	}
	collateral := "testwait: " + name + "eventually: this give-up is collateral: the wait began after the margin before the test binary's deadline had passed, so it did not wait; " +
		"the wait that used the deadline up gave up earlier: " + name + "until, waiting for the fact that never comes, whose goroutine stacks are printed with that test's failure"
	if !strings.Contains(stdout.String(), collateral+"\n") {
		t.Errorf("the child's wait begun past the margin did not say it is collateral and name the give-up that used the deadline up:\n%s", collateral)
	}
}

// heardReading is what the child's wait reads: a time it heard and one it never did, each a
// pointer, as the delivery hub's freshness reading in packages/envoy holds them.
type heardReading struct {
	heard, missing *time.Time
}

// TestAGiveUpDumpChild is the child of TestAGiveUpPrintsTheStacksAndWhatItsReadingHolds. Its first
// wait holds; its second starts a goroutine that holds the fact back, takes that goroutine's word
// that it runs inside holdsTheFactBack (so the goroutine's stack names it however late the machine
// runs it), and waits, from awaitTheFactThatNeverComes, until its planted deadline's margin passes
// on a stepped clock; its third begins past the margin. It runs only when the parent re-executes
// this binary.
func TestAGiveUpDumpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAGiveUpPrintsTheStacksAndWhatItsReadingHolds")
	}
	t.Run("holds", func(t *testing.T) {
		Until(t, "a reading that holds", func() (int, bool) { return 1, true })
	})
	t.Run("until", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release) })
		go holdsTheFactBack(started, release)
		<-started
		c := &steppedClock{now: time.Now()}
		onClock(t, c)
		awaitTheFactThatNeverComes(plantedDeadline{t, c.Now().Add(margin + 50*time.Millisecond)})
	})
	t.Run("eventually", func(t *testing.T) {
		Eventually(plantedDeadline{t, time.Now()}, "a condition after the margin", func() bool { return false })
	})
}

// awaitTheFactThatNeverComes waits until its reading holds, which it never does, and logs that the
// forcing fired as it first reads.
func awaitTheFactThatNeverComes(t testing.TB) {
	t.Helper()
	heard := time.Date(2026, 11, 25, 10, 18, 6, 0, time.UTC)
	reads := 0
	Until(t, "the fact that never comes", func() (heardReading, bool) {
		if reads++; reads == 1 {
			t.Log("forcing fired: the wait's deadline is planted 50ms of a stepped clock away, and the fact never comes")
		}
		return heardReading{heard: &heard}, false
	})
}

// holdsTheFactBack is the goroutine the child's test starts, which holds the fact back until the
// test ends. It closes started once it runs, inside this function.
func holdsTheFactBack(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}
