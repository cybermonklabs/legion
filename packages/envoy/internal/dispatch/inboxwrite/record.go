// Package inboxwrite is the one writer of the threads a person takes part in and of what reaches
// them there (the spec's R2, R4 and R9). Every write path that stores a comment, a
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
	// OwnWrite says the handler that took the write marks it as its writer's own: a person's answer
	// or reply made at that moment (Own). Such a write takes its thread out of the writer's Inbox
	// where PlanRecord's four conditions hold (the own-answer rule, R1, D5). A write that is no
	// person's act at that moment - the carried approval answer, the restore of a retracted ask, the
	// seed's writes, the backfill's - leaves it false.
	OwnWrite bool
	// ask is the answered ask as its answer writer read it before the update that writes the
	// answer (ReadAsk), whose catch-up Record appends first.
	ask *Ask
	// waiting is, for an ask's create, the person the ask waits on as its writer read it
	// (WaitingOn), and nil where Record reads it itself (waitingOn).
	waiting *string
	// parent is the node the write's head pointer names (RepliesTo), for a write whose record is
	// planned before its own pointer tuples are written; nil where the record reads them.
	parent *thread
	// root is a write whose record is planned before its own head pointer tuples are written and
	// that answers nothing (StartsThread): the root of its own thread, which the write makes.
	root bool
	// mentioned is, for a comment or a message, the login of each person a mention of its text
	// newly names (ReachesMentioned), in the order the text first names them.
	mentioned []string
	// mentionedPeople is, for a comment or a message an older build wrote, the mentions tuple to
	// each person its text names, and identities to each session and role, which RecordOlder
	// appends under the write's event (Names).
	mentionedPeople, identities []graphstore.Tuple
	// unstored is a write whose head pointers the store holds no tuple of (Unstored).
	unstored bool
	// members are the thread's members as the write's own read phase read them (Members, or an
	// answer's ReadAskAndMembers), membersRead whether it did; PlanRecord then reads none.
	members     []member
	membersRead bool
}

