package ompsessions

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// recorder is a pgx QueryTracer that keeps the text of every statement its connection sends, in
// order: pgx traces each Exec, Query and QueryRow, and Begin, Commit and Rollback are Execs of
// their own.
type recorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *recorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, data.SQL)
	return ctx
}

func (r *recorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take is what the connection sent since the last take.
func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	sent := r.sent
	r.sent = nil
	return sent
}

// shown is statement as a failure names it: OpenRead or OpenWrite by its name, since each runs to
// many lines, and any other statement quoted, to its first line.
func shown(statement string) string {
	switch statement {
	case OpenRead:
		return "OpenRead"
	case OpenWrite:
		return "OpenWrite"
	}
	if first, _, more := strings.Cut(statement, "\n"); more {
		return fmt.Sprintf("%q…", first)
	}
	return fmt.Sprintf("%q", statement)
}

// orderFailure is what is wrong with the order of sent, the statements of one or more calls on a
// connection, "" for nothing. Every statement belongs to a transaction, whose first message is
// OpenRead or OpenWrite whole, then, in a write, the read-only check (readOnlyCheck), then the
// work, at least one statement of it, and which ends with commit or rollback. A call sends at least
// one transaction.
func orderFailure(sent []string) string {
	if len(sent) == 0 {
		return "the call sent no statement"
	}
	opens := func(statement string) bool {
		return statement == OpenRead || statement == OpenWrite || strings.HasPrefix(strings.ToLower(statement), "begin")
	}
	for i := 0; i < len(sent); {
		opening := sent[i]
		if opening != OpenRead && opening != OpenWrite {
			return fmt.Sprintf("statement %d, %s, is not OpenRead or OpenWrite whole: the transaction opens another way, "+
				"or the statement runs outside one", i+1, shown(opening))
		}
		end := i + 1
		if opening == OpenWrite {
			if end >= len(sent) || sent[end] != readOnlyCheck {
				got := "nothing"
				if end < len(sent) {
					got = shown(sent[end])
				}
				return fmt.Sprintf("statement %d, after OpenWrite, is %s, want the read-only check %q", end+1, got, readOnlyCheck)
			}
			end++
		}
		work := 0
		for end < len(sent) && sent[end] != "commit" && sent[end] != "rollback" {
			if opens(sent[end]) {
				return fmt.Sprintf("statement %d, %s, opens a transaction inside another", end+1, shown(sent[end]))
			}
			end++
			work++
		}
		if end == len(sent) {
			return fmt.Sprintf("the transaction %s opened never ends with commit or rollback", shown(opening))
		}
		if work == 0 {
			return fmt.Sprintf("the transaction %s opened runs no work", shown(opening))
		}
		i = end + 1
	}
	return ""
}

// misordered is a transaction helper of the shape the package's transactions had before the
// openings, the control TestEveryTransactionPinsBeforeItsWork must catch: pgx's own begin read
// only, sent as a message of its own, then each pin as a statement of its own, then the work.
func misordered(ctx context.Context, conn *pgx.Conn) error {
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		for _, statement := range []string{"SET LOCAL search_path = pg_catalog, pg_temp", "SET LOCAL row_security = off",
			"SET LOCAL restrict_nonsystem_relation_kind = 'view, foreign-table'", "SELECT 1"} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// Every call of the package sends its statements in transactions whose first message is OpenRead
// or OpenWrite whole, one simple-protocol message that pins each setting the transaction depends
// on, then, in a write, the read-only check, and only then their work, ending with commit or
// rollback: a statement before the opening, or an opening sent as several messages, would run under
// what another client left on the connection, and a left idle_in_transaction_session_timeout could
// end the connection between two of them, which TestEveryPoolPinsItsSearchPath, reading inside the
// work, cannot see. The order is read from a pgx QueryTracer on the package's own kind of
// connection, a Conn around pgx.Connect's as ConnectURL builds one, for each exported call that
// runs a statement. The control is a helper of the shape the transactions had before the openings,
// pgx's begin read only and then each pin as a statement of its own: the check must name its begin.
func TestEveryTransactionPinsBeforeItsWork(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.PlainDatabase(t, "legion_ompsessions_order")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	traced := &recorder{}
	config.Tracer = traced
	config.DefaultQueryExecMode = execMode
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	sessions := &Conn{c: conn}
	t.Cleanup(func() { _ = sessions.Close(context.Background()) })

	live := t.Run("control: a transaction opened by begin, then a statement for each pin", func(t *testing.T) {
		traced.take()
		if err := misordered(ctx, conn); err != nil {
			t.Fatal(err)
		}
		failure := orderFailure(traced.take())
		if !strings.Contains(failure, `statement 1, "begin read only", is not OpenRead or OpenWrite whole`) {
			t.Errorf("the check found %q, want it to name the begin read only that opened the transaction", failure)
		}
		t.Logf("control: %s", failure)
	})
	if !live {
		t.Fatal("a check that misses a transaction opened another way holds nothing: the rows below would pass whatever the package sends")
	}

	content := []byte("{\"n\":1}\n")
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"CreateTables", func() error { return CreateTables(ctx, sessions) }},
		{"Import", func() error {
			_, err := Import(ctx, sessions, podSessionFile, content, time.UnixMilli(podSessionWritten))
			return err
		}},
		{"Written", func() error {
			_, _, err := Written(ctx, sessions, podSessionFile)
			return err
		}},
		{"FindByID", func() error {
			_, _, err := FindByID(ctx, sessions, podSessionID)
			return err
		}},
		{"Read", func() error {
			_, _, err := Read(ctx, sessions, podSessionFile)
			return err
		}},
		{"Replace", func() error { return Replace(ctx, sessions, claudeTranscript, Absent, content, time.Now()) }},
		{"Append", func() error { return Append(ctx, sessions, claudeTranscript, int64(len(content)), content, time.Now()) }},
	} {
		t.Run(call.name, func(t *testing.T) {
			traced.take()
			if err := call.run(); err != nil {
				t.Fatal(err)
			}
			sent := traced.take()
			if failure := orderFailure(sent); failure != "" {
				t.Errorf("%s: %s; it sent %q", call.name, failure, sent)
			}
		})
	}
}
