// Package inboxwrite is the one writer of the threads a person takes part in and of what reaches
// them there (dispatch://INFRA-1152 D3, R2, R4 and R9). Every write path that stores a comment, a
// message, an ask or an answer records it after its event, in that write's own transaction, and
// the record appends the participation and delivery tuples of the write through graphstore and
// writes the user_threads projection of them. The triggers of migrations 0095-0097, which wrote
// that projection before, pass a write this release marked by (migration 20261116160001), and keep
// answering a write an older build makes during a roll until the backfill takes it once.
//
// A write that runs in two phases - a read phase that takes its owner lock and reads, and a write
// phase that sends every statement as one batch (store.WriteBatch) - plans its record in the read
// phase (PlanRecord) and queues it right after its event (Recording.Queue), so nothing is read back
// in Go between two statements of the write phase; a writer with no write phase records at once
// (Record).
//
// It sits below api and docs, which both write asks and answers, and the seed calls it too, since
// the seed sets the markers the triggers pass by.
package inboxwrite

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Arm is what a write does in the thread it is written in.
type Arm int

const (
	// Replies is a write the thread's other participants are told of: a comment, a message, a
	// broadcast copy, a session's reply, and an answer, whose node is the ask it answers. Its
	// writer takes part, every other participant is delivered it, and each of them who holds a
	// row of the thread sees the row move.
	Replies Arm = iota + 1
	// Opens is an ask's create: the asker takes part in the thread the ask opens and keeps a row
	// of it, read through the ask, and the person the ask waits on is reached by it and takes part
	// in it (the assignee arm, waitingOnTuples); nobody else is told of it.
	Opens
	// TakesPart is a write that takes part and delivers nothing: a decline, which closes an ask
	// with no answer and moves no row of anybody's.
	TakesPart
)

// Write is one write as Record records it: its own node, which is also the node of the thread
// it opens, who wrote it, its moment and the owner it belongs to, which names the project its
// thread's rows are kept under.
type Write struct {
	Arm Arm
	// NodeKind and NodeID are the write's own node: a comment, a message, or the ask a create
	// opens and an answer or a decline closes.
	NodeKind, NodeID string
	// Author is whose write it is, as graphstore.AuthorOf reads it: a person or a session takes
	// part, and an actor that names nobody takes no part and keeps no row.
	Author model.Actor
	// Who is the actor the tuples record as their writer.
	Who model.Actor
	// At is the write's own moment: the row's created_at, or the answer's at.
	At time.Time
	// IssueKey and ArtifactID are the owner the write belongs to, which a user_threads row names,
	// and Project the project that owner resolves to (graphstore.ScopeOf), "" for an owner that
	// resolves to none: a thread such as an agent conversation's file, or a direct message, keeps
	// no row, while its participation and its deliveries are tuples all the same.
	IssueKey, ArtifactID *string
	Project              string
	// Starter is, for a session's reply under a direct conversation, the person who started it:
	// who wrote its root, a message on no issue a person sent to a session (R2's
	// direct-conversation arm, which user_threads_message never reached, returning on a message
	// on no issue). The reply reaches them whether or not their participates_in stands. "" for
	// any other write.
	Starter string
	// ask is the answered ask as its answer writer read it before the update that writes the
	// answer (ReadAsk), whose catch-up Record appends first.
	ask *Ask
	// waiting is, for an ask's create, the person the ask waits on as its writer read it
	// (WaitingOn), and nil where Record reads it itself (waitingOn).
	waiting *string
	// parent is the node the write's head pointer names (RepliesTo), for a write whose record is
	// planned before its own pointer tuples are written; nil where the record reads them.
	parent *thread
}

// Comment is a comment's write, a reply in the thread its head pointers name.
func Comment(id string, author model.Actor, at time.Time, issueKey, artifactID *string, project string) Write {
	return Write{Arm: Replies, NodeKind: graphstore.KindComment, NodeID: id, Author: author, Who: author, At: at,
		IssueKey: issueKey, ArtifactID: artifactID, Project: project}
}

// Message is a message's write - an issue message, a direct message, a broadcast copy or a
// session's reply - a reply in the thread its replies_to chain names.
func Message(id string, author model.Actor, at time.Time, issueKey *string, project string) Write {
	return Write{Arm: Replies, NodeKind: graphstore.KindMessage, NodeID: id, Author: author, Who: author, At: at,
		IssueKey: issueKey, Project: project}
}

