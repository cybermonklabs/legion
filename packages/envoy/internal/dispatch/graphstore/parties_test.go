package graphstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A node's parties are stored as one value, joined on a space, and the column is split back on a
// space, so a party that held white space would become two parties in the column, and an empty
// party would be none. PartiesValue is the one place that joins them, and it refuses such a party
// with ErrBadParty. Every write that binds parties goes through it - a node write (Write,
// QueueNodes, the publisher's arms) and the backfill's fill (FillScopes) - so no row ever holds
// a party the writer did not mean, and nothing of the refused write reaches the store.
//
// The control of both tests is the refusal taken out of PartiesValue: the join then answers
// "session:a b" as the two parties "session:a" and "b", and every case below fails.

// badParties are the parties PartiesValue refuses: white space of each class strings.TrimSpace
// knows, at the end and inside, and the empty party.
var badParties = []string{"session:a b", "session:a\tb", "session:a\u00a0b", " session:a", ""}

// TestPartiesValueRefusesAPartyThatHoldsWhiteSpace is the join itself and the caller that binds
// nothing: the publisher's arms, which build their statement without a database. A node write
// queued on a batch (QueueWrite, QueueNodes) answers nothing when it is queued and refuses such a
// party when its batch is sent, which TestAWriteBetweenAPartyThatHoldsWhiteSpaceWritesNothing holds.
func TestPartiesValueRefusesAPartyThatHoldsWhiteSpace(t *testing.T) {
	at := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	for _, party := range badParties {
		if value, err := PartiesValue([]string{PartySession("ok"), party}); !errors.Is(err, ErrBadParty) {
			t.Errorf("PartiesValue of %q answered %q, %v, want ErrBadParty", party, value, err)
		}
		node := MessageNode("m-bad", "", []string{party}, who, at)
		if _, _, err := WriteArms(1, []Node{node}, nil, nil); !errors.Is(err, ErrBadParty) {
			t.Errorf("WriteArms of a node between %q answered %v, want ErrBadParty", party, err)
		}
	}
	if value, err := PartiesValue([]string{PartySession("s-2"), PartyPerson("Bob"), PartySession("s-2")}); err != nil || value != "person:bob session:s-2" {
		t.Errorf("PartiesValue of good parties answered %q, %v, want %q", value, err, "person:bob session:s-2")
	}
}

// TestAWriteBetweenAPartyThatHoldsWhiteSpaceWritesNothing is the writers that bind parties into a
// statement of their own, against the store: each refuses, and no row of the node is written.
func TestAWriteBetweenAPartyThatHoldsWhiteSpaceWritesNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	bad := MessageNode("44444444-4444-4444-8444-000000000001", "", []string{PartySession("a b")}, who, at)
	for name, write := range map[string]func(pgx.Tx) error{
		"Write": func(tx pgx.Tx) error { return Write(ctx, tx, []Node{bad}, nil) },
		"QueueNodes": func(tx pgx.Tx) error {
			write := PlanUncaused([]Node{bad}, nil)
			return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				QueueNodes(b, write)
				return nil
			})
		},
		"FillScopes": func(tx pgx.Tx) error {
			_, err := FillScopes(ctx, tx, []Scope{{Kind: KindMessage, ID: bad.ID, Parties: bad.Parties}})
			return err
		},
	} {
		if err := pgx.BeginFunc(ctx, database.Pool, write); !errors.Is(err, ErrBadParty) {
			t.Errorf("%s of a node between %v answered %v, want ErrBadParty", name, bad.Parties, err)
		}
	}
	var held int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where id = $1`, bad.ID).Scan(&held); err != nil || held != 0 {
		t.Errorf("the store holds %d row(s) of the refused node (%v), want none", held, err)
	}
}
