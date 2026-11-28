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

// Routine marks err routine: a wait nobody has to act on, because what it waits for comes on its
// own or was asked for already — a holder that has not stopped yet, a launch the wake pacing
// parked, the hold of every start that an operator's project release asked for. An outbox row's
// attempts at a routine wait stay at Debug after its first, however long it lasts, where those at
// any other wait turn into a warning once it has lasted; and the supervise machine's logs of a
// decision's error made only of routine waits keep it below Error. The mark changes nothing else:
// it wraps err and unwraps to it, so it is a wait exactly where err is one (errors.Is(err,
// ErrWaiting)), names err's moment (UntilAt), and matches every sentinel err matches. A refusal a
// caller already retries as a wait of its own without being one, such as the runtime's refusal of
// a start the project release holds, may be marked as well, and stays no wait for every other
// reader. IsRoutine finds the mark anywhere in a chain, and OnlyRoutine whether every error in it
// carries one.
func Routine(err error) error {
	return &routine{err: err}
}

// IsRoutine is whether err's chain holds an error marked routine (Routine), a wait or a refusal
// its caller retries as one, through every %w and errors.Join. It is true of a routine wait
// joined to a fault as well, so it is the test of a reader whose error is that one wait or
// refusal and nothing beside it; OnlyRoutine is the test of an error joined from several.
func IsRoutine(err error) bool {
	var r *routine
	return errors.As(err, &r)
}

// OnlyRoutine is whether every error in err's chain is marked routine (Routine): each branch,
// through every %w and errors.Join, reaches a mark before it ends, and nil holds none. It differs
// from IsRoutine where a routine wait is joined to anything unmarked — a fault, or a wait nobody
// marked — which IsRoutine is true of and this is not. So each reader takes the test that fits
// what its error can hold. A decision's error joins its answer with whatever else the decision
// met (supervise's Machine.handle joins the row's answer with the delivery's settle and the
// row-move check), and the level it is logged at takes this one (supervise's
// DecisionErrorLevel), so a fault beside a routine wait is still logged as a fault. A reader whose
// error is one wait or refusal alone takes IsRoutine: the outbox's warning of a row that waits
// (internal/daemon's waitWarnAttempts), which reads only an error that is a wait already, and the
// launch check's refusal (supervise's relaunchRefused), which is the refusal and nothing else.
func OnlyRoutine(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case *routine:
		return true
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		for _, child := range children {
			if !OnlyRoutine(child) {
				return false
			}
		}
		return len(children) > 0
	case interface{ Unwrap() error }:
		return OnlyRoutine(e.Unwrap())
	default:
		return false
	}
}

type routine struct{ err error }

func (r *routine) Error() string { return r.err.Error() }

func (r *routine) Unwrap() error { return r.err }
