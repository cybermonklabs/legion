package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// The cause of a start the leak check refused is read off the error however the launch wrapped
// it: the relaunch joins the start's error with its cleanup's and names the step and the claim
// around it, and the supervisor wraps that once more when the claim's launches run out. An error
// that carries no refusal answers no cause, so a start that failed for another reason clears the
// claim's.
func TestLeakCheckCauseIsReadThroughTheLaunchsWrapping(t *testing.T) {
	const cause = "the shared public leak check could not be installed: the daemon has no GitHub credential to read theorem-labs/agent-infra"
	refusal := LeakCheckRefusal{Cause: cause}
	if refusal.Error() != cause {
		t.Fatalf("the refusal reads %q, want its cause %q", refusal.Error(), cause)
	}
	start := errors.Join(refusal, nil)
	launch := fmt.Errorf("launch legion-p-i-1-worker: start through its authenticated launcher: %w", start)
	ranOut := fmt.Errorf("launch failures ran out: %w", launch)
	for name, err := range map[string]error{"the refusal": refusal, "the start's": start, "the launch's": launch, "the claim's": ranOut} {
		if got := LeakCheckCause(err); got != cause {
			t.Errorf("%s error %q carries the cause %q, want %q", name, err, got, cause)
		}
	}
	want := "launch failures ran out: launch legion-p-i-1-worker: start through its authenticated launcher: " + cause
	if ranOut.Error() != want {
		t.Errorf("the claim's failure reads %q, want %q", ranOut.Error(), want)
	}
	for name, err := range map[string]error{
		"no error":      nil,
		"another one":   errors.New("launcher legion-p-i-1-worker start generation 1: generation 1 is still running"),
		"a wrapped one": fmt.Errorf("launch x: %w", errors.New("launcher connection closed")),
	} {
		if got := LeakCheckCause(err); got != "" {
			t.Errorf("%s carries the cause %q, want none", name, got)
		}
	}
}
