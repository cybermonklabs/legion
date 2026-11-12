package refs

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A rebuild and an edit of the same source race unless they are ordered: the rebuild reads a
// source's text, an editor writes new text and the edges of that new text, and the rebuild then
// replaces those edges with the ones it built from the text it read, so the edit's own index is
// lost until something re-indexes that source (stored-document finding 9 of
// dispatch://INFRA-63/artifact/ha-audit-md, dispatch://INFRA-3285).
//
// The row owns both halves of that interleaving, so the scheduler decides neither. readGatePool
// below is the hook: it reports the rebuild's own read of the comment's text, after that read has
// returned and while the rebuild's transaction is still open, and holds the rebuild there until
// this test releases it. Inside that window the test asks the database which build it is reading,
// with one statement and no clock: `for no key update nowait` on the same row answers
// 55P03 lock_not_available when the rebuild's read took that lock, and succeeds when it did not.
//
// So each build has one outcome rather than a likely one. With the lock the edit is the writer
// that waits, as a live editor does, and it lands after the rebuild's replacement, whose edges it
// then replaces: the stored edge is the edited text's. Without it the edit commits in full while
// the rebuild is held between its read and its write, and the rebuild then replaces the edit's
// edges with the ones it built from the text it read: the stored edge is the old text's, and the
// assertion below names it.
func TestARebuildBesideAnEditKeepsTheEditsReferences(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	source := resolverSource(2)
	rebuildFixture(t, database, source, "ACME-1 is the issue this comment names.")

	// A second issue for the edit to name, so the two texts have different edges.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the second issue: %v", err)
	}
	insertResolverIssue(t, tx, "ACME-2", 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the second issue: %v", err)
	}

	var once sync.Once
	didRead := make(chan struct{})
	release := make(chan struct{})
	gated := readGatePool{Pool: database.Pool, read: func(sql string) {
		if !strings.Contains(sql, "from comments where id") {
			return
		}
		once.Do(func() { close(didRead) })
		<-release
	}}

	rebuilt := make(chan error, 1)
	go func() {
		_, err := RebuildAll(ctx, gated, resolverServer)
		rebuilt <- err
	}()
	select {
	case <-didRead:
	case err := <-rebuilt:
		close(release)
		t.Fatalf("the rebuild ended before it read the comment's text: %v", err)
	}

	// Which build this is, asked of the database rather than of the clock: the rebuild's read
	// either took the row's lock or did not.
	probe, err := database.Pool.Begin(ctx)
	if err != nil {
		close(release)
		t.Fatalf("begin the edit: %v", err)
	}
	var reached int
	err = probe.QueryRow(ctx,
		`select 1 from comments where id = $1::uuid for no key update nowait`, source).Scan(&reached)
	var refused *pgconn.PgError
	locked := errors.As(err, &refused) && refused.Code == "55P03"
	if err != nil && !locked {
		_ = probe.Rollback(ctx)
		close(release)
		t.Fatalf("ask whether the rebuild holds the comment's row: %v", err)
	}

	if locked {
		// The rebuild holds the row, so the edit is a writer that waits. It waits here by
		// letting the rebuild finish, which is what the lock makes it do in a server.
		_ = probe.Rollback(ctx)
		close(release)
		if err := <-rebuilt; err != nil {
			t.Fatalf("rebuild the references: %v", err)
		}
		editBeside(t, ctx, database.Pool, source)
	} else {
		// Nothing holds the row, so the edit goes first and in full, and the rebuild writes
		// after it from the text it read before it.
		editBesideTx(t, ctx, probe, source)
		close(release)
		if err := <-rebuilt; err != nil {
			t.Fatalf("rebuild the references: %v", err)
		}
	}

	want := []Target{{"issue", "ACME-2"}}
	slices.SortFunc(want, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	if got := storedRefs(t, database.Pool, "comment", source); !reflect.DeepEqual(got, want) {
		t.Errorf("after a rebuild beside an edit the comment stored %v; want %v, the edited text's", got, want)
	}
}

// editedBody is the text the edit writes, which names a different issue from the text the
// rebuild reads.
const editedBody = "ACME-2 is the issue this comment names now."

// editBeside writes the edited text and the edges of that text in a transaction of its own, as a
// live writer does.
func editBeside(t *testing.T, ctx context.Context, pool Pool, source string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer tx.Rollback(ctx)
	editBesideTx(t, ctx, tx, source)
}

// editBesideTx is that same write inside a transaction the caller opened, and commits it.
func editBesideTx(t *testing.T, ctx context.Context, tx pgx.Tx, source string) {
	t.Helper()
	if _, err := tx.Exec(ctx,
		`update comments set body = $2 where id = $1::uuid`, source, editedBody); err != nil {
		t.Fatalf("write the edited text: %v", err)
	}
	located := NewReader(ctx, tx, "comment", source, resolverServer).Mentions(editedBody)
	if _, err := replaceRefs(ctx, tx, "comment", source, text.Refs(located), nil); err != nil {
		t.Fatalf("write the edges of the edited text: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
}

// readGatePool is a Pool that reports each read a transaction of the rebuild makes, with the
// statement it made, once that read has returned.
type readGatePool struct {
	Pool
	read func(sql string)
}

func (p readGatePool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return tx, err
	}
	return readGateTx{Tx: tx, read: p.read}, nil
}

type readGateTx struct {
	pgx.Tx
	read func(sql string)
}

func (t readGateTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return readGateRow{Row: t.Tx.QueryRow(ctx, sql, args...), sql: sql, read: t.read}
}

type readGateRow struct {
	pgx.Row
	sql  string
	read func(sql string)
}

func (r readGateRow) Scan(into ...any) error {
	err := r.Row.Scan(into...)
	r.read(r.sql)
	return err
}
