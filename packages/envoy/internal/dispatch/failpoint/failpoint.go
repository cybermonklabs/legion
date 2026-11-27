// Package failpoint is the seam a test injects a failure through between one writer's store write
// and its projection write (the spec's R9, c-census). A server whose test hooks are on
// carries one Armed set on every request it serves (With); PUT
// /api/v1/writes/_test/fail-between/{writer} arms it for the next write of one writer; and each
// writer below calls Planned as it plans its write and queues Queue between its two statements,
// whose statement answers ErrInjected once for an armed writer and is queued for no other. The
// writer then fails as any failed statement fails it: its transaction rolls back, both sides with
// it, and its event, appended in that transaction, reaches nobody.
//
// For a request no Armed set rides on - every request of a server without test hooks - Planned
// answers false at once, so a deployment pays one context lookup at each point and nothing else.
package failpoint

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The writers a failure can be injected into, by the name the route arms them under: each writer
// of a projection of the one inbox the census names.
const (
	// Comment is the comment route (api's createCommentFor): between the comment's node and the
	// comments row that names it.
	Comment = "comment"
	// Message is the message route (api's createStoredMessage): between the message's node and
	// the messages row that names it.
	Message = "message"
	// DirectReply is a session's direct reply (api's insertSessionReply): between the reply's node
	// and its messages row.
	DirectReply = "direct-reply"
	// AskAnswer is an ask's answer (asks.Settled.Queue): between the node write of the answer and
	// the update of the asks row. Every writer that stores an answer holds its tuples for the
	// event it appends after that update (asks.Change.Held), so the failure falls before them.
	AskAnswer = "ask-answer"
	// InboxDismiss, InboxRead, ThreadRead, AgentState and AskSnooze are the five read-state
	// routes (api/inbox_dismiss.go, api/inbox_read.go, putThreadRead in api/replies.go,
	// api/state.go and api/ask_snooze.go): each between the statement that writes its projection
	// and the tuples graphstore.FromPerson appends under the route's event.
	InboxDismiss = "inbox-dismiss"
	InboxRead    = "inbox-read"
	ThreadRead   = "thread-read"
	AgentState   = "agent-state"
	AskSnooze    = "ask-snooze"
	// UserThreads is the one writer of user_threads (inboxwrite.Record): between the delivered_to
	// tuples a reply appends and the move of the rows of the people they reach.
	UserThreads = "user-threads"
	// AgentDismissalClear is the Go writer in a session's reply that clears its root's Agent
	// dismissals (inboxwrite.Record, the clear of insertSessionReply's reply): between the
	// retraction of each user_agent_thread_dismissed row's dismissed tuple and the delete of the rows.
	AgentDismissalClear = "agent-dismissal-clear"
)

// Writers is every writer a failure can be injected into.
var Writers = []string{
	Comment, Message, DirectReply, AskAnswer,
	InboxDismiss, InboxRead, ThreadRead, AgentState, AskSnooze,
	UserThreads, AgentDismissalClear,
}

// ErrInjected is the failure Queue's statement answers for an armed writer.
var ErrInjected = errors.New("failure injected between the store write and the projection write (PUT /api/v1/writes/_test/fail-between)")

// Armed is the set of writers whose next write fails, one failure each, shared by every request a
// server serves.
type Armed struct {
	mu    sync.Mutex
	armed map[string]bool
}

// NewArmed is an empty set.
func NewArmed() *Armed { return &Armed{armed: map[string]bool{}} }

// Known reports whether writer is one of Writers.
func Known(writer string) bool { return slices.Contains(Writers, writer) }

// Arm makes the next write of writer fail. Arming a writer already armed arms it once.
func (a *Armed) Arm(writer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed[writer] = true
}

// take disarms writer and reports whether it was armed.
func (a *Armed) take(writer string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.armed[writer] {
		return false
	}
	delete(a.armed, writer)
	return true
}

type armedKey struct{}

// With is ctx carrying armed, which every Planned under it reads.
func With(ctx context.Context, armed *Armed) context.Context {
	return context.WithValue(ctx, armedKey{}, armed)
}

// Planned decides, as a writer whose two writes are statements of one write phase plans its write,
// whether the write fails: true where the set ctx carries armed writer, which this disarms, and
// false otherwise. The writer then queues Queue between the two statements, so the failure falls
// between its store write and its projection write.
func Planned(ctx context.Context, writer string) bool {
	armed, carried := ctx.Value(armedKey{}).(*Armed)
	return carried && armed.take(writer)
}

// Queue queues on b, between a writer's store write and its projection write, the statement a
// write that Planned armed fails at: it changes nothing, and its result answers ErrInjected, which
// stops the write phase and is what it returns, so the transaction rolls back with both writes.
// It queues nothing for a write that is not to fail, so a deployment's write phase is the one it
// would be without the seam.
func Queue(b *store.WriteBatch, fails bool) {
	if !fails {
		return
	}
	b.Queue("", `select 1`).Exec(func(pgconn.CommandTag) error { return ErrInjected })
}
