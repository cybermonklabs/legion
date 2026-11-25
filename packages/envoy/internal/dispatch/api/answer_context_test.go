package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

type answerContextValueKey struct{}

// boundsAnswerFloor's context is a context type of this package's own (answerContext), a field
// of the answer, not context.WithValue's, so every context a read's handler derives from it must
// still behave as one derived from context.WithValue's would. A WithCancel, a WithTimeout or a
// context.AfterFunc on it ends when its parent ends and carries the parent's cause. Each one
// still reaches the answer, so a handler can tell the floor what to leave out
// (leaveOutOfAnswerFloor), and still reads every other value of the parent.
func TestAContextDerivedFromAReadsAnswerContextEndsWithItsParentAndReachesTheAnswer(t *testing.T) {
	t.Parallel()
	cause := errors.New("the read's own end")
	parent, end := context.WithCancelCause(context.WithValue(context.Background(), answerContextValueKey{}, "the parent's value"))
	answer := &boundedAnswer{}
	carried := boundsAnswerFloor(parent, answer)
	if _, own := carried.(*answerContext); !own {
		t.Fatalf("boundsAnswerFloor's context is a %T, want the answer's own answerContext, which is what this row tests", carried)
	}
	withCancel, stopCancel := context.WithCancel(carried)
	defer stopCancel()
	withTimeout, stopTimeout := context.WithTimeout(carried, time.Hour)
	defer stopTimeout()
	afterRan := make(chan struct{})
	stopAfter := context.AfterFunc(carried, func() { close(afterRan) })
	defer stopAfter()
	derived := map[string]context.Context{
		"boundsAnswerFloor's context": carried,
		"a WithCancel on it":          withCancel,
		"a WithTimeout on it":         withTimeout,
	}
	for name, ctx := range derived {
		answer.outside = nil
		leaveOutOfAnswerFloor(ctx, func() time.Duration { return time.Second })
		if answer.outside == nil {
			t.Errorf("%s did not reach the answer: leaveOutOfAnswerFloor set nothing on it", name)
		}
		if got := ctx.Value(answerContextValueKey{}); got != "the parent's value" {
			t.Errorf("%s reads the parent's value as %v, want %q", name, got, "the parent's value")
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("%s has ended before its parent did: %v", name, err)
		}
	}

	end(cause)
	for name, ctx := range derived {
		testwait.Receive(t, name+" to end once its parent ended", ctx.Done())
		if err := ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s ended with %v, want context.Canceled, as its parent did", name, err)
		}
		if got := context.Cause(ctx); got != cause {
			t.Errorf("%s carries the cause %v, want its parent's %v", name, got, cause)
		}
	}
	testwait.Receive(t, "a context.AfterFunc on boundsAnswerFloor's context to run once its parent ended", afterRan)
}