// Opened is an ask's create by author at the ask's created_at.
func Opened(askID string, author model.Actor, at time.Time, issueKey, artifactID *string, project string) Write {
	return Write{Arm: Opens, NodeKind: graphstore.KindAsk, NodeID: askID, Author: author, Who: author, At: at,
		IssueKey: issueKey, ArtifactID: artifactID, Project: project}
}

// WaitingOn is w, an ask's create, with waiting, the person the ask waits on as its writer read
// them through asks.AskedOfPersonExpression, under the locks waitingOn reads under - the owner
// lock and, where the ask falls through to its project's default, the project's (HoldDefault) -
// so nothing it read moves before the create commits: Record's assignee arm then delivers the ask
// to that person without reading them a second time. A create that names nobody this way leaves
// the read to Record.
func (w Write) WaitingOn(waiting string) Write {
	w.waiting = &waiting
	return w
}

// RepliesTo is w, a comment or a message whose record is planned before its own head pointer
// tuples are written (PlanRecord), with the node that pointer names: for a comment, the ask it is
// on, else the comment it replies to, which is the head of its thread; for a message, the message
// it answers (in_reply_to). A write that names none is the root of its own thread.
func (w Write) RepliesTo(kind, id string) Write {
	w.parent = &thread{kind: kind, id: id}
	return w
}

// Answered is the write of answer to ask, which its writer read before its update (ReadAsk), made
// by who: the person the answer names answered, at its own moment.
func Answered(ask Ask, answer model.AskAnswer, who model.Actor) Write {
	return Write{Arm: Replies, NodeKind: graphstore.KindAsk, NodeID: ask.id,
		Author: model.Actor{Kind: "user", ID: answer.User}, Who: who, At: answer.At,
		IssueKey: ask.issueKey, ArtifactID: ask.artifactID, Project: ask.project, ask: &ask}
}

// Declined is a decline of askID by who at at.
func Declined(askID string, who model.Actor, at time.Time) Write {
	return Write{Arm: TakesPart, NodeKind: graphstore.KindAsk, NodeID: askID, Author: who, Who: who, At: at}
}

// ThreadLogins is the thread_logins an event of this write names, given logins, the logins with
// a row on its thread before Record runs (events.ThreadLogins): those, and the writer's own where
// the write keeps them a row. Record runs after the event, so the event names the row its writer
// is about to hold, as it named the row the trigger wrote before the event when the triggers kept
// the rows. The result is sorted and never nil.
func (w Write) ThreadLogins(logins []string) []string {
	if logins == nil {
		logins = []string{}
	}
	login, keeps := w.keptLogin()
	if !keeps || slices.Contains(logins, login) {
		return logins
	}
	logins = append(slices.Clone(logins), login)
	slices.Sort(logins)
	return logins
}

// KeptLogins is the login whose row the write keeps, which its event names beside the rows the
// thread held before it, for an event whose insert reads those rows itself
// (events.ThreadLoginsOf): the writer's own where the write keeps them a row, none otherwise.
func (w Write) KeptLogins() []string {
	if login, keeps := w.keptLogin(); keeps {
		return []string{login}
	}
	return nil
}

// keptLogin is the login whose row the write keeps: its writer's, where its writer is a person the
// store can key (graphstore.AddressFits) and its owner resolves to a project, which is what a row
// names.
func (w Write) keptLogin() (string, bool) {
	if w.Arm == TakesPart || w.Project == "" {
		return "", false
	}
	kind, id, named := graphstore.AuthorOf(w.Author)
	return id, named && kind == graphstore.KindPerson && graphstore.AddressFits(id)
}

