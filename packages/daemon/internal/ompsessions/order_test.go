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

// orderFailure is what is wrong with the order of sent, the statements of one or more calls on a
// connection, "" for nothing. Every statement belongs to a transaction, which opens with begin,
// then pinSearchPath, then, in a read-only transaction, refuseRowSecurity, then
// restrictRelationKinds, then the work, at least one statement of it, and ends with commit or
// rollback. A call sends at least one transaction.
func orderFailure(sent []string) string {
	if len(sent) == 0 {
		return "the call sent no statement"
	}
	for i := 0; i < len(sent); {
		begin := strings.ToLower(sent[i])
		if !strings.HasPrefix(begin, "begin") {
			return fmt.Sprintf("statement %d, %q, runs outside a transaction: want begin first", i+1, sent[i])
		}
		want := []string{pinSearchPath}
		if strings.Contains(begin, "read only") {
			want = append(want, refuseRowSecurity)
		}
		want = append(want, restrictRelationKinds)
		for j, statement := range want {
			at := i + 1 + j
			if at >= len(sent) || sent[at] != statement {
				got := "nothing"
				if at < len(sent) {
					got = fmt.Sprintf("%q", sent[at])
				}
				return fmt.Sprintf("statement %d after %q is %s, want %q", at+1, sent[i], got, statement)
			}
		}
		end := i + 1 + len(want)
		work := 0
		for end < len(sent) && sent[end] != "commit" && sent[end] != "rollback" {
			if strings.HasPrefix(strings.ToLower(sent[end]), "begin") {
				return fmt.Sprintf("statement %d, %q, opens a transaction inside another", end+1, sent[end])
			}
			end++
			work++
		}
		if end == len(sent) {
			return fmt.Sprintf("the transaction %q opened never ends with commit or rollback", sent[i])
		}
		if work == 0 {
			return fmt.Sprintf("the transaction %q opened runs no work", sent[i])
		}
		i = end + 1
	}
	return ""
}

// misordered is a transaction helper that runs a statement before the pin, the control
// TestEveryTransactionPinsBeforeItsWork must catch.
func misordered(ctx context.Context, conn *pgx.Conn) error {
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		for _, statement := range []string{"SELECT 1", pinSearchPath, refuseRowSecurity, restrictRelationKinds, "SELECT 2"} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// Every call of the package sends its statements in transactions that open, in order, with
// begin, the search_path pin, row_security off in a read-only one, and the restrict setting, and
// only then run their work, ending with commit or rollback: a statement before the pin would run
// under the login's own search_path, which TestEveryPoolPinsItsSearchPath, reading inside the
// work, cannot see. The order is read from a pgx QueryTracer on the package's own kind of
// connection, a Conn around pgx.Connect's as ConnectURL builds one, for each exported call that
// runs a statement. The control is a helper that runs a statement before the pin: the check must
// name it.
func TestEveryTransactionPinsBeforeItsWork(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.Database(t, "legion_ompsessions_order")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	traced := &recorder{}
	config.Tracer = traced
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	sessions := &Conn{c: conn}

	live := t.Run("control: a statement before the pin", func(t *testing.T) {
		traced.take()
		if err := misordered(ctx, conn); err != nil {
			t.Fatal(err)
		}
		failure := orderFailure(traced.take())
		if !strings.Contains(failure, `is "SELECT 1", want "`+pinSearchPath+`"`) {
			t.Errorf("the check found %q, want it to name SELECT 1 run before the pin", failure)
		}
		t.Logf("control: %s", failure)
	})
	if !live {
		t.Fatal("a check that misses a statement before the pin holds nothing: the rows below would pass whatever the package sends")
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
