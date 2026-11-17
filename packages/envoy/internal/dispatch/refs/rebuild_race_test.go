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

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A rebuild and an edit of the same source race unless they are ordered: the rebuild reads a
// source's text, an editor writes new text and the edges of that new text, and the rebuild then
// replaces those edges with the ones it built from the text it read, so the edit's own index is
// lost until something re-indexes that source (stored-document finding 9 of
// dispatch://INFRA-63/artifact/ha-audit-md, dispatch://INFRA-3285).
//
// The two rows below are the two kinds of writer, and they are two rows rather than one because
// the mechanism differs: an edit of an ask, a comment or a message updates the row its text is
// in, so a read of that row `for no key update` conflicts with it, while an edit of a document
// inserts a new version row and takes its OWNER row instead, so a lock on the version row this
// read finds orders nothing at all. One row per mechanism, each failing for its own reason.
//
// Each row owns both halves of its interleaving, so the scheduler decides neither. readGatePool
// is the hook: it reports the rebuild's own read of the text, after that read has returned and
// while the rebuild's transaction is still open, and holds the rebuild there until the test
// releases it. Inside that window the test asks the database which build it is reading, with one
// statement and no clock: `for no key update nowait` on the row the fix locks answers
// 55P03 lock_not_available when the rebuild holds it, and succeeds when it does not.
//
// So each build has one outcome rather than a likely one. With the lock the edit is the writer
// that waits, as a live editor does, and it lands after the rebuild's replacement, whose edges it
// then replaces: the stored edge is the edited text's. Without it the edit commits in full while
// the rebuild is held between its read and its write, and the rebuild then replaces the edit's
// edges with the ones it built from the text it read: the stored edge is the old text's, and each
// assertion below names it.
func TestARebuildBesideAnEditKeepsTheEditsReferences(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	source := resolverSource(2)
	rebuildFixture(t, database, source, "ACME-1 is the issue this comment names.")
	writeInTx(t, ctx, database.Pool, func(tx pgx.Tx) {
		insertResolverIssue(t, tx, "ACME-2", 2)
	})

	gate := newReadGate("from comments where id")
	defer gate.releaseRebuild()
	rebuilt := gate.rebuild(t, ctx, database)

	// Which build this is, asked of the database rather than of the clock: the rebuild's read
	// of the comment either took that row's lock or did not.
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate,
		`select true from comments where id = $1::uuid for no key update nowait`, source)

	edit := func(tx pgx.Tx) {
		editText(t, ctx, tx,
			`update comments set body = $2 where id = $1::uuid`, "comment", source, editedComment)
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "comment", source, []Target{{"issue", "ACME-2"}})
}

// The document path of the same race, which the lock above does not reach: a document edit never
// writes the version row the rebuild reads, so the row the rebuild must take is the one that
// edit takes, its owner row - the issue row here, the artifact row for a project document.
func TestARebuildBesideADocumentEditKeepsTheEditsReferences(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var document string
	writeInTx(t, ctx, database.Pool, func(tx pgx.Tx) {
		createReferenceProject(t, tx, "ACME")
		insertResolverIssue(t, tx, "ACME-1", 1)
		insertResolverIssue(t, tx, "ACME-2", 2)
		document = insertScopeDocument(t, tx, "ACME-1", "spec", true)
		insertScopeVersion(t, tx, document, 1, "ACME-1 is the issue this document names.")
	})

	gate := newReadGate("from artifact_versions")
	defer gate.releaseRebuild()
	rebuilt := gate.rebuild(t, ctx, database)

	// The owner row is what a document writer takes, so it is what this asks about.
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate,
		`select true from issues where key = $1 for no key update nowait`, "ACME-1")

	edit := func(tx pgx.Tx) {
		lockDocumentOwnerOrFail(t, ctx, tx, document)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, $2)
		`, document, editedDocument); err != nil {
			t.Fatalf("write the edited document version: %v", err)
		}
		writeEdges(t, ctx, tx, "artifact", document, editedDocument)
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "artifact", document, []Target{{"issue", "ACME-2"}})
}

// The same race for a PROJECT document, whose owner is its own artifact row rather than an issue:
// lockDocumentOwner takes one row or the other, and this is the branch the row above never
// reaches. A project document stands under no issue, so a writer of it takes the artifact row
// (lockArtifactOwner's `issue_key is null` arm), and that is the row this probe asks about.
func TestARebuildBesideAProjectDocumentEditKeepsTheEditsReferences(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var document string
	writeInTx(t, ctx, database.Pool, func(tx pgx.Tx) {
		createReferenceProject(t, tx, "ACME")
		insertResolverIssue(t, tx, "ACME-1", 1)
		insertResolverIssue(t, tx, "ACME-2", 2)
		document = insertProjectDocument(t, tx, "ACME", "handbook")
		insertScopeVersion(t, tx, document, 1, "ACME-1 is the issue this project document names.")
	})

	gate := newReadGate("from artifact_versions")
	defer gate.releaseRebuild()
	rebuilt := gate.rebuild(t, ctx, database)

	// The artifact row is what a writer of a project document takes, so it is what this asks about.
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate,
		`select true from artifacts where id = $1::uuid for no key update nowait`, document)

	edit := func(tx pgx.Tx) {
		lockDocumentOwnerOrFail(t, ctx, tx, document)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, $2)
		`, document, editedDocument); err != nil {
			t.Fatalf("write the edited project document version: %v", err)
		}
		writeEdges(t, ctx, tx, "artifact", document, editedDocument)
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "artifact", document, []Target{{"issue", "ACME-2"}})
}

