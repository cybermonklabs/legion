package api

import (
	"context"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// going_quick.go holds the bounds of the two model calls that stay in the server and out of the
// agent's runs (R15): the short answer of R3, which streams as the person types, and Jev's choice
// of R2, which its run asks first. Both are one going_requests row with its going_events, which
// startGoingRun answers, and each is bounded so that typing cannot take the server's memory or
// the chat model's quota (R16; section 5 of the background-runs design).
//
// The short answer is one request of one person: a loop of at most contracts.GoingModelCallMax
// (4) model calls, its searches and reads between them. Its bounds are:
//
//   - one in flight for each person: a newer question of the same credential ends the one before
//     it, as its author's cancel does (cancelGoingRequest), so a keystroke cancels the answer to
//     the words before it;
//   - goingQuickSlots at once in this server: the next waits for a free slot, within its window;
//   - each model call's stream read through going's 4 MiB limit (answerStreamLimit), past which
//     that call fails the answer, and Jev's answer through agentloop's own limit of a decision;
//   - goingQuickTime, 30 s from the POST, for the whole loop - every call, search and read of it
//     together, not each call - past which the answer fails with ANSWER_DID_NOT_ARRIVE;
//   - 25% of the chat model's quota, which the answerer paces its calls to (going's
//     answerQuotaShare), the agent pacing its runs to the other 75%.

const (
	// goingQuickSlots is how many short answers this server answers at once.
	goingQuickSlots = 32
	// goingQuickTime is how long one short answer may take, from the POST that asked it: its
	// record's window (going_requests.ends_at).
	goingQuickTime = contracts.GoingRunWindow
)

// goingQuick is this server's bounds of its short answers: their slots, and the turn of each
// person with a question in flight. Its zero value is ready.
type goingQuick struct {
	once  sync.Once
	slots chan struct{}
	mu    sync.Mutex
	turns map[string]*goingQuickTurn
}

// goingQuickTurn is one question's turn as its person's one question in flight: its context
// ends once a newer question of the person takes the turn, or once the question leaves it.
type goingQuickTurn struct {
	person string
	ctx    context.Context
	end    context.CancelFunc
}

// init makes the slots and the turns, once.
func (q *goingQuick) init() {
	q.once.Do(func() {
		q.slots = make(chan struct{}, goingQuickSlots)
		q.turns = map[string]*goingQuickTurn{}
	})
}

// take makes a new turn person's one question in flight, ending the turn of the question before
// it, and answers the new turn.
func (q *goingQuick) take(person string) *goingQuickTurn {
	q.init()
	turn := &goingQuickTurn{person: person}
	turn.ctx, turn.end = context.WithCancel(context.Background())
	q.mu.Lock()
	before := q.turns[person]
	q.turns[person] = turn
	q.mu.Unlock()
	if before != nil {
		before.end()
	}
	return turn
}

// leave ends turn, whose question is over, and frees its person's turn unless a newer question
// took it.
func (q *goingQuick) leave(turn *goingQuickTurn) {
	q.init()
	q.mu.Lock()
	if q.turns[turn.person] == turn {
		delete(q.turns, turn.person)
	}
	q.mu.Unlock()
	turn.end()
}

// slot waits for one of the server's slots until ctx ends, and answers its release; false where
// ctx ended first.
func (q *goingQuick) slot(ctx context.Context) (func(), bool) {
	q.init()
	select {
	case q.slots <- struct{}{}:
		return func() { <-q.slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// goingQuickPerson is the key a question's author is one person by: the credential's kind and
// id, and the person and the workload it names, which model.Actor.SameCredential compares.
func goingQuickPerson(author model.Actor) string {
	var owner, service string
	if author.Owner != nil {
		owner = *author.Owner
	}
	if author.Service != nil {
		service = *author.Service
	}
	return strings.Join([]string{author.Kind, author.ID, owner, service}, "\x00")
}
