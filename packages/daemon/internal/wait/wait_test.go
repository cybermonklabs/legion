package wait

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A wait that names its moment is a wait as any other, and UntilAt reads the moment back from
// anywhere in the chain; a wait that names none, or a failure, has none.
func TestUntilIsAWaitThatNamesItsMoment(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	err := Until(at, "tree LEGION-300 lingers past its deadline; member LEGION-301's production check is open")
	if !errors.Is(err, ErrWaiting) {
		t.Fatalf("errors.Is(Until, ErrWaiting) = false, want a wait")
	}
	if err.Error() != "tree LEGION-300 lingers past its deadline; member LEGION-301's production check is open" {
		t.Fatalf("Until message = %q", err.Error())
	}
	got, ok := UntilAt(err)
	if !ok || !got.Equal(at) {
		t.Fatalf("UntilAt(Until) = %s, %v; want %s, true", got, ok, at)
	}
	wrapped := fmt.Errorf("linger of LEGION-300: %w", err)
	if got, ok := UntilAt(wrapped); !ok || !got.Equal(at) {
		t.Fatalf("UntilAt of a wrapped Until = %s, %v; want %s, true", got, ok, at)
	}
	if !errors.Is(wrapped, ErrWaiting) {
		t.Fatalf("a wrapped Until is no longer a wait")
	}
	for name, other := range map[string]error{
		"a plain wait":      New("start waits for a turn"),
		"a formatted wait":  Errorf("start of %s waits", "LEGION-300"),
		"a failure":         errors.New("GitHub refused"),
		"a wrapped failure": fmt.Errorf("compare: %w", errors.New("GitHub refused")),
	} {
		if got, ok := UntilAt(other); ok || !got.IsZero() {
			t.Errorf("UntilAt(%s) = %s, %v; want the zero time and false", name, got, ok)
		}
	}
}

// A wait marked routine is the wait it marks: errors.Is matches ErrWaiting and the wait's own
// sentinel, UntilAt reads the moment of a routine Until, and the message is the wait's. IsRoutine
// finds the mark through %w and errors.Join, beside a failure too; a plain wait, a plain Until, a
// failure and nil are not routine. A refusal that is no wait stays no wait once marked.
func TestARoutineWaitIsTheWaitItMarks(t *testing.T) {
	held := New("the project release holds every start until the daemon's next boot")
	routine := Routine(held)
	if !errors.Is(routine, ErrWaiting) || !errors.Is(routine, held) {
		t.Fatalf("a routine wait matches ErrWaiting %t and its own sentinel %t, want both", errors.Is(routine, ErrWaiting), errors.Is(routine, held))
	}
	if routine.Error() != held.Error() {
		t.Fatalf("a routine wait's message = %q, want the wait's, %q", routine.Error(), held.Error())
	}
	at := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	parked := Routine(Until(at, "a delivery waits for its launch's turn"))
	if got, ok := UntilAt(parked); !ok || !got.Equal(at) {
		t.Fatalf("UntilAt of a routine Until = %s, %v; want %s, true", got, ok, at)
	}
	for name, err := range map[string]error{
		"a routine wait":                   routine,
		"a routine Until":                  parked,
		"a routine wait wrapped with %w":   fmt.Errorf("launch LEGION-209: %w", routine),
		"a routine wait joined":            errors.Join(routine),
		"a routine wait joined to a fault": errors.Join(errors.New("the store did not answer"), routine),
		"a joined routine wait wrapped":    fmt.Errorf("retire LEGION-209: %w", errors.Join(parked)),
		"a wrapped routine wait wrapped":   fmt.Errorf("outbox row 7: %w", fmt.Errorf("start: %w", routine)),
	} {
		if !IsRoutine(err) || !errors.Is(err, ErrWaiting) {
			t.Errorf("%s: IsRoutine = %t, a wait = %t; want both", name, IsRoutine(err), errors.Is(err, ErrWaiting))
		}
	}
	if got, ok := UntilAt(fmt.Errorf("retire LEGION-209: %w", errors.Join(parked))); !ok || !got.Equal(at) {
		t.Fatalf("UntilAt of a routine Until joined and wrapped = %s, %v; want %s, true", got, ok, at)
	}
	for name, err := range map[string]error{
		"a plain wait":         held,
		"a wrapped plain wait": fmt.Errorf("launch LEGION-209: %w", Errorf("start of %s waits", "LEGION-209")),
		"a plain Until":        Until(at, "a delivery waits for its launch's turn"),
		"a failure":            errors.New("GitHub refused"),
		"nil":                  nil,
	} {
		if IsRoutine(err) {
			t.Errorf("IsRoutine(%s) = true, want false: nothing marked it", name)
		}
	}
	refusal := errors.New("the project release holds every pod of the project until the daemon's next boot")
	if marked := Routine(refusal); !IsRoutine(marked) || errors.Is(marked, ErrWaiting) || !errors.Is(marked, refusal) {
		t.Fatalf("a marked refusal is routine %t, a wait %t, the refusal %t; want routine, no wait, the refusal",
			IsRoutine(marked), errors.Is(marked, ErrWaiting), errors.Is(marked, refusal))
	}
}