// Members is w with its thread's members read off column, the value of MembersColumn the write's
// own read phase read in a statement it makes already, under the owner lock: PlanRecord then
// reads no members of its own. The column must be over the root w's record is written in
// (threadOf): a comment's head, which RepliesTo names.
func (w Write) Members(column []byte) (Write, error) {
	members, err := decodeMembers(column)
	if err != nil {
		return w, fmt.Errorf("read the members of the thread %s/%s is written in: %w", w.NodeKind, w.NodeID, err)
	}
	w.members, w.membersRead = members, true
	return w, nil
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

// Own is w marked as its writer's own write (OwnWrite). The handlers of a person's answer and of
// a comment or a message call it; PlanRecord still decides whether the write dismisses.
func (w Write) Own() Write {
	w.OwnWrite = true
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

// StartsThread is w, a comment or a message whose record is planned before its own head pointer
// tuples are written (PlanRecord) and whose row answers nothing: the root of its own thread,
// which the write makes, under an id it drew for itself. The plan reads nothing of that thread:
// the write's own head pointers are not written yet and name nothing (threadOf), and nobody holds
// a row of, takes part in or dismissed a node no write has made yet (readMembers), so the record
// reaches the people its text newly mentions, and, for a session's reply, the person who started
// its conversation, and nobody else.
func (w Write) StartsThread() Write {
	w.root = true
	return w
}

// Unstored is w, a comment or a message whose head pointers the store holds no tuple of: one past
// the store's address bound - a comment left whole out of the store (api's commentStoreWrite
// .unprojected), a comment whose pointer the bound cannot key (pointersStored false), a message
// whose replies_to end it cannot key (graphstore.MessageFacts' projected false) - whose row the
// old arms of graph_edges answer. Such a write states no delivered_to: the census reads the
// thread a delivery was written in off the write's own head pointers (graphcensus's
// deliveryThread), so a delivery of a write with none is a delivery into no thread, and a row of
// its thread moved beside none would break R6's equation on a marked row. So its record is the
// projection the trigger body user_threads_record wrote of it, written here in the write's own
// transaction after its event (Recording.Queue): each row of its thread but its writer's moves
// and gives up its marker, as an update of that body's did (migration 20261116160000), the
// writer keeps a row of their own, also unmarked, and a session's reply to a message ends its
// root's Agent dismissals, as the trigger user_agent_thread_dismissed_clear did. Each row it
// leaves unmarked is the backfill's thread pass's, which marks it under the thread's lock. Its
// row carries threads_recorded (migrations 20261116124447 and 20261116114515), which the triggers
// pass it by (20261116160001), so the triggers write nothing of it while they stand and the
// root's drop of them finds no row of this release they would still write for.
func (w Write) Unstored() Write {
	w.unstored = true
	return w
}

// ReachesMentioned is w, a comment or a message, reaching each person a mention of its text newly
// names: a mentions tuple to that person among the facts its index states
// (graphstore.Uncaused.People, refs.PlanItemIndex). The write delivers to each of them beside its
// thread's other participants (R2's mention arm), and moves the row each holds, in the one
// ascending-login pass over the thread's rows. Their participation is no fact of the record: the
// writer that resolves the mention appends it (MentionParticipation, R4's mention arm). A person a
// mention of the same item named before, whose tuple stands, is no fact of this index, and so is
// none of them.
func (w Write) ReachesMentioned(indexed graphstore.Uncaused) Write {
	mentioned := slices.Clone(w.mentioned)
	for _, login := range mentionedLogins(indexed) {
		if !slices.Contains(mentioned, login) {
			mentioned = append(mentioned, login)
		}
	}
	w.mentioned = mentioned
	return w
}

// mentionedLogins is the login of each person a mentions tuple of indexed names, once each, in the
// order indexed states them.
func mentionedLogins(indexed graphstore.Uncaused) []string {
	var logins []string
	for _, tuple := range indexed.People {
		if tuple.Kind != graphstore.TupleMentions || tuple.ToKind != graphstore.KindPerson || !tuple.Asserted {
			continue
		}
		if login := model.CanonicalLogin(tuple.ToID); !slices.Contains(logins, login) {
			logins = append(logins, login)
		}
	}
	return logins
}

// Names is w, a comment or an issue message an older build wrote, with the mentions tuples out of
// its node to each identity its text names as refs' item index states them of this release's own
// write of the same text (refs.ItemIdentityMentions), which that build stated none of: identities
// to the sessions and roles, people to the persons. RecordOlder appends each under the write's
// event; delivers the write to each person beside the people the old trigger raised the rows of
// (ReachesMentioned, R2's mention arm), and has each person but its writer take part in its
// thread's root (MentionParticipation, R4's): what this release's write of the text states of them
// (api's createCommentFor and createStoredMessage). A session or a role a mention names is reached
// by nothing of the record: a session is told of a write through its delivery routes and the
// outbox, which the backfill's comment mention, attempt and published-delivery passes state.
func (w Write) Names(identities, people []graphstore.Tuple) Write {
	w.identities, w.mentionedPeople = identities, people
	return w.ReachesMentioned(graphstore.Uncaused{People: people})
}

// MentionParticipation is R4's mention arm for the facts indexed states of one comment or message
// written in the thread whose root is rootKind/rootID (Recording.Root, ThreadRoot): a
// participates_in from each person a mentions tuple of indexed names but who to that root, made by
// who at at under event. The writer that resolves the mention appends it through
// graphstore.QueueToPerson - a comment's or a message's create after its record (Recording.Queue),
// so it falls after the lock pass over the thread's rows, and an edit beside what its index states
// - and never Record, which reads a write's author and its deliveries and appends no mention arm.
// A person who takes part already states the same relation again, which reads as it stood.
func MentionParticipation(rootKind, rootID string, indexed graphstore.Uncaused, who model.Actor, at time.Time, event int64) []graphstore.Tuple {
	writer := personOf(who)
	var taking []graphstore.Tuple
	for _, login := range mentionedLogins(indexed) {
		if login == writer {
			continue
		}
		taking = append(taking, graphstore.Tuple{
			FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleParticipatesIn,
			ToKind: rootKind, ToID: rootID, At: at, Who: who, SourceEvent: event, Asserted: true,
		})
	}
	return taking
}

// Answered is the write of answer to ask, which its writer read before its update (ReadAsk), made
// by who: the person the answer names answered, at its own moment.
func Answered(ask Ask, answer model.AskAnswer, who model.Actor) Write {
	return Write{Arm: Replies, NodeKind: graphstore.KindAsk, NodeID: ask.id,
		Author: model.Actor{Kind: "user", ID: answer.User}, Who: who, At: answer.At,
		IssueKey: ask.issueKey, ArtifactID: ask.artifactID, Project: ask.project, ask: &ask,
		members: ask.members, membersRead: ask.membersRead}
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
	kind, id, named := storeAuthor(w.Author)
	return id, named && kind == graphstore.KindPerson
}

// storeAuthor is who wrote a write as the store keys them: the kind and id of actor's node
// (graphstore.AuthorOf), named where actor names one whose id fits the store's address bound
// (graphstore.AddressFits).
func storeAuthor(actor model.Actor) (kind, id string, named bool) {
	kind, id, named = graphstore.AuthorOf(actor)
	return kind, id, named && graphstore.AddressFits(id)
}

// deliveredTo is the delivered_to from fromKind/fromID to the person login, made by who at at,
// under event.
func deliveredTo(fromKind, fromID, login string, at time.Time, who model.Actor, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: fromKind, FromID: fromID, Kind: graphstore.TupleDeliveredTo,
		ToKind: graphstore.KindPerson, ToID: login,
		At: at, Who: who, SourceEvent: event, Asserted: true,
	}
}

// Record records w, whose event is event (graphstore.NoEvent for a write no event records, the
// seed's), in tx, the write's own transaction, after that event and after every fact the write
// states of its own node, its thread pointers among them: it is PlanRecord and Recording.Queue back
// to back, the queued statements run at once as a batch of their own (store.RunWrite). For a write
// by A into thread T, the thread its head pointer tuples name (threadOf), it takes these steps, in
// this order:
//
//  0. an answer first appends every tuple of its ask the marker test still owes (owedBy): the
//     participation and the citations of the ask's create where the ask's marker is null, and
//     the participation of an older build's answer the ask holds;
//  1. participates_in from A to T, whether A is a person or a session and whether or not T's
//     owner resolves to a project; and, for an ask's create, the assignee arm: participates_in
//     from the person the ask waits on - as the create read them (Write.WaitingOn), else read
//     here (waitingOn) - to it and delivered_to from it to them (waitingOnTuples), which raises
//     no version;
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
//     recipientFilter.keep), and to each person but A its text newly mentions
//     (Write.ReachesMentioned), beside every delivered_to the catch-up owes and, for a session's
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
//     every writer of a read mark states it (the spec's R6). Where the write dismisses its row
//     (ownDismissal's dismissesRow), the same statement also sets the row's dismissed_at to the
//     write's moment, on a row it begins and on one already there, whatever the row's read mark
//     and version: the dismissal takes no version guard (the own-answer rule, R1). Before that
//     statement, A's dismissed tuple to T's root, at the write's moment and under its event
//     (graphstore.QueueFromPerson), so the row and the tuple commit together;
//  6. where the write dismisses an Agent thread (ownDismissal's dismissesAgentRoot) - a reply
//     under a message on no issue, which keeps no user_threads row and so takes no step 5 - A's
//     dismissed tuple to the root message, then A's user_agent_thread_dismissed row at that root,
//     both at the write's moment, made or moved whatever the row held, so the row always matches
//     the newest tuple; then, as a Dismiss press of the thread marks them, each session's reply
//     under the root that A's read mark of its session does not cover and A has not read by id,
//     marked read by id at the write's moment - its user_agent_reply_read row and its reply_read
//     tuple, stated by the statement that inserts the row (agentRepliesRead, graphstore.MarkArm) -
//     so the thread A no longer sees in their Inbox holds nothing unread. The writer took the root
//     `for update` before its own row's insert and read under that lock that a session answered
//     the root (api's createStoredMessage, D6): a root no session answered is no Agent row, and a
//     reply under it takes none of this step (Recording.Unanswered).
//
// A session's reply to a message takes, between steps 1 and 2, its thread root's Agent
// dismissals: the root `for no key update`, which its writer took before the reply's insert
// (Recording.QueueRootLock), then the retraction of the dismissed tuple of each person whose
// user_agent_thread_dismissed row names it, then the delete of those rows, each statement reading
// the rows as they stand once the root is held, as the trigger user_agent_thread_dismissed_clear
// does for an older build's reply (migrations 20261105093507 and 20261125053125).
//
// Steps 2 to 4 hold one lock rule: the rows are locked before the delivered_to of the people
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
	// delivered is, for a reply, each person in the thread's participant set but A, and each person
	// its text newly mentions but A, by login, and moved each of them who holds a row of the thread;
	// starter is the person who started a direct conversation, whom a session's reply reaches beside
	// them.
	delivered, moved []string
	starter          string
	// dismisses is what the write takes out of A's Inbox (ownDismissal, R1): nothing, its thread on
	// A's own row (step 5), or the Agent thread at its root, whose mark is a row of its own (step 6).
	dismisses dismissal
	// failClear and failThreads are whether a test armed the failure between the retraction of the
	// Agent dismissals and the delete of their rows (failpoint.AgentDismissalClear), and between
	// the deliveries and the move of the rows they reach (failpoint.UserThreads), decided as the
	// record was planned.
	failClear, failThreads bool
}

