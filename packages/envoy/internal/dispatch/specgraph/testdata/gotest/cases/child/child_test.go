// Package child has a test that runs a child go test, which times out, and prints the child's
// output as it is, the kill line at the start of a line of it. Then the test fails.
package child

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// childEnv tells TestChildSleeps that it runs in the child go test.
const childEnv = "ACME_GOTEST_CHILD"

func TestChildTimesOut(t *testing.T) {
	child := exec.Command("go", "test", "-count=1", "-trimpath", "-timeout", "1s", "-run", "^TestChildSleeps$", ".")
	child.Env = append(os.Environ(), childEnv+"=1")
	out, err := child.CombinedOutput()
	fmt.Print(string(out))
	if err == nil {
		t.Fatal("the child go test passed")
	}
	t.Fatalf("the child go test timed out: %v", err)
}

// TestChildSleeps sleeps past the child's -timeout. In any other run it skips.
func TestChildSleeps(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("runs only in the child go test of TestChildTimesOut")
	}
	time.Sleep(5 * time.Second)
}
