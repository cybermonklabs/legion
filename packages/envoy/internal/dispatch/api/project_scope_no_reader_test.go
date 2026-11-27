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

// TestASweepResumeOfASessionRouteWithNoReaderStillDelivers (R9): the delivery sweep resumes an
// attempt of a message to session s1 that its sender left pending, with no reader in its context,
// and the listener stores the send, as before: the resolve of a session route held to its author's
// reader when it was written reads the session as the server's reader (resolveDeliveryTarget), so
// sessionHiddenFromReader's closed answer to a context with no reader refuses none. So it does in
// a deployment that holds no private project, and in one that holds one.
//
// Control: resolveDeliveryTarget's session branch passing its context as it came, so the session
// check reads a context with no reader, leaves the listener with no send for s1, and the row fails
// naming it.
func TestASweepResumeOfASessionRouteWithNoReaderStillDelivers(t *testing.T) {
	for _, deployment := range []struct {
		name    string
		private bool
	}{{"no private project", false}, {"a private project", true}} {
		t.Run(deployment.name, func(t *testing.T) {
			listener := stoppedAndLive(t)
			_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
			ctx := context.Background()
			if deployment.private {
				if _, err := database.Pool.Exec(ctx, `
					insert into projects (key, name, default_assignee, owner, visibility)
					values ('PRIV', 'Private', 'alice', 'alice', 'private')`); err != nil {
					t.Fatalf("make a private project: %v", err)
				}
			}
			var messageID string
			if err := database.Pool.QueryRow(ctx, `
				insert into messages (author, body, target)
				values ('{"kind":"user","id":"alice"}', 'Carry on.', 'session:s1')
				returning id::text
			`).Scan(&messageID); err != nil {
				t.Fatalf("seed the message: %v", err)
			}
			// The attempt its own request committed and never settled: pending, claimed ten
			// minutes ago, which is what the sweep takes.
			if _, err := database.Pool.Exec(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state,
				                                claimed_at, created_at, requested_by)
				values ($1::uuid, 1, 'steer', 's1', 'pending',
				        now() - interval '10 minutes', now() - interval '10 minutes',
				        '{"kind":"user","id":"alice"}')
			`, messageID); err != nil {
				t.Fatalf("strand the attempt: %v", err)
			}

			if err := served.resumeOwedDeliveries(ctx); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			attempts := messageAttempts(t, database, messageID)
			if len(attempts) != 1 || attempts[0].State != "queued" || attempts[0].Error != nil {
				t.Fatalf("the resumed attempt = %#v, want one queued attempt with no error", attempts)
			}
			if sends := listener.sentTo("s1"); len(sends) != 1 {
				t.Fatalf("the listener stored %d sends for s1 and the attempt is %#v, want the one resumed send", len(sends), attempts[0])
			}
		})
	}
}