// Root is the root of the thread the record is written in, as (kind, id), as the plan read it
// (threadOf): what a mention's participation points at (MentionParticipation).
func (r Recording) Root() (string, string) {
	return r.thread.kind, r.thread.id
}

// AgentDismissal is the root message of the Agent thread the record takes out of its writer's
// Inbox (step 6), and whether it does, as the plan reads it. The writer takes that root `for
// update` before its own row's insert, as the dismiss route takes an Agent root before the
// person's rows (D6): taken after the insert, whose foreign key holds its parent `for key share`,
// the lock would be an upgrade that can deadlock against the dismiss route. Once the lock is
// granted it reads whether a session answered the root, and drops the dismissal where none did
// (Unanswered). Step 6's upsert of the writer's user_agent_thread_dismissed row
// (queueAgentDismissal) then moves the root's own agent_dismissed_at where the writer sent the
// root, as the dismiss route's mark does, through the trigger of migration 20261125053125, under
// this lock: every writer of that column takes the root first.
func (r Recording) AgentDismissal() (string, bool) {
	if r.dismisses != dismissesAgentRoot {
		return "", false
	}
	return r.thread.id, true
}

// Unanswered is r, the record of a reply that takes an Agent thread out of its writer's Inbox
// (AgentDismissal), for a root no session answered, as the writer read it under the root's `for
// update`: r dismissing nothing, so it takes none of step 6 - no dismissed tuple, no
// user_agent_thread_dismissed row and no reply marked read. The Inbox's reads draw an Agent row
// from the session replies of its root's thread, through an inner lateral (api's myAgentRowFacts),
// so a root no session answered is no Agent row, and a mark of it would be the mark of no row,
// which a Dismissed page cut to its marks counts and then drops. No session's reply commits
// between that read and this write's commit: one of this release takes the root `for no key
// update` before its insert (QueueRootLock), one of an older build takes it `for key share` in
// the trigger user_agent_thread_dismissed_clear (migrations 20261105093507 and 20261125053125),
// and either level waits for `for update`.
func (r Recording) Unanswered() Recording {
	if r.dismisses == dismissesAgentRoot {
		r.dismisses = dismissesNothing
	}
	return r
}