// Record records w, whose event is event (graphstore.NoEvent for a write no event records, the
// seed's), in tx, the write's own transaction, after that event and after every fact the write
// states of its own node, its thread pointers among them: it is PlanRecord and Recording.Queue back
// to back, the queued statements run at once as a batch of their own (store.RunWrite). For a write
// by A into thread T, the thread its head pointer tuples name (threadOf), it takes these steps, in
// this order:
//
//  0. an answer first appends every tuple of its ask the marker test still owes (decision 16,
//     owedBy): the participation and the citations of the ask's create where the ask's marker
//     is null, and the participation of an older build's answer the ask holds;
//  1. participates_in from A to T, whether A is a person or a session and whether or not T's
//     owner resolves to a project; and, for an ask's create, the assignee arm: participates_in
//     from the person the ask waits on - as the create read them (Write.WaitingOn), else read
//     here (waitingOn) - to it and delivered_to from it to them (waitingOnTuples), which raises
//     no version (P13);
//  2. for a reply, every user_threads row of T, `for no key update` in ascending login order, in
//     one pass (queueThreadLock) - which takes the rows of the catch-up's persons with them, since
//     each is a row of T - and the live participant set, read in the plan (readMembers): the
//     current participates_in tuples into T, the persons step 0 states take part in it, whom the
//     plan's read is made too early to see (withOwedParticipants), and the login of each row of T
//     the marker test passes. Such a row is the old arm's record that its person took part
//     through an older build, during a roll and before the backfill gives them their tuple, and
//     reading it is the marker test passing; no column of a marked row is read to decide anything;
//  3. for a reply, through graphstore.ToPerson, one delivered_to from the write's node to each
//     person in that set but A, so a person never draws their own write (outbox's
//     recipientFilter.keep), beside every delivered_to the catch-up owes and, for a session's
//     reply under a direct conversation, the one to the person who started it (w.Starter);
//  4. for a reply, the move of each row of T a person of that set but A holds: replied_at,
//     last_reply_id, version + 1 and dismissed_at cleared, and, for each person of that set but A
//     whose dismissed tuple into T stands, the retraction of that tuple
//     (graphstore.QueueMarksEnded, which reads the dismissals where it ends them);
//  5. A's own row, where A is a person and T's owner resolves to a project: its read mark moves to
//     the write where it stood earlier, and a row this write begins records marked_version 0 and
//     marked_seq at the log position of this moment (migration 20261116160000); then, where the
//     upsert moved the mark, A's read_through to T at the moment that row's read mark stands at
//     once the upsert ran, stated by the same statement (queueKeepRow, graphstore.MarkArm), as
//     every writer of a read mark states it (dispatch://INFRA-1152 R6).
//
// A session's reply to a message takes, between steps 1 and 2, its thread root's Agent
// dismissals: the root `for key share`, then the retraction of the dismissed tuple of each person
// whose user_agent_thread_dismissed row names it, then the delete of those rows, each statement
// reading the rows as they stand once the root is held, as the trigger
// user_agent_thread_dismissed_clear does for an older build's reply (migration 20261105093507).
//
// Steps 2 to 4 hold the lock rule of D4: the rows are locked before the delivered_to of the people
// holding them is appended, and the backfill marks a row under the same lock, so no delivery falls
// on both sides of a mark. The owner lock every such write holds first (store.LockOwner) orders
// two writes into one thread, and the lock order written on api/inbox_read.go's
// markInboxReadThreads states this step.
func Record(ctx context.Context, tx pgx.Tx, w Write, event int64) error {
	recording, err := PlanRecord(ctx, tx, w)
	if err != nil {
		return err
	}
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return recording.Queue(b, event) })
}

// Recording is w's record as PlanRecord planned it in the write's read phase: the thread it is
// written in, what an answer's catch-up owes, the person an ask's create reaches, and, for a reply,
// who takes part in the thread and which of their rows move - everything Queue writes, which reads
// nothing.
type Recording struct {
	w      Write
	thread thread
	// authorKind and authorID are A as graphstore.AuthorOf reads them, named whether A names
	// anybody the store can key, and person A's login where A is a person, "" otherwise.
	authorKind, authorID string
	named                bool
	person               string
	owed                 owed
	// waiting is, for an ask's create, the person the ask waits on.
	waiting string
	// clears is, for a session's reply to a message, whether the reply ends the Agent dismissals of
	// its root, which its write phase reads where it ends them.
	clears bool
	// delivered is, for a reply, each person in the thread's participant set but A, by login, and
	// moved each of them who holds a row of the thread; starter is the person who started a direct
	// conversation, whom a session's reply reaches beside them.
	delivered, moved []string
	starter          string
	// failClear and failThreads are whether a test armed the failure between the retraction of the
	// Agent dismissals and the delete of their rows (failpoint.AgentDismissalClear), and between
	// the deliveries and the move of the rows they reach (failpoint.UserThreads), decided as the
	// record was planned.
	failClear, failThreads bool
}

