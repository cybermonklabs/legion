package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

type ticketContextValueKey struct{}

// WithTicket's first context for a ticket is a context type of this package's own (ticketContext),
// a field of the ticket, not context.WithValue's. So every context the request derives from it
// must still behave as one derived from context.WithValue's would. A WithCancel, a WithTimeout or
// a context.AfterFunc on it ends when its parent ends and carries the parent's cause. Each reads
// the ticket and every other value of the parent. A second WithTicket for the same ticket, which
// makes a context of its own, still finds that ticket.
func TestAContextDerivedFromATicketsContextEndsWithItsParentAndReadsItsValues(t *testing.T) {
	l := newLimiter(1, 1, patient)
	ticket := admit(t, l, Read, 1)
	defer ticket.Release()
	cause := errors.New("the request's own end")
	parent, end := context.WithCancelCause(context.WithValue(context.Background(), ticketContextValueKey{}, "the parent's value"))
	carried := WithTicket(parent, ticket)
	if _, own := carried.(*ticketContext); !own {
		t.Fatalf("WithTicket's first context for a ticket is a %T, want the ticket's own ticketContext, which is what this row tests", carried)
	}
	withCancel, stopCancel := context.WithCancel(carried)
	defer stopCancel()
	withTimeout, stopTimeout := context.WithTimeout(carried, time.Hour)
	defer stopTimeout()
	afterRan := make(chan struct{})
	stopAfter := context.AfterFunc(carried, func() { close(afterRan) })
	defer stopAfter()
	second := WithTicket(carried, ticket)
	derived := map[string]context.Context{
		"WithTicket's first context": carried,
		"a WithCancel on it":         withCancel,
		"a WithTimeout on it":        withTimeout,
		"a second WithTicket on it":  second,
	}
	for name, ctx := range derived {
		if got := TicketFrom(ctx); got != ticket {
			t.Errorf("%s carries ticket %p, want %p", name, got, ticket)
		}
		if got := ctx.Value(ticketContextValueKey{}); got != "the parent's value" {
			t.Errorf("%s reads the parent's value as %v, want %q", name, got, "the parent's value")
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("%s has ended before its parent did: %v", name, err)
		}
	}

	end(cause)
	for name, ctx := range derived {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not end within 5 s of its parent", name)
		}
		if err := ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s ended with %v, want context.Canceled, as its parent did", name, err)
		}
		if got := context.Cause(ctx); got != cause {
			t.Errorf("%s carries the cause %v, want its parent's %v", name, got, cause)
		}
	}
	select {
	case <-afterRan:
	case <-time.After(5 * time.Second):
		t.Fatal("a context.AfterFunc on WithTicket's first context did not run within 5 s of its parent's end")
	}
}
