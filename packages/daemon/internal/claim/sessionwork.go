package claim

import "time"

// SessionWork is what one activity report of a session says about the session's own background
// work, read against a moment: the newest moment a turn of the session is known to have been
// under way. A report read before that moment has said nothing about the turn under way then, so
// it reads as no report at all. A caller holds on a session with no report as it holds on one that
// names running work, which is what makes a host that cannot report degrade safely rather than
// lose work it was still doing.
type SessionWork int

const (
	// WorkUnreported is the zero value: the session has sent no report, or its newest report was
	// read before the moment.
	WorkUnreported SessionWork = iota
	// WorkRunning is a report read at or after the moment that names running background work of
	// the session's own.
	WorkRunning
	// WorkSettled is a report read at or after the moment that names no running background work.
	WorkSettled
)

// ReadSessionWork reads one report against lastTurn: reportedAt is when the session read what it
// was doing, zero for a session that has sent no report, and background is whether that report
// names running background work of its own (BackgroundWork.Running). A report read at the same
// instant as lastTurn counts, and a zero lastTurn is a session with no turn the caller knows of,
// against which every report counts.
//
// It is one test with two callers, and each passes its own moment. The idle release
// (workflow.IdleRelease) passes pods.IdleClaim.LastTurnAt, the latest of the claim's pending task
// delivery's send acknowledgement and the confirmation of the turn that send started. The pod
// update round's early end (internal/daemon's podupdate.go) passes supervise.Claim.LastTurnStart,
// which every turn moves, a turn started by an Envoy message or by a job's result included: such a
// turn moves no delivery, and it can start background work.
func ReadSessionWork(reportedAt, lastTurn time.Time, background bool) SessionWork {
	switch {
	case reportedAt.IsZero() || reportedAt.Before(lastTurn):
		return WorkUnreported
	case background:
		return WorkRunning
	default:
		return WorkSettled
	}
}