// PlanRecord plans w's record in the write's read phase, in tx, under the owner lock every writer
// of w's thread takes first: the thread (threadOf), what an answer's catch-up owes (owedBy), the
// person an ask's create reaches where the create did not read them (waitingOn), and for a reply
// the thread's participants and rows (readMembers). It reads and takes no row lock, so it holds no
// transaction id: each read is of rows a writer of this thread changes only under that owner lock.
// What the dismiss route writes under the person's rows and the root's lock instead - a dismissal -
// is read in the write phase, by the statement that ends it (Recording.Queue): the Agent
// dismissals a session's reply ends (queueAgentDismissalsClear) and the dismissed tuples of the
// people a reply reaches (graphstore.QueueMarksEnded).
func PlanRecord(ctx context.Context, tx pgx.Tx, w Write) (Recording, error) {
	thread, err := threadOf(ctx, tx, w)
	if err != nil {
		return Recording{}, err
	}
	r := Recording{w: w, thread: thread}
	r.authorKind, r.authorID, r.named = graphstore.AuthorOf(w.Author)
	r.named = r.named && graphstore.AddressFits(r.authorID)
	if r.named && r.authorKind == graphstore.KindPerson {
		r.person = r.authorID
	}
	if r.owed, err = owedBy(ctx, tx, w.ask); err != nil {
		return Recording{}, err
	}
	if w.Arm == Opens {
		if w.waiting != nil {
			r.waiting = *w.waiting
		} else if r.waiting, err = waitingOn(ctx, tx, w.NodeID); err != nil {
			return Recording{}, err
		}
	}
	if w.Arm != Replies {
		return r, nil
	}
	// A session's reply to a message brings its root back to everyone who dismissed it, which its
	// write phase reads where it ends them (queueAgentDismissalsClear).
	if w.NodeKind == graphstore.KindMessage && r.authorKind == graphstore.KindSession && thread.id != w.NodeID {
		r.clears = true
		r.failClear = failpoint.Planned(ctx, failpoint.AgentDismissalClear)
	}
	members, err := readMembers(ctx, tx, thread)
	if err != nil {
		return Recording{}, err
	}
	members = withOwedParticipants(members, r.owed.people)
	// The write reaches every other participant: each whose participates_in stands, and each who
	// holds a row of T the marker test passes; and a session's reply reaches the person who
	// started its direct conversation, whether or not they still take part.
	r.starter = model.CanonicalLogin(w.Starter)
	for _, member := range members {
		if member.login == r.person || !(member.takesPart || (member.row && !member.marked)) {
			continue
		}
		r.delivered = append(r.delivered, member.login)
		if member.row {
			r.moved = append(r.moved, member.login)
		}
	}
	r.failThreads = failpoint.Planned(ctx, failpoint.UserThreads)
	return r, nil
}

// withOwedParticipants is members, the thread as readMembers read it, with every person the
// write's catch-up states takes part in it (owed.people's participates_in, step 0 of Queue): each
// takes part from that step on, which the read in the plan, made before the batch that writes
// them is sent, cannot see. Record took them as members once its step 0 had written them, so the
// write delivers to each and moves the row each holds, in the one ascending-login pass over the
// thread's rows (queueThreadLock), as it does every other member. A person the read found keeps
// the row and the dismissal it read; one it did not find holds neither.
func withOwedParticipants(members []member, owed []graphstore.Tuple) []member {
	added := false
	for _, tuple := range owed {
		if tuple.Kind != graphstore.TupleParticipatesIn || tuple.FromKind != graphstore.KindPerson || !tuple.Asserted {
			continue
		}
		if index := slices.IndexFunc(members, func(held member) bool { return held.login == tuple.FromID }); index >= 0 {
			members[index].takesPart = true
			continue
		}
		members = append(members, member{login: tuple.FromID, takesPart: true})
		added = true
	}
	if added {
		slices.SortFunc(members, func(a, b member) int { return strings.Compare(a.login, b.login) })
	}
	return members
}

