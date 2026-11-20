package hostproc

import "testing"

// A binary that is no test reads the /proc it is given: a launcher in a pod lists its container's
// /proc because that is its job, and a refusal there would end PID 1 of the pod. The control is the
// same call in this test binary, which is refused.
func TestABinaryThatIsNoTestIsNeverRefused(t *testing.T) {
	var exited []int
	previousExit, previousInTest := exit, inTest
	t.Cleanup(func() { exit, inTest = previousExit, previousInTest })
	exit = func(code int) { exited = append(exited, code) }

	inTest = func() bool { return false }
	Check("a pod's launcher", "/proc", Reason{})
	if len(exited) != 0 {
		t.Fatalf("a binary that is no test exited %v on a listing of /proc", exited)
	}

	inTest = func() bool { return true }
	Check("a test's launcher", "/proc", Reason{})
	if len(exited) != 1 || exited[0] != RefusalStatus {
		t.Fatalf("the control, the same call in a test binary, exited %v, want [%d]", exited, RefusalStatus)
	}
}