// dismissal is what a write takes out of its writer's Inbox as their own dismissal
// (ownDismissal), and so which of Record's steps writes it.
type dismissal int

const (
	// dismissesNothing is a write that dismisses nothing.
	dismissesNothing dismissal = iota
	// dismissesRow is a write that dismisses its thread on its writer's own user_threads row, beside
	// their dismissed tuple to the thread's root (step 5).
	dismissesRow
	// dismissesAgentRoot is a reply that dismisses its Agent thread, which keeps no user_threads
	// row: the writer's dismissed tuple to the root message, their user_agent_thread_dismissed row
	// at it and the thread's replies marked read (step 6).
	dismissesAgentRoot
)

// ownDismissal is D5's rule (the own-answer rule, R1): what w, written in the thread rooted at
// root, takes out of its writer's Inbox. It dismisses when its handler marked it its writer's own
// (Write.OwnWrite), its writer is a person (person, the login PlanRecord read, "" for anybody
// else), its tuples' writer acts as itself with no agent acting for them (Who.Act), and it is an
// answer or a reply - not the write that starts the thread: a message on no project, an Agent
// thread's reply, dismisses that thread at its root (dismissesAgentRoot), since its thread keeps no
// user_threads row, and any other write its thread on its writer's own row (dismissesRow). The
// Agent root's dismissal stands only where a session answered the root, which the writer reads
// under the root's lock (Recording.Unanswered). An Unstored write dismisses nothing: its record is
// the trigger body's projection, which states no tuple and never dismissed
// (Recording.queueUnstored), so its writer takes no Agent root for it either.
func ownDismissal(w Write, person string, root thread) dismissal {
	switch {
	case !w.OwnWrite || w.unstored || person == "" || w.Who.Act != nil || (w.ask == nil && w.NodeID == root.id):
		return dismissesNothing
	case w.NodeKind == graphstore.KindMessage && w.Project == "":
		return dismissesAgentRoot
	default:
		return dismissesRow
	}
}