// insertProjectDocument writes a document of project rather than of an issue: its artifact row
// carries no issue_key, which is what makes its own row the owner a writer locks.
func insertProjectDocument(t *testing.T, tx pgx.Tx, project, slug string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ($1, $2, $2, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`, project, slug).Scan(&id); err != nil {
		t.Fatalf("insert project document %s: %v", slug, err)
	}
	return id
}

// The texts the edits write, each naming a different issue from the text the rebuild reads.
const (
	editedComment  = "ACME-2 is the issue this comment names now."
	editedDocument = "ACME-2 is the issue this document names now."
)

// readGate holds a rebuild inside its transaction, once that rebuild has read the text of the
// source whose read statement names match.
type readGate struct {
	match   string
	once    sync.Once
	read    chan struct{}
	release chan struct{}
	freed   sync.Once
}

func newReadGate(match string) *readGate {
	return &readGate{match: match, read: make(chan struct{}), release: make(chan struct{})}
}

// releaseRebuild lets a held rebuild run on. Every exit of a row runs it, a t.Fatal's included,
// so no rebuild is left holding a connection the pool's own close would then wait for.
func (g *readGate) releaseRebuild() { g.freed.Do(func() { close(g.release) }) }

// rebuild starts one rebuild through the gate and waits until it has read the gated text.
func (g *readGate) rebuild(t *testing.T, ctx context.Context, database *store.Store) chan error {
	t.Helper()
	gated := readGatePool{Pool: database.Pool, read: func(sql string) {
		if !strings.Contains(sql, g.match) {
			return
		}
		g.once.Do(func() { close(g.read) })
		<-g.release
	}}
	rebuilt := make(chan error, 1)
	go func() {
		_, err := RebuildAll(ctx, gated, resolverServer)
		rebuilt <- err
	}()
	select {
	case <-g.read:
	case err := <-rebuilt:
		t.Fatalf("the rebuild ended before it read the text this row edits: %v", err)
	}
	return rebuilt
}

// order runs the edit on whichever side of the rebuild the lock puts it, and waits for both.
func (g *readGate) order(t *testing.T, ctx context.Context, pool Pool, probe pgx.Tx, locked bool, rebuilt chan error, edit func(pgx.Tx)) {
	t.Helper()
	if locked {
		// The rebuild holds the row, so the edit is a writer that waits. It waits here by
		// letting the rebuild finish, which is what the lock makes it do in a server.
		_ = probe.Rollback(ctx)
		g.releaseRebuild()
		if err := <-rebuilt; err != nil {
			t.Fatalf("rebuild the references: %v", err)
		}
		writeInTx(t, ctx, pool, edit)
		return
	}
	// Nothing holds the row, so the edit goes first and in full, and the rebuild writes after
	// it from the text it read before it.
	edit(probe)
	if err := probe.Commit(ctx); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	g.releaseRebuild()
	if err := <-rebuilt; err != nil {
		t.Fatalf("rebuild the references: %v", err)
	}
}

// beginProbe opens the transaction the edit runs in, which first asks whether the rebuild holds
// the row.
func beginProbe(t *testing.T, ctx context.Context, pool Pool, gate *readGate) pgx.Tx {
	t.Helper()
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	return probe
}

// probeRefused reports whether the rebuild holds the row this statement asks for, which is the
// question `nowait` has the server answer: 55P03 is the lock the fix takes, and no error is its
// absence.
func probeRefused(t *testing.T, ctx context.Context, probe pgx.Tx, gate *readGate, sql string, arg any) bool {
	t.Helper()
	err := probe.QueryRow(ctx, sql, arg).Scan(new(bool))
	var refused *pgconn.PgError
	if errors.As(err, &refused) && refused.Code == "55P03" {
		return true
	}
	if err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("ask whether the rebuild holds the row an edit takes: %v", err)
	}
	return false
}

// editText writes new text with one statement and then the edges of that text, as a live writer
// of an ask, a comment or a message does.
func editText(t *testing.T, ctx context.Context, tx pgx.Tx, sql, kind, id, body string) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, id, body); err != nil {
		t.Fatalf("write the edited text: %v", err)
	}
	writeEdges(t, ctx, tx, kind, id, body)
}

// writeEdges stores the edges of one text, as every writer does after it writes.
func writeEdges(t *testing.T, ctx context.Context, tx pgx.Tx, kind, id, body string) {
	t.Helper()
	located := NewReader(ctx, tx, kind, id, resolverServer).Mentions(body)
	if _, err := replaceRefs(ctx, tx, kind, id, text.Refs(located), nil); err != nil {
		t.Fatalf("write the edges of the edited text: %v", err)
	}
}

// lockDocumentOwnerOrFail takes the owner row a document writer takes before it writes.
func lockDocumentOwnerOrFail(t *testing.T, ctx context.Context, tx pgx.Tx, document string) {
	t.Helper()
	if err := lockDocumentOwner(ctx, tx, document); err != nil {
		t.Fatalf("lock the document's owner as a writer does: %v", err)
	}
}

// writeInTx runs one write in a transaction of its own and commits it.
func writeInTx(t *testing.T, ctx context.Context, pool Pool, write func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer tx.Rollback(ctx)
	write(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
}

// wantRefs reads the edges one source stores and reports them against the ones the edited text
// writes.
func wantRefs(t *testing.T, q Queryer, kind, id string, want []Target) {
	t.Helper()
	slices.SortFunc(want, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	if got := storedRefs(t, q, kind, id); !reflect.DeepEqual(got, want) {
		t.Errorf("after a rebuild beside an edit the %s stored %v; want %v, the edited text's", kind, got, want)
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