// Queue queues the record on a write phase's batch, right after the write's event, whose id the
// tuples carry as event (graphstore.LatestEvent for an event the batch appends right before, which
// nothing between them may follow with another), in Record's steps and order. Each tuple an
// answer's catch-up owes carries the event of the write it belongs to instead.
func (r Recording) Queue(b *store.WriteBatch, event int64) error {
	w, thread := r.w, r.thread

	// 0. What an answer owes of an ask an older build wrote: its participation and its
	// citations now, its deliveries with step 3's.
	if r.owed.cites {
		queueMarkCitations(b, w.NodeID)
	}
	if err := graphstore.QueueToPerson(b, r.owed.people); err != nil {
		return fmt.Errorf("record what ask %s owes of its create and its older answer: %w", w.NodeID, err)
	}
	if len(r.owed.others) > 0 {
		if err := graphstore.QueueWrite(b, nil, r.owed.others); err != nil {
			return fmt.Errorf("record what ask %s owes of its create: %w", w.NodeID, err)
		}
	}

	// 1. A takes part in T. An ask's create also reaches the person the ask waits on, who takes
	// part in it too, in the same call.
	var people []graphstore.Tuple
	if r.named {
		taking := graphstore.Tuple{
			FromKind: r.authorKind, FromID: r.authorID, Kind: graphstore.TupleParticipatesIn,
			ToKind: thread.kind, ToID: thread.id,
			At: w.At, Who: w.Who, SourceEvent: event, Asserted: true,
		}
		if r.person != "" {
			people = append(people, taking)
		} else if err := graphstore.QueueWrite(b, nil, []graphstore.Tuple{taking}); err != nil {
			return fmt.Errorf("record that %s/%s takes part in %s/%s: %w", r.authorKind, r.authorID, thread.kind, thread.id, err)
		}
	}
	if w.Arm == Opens {
		people = append(people, waitingOnTuples(w.NodeID, r.waiting, r.person, w.At, w.Who, event)...)
	}
	if err := graphstore.QueueToPerson(b, people); err != nil {
		return fmt.Errorf("record who takes part in %s/%s by %s/%s: %w", thread.kind, thread.id, w.NodeKind, w.NodeID, err)
	}
	if w.Arm == TakesPart {
		return nil
	}

	if w.Arm == Replies {
		// A session's reply to a message brings its root back to everyone who dismissed it.
		if r.clears {
			if err := queueAgentDismissalsClear(b, thread.id, r.failClear, w, event); err != nil {
				return err
			}
		}

		// 2. Every row of T, locked in one pass.
		queueThreadLock(b, thread)

		// 3. The write reaches every other participant, and a session's reply the person who
		// started its direct conversation.
		deliveries := r.owed.deliveries
		delivered := func(login string) {
			deliveries = append(deliveries, graphstore.Tuple{
				FromKind: w.NodeKind, FromID: w.NodeID, Kind: graphstore.TupleDeliveredTo,
				ToKind: graphstore.KindPerson, ToID: login,
				At: w.At, Who: w.Who, SourceEvent: event, Asserted: true,
			})
		}
		starterDelivered := false
		for _, login := range r.delivered {
			delivered(login)
			starterDelivered = starterDelivered || model.CanonicalLogin(login) == r.starter
		}
		if r.starter != "" && r.starter != r.person && !starterDelivered {
			delivered(r.starter)
		}
		if err := graphstore.QueueToPerson(b, deliveries); err != nil {
			return fmt.Errorf("deliver %s/%s to the participants of %s/%s: %w", w.NodeKind, w.NodeID, thread.kind, thread.id, err)
		}
		failpoint.Queue(b, r.failThreads)

		// 4. Each of their dismissals of T that stands ends, and each row of T those people hold
		// moves.
		if len(r.delivered) > 0 {
			if err := graphstore.QueueMarksEnded(b, graphstore.TupleDismissed, thread.kind, thread.id, graphstore.MarkedBy(r.delivered), w.At, w.Who, event); err != nil {
				return fmt.Errorf("retract the dismissals of %s/%s: %w", thread.kind, thread.id, err)
			}
		}
		queueMoveRows(b, thread, w, r.moved)
	}

	// 5. A's own row, and the read mark it stands at where this write moved it.
	if login, keeps := w.keptLogin(); keeps {
		if err := queueKeepRow(b, thread, w, login, event); err != nil {
			return err
		}
	}
	return nil
}

// dismissalsEnded is the retraction of the dismissed tuple from each person of logins to thread's
// root, made by w's writer at w's moment and under its event.
func dismissalsEnded(logins []string, thread thread, w Write, event int64) []graphstore.Tuple {
	ended := make([]graphstore.Tuple, 0, len(logins))
	for _, login := range logins {
		ended = append(ended, graphstore.Tuple{
			FromKind: graphstore.KindPerson, FromID: model.CanonicalLogin(login), Kind: graphstore.TupleDismissed,
			ToKind: thread.kind, ToID: thread.id,
			At: w.At, Who: w.Who, SourceEvent: event, Asserted: false,
		})
	}
	return ended
}
