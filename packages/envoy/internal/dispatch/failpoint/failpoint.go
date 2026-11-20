// Package failpoint is the seam a test injects a failure through between one writer's store write
// and its projection write (dispatch://INFRA-1152 R9, c-census). A server whose test hooks are on
// carries one Armed set on every request it serves (With); PUT
// /api/v1/writes/_test/fail-between/{writer} arms it for the next write of one writer; and each
// writer below calls Between at the point between its two writes, which answers ErrInjected once
// for an armed writer and nil for any other. The writer then fails as any failed statement fails
// it: its transaction rolls back, both sides with it, and its event, appended in that
// transaction, reaches nobody.
//
// A request no Armed set rides on - every request of a server without test hooks - answers nil at
// once, so a deployment pays one context lookup at each point and nothing else.
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
// of a projection of dispatch://INFRA-1152 the census names.
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
	// AskAnswer is an ask's answer (asks.Settle): between the answer's tuples and the update of
	// the asks row that projects them.
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

// Writers is every writer a failure can be injected into, in the order c-census runs them.
var Writers = []string{
	Comment, Message, DirectReply, AskAnswer,
	InboxDismiss, InboxRead, ThreadRead, AgentState, AskSnooze,
	UserThreads, AgentDismissalClear,
}

// ErrInjected is the failure Between answers for an armed writer.
var ErrInjected = errors.New("failure injected between the store write and the projection write (PUT /api/v1/writes/_test/fail-between)")

// Armed is the set of writers whose next write fails, one failure each, shared by every request a
// server serves.
type Armed struct {
	mu    sync.Mutex
	armed map[string]bool
	taken map[string]int
}

// NewArmed is an empty set.
func NewArmed() *Armed { return &Armed{armed: map[string]bool{}, taken: map[string]int{}} }

// Known reports whether writer is one of Writers.
func Known(writer string) bool { return slices.Contains(Writers, writer) }

// Arm makes the next write of writer fail. Arming a writer already armed arms it once.
func (a *Armed) Arm(writer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed[writer] = true
}

// Taken is how many failures writer's writes have taken since the set was made.
func (a *Armed) Taken(writer string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.taken[writer]
}

// take disarms writer and reports whether it was armed.
func (a *Armed) take(writer string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.armed[writer] {
		return false
	}
	delete(a.armed, writer)
	a.taken[writer]++
	return true
}

type armedKey struct{}

// With is ctx carrying armed, which every Between under it reads.
func With(ctx context.Context, armed *Armed) context.Context {
	return context.WithValue(ctx, armedKey{}, armed)
}

// Between is the point between writer's store write and its projection write: ErrInjected where
// the set ctx carries armed writer, which this disarms, and nil otherwise.
func Between(ctx context.Context, writer string) error {
	armed, carried := ctx.Value(armedKey{}).(*Armed)
	if !carried || !armed.take(writer) {
		return nil
	}
	return ErrInjected
}

// Planned is Between for a writer whose two writes are statements of one write phase, which runs
// no Go between them: it decides, as the writer plans its write, whether the write fails - true
// where the set ctx carries armed writer, which this disarms - and the writer then queues Queue
// between the two statements, so the failure falls where Between would have answered it.
func Planned(ctx context.Context, writer string) bool {
	return Between(ctx, writer) != nil
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