// QueueRootLock queues, for a session's reply to a message, the lock of the root of the thread the
// reply is written in, `for no key update` (queueRootLock): its writer queues it before the
// reply's insert, and before any lock of a message or an attempt below the root it takes, so the
// root is the first row of the thread the reply holds. The reply's clear deletes the root's Agent
// dismissals (queueAgentDismissalsClear), and each delete updates the root's row
// (agent_dismissed_at, migration 20261125053125). Without this lock the reply would reach the root
// holding no more than the `for key share` its foreign key takes where it answers the root
// directly, so a restore holding the root `for no key update` could wait for a mark the reply's
// delete holds while the reply's update waits for the root, and Postgres would fail one of the
// two. Any other record queues nothing: a person's own reply under an Agent thread takes the root
// `for update` instead (AgentDismissal), and no write is both a session's and a person's.
func (r Recording) QueueRootLock(b *store.WriteBatch) {
	if r.clears {
		queueRootLock(b, r.thread.id)
	}
}

// PlanRecord plans w's record in the write's read phase, in tx, under the owner lock every writer
// of w's thread takes first: the thread (threadOf), what an answer's catch-up owes (owedBy), the
// person an ask's create reaches where the create did not read them (waitingOn), and for a reply
// the thread's participants and rows (readMembers), which a write that starts its own thread has
// none of and does not read (StartsThread). It reads and takes no row lock, so it holds no
// transaction id: each read is of rows a writer of this thread changes only under that owner lock.
// What the dismiss route writes under the person's rows and the root's lock instead - a dismissal -
// is read in the write phase, by the statement that ends it (Recording.Queue): the Agent
// dismissals a session's reply ends (queueAgentDismissalsClear) and the dismissed tuples of the
// people a reply reaches (graphstore.QueueMarksEnded).
func PlanRecord(ctx context.Context, tx pgx.Tx, w Write) (Recording, error) {
	// A message's reply walks to its root and reads that root's members in one statement
	// (threadAndMembers); a write that read its members already reads none.
	var thread thread
	var err error
	members, membersRead := w.members, w.membersRead
	if w.NodeKind == graphstore.KindMessage && w.Arm == Replies && !w.unstored && !w.root && !membersRead {
		thread, members, err = threadAndMembers(ctx, tx, w)
		membersRead = true
	} else {
		thread, err = threadOf(ctx, tx, w)
	}
	if err != nil {
		return Recording{}, err
	}
	r := Recording{w: w, thread: thread}
	r.authorKind, r.authorID, r.named = storeAuthor(w.Author)
	if r.named && r.authorKind == graphstore.KindPerson {
		r.person = r.authorID
	}
	r.dismisses = ownDismissal(w, r.person, thread)
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
	// A write the store holds no head pointer of moves its thread's rows as the trigger body did,
	// all of them but its writer's, and delivers nothing (Unstored): there is no set to read.
	if w.unstored {
		return r, nil
	}
	if !w.root && !membersRead {
		if members, err = readMembers(ctx, tx, thread); err != nil {
			return Recording{}, err
		}
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
	// Each person the text newly mentions is reached too, whether or not they take part.
	for _, login := range w.mentioned {
		if login == r.person || slices.Contains(r.delivered, login) {
			continue
		}
		r.delivered = append(r.delivered, login)
		if index := slices.IndexFunc(members, func(held member) bool { return held.login == login }); index >= 0 && members[index].row {
			r.moved = append(r.moved, login)
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
// answer's catch-up owes carries the event of the write it belongs to instead. The store's queue
// writers it calls answer nothing, their refusals failing Send (graphstore.QueueWrite); its error
// is that of a step of its own that cannot be queued.
func (r Recording) Queue(b *store.WriteBatch, event int64) error {
	w, thread := r.w, r.thread
	if w.unstored {
		r.queueUnstored(b, event)
		return nil
	}

	// 0. What an answer owes of an ask an older build wrote: its participation and its
	// citations now, its deliveries with step 3's.
	if r.owed.cites {
		queueMarkCitations(b, w.NodeID)
	}
	graphstore.QueueToPerson(b, r.owed.people)
	if len(r.owed.others) > 0 {
		graphstore.QueueWrite(b, nil, r.owed.others)
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
		} else {
			graphstore.QueueWrite(b, nil, []graphstore.Tuple{taking})
		}
	}
	if w.Arm == Opens {
		people = append(people, waitingOnTuples(w.NodeID, r.waiting, r.person, w.At, w.Who, event)...)
	}
	graphstore.QueueToPerson(b, people)
	if w.Arm == TakesPart {
		return nil
	}

	if w.Arm == Replies {
		// A session's reply to a message brings its root back to everyone who dismissed it.
		if r.clears {
			queueAgentDismissalsClear(b, thread.id, r.failClear, w, event)
		}

		// 2. Every row of T, locked in one pass.
		queueThreadLock(b, thread)

		// 3. The write reaches every other participant, each person its text newly mentions, and a
		// session's reply the person who started its direct conversation.
		deliveries := r.owed.deliveries
		delivered := func(login string) {
			deliveries = append(deliveries, deliveredTo(w.NodeKind, w.NodeID, login, w.At, w.Who, event))
		}
		starterDelivered := false
		for _, login := range r.delivered {
			delivered(login)
			starterDelivered = starterDelivered || model.CanonicalLogin(login) == r.starter
		}
		if r.starter != "" && r.starter != r.person && !starterDelivered {
			delivered(r.starter)
		}
		graphstore.QueueToPerson(b, deliveries)
		failpoint.Queue(b, r.failThreads)

		// 4. Each of their dismissals of T that stands ends, and each row of T those people hold
		// moves.
		if len(r.delivered) > 0 {
			graphstore.QueueMarksEnded(b, graphstore.TupleDismissed, thread.kind, thread.id, graphstore.MarkedBy(r.delivered), w.At, w.Who, event)
		}
		queueMoveRows(b, thread, w, r.moved)
	}

	switch r.dismisses {
	case dismissesNothing, dismissesRow:
		// 5. A's own row, and the read mark it stands at where this write moved it; where the write
		// dismisses its row, A's dismissed tuple and the row's dismissed_at with it.
		if login, keeps := w.keptLogin(); keeps {
			if err := queueKeepRow(b, thread, w, login, event, r.dismisses == dismissesRow); err != nil {
				return err
			}
		}
	case dismissesAgentRoot:
		// 6. A reply that dismisses its Agent thread, which keeps no row of A's: A's dismissed tuple
		// to the root message, then A's row of it, which the root lock its writer took first orders
		// against the dismiss route, then the replies under the root A has not read, each marked
		// read by id with its reply_read, as a Dismiss press marks them.
		graphstore.QueueFromPerson(b, w.At, []graphstore.Tuple{dismissalOf(r.person, thread, w, event)})
		queueAgentDismissal(b, r.person, thread.id, w)
		read, params, err := graphstore.MarkArm(4, "replies_read", graphstore.TupleReplyRead, graphstore.KindMessage, w.Who, event)
		if err != nil {
			return fmt.Errorf("record %s's reads of the replies under message %s: %w", r.person, thread.id, err)
		}
		b.Queue(fmt.Sprintf("mark the replies under agent thread %s read for %s", thread.id, r.person),
			agentRepliesRead+"\n"+read+"\nselect 1", append([]any{r.person, thread.id, w.At}, params...)...)
	}
	return nil
}

// dismissalOf is the dismissed tuple from login to thread's root that w states as its writer's own
// dismissal (steps 5 and 6), at w's moment and under its event.
func dismissalOf(login string, thread thread, w Write, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleDismissed,
		ToKind: thread.kind, ToID: thread.id,
		At: w.At, Who: w.Who, SourceEvent: event, Asserted: true,
	}
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

// queueUnstored is the record of an Unstored write: its root's Agent dismissals, for a session's
// reply to a message, cleared as Record clears them; then, for a thread whose owner resolves to a
// project - the only one that holds rows, and the only one user_threads_record wrote any row of -
// every row of the thread locked in Record's one pass, each row of it but its writer's moved and
// unmarked (queueMoveUnmarked), and its writer's own row kept, unmarked (queueKeepUnmarkedRow).
// It states nothing else: no participation, no delivery and no read mark, since no row it writes
// is marked, and the backfill's thread pass states what each row says when it marks it.
func (r Recording) queueUnstored(b *store.WriteBatch, event int64) {
	w, thread := r.w, r.thread
	if r.clears {
		queueAgentDismissalsClear(b, thread.id, r.failClear, w, event)
	}
	if w.Project == "" {
		return
	}
	queueThreadLock(b, thread)
	// The trigger body leaves out of its move the row of a person writer, by their canonical login
	// (user_threads_record's v_writer), and keeps them a row; r.person is that login where it fits
	// the store's bound, which keptLogin holds a row to as well.
	queueMoveUnmarked(b, thread, w, r.person)
	if login, keeps := w.keptLogin(); keeps {
		queueKeepUnmarkedRow(b, thread, w, login)
	}
}
