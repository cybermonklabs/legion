// Package inboxcompare is dispatch://INFRA-1152's comparison of the one read, GET /api/v1/me/inbox,
// with the three reads it replaces, and the run rows the gate reads (R8, R10).
// `envoy-dispatch inbox-compare` runs the comparison (cmd/dispatch/inbox_compare.go) and
// GET /api/v1/inbox/comparison answers the newest run's counts (api/inbox_comparison.go).
//
// It runs in the branch alone, against two loopback servers that serve one test database: the
// build of the release before on the old URL and this build on the new one. The commit of the
// branch that moves every caller off the three reads deletes it, with the command, the route, the
// page and the table, so none of them reaches a release.
package inboxcompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The arms of a comparison: the three reads the one read replaces, each asked as the person. A
// difference names the arm whose row the one read did not answer, and a count is kept per person
// and per arm, so agreement in one arm cancels nothing in another.
const (
	// ArmInbox is GET /api/v1/inbox?assignee=me: the open asks that wait on the person.
	ArmInbox = "inbox"
	// ArmReplies is GET /api/v1/me/replies?kind=reply,agent&state=inbox: the threads the person
	// wrote in that somebody else wrote in, and their one-off messages to a session holding a
	// reply from it.
	ArmReplies = "replies"
	// ArmConversations is GET /api/v1/agents/{session_id}/messages, asked for each session the
	// person started a conversation with, of which the comparison reads the person's own
	// conversations: each root they wrote and each reply the session wrote under it.
	ArmConversations = "conversations"
)

// Arms is every arm, in the order a run reports them.
var Arms = []string{ArmInbox, ArmReplies, ArmConversations}

// ArmRead is the read arm asks, as the run's report names it.
func ArmRead(arm string) string {
	switch arm {
	case ArmInbox:
		return "GET /api/v1/inbox?assignee=me"
	case ArmReplies:
		return "GET /api/v1/me/replies?kind=reply,agent&state=inbox"
	case ArmConversations:
		return "GET /api/v1/agents/{session_id}/messages"
	}
	return arm
}

// The relation kinds a difference is counted under, R10's five. An old row is of the first three:
// an Inbox ask row is the assigned open ask, a Reply row the thread reply, and an Agent row, or a
// root or a session's reply of the person's own conversation, the direct reply. Participation and
// the mention are kinds the one read answers and no old read answers a row of: an authored comment
// nobody answered, a thread on a closed issue and a mention are entries of the one read alone. So
// no old row is of either, and their counts are the zero of an old read that holds none of them.
const (
	KindAssignedAsk   = "assigned_ask"
	KindThreadReply   = "thread_reply"
	KindDirectReply   = "direct_reply"
	KindParticipation = "participation"
	KindMention       = "mention"
)

// Kinds is every kind a run counts differences under, in the order it reports them.
var Kinds = []string{KindAssignedAsk, KindThreadReply, KindDirectReply, KindParticipation, KindMention}

// The shapes of the corpus a run must find before its count proves anything: every kind of row the
// three reads answer, a person with rows and a person with none, and a reply chain at the walk's
// bound. Each is counted over the people the run reached, so a person it did not reach adds to no
// shape. A run that finds none of one names it and refuses: zero differences over a corpus without
// every shape is no proof the one read answers that shape.
const (
	// ShapeInboxAsk is a row of GET /api/v1/inbox?assignee=me, an open ask that waits on a person.
	ShapeInboxAsk = "inbox_ask"
	// ShapeAskThread, ShapeCommentThread and ShapeMessageThread are Reply rows of GET
	// /api/v1/me/replies of each kind of thread: an ask, a comment thread and an issue message
	// thread a person wrote in that somebody else wrote in.
	ShapeAskThread     = "ask_thread"
	ShapeCommentThread = "comment_thread"
	ShapeMessageThread = "message_thread"
	// ShapeAgentRow is an Agent row of GET /api/v1/me/replies: a person's one-off message to a
	// session holding a reply from that session.
	ShapeAgentRow = "agent_row"
	// ShapeConversationReply is a session's reply under a root a person wrote, in GET
	// /api/v1/agents/{session_id}/messages.
	ShapeConversationReply = "conversation_reply"
	// ShapePersonWithRows and ShapePersonWithNone count the people the run reached whose three
	// reads answered some row, and none.
	ShapePersonWithRows = "person_with_rows"
	ShapePersonWithNone = "person_with_none"
	// ShapeReplyChain is a comment reply chain ChainLinks links long, from its deepest comment up
	// reply_to to its root, holding a comment by a person the run reached: the depth the outbox's
	// walk up a reply_to chain stops at.
	ShapeReplyChain = "reply_chain_at_bound"
)

