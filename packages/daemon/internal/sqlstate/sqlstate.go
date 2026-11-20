// Package sqlstate is the one rule by which the daemon reads a Postgres server's own refusal: the
// SQLSTATE classes in which the server answers about something other than what it was asked. The
// daemon's store owes a claim write such a refusal left unmade (supervise's writeOwed), and a
// start waits out a session store that answers so, a minute at a time with its session kept
// (ompsessions.StoreFault). Both read a server's answer through NotJudged, so the two stores sort
// it the same way.
package sqlstate

// NotJudged is whether code, a SQLSTATE a server answered with, is of one of the six classes in
// which the server answers about its own state rather than judging what it was asked, and would
// answer otherwise once that state passes:
//
//   - 08, connection exception: the connection itself is what failed, and what was asked went down
//     with it.
//   - 40, transaction rollback: the server rolled its own transaction back — a serialization
//     failure, a deadlock it broke — so what was asked was abandoned rather than judged.
//   - 53, insufficient resources: the memory, the disk or the connection slots the server had not
//     got for this request, and will have again; 53300, no client slot left, refuses a whole
//     connection while the server is full.
//   - 55, object not in prerequisite state: an object another session holds, a lock not available
//     chief among them, rather than a request the server read and refused.
//   - 57, operator intervention: a backend terminated, a query cancelled, a database shutting down
//     or starting up (57P01 and 57P03, the two a managed restart sends) — the platform's doing or
//     an operator's, not the request's.
//   - 58, system error: a file or an input/output error of the server's own, which says nothing
//     about what was asked of it.
//
// Any other class is the server judging what it was asked: a statement it refused, and equally a
// connection it refused over its configuration, a password it does not take (28P01) or a database
// it does not hold (3D000). A code shorter than two characters has no class to read and judges
// nothing, so it is read with the six rather than taken for a decision.
func NotJudged(code string) bool {
	if len(code) < 2 {
		return true
	}
	switch code[:2] {
	case "08", "40", "53", "55", "57", "58":
		return true
	default:
		return false
	}
}
