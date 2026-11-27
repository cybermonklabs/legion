package graphstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A node reserved for its author reaches the person its made_by proved and no other, by the one
// rule (AuthorIsPerson in Go, AuthorIsSQL in SQL), on both of ToPerson's halves: the Go check
// QueueToPerson makes of the nodes it read, and the SQL read a move of who an ask waits on makes
// over the rows its own statement derives (ReservedForAuthorRead). The two rows are where a copy
// of the rule that read who wrote the node (AuthorOf) parted from it: a node made by a session
// alice owns is alice's, and the read shows it to her, so a tuple to her is appended; a node made
// by bob's actor carrying alice as its owner is alice's too, and the read hides it from bob, so a
// tuple to bob is refused.
func TestANodeReservedForItsAuthorReachesThePersonItsMakerProvedAndNoOther(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	alice := "alice"
	for _, row := range []struct {
		name    string
		id      string
		maker   model.Actor
		person  string
		reaches bool
	}{
		{"a session alice owns made it, and the tuple names alice", "author-private-reach-session",
			model.Actor{Kind: "session", ID: "s1", Owner: &alice}, "alice", true},
		{"bob's actor carrying alice as its owner made it, and the tuple names bob", "author-private-reach-owner",
			model.Actor{Kind: "user", ID: "bob", Owner: &alice}, "bob", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			writeNode(t, ctx, database, Node{
				Kind: KindComment, ID: row.id, Supertag: KindComment,
				Visibility: VisibilityAuthor, State: StateLive,
				MadeAt: at, MadeBy: row.maker, SourceEvent: NoEvent,
			})
			if got := AuthorIsPerson(row.maker, row.person); got != row.reaches {
				t.Errorf("AuthorIsPerson answers %t for %s, want %t", got, row.person, row.reaches)
			}

			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			// The SQL half first, over the same tuple as a statement of the caller's derives it.
			read := tx.QueryRow(ctx, `
				with stated (`+ToPersonRows+`) as (
					values (1, $1::text, $2::text, $3::text, $4::text, $5::text, true)
				)`+ReservedForAuthorRead("stated"),
				KindComment, row.id, TupleMentions, KindPerson, row.person)
			requireReach(t, "ReservedForAuthorRead", RefuseReservedForAuthor(read), row.reaches)
			// Then the Go half, through ToPerson itself.
			requireReach(t, "ToPerson", ToPerson(ctx, tx, []Tuple{{
				FromKind: KindComment, FromID: row.id, Kind: TupleMentions, ToKind: KindPerson, ToID: row.person,
				At: at, Who: row.maker, SourceEvent: NoEvent, Asserted: true,
			}}), row.reaches)
		})
	}
}

// requireReach fails unless err, what half answered for one tuple, is nil where the tuple's
// person is the node's author (reaches) and ErrReservedForAuthor where they are not.
func requireReach(t *testing.T, half string, err error, reaches bool) {
	t.Helper()
	switch {
	case reaches && err != nil:
		t.Errorf("%s refused the tuple to the node's author: %v", half, err)
	case !reaches && !errors.Is(err, ErrReservedForAuthor):
		t.Errorf("%s answered %v for a tuple to a person the node is hidden from, want ErrReservedForAuthor", half, err)
	}
}
