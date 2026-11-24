package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheHiddenFromReaderChecksAnswerAContextWithNoReaderHidden: each check that holds a write to
// what its request's reader sees - sessionHiddenFromReader, sessionsHiddenFromReader,
// personHiddenFromReader and groupTitleRule - fails closed on a context that holds no reader, as a
// GET request's context does before any gate has put one in: s-pub, bob and s-pub's title are each
// answered hidden there, though carol, the request's caller, sees all three once her reader is in
// the context (the row's control on the objects themselves). A reader with no viewer reads as the
// closed viewer it binds, so it sees no person either. With the reader in the context, every
// answer is what each check gave before.
//
// Control: any one check answering a context with no reader as it did before - not hidden, or
// the group's name held to every title (events.EveryEvent) - fails the row naming that check.
func TestTheHiddenFromReaderChecksAnswerAContextWithNoReaderHidden(t *testing.T) {
	walk := seedHiddenWrites(t)
	stampSessionTitle(t, walk, walk.issue[seedPublic], "s-pub", "pub codename")
	request := walk.readerRequest(t, callerCarol)
	unread := request.Context()
	if reader, ok := readerFromContext(unread); ok {
		t.Fatalf("carol's GET request holds the reader %+v before any gate, want none", reader)
	}
	carol, ok := walk.server.readerOf(httptest.NewRecorder(), request)
	if !ok {
		t.Fatal("resolve carol's reader")
	}
	read := withReader(unread, carol)

	checks := []struct {
		name   string
		hidden func(context.Context) (bool, error)
	}{
		{"sessionHiddenFromReader of s-pub", func(ctx context.Context) (bool, error) {
			return walk.server.sessionHiddenFromReader(ctx, "s-pub")
		}},
		{"sessionsHiddenFromReader of s-pub", func(ctx context.Context) (bool, error) {
			hidden, err := walk.server.sessionsHiddenFromReader(ctx, []string{"s-pub"})
			if err != nil {
				return false, err
			}
			return hidden("s-pub"), nil
		}},
		{"personHiddenFromReader of bob", func(ctx context.Context) (bool, error) {
			return personHiddenFromReader(ctx, walk.database.Pool, seedBob)
		}},
		// The group's name is s-pub's title: a rule that reads s-pub's title refuses it naming
		// s-pub, and a rule that sees no session holds the name to no title.
		{"groupTitleRule over s-pub's title", func(ctx context.Context) (bool, error) {
			tx, err := walk.database.Pool.Begin(ctx)
			if err != nil {
				return false, err
			}
			defer tx.Rollback(ctx)
			_, err = delivery.CreateSessionGroup(ctx, tx, "pub codename", []string{"s-nobody"},
				model.Actor{Kind: "user", ID: seedCarol}, groupTitleRule(ctx))
			var titled *delivery.SessionGroupNameIsTitleError
			switch {
			case errors.As(err, &titled) && titled.Session == "s-pub":
				return false, nil
			case err != nil:
				return false, err
			}
			return true, nil
		}},
	}
	for _, check := range checks {
		for _, on := range []struct {
			name string
			ctx  context.Context
			want bool
		}{{"a context with no reader", unread, true}, {"carol's reader", read, false}} {
			hidden, err := check.hidden(on.ctx)
			if err != nil {
				t.Fatalf("%s on %s: %v", check.name, on.name, err)
			}
			if hidden != on.want {
				t.Errorf("%s on %s answered hidden %t, want %t", check.name, on.name, hidden, on.want)
			}
		}
	}

	viewerless := withReader(unread, model.Actor{Kind: "user", ID: seedCarol})
	if hidden, err := personHiddenFromReader(viewerless, walk.database.Pool, seedBob); err != nil || !hidden {
		t.Errorf("personHiddenFromReader of bob for a reader with no viewer answered hidden %t (%v), want hidden: it reads as the closed viewer", hidden, err)
	}
}
