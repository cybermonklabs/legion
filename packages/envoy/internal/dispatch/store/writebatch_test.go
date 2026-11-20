package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A batch reads its answers by how it is sent, and each sender refuses, before anything is sent,
// the read the other one takes: Send, whose transaction a callback's error abandons, refuses an
// observed read, whose error undoes nothing; SendCommitting, whose batch commits at its end,
// refuses a QueryRow, a Query and an Exec callback, whose error would read as the refusal of a
// write already committed. Neither touches its transaction or its pool when it refuses, so the
// test hands them none.
func TestEachBatchSenderRefusesTheOtherSendersReads(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s = %v, want a refusal naming %q", what, err, want)
		}
	}
	t.Run("Send refuses an observed read", func(t *testing.T) {
		var b WriteBatch
		b.Queue("", `select 1`)
		b.Queue("", `update t set c = 1 returning c`).Observe(func(pgx.Rows) error { return nil })
		refused(t, "Send of a batch holding an Observe", b.Send(ctx, nil), "statement 2 of the batch observes")
	})
	for _, read := range []struct {
		name  string
		queue func(Queued)
		want  string
	}{
		{"QueryRow", func(q Queued) { q.QueryRow(func(pgx.Row) error { return nil }) }, "statement 1 of the batch reads"},
		{"Query", func(q Queued) { q.Query(func(pgx.Rows) error { return nil }) }, "statement 1 of the batch reads"},
		{"an Exec callback", func(q Queued) { q.Exec(func(pgconn.CommandTag) error { return nil }) }, "statement 1 of the batch hands"},
	} {
		t.Run("SendCommitting refuses "+read.name, func(t *testing.T) {
			var b WriteBatch
			read.queue(b.Queue("", `update t set c = 1 returning c`))
			b.Queue("", `select 1`).Observe(func(pgx.Rows) error { return nil })
			refused(t, "SendCommitting of a batch holding "+read.name, b.SendCommitting(ctx, nil), read.want)
		})
	}
	t.Run("an Exec with no callback is a dropped result either sender takes", func(t *testing.T) {
		var b WriteBatch
		q := b.Queue("", `update t set c = 1`)
		q.Exec(func(pgconn.CommandTag) error { return nil })
		q.Exec(nil)
		if err := b.refuse(readExec, "hands"); err != nil {
			t.Fatalf("a statement whose callback was dropped again is read as %v, want a dropped result", err)
		}
	})
}