// Shapes is every shape a run requires, in the order it reports them.
var Shapes = []string{
	ShapeInboxAsk, ShapeAskThread, ShapeCommentThread, ShapeMessageThread, ShapeAgentRow,
	ShapeConversationReply, ShapePersonWithRows, ShapePersonWithNone, ShapeReplyChain,
}

// ChainLinks is outbox.maxCommentThreadDepth (outbox/publisher.go), the bound of the walk up a
// comment's reply_to chain, which ShapeReplyChain's chain reaches. It is copied, as the corpus of
// api's one_inbox_scale_seed_test.go copies it, and TestTheChainIsAsLongAsTheOutboxWalk holds it
// to the declaration.
const ChainLinks = 32

// Difference is one row an old read answered and the one read did not.
type Difference struct {
	// Person is the canonical login the reads were asked as.
	Person string `json:"person"`
	// Arm is the old read that answered the row (ArmInbox, ArmReplies or ArmConversations).
	Arm string `json:"arm"`
	// Kind is the relation kind it is counted under (Kinds).
	Kind string `json:"kind"`
	// Row is the old read's row, as its read names it: ask:<id>, reply:<thread id>,
	// agent:<root message id>, or message:<id> for a conversation's root or reply.
	Row string `json:"row"`
	// Node is the node that row stands for, <kind>:<id>, which the one read answers no entry of.
	Node string `json:"node"`
}

// Run is one row of inbox_comparison_runs.
type Run struct {
	ID    int64
	RanAt time.Time
	// DifferenceCount is how many rows the old reads answered and the one read did not; Store
	// writes len(Differences), and Newest reads it without the rows.
	DifferenceCount int
	// PeopleChecked is every person the database holds; PeopleUncovered those the run did not
	// reach.
	PeopleChecked   int
	PeopleUncovered int
	// Shapes holds each of Shapes with the rows the run found of it, a shape of none included.
	Shapes map[string]int
	// Differences are the rows themselves: Store writes them, and Newest leaves them unread.
	Differences []Difference
}

// MissingShapes is every required shape the run found no row of, in Shapes' order.
func (run Run) MissingShapes() []string {
	var missing []string
	for _, shape := range Shapes {
		if run.Shapes[shape] == 0 {
			missing = append(missing, shape)
		}
	}
	return missing
}

// Met is the gate over this run: no difference, nobody uncovered, and every shape in the corpus.
func (run Run) Met() bool {
	return run.DifferenceCount == 0 && run.PeopleUncovered == 0 && len(run.MissingShapes()) == 0
}

// Querier is what reads and writes a run row: a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store inserts run as a row of its own, setting its DifferenceCount from its Differences and its
// ID and RanAt from the row. It is the one writer of inbox_comparison_runs, and it only inserts: a
// later run leaves every earlier row as it was written.
func Store(ctx context.Context, db Querier, run *Run) error {
	shapes := make(map[string]int, len(Shapes))
	for _, shape := range Shapes {
		shapes[shape] = run.Shapes[shape]
	}
	differences := run.Differences
	if differences == nil {
		differences = []Difference{}
	}
	encodedShapes, err := json.Marshal(shapes)
	if err != nil {
		return fmt.Errorf("encode the run's shapes: %w", err)
	}
	encodedDifferences, err := json.Marshal(differences)
	if err != nil {
		return fmt.Errorf("encode the run's differences: %w", err)
	}
	run.DifferenceCount = len(differences)
	if err := db.QueryRow(ctx, `
		insert into inbox_comparison_runs (difference_count, people_checked, people_uncovered, shapes, differences)
		values ($1, $2, $3, $4, $5)
		returning id, ran_at
	`, run.DifferenceCount, run.PeopleChecked, run.PeopleUncovered, encodedShapes, encodedDifferences).Scan(&run.ID, &run.RanAt); err != nil {
		return fmt.Errorf("store the comparison's run row: %w", err)
	}
	run.Shapes = shapes
	return nil
}

// Newest reads the newest run row's counts and shapes, and none of its differences; nil when no
// run has been stored.
func Newest(ctx context.Context, db Querier) (*Run, error) {
	var run Run
	var shapes []byte
	err := db.QueryRow(ctx, `
		select id, ran_at, difference_count, people_checked, people_uncovered, shapes
		from inbox_comparison_runs
		order by id desc
		limit 1
	`).Scan(&run.ID, &run.RanAt, &run.DifferenceCount, &run.PeopleChecked, &run.PeopleUncovered, &shapes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest comparison run: %w", err)
	}
	if err := json.Unmarshal(shapes, &run.Shapes); err != nil {
		return nil, fmt.Errorf("decode the shapes of comparison run %d: %w", run.ID, err)
	}
	return &run, nil
}
