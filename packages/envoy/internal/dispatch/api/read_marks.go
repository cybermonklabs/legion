package api

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What a person has read, cleared, dismissed or snoozed is a tuple from their person node
// (the spec's R6, graphstore.FromPerson), and the six read-state tables are its
// projections. Every route that writes one of them states the mark as that tuple in its own
// transaction:
//
//   - a thread read (putThreadRead, api/replies.go) and Mark all read (markInboxReadThreads,
//     api/inbox_read.go) state read_through to the thread's root;
//   - a dismissal of a Reply thread states dismissed to its root and read_through, since
//     dismissing marks the thread read, and its restore the dismissal's retraction
//     (dismissInboxThreads, api/inbox_dismiss.go);
//   - a dismissal of an Agent thread states dismissed to its root message, and its restore the
//     retraction (dismissInboxAgentThreads);
//   - a Clear and a read mark of a conversation state cleared and read_through to the session,
//     and a reply read by its id reply_read to the reply, which the prune of a row the session's
//     read mark passed retracts (putUserAgentState and pruneAgentReplies, api/state.go; the
//     mark-all and the dismissal write replies by id too, markInboxReadAgentReplies);
//   - a snooze states snoozed to the ask at its snoozed_until, and an unsnooze its retraction
//     (putAskSnooze and deleteAskSnooze, api/ask_snooze.go);
//   - the removal of an untouched document retracts each mark of every user_threads row it
//     deletes (removeArtifact, api/artifact_removal.go).
//
// The one writer of a thread states the rest: a person's own write moves their read mark
// (inboxwrite.Record's step 5, queueKeepRow), and a write by somebody else retracts their
// dismissal, of a Reply thread or of an Agent thread, as it clears the row.
//
// Each route's statement answers exactly what it stored - the rows it moved, at the moment each
// now holds - and each tuple states that and nothing more: a mark only moves forward, a row a
// version check skipped is not dismissed, a reply already read by its id is not read again. Its
// event is decided by what that statement moved, since a write that moves nothing announces
// nothing, so the statement runs first, then the event, then the tuples under it, which is the
// order a write takes whose cause is appended after the row it writes (graphstore.Uncaused): the
// tuples are caused by the write's own event and commit with the row. A row of the five tables
// other than user_threads is marked by the statement that writes it (tuples_stored_at, migration
// 20261116170000), at its tuple's own moment, where the tuple's ends fit the store's address bound
// (markFits); user_threads' marker is its one writer's and the backfill's (migration
// 20261116160000). A row a statement leaves as it was keeps the marker it holds.

// personMark is the tuple of one mark a route's write states from its caller's person node: kind
// from the person to toKind/toID at the mark's own moment at, asserted for a mark the write made
// and retracted for one it ended, made by actor and caused by event: graphstore.LatestEvent for
// every route, which queues the mark right after the event it appends. The person is the actor's
// login as the route read it, which graphstore.FromPerson canonicalises, so the raw actor id
// user_agent_state keys its Clear on and the canonical login every other table keys on name one
// node.
func personMark(actor model.Actor, kind, toKind, toID string, at time.Time, asserted bool, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: graphstore.KindPerson, FromID: actor.ID, Kind: kind,
		ToKind: toKind, ToID: toID,
		At: at, Who: actor, SourceEvent: event, Asserted: asserted,
	}
}

// markFits is whether a mark from actor to each of ends is a tuple the store can key
// (graphstore.AddressFits of both of its ends, as FromPerson asks it); a far end that is an id the
// store always keys - a thread's, a message's, an ask's - need not be named. A row whose mark is
// not is written unmarked, so its old arm answers it, as a row an older build writes is answered.
func markFits(actor model.Actor, ends ...string) bool {
	if !graphstore.AddressFits(model.CanonicalLogin(actor.ID)) {
		return false
	}
	for _, end := range ends {
		if !graphstore.AddressFits(end) {
			return false
		}
	}
	return true
}
