package testwait

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const teardownDeadlineChildEnv = "TESTWAIT_TEARDOWN_DEADLINE_CHILD"

// TestTeardownDeadlineLetsPostTestCleanupUseTheTimeCmdGoLeaves verifies the deadline TestMain
// teardowns use: go test stops its own alarm when m.Run returns but cmd/go still kills the binary a
// minute after -test.timeout. The teardown gets TeardownGrace beyond that alarm, not a fixed retry
// window whose expiry treats a slow database backend as a failed test.
func TestTeardownDeadlineLetsPostTestCleanupUseTheTimeCmdGoLeaves(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTeardownDeadlineChild$", "-test.timeout=1m", "-test.v")
	child.Env = append(os.Environ(), teardownDeadlineChildEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestTeardownDeadlineChild (") {
		t.Fatalf("the child did not pass:\n%s\n%v", output, err)
	}
}

func TestTeardownDeadlineChild(t *testing.T) {
	if os.Getenv(teardownDeadlineChildEnv) == "" {
		t.Skip(teardownDeadlineChildEnv + " is not set: the parent runs this test in a binary whose -test.timeout is one minute")
	}
	deadline, bounded := flagDeadline()
	if !bounded {
		t.Fatal("flagDeadline found no deadline in the child run with -test.timeout=1m")
	}
	got := TeardownDeadline()
	want := deadline.Add(TeardownGrace)
	if !got.Equal(want) {
		t.Fatalf("TeardownDeadline is %s, want the test binary's deadline plus its post-test grace, %s", got, want)
	}
}
