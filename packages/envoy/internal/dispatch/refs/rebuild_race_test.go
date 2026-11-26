package refs

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// Every writer of a source's text takes the source's owner lock before its first read
// (store.LockOwner): an edit of an ask, a comment or a message, and an edit of a document, which
// inserts a new version row and never writes the one this read finds. So the rebuild takes that
// lock too, before it reads (readSourceUnderOwner), and the three rows below are the three owners
// a source has: the issue of a comment, the issue of an issue's document, and the artifact of a
// project document. One row per owner, each failing for its own reason.
//
// Each row owns both halves of its interleaving, so the scheduler decides neither. readGatePool
// is the hook: it reports the rebuild's own read of the text, after that read has returned and
// while the rebuild's transaction is still open, and holds the rebuild there until the test
// releases it. Inside that window the test asks the database which build it is reading, with one
// statement and no clock: a try of the owner lock the fix takes (store.TryLockOwners) is refused
// when the rebuild holds it, and taken when it does not.
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

	gate := newReadGate("coalesce(edited_at, created_at) from comments where id")
	defer gate.releaseRebuild()
	rebuilt := gate.rebuild(t, ctx, database)

	// Which build this is, asked of the database rather than of the clock: the rebuild's read
	// of the comment either took its issue's owner lock or did not.
	owner := store.IssueOwner("ACME-1")
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate, owner)

	edit := func(tx pgx.Tx) {
		lockOwnerOrFail(t, ctx, tx, owner)
		editText(t, ctx, tx,
			`update comments set body = $2 where id = $1::uuid`, "comment", source, editedComment, prefixAuthor)
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "comment", source, []Target{{"issue", "ACME-2"}})
}

// The document path of the same race: a document edit never writes the version row the rebuild
// reads, and holds its owner's lock while it inserts the next one, the issue's here, the
// artifact's for a project document.
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

	// The owner lock is what a document writer takes, so it is what this asks about.
	owner := store.IssueOwner("ACME-1")
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate, owner)

	edit := func(tx pgx.Tx) {
		lockOwnerOrFail(t, ctx, tx, owner)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, $2)
		`, document, editedDocument); err != nil {
			t.Fatalf("write the edited document version: %v", err)
		}
		writeEdges(t, ctx, tx, "artifact", document, editedDocument, model.Actor{})
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "artifact", document, []Target{{"issue", "ACME-2"}})
}

// The same race for a PROJECT document, whose owner is the artifact itself rather than an issue:
// store.OwnerOf answers one or the other, and this is the arm the row above never reaches. A
// project document stands under no issue, so a writer of it takes the artifact's owner lock
// (lockArtifactOwner's `issue_key is null` arm), and that is the lock this probe asks about.
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

	// The artifact's owner lock is what a writer of a project document takes, so it is what this
	// asks about.
	owner := store.ArtifactOwner(document)
	probe := beginProbe(t, ctx, database.Pool, gate)
	locked := probeRefused(t, ctx, probe, gate, owner)

	edit := func(tx pgx.Tx) {
		lockOwnerOrFail(t, ctx, tx, owner)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown) values ($1, 2, $2)
		`, document, editedDocument); err != nil {
			t.Fatalf("write the edited project document version: %v", err)
		}
		writeEdges(t, ctx, tx, "artifact", document, editedDocument, model.Actor{})
	}
	gate.order(t, ctx, database.Pool, probe, locked, rebuilt, edit)

	wantRefs(t, database.Pool, "artifact", document, []Target{{"issue", "ACME-2"}})
}

// insertProjectDocument writes a document of project rather than of an issue: its artifact row
// carries no issue_key, which is what makes the artifact itself the owner a writer locks.
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
// source whose read statement holds match.
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
		_, err := RebuildAll(ctx, gated, text.NewOrigins(resolverServer))
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
		// The rebuild holds the owner, so the edit is a writer that waits. It waits here by
		// letting the rebuild finish, which is what the lock makes it do in a server.
		_ = probe.Rollback(ctx)
		g.releaseRebuild()
		if err := <-rebuilt; err != nil {
			t.Fatalf("rebuild the references: %v", err)
		}
		writeInTx(t, ctx, pool, edit)
		return
	}
	// Nothing holds the owner, so the edit goes first and in full, holding the lock the probe
	// took, and the rebuild writes after it from the text it read before it.
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
// the owner.
func beginProbe(t *testing.T, ctx context.Context, pool Pool, gate *readGate) pgx.Tx {
	t.Helper()
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	return probe
}

// probeRefused reports whether the rebuild holds owner's lock, which is the question a try of
// that lock has the server answer without waiting: refused is the lock the fix takes, and taken
// is its absence, after which the probe holds the lock the edit then writes under.
func probeRefused(t *testing.T, ctx context.Context, probe pgx.Tx, gate *readGate, owner store.Owner) bool {
	t.Helper()
	taken, err := store.TryLockOwners(ctx, probe, owner)
	if err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("ask whether the rebuild holds the owner lock an edit takes: %v", err)
	}
	return len(taken) == 0
}

// editText writes new text with one statement and then the edges of that text, as a live writer
// of an ask, a comment or a message does, for writer, the author the source's row records.
func editText(t *testing.T, ctx context.Context, tx pgx.Tx, sql, kind, id, body string, writer model.Actor) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, id, body); err != nil {
		t.Fatalf("write the edited text: %v", err)
	}
	writeEdges(t, ctx, tx, kind, id, body, writer)
}

// writeEdges stores the edges of one text, as every writer does after it writes, its short ids
// resolved for writer, the author the source records.
func writeEdges(t *testing.T, ctx context.Context, tx pgx.Tx, kind, id, body string, writer model.Actor) {
	t.Helper()
	located := NewReader(ctx, tx, writer, kind, id, resolverServer).Mentions(body)
	if _, err := replaceRefs(ctx, tx, kind, id, text.Refs(located), nil, true); err != nil {
		t.Fatalf("write the edges of the edited text: %v", err)
	}
}

// lockOwnerOrFail takes the owner lock every writer of a source takes before it writes.
func lockOwnerOrFail(t *testing.T, ctx context.Context, tx pgx.Tx, owner store.Owner) {
	t.Helper()
	if err := store.LockOwner(ctx, tx, owner); err != nil {
		t.Fatalf("lock the source's owner as a writer does: %v", err)
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
// statement it made, once that read has returned: a QueryRow's once it is scanned, and each
// statement of a batch once the batch is closed, which is how the text is read after the owner
// lock (store.LockOwnerAndRead).
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

func (t readGateTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	sqls := make([]string, len(b.QueuedQueries))
	for index, queued := range b.QueuedQueries {
		sqls[index] = queued.SQL
	}
	return readGateBatch{BatchResults: t.Tx.SendBatch(ctx, b), sqls: sqls, read: t.read}
}

type readGateBatch struct {
	pgx.BatchResults
	sqls []string
	read func(sql string)
}

func (b readGateBatch) Close() error {
	err := b.BatchResults.Close()
	for _, sql := range b.sqls {
		b.read(sql)
	}
	return err
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
