package legionfacts

import "time"

// close.go is the one computation a reader of the stored facts makes (dispatch://INFRA-2916, R21):
// it closes each part of an issue still running at the reader's own clock. Every reader of a
// stored row reads it through At - the board (Read), the evals' items and the Legion day rows
// (ReadClosedIn) - so no reader keeps a rule of its own for how a fact grows, and none reads an
// in-process cache.

// At is the row as of now: each fact that grows after the moment the row is closed at (AsOf)
// grown by its rate over the time between (metrics.ClosedFacts.FactsAt), a bar still open ending at
// now, and AsOf moved to now. That is what the computation answers run at now over the same
// inputs (metrics.ClosedFacts), since no input of the issue is newer than AsOf. Every other
// field is the row's own, and a row with no growth reads as stored: a closed issue's, but for a
// slot it never gave up, whose hours run to the computation's end (metrics.Growth).
//
// A now at or before AsOf answers the row as stored: a reader whose clock stands behind the
// writer's reads the facts as of the newest input rather than shrinking them. A day builder that
// counts an open issue closes it at its day's end, the moment its day row is of: the Legion day
// rows count an issue on its close day alone (evals.ReadLegionDay), which no open issue has, and
// the same call answers either.
func (row Row) At(now time.Time) Row {
	if len(row.Growth) == 0 || !now.After(row.AsOf) {
		return row
	}
	row.IssueFacts = row.FactsAt(now)
	row.AsOf = now
	return row
}
