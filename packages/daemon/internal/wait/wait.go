// Package wait names the one outcome of a side effect that is neither success nor failure: the
// effect cannot act yet, and will once something else happens — a turn ends, a stop lands, a
// tree's cleanup confirms. An error that is a wait (errors.Is(err, ErrWaiting)) is retried on the
// outbox's backoff and logged as a wait, never as a failure; each wait keeps its own sentinel or
// message, so its callers still tell one wait from another. A wait that knows when it ends
// (Until) names that moment, and the outbox runs the row again then instead of on its backoff.
package wait

import (
	"errors"
	"fmt"
	"time"
)

// ErrWaiting is what every wait matches.
var ErrWaiting = errors.New("waiting")

// New is a wait sentinel with text as its message: errors.Is matches it by identity, as any
// sentinel, and ErrWaiting too.
func New(text string) error {
	return &waiting{err: errors.New(text)}
}

// Errorf is a wait whose message, and whatever its %w wraps, are fmt.Errorf's.
func Errorf(format string, args ...any) error {
	return &waiting{err: fmt.Errorf(format, args...)}
}

type waiting struct{ err error }

func (w *waiting) Error() string { return w.err.Error() }

func (w *waiting) Unwrap() error { return w.err }

func (w *waiting) Is(target error) bool { return target == ErrWaiting }

// Until is a wait that names when to run the row again: at, instead of the outbox's backoff
// (UntilAt reads it back). It is a wait as any other (errors.Is matches ErrWaiting), with text as
// its message: for an effect whose next chance is a known moment — a lingering tree's close
// waiting for a member's production check, bounded at a time the record holds — rather than
// something that happens.
func Until(at time.Time, text string) error {
	return &until{waiting: waiting{err: errors.New(text)}, at: at}
}

// UntilAt is the moment a wait in err's chain named with Until, and whether one did.
func UntilAt(err error) (time.Time, bool) {
	var w *until
	if !errors.As(err, &w) {
		return time.Time{}, false
	}
	return w.at, true
}

type until struct {
	waiting
	at time.Time
}

// Routine marks err, a wait, routine: a wait nobody has to act on, because what it waits for
// comes on its own or was asked for already — a holder that has not stopped yet, a launch the
// wake pacing parked, the hold of every start that an operator's project release asked for. An
// outbox row's attempts at a routine wait stay at Debug after its first, however long it lasts,
// where those at any other wait turn into a warning once it has lasted; and the supervise
// machine's logs of a decision's error keep it below Error. The mark changes nothing else: it
// wraps err and unwraps to it, so it is a wait exactly where err is one (errors.Is(err,
// ErrWaiting)), names err's moment (UntilAt), and matches every sentinel err matches. A refusal a
// caller already retries as a wait of its own without being one, such as the runtime's refusal of
// a start the project release holds, may be marked as well, and stays no wait for every other
// reader. IsRoutine finds the mark anywhere in a chain.
func Routine(err error) error {
	return &routine{err: err}
}

// IsRoutine is whether err's chain holds a wait marked routine (Routine), through every %w and
// errors.Join.
func IsRoutine(err error) bool {
	var r *routine
	return errors.As(err, &r)
}

type routine struct{ err error }

func (r *routine) Error() string { return r.err.Error() }

func (r *routine) Unwrap() error { return r.err }
