package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One renderer answers every read of a document's version. A version the writer rendered from the
// tree holds a version_renders row, written in the transaction that writes the version's tree
// (QueueVersionRender), and renders again from the tree's tuples as of the row's time; every other
// version - each one written before the tree was rendered from at all among them - answers the
// markdown it stores, byte for byte, and says so. A version whose markdown is null holds no bytes,
// and says that rather than answering an empty document. The boundary is the row and not a clock:
// no tuple, whatever it is dated, moves a version that holds no row off its stored bytes.

// RenderSource is what a Rendering's bytes are, as the version routes name it in their
// Dispatch-Render-Source header.
type RenderSource string

const (
	// RenderedFromTree is a render of the tree's tuples as of a time.
	RenderedFromTree RenderSource = "tree"
	// StoredBytes is a version's stored markdown, byte for byte.
	StoredBytes RenderSource = "stored"
	// NoBytes is a version that stores no markdown: its Markdown is empty and is no document.
	NoBytes RenderSource = "none"
)

// Rendering is a document's markdown as one read answers it, and what those bytes are.
type Rendering struct {
	Markdown string
	Source   RenderSource
}

// RenderAt renders the document artifactID from the tree's tuples as of at, in the text order its
// author wrote: the head where at is now, and a version as of the time
// its version_renders row records (RenderVersion). It reads the document's members, each block
// whose newest in_document dated at or before at names the document (refs.TreeMembers), and of
// each its newest text value, state, text_parent and text_rank dated at or before at, with every
// retraction dated before it applied (refs.TreeFactsOf at a render's cut, refs.TreeCut's Dated). It
// leaves out a member whose state then is gone, and one that holds no text value, no text_parent or
// no text_rank then. It prints the document's text children, the members whose text_parent names
// the document's node, in increasing text_rank, compared byte for byte (a tie, which no pass
// writes, by the lower address), each by its text value: its whole slice of the markdown the
// version was written from, the bytes of its own text children included, so every member prints
// once, and two members of one text each print at their own place. It reads no part_of tuple and
// no rank value, so a node's parent and its place among its siblings change no byte of it. The
// text children's renderings are joined by one blank line and the last line ends; a document that
// holds no text child renders as no bytes at all.
//
// who is the reader the render answers; the render leaves out no node for a reader, so every
// reader reads one rendering.
func RenderAt(ctx context.Context, q Queryer, who model.Actor, artifactID string, at time.Time) (Rendering, error) {
	var refKey string
	if err := q.QueryRow(ctx, `select ref_key from artifacts where id = $1`, artifactID).Scan(&refKey); err != nil {
		return Rendering{}, fmt.Errorf("read the document %s to render: %w", artifactID, err)
	}
	cut := refs.TreeCut{At: at, Dated: true}
	members, err := refs.TreeMembers(ctx, q, refKey, cut)
	if err != nil {
		return Rendering{}, err
	}
	facts, err := refs.TreeFactsOf(ctx, q, members, cut)
	if err != nil {
		return Rendering{}, fmt.Errorf("read the blocks of %s to render: %w", refKey, err)
	}
	type textChild struct{ rank, address, text string }
	children := make([]textChild, 0, len(members))
	for _, member := range members {
		held := facts[member]
		text, written := held.Values[graphstore.AttributeText]
		rank, ranked := held.Values[refs.AttributeTextRank]
		if !written || !ranked || held.Values[graphstore.AttributeState] == graphstore.StateGone {
			continue
		}
		if slices.ContainsFunc(held.Relations, func(relation refs.TreeRelation) bool {
			return relation.Kind == refs.TupleTextParent && relation.To.Kind == graphstore.KindArtifact && relation.To.ID == refKey
		}) {
			children = append(children, textChild{rank: rank, address: member.ID, text: text})
		}
	}
	if len(children) == 0 {
		return Rendering{Source: RenderedFromTree}, nil
	}
	slices.SortFunc(children, func(a, b textChild) int {
		return cmp.Or(strings.Compare(a.rank, b.rank), strings.Compare(a.address, b.address))
	})
	length := 2*len(children) - 1
	for _, child := range children {
		length += len(child.text)
	}
	var rendered strings.Builder
	rendered.Grow(length)
	for index, child := range children {
		if index > 0 {
			rendered.WriteString("\n\n")
		}
		rendered.WriteString(child.text)
	}
	rendered.WriteByte('\n')
	return Rendering{Markdown: rendered.String(), Source: RenderedFromTree}, nil
}

// versionRenderStatement is what a read of version $2 of document $1 needs: the time its
// version_renders row renders it as of, null where it holds none, and only then its stored
// markdown and whether that is null, so a version that renders from the tree reads no stored byte.
const versionRenderStatement = `
	select r.at, r.at is null and v.markdown is null, case when r.at is null then v.markdown end
	from artifact_versions v
	left join version_renders r on r.artifact_id = v.artifact_id and r.number = v.number
	where v.artifact_id = $1 and v.number = $2`

// RenderVersion is version number of the document artifactID as every read of it answers it: with
// a version_renders row, RenderAt as of the row's time; without one, the version's stored markdown
// byte for byte as StoredBytes, and NoBytes, with an empty Markdown, where that markdown is null. A
// version the document does not hold is pgx.ErrNoRows.
func RenderVersion(ctx context.Context, q Queryer, who model.Actor, artifactID string, number int) (Rendering, error) {
	var at *time.Time
	var empty bool
	var markdown *string
	if err := q.QueryRow(ctx, versionRenderStatement, artifactID, number).Scan(&at, &empty, &markdown); err != nil {
		return Rendering{}, fmt.Errorf("read version %d of %s: %w", number, artifactID, err)
	}
	switch {
	case at != nil:
		return RenderAt(ctx, q, who, artifactID, *at)
	case empty:
		return Rendering{Source: NoBytes}, nil
	}
	return Rendering{Markdown: *markdown, Source: StoredBytes}, nil
}

// errVersionRenderCut refuses a version_renders row of a record read at a render's cut, which
// names no seq the record read.
var errVersionRenderCut = errors.New("docs: a version's record is read at a cut of the tuple log's seq, not at a render's time")

// QueueVersionRender queues on b, the write phase of the transaction that writes version number of
// the document artifactID's tree, that version's version_renders row: the digest of record, the
// record of the document's query the version was written with, which artifact_version_text_sha256
// then answers for the version and an approval of it pins; the time the version renders as of,
// the time of the record's cut, which is no earlier than any tuple the transaction dated before it
// read the cut (refs.HeadCut); and the highest graph_tuples.seq the record read. The row commits
// with the version or not at all. It writes a table of its own and no tuple of the store. A version
// that holds its row keeps it: the first pass that states a version records it, and a later pass
// that states the version again - a room pass stating each fragment's newest version, the
// reconciliation stating a version ReviseQueries wrote - records nothing.
func QueueVersionRender(b *store.WriteBatch, artifactID string, number int, record QueryRecord) error {
	if record.Cut.Dated || record.Cut.At.IsZero() {
		return errVersionRenderCut
	}
	b.Queue("write the version_renders row of version "+fmt.Sprint(number), `
		insert into version_renders (artifact_id, number, record_sha256, at, read_seq) values ($1, $2, $3, $4, $5)
		on conflict (artifact_id, number) do nothing`,
		artifactID, number, record.Digest, record.Cut.At, record.Cut.ReadSeq)
	return nil
}

// queryOfBand is the query whose record can cover the versions of an issue's document of each band
// (refs.DocumentBandSQL, the one statement of which document plays which part): the spec, the
// Verification document and the plan. A document of any other band - the issue room's own
// fragment, an upload, a project's document - is covered by no query's record, and its versions
// take no version_renders row: they answer their stored bytes. Of a document of one of these
// bands, a query's record covers only a query version this build's version write made
// (madeQueryVersion); every other version answers its stored bytes too.
var queryOfBand = map[string]string{
	string(refs.RankBandSpec):         graphstore.QuerySpec,
	string(refs.RankBandVerification): graphstore.QueryVerification,
	string(refs.RankBandPlan):         graphstore.QueryPlan,
}

// statedVersion is a version a pass of a transaction stated (indexRenderedVersion): its document
// and its number.
type statedVersion struct {
	artifactID string
	number     int
}

// recordStatedVersions records on the ledger that runs tx's write phase each version of versions a
// pass of tx stated. That ledger writes the version_renders row of each one a query's record
// covers (readCovered) once its write phase has run (Ledger.writeStatedRenders). A version a pass
// states in a transaction no ledger of ctx knows takes no row and answers its stored bytes.
func recordStatedVersions(ctx context.Context, tx pgx.Tx, versions refs.Versions) {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer != tx && ledger.tx != tx {
		return
	}
	for _, artifactID := range slices.Sorted(maps.Keys(versions)) {
		if number := versions[artifactID].Number; number > 0 {
			ledger.stated = append(ledger.stated, statedVersion{artifactID: artifactID, number: number})
		}
	}
}

// statedDocumentsStatement reads, for each version the arrays $1 and $2 name pair by pair - a
// stored document and a number - what says whether a query's record covers it (readCovered): the
// document's issue and band (refs.DocumentBandSQL, queryOfBand); whether the store holds the
// version's node present, at the address graphstore.VersionID gives it and at any state but gone,
// since a node held gone is no present node (graphcensus reads a row at its address as a later row
// at a freed address), and whether that node holds definition_version; whether the store holds
// the version's row; and what the version's node takes of its document
// (graphstore.VersionOwnerColumns), as ReadVersionOwner reads it.
var statedDocumentsStatement = `
	select s.id::text, s.number, coalesce(d.issue_key, ''), ` + refs.DocumentBandSQL("d") + `,
	       n.id is not null, coalesce(n.attributes ? '` + graphstore.AttributeDefinitionVersion + `', false),
	       exists (select 1 from artifact_versions v where v.artifact_id = s.id and v.number = s.number),
	       ` + graphstore.VersionOwnerColumns("s.id") + `
	from unnest($1::uuid[], $2::int[]) as s (id, number)
	join artifacts d on d.id = s.id
	left join graph_nodes n on n.kind = '` + graphstore.KindVersion + `' and n.id = d.ref_key || '@v' || s.number
		and n.state <> '` + graphstore.StateGone + `'`

// queryOf is the issue and the query whose record covers a version.
type queryOf struct{ issue, query string }

// madeQueryVersion reports whether a version a pass stated is a query version this build's version
// write made, whose node holds definition_version once the transaction's write phase has run: the
// one kind of version a query's record covers. Where the store holds the version's node present
// (held), that node holds definition_version (defined). Where it holds none and this transaction
// queued the version's insert (queued), the node that write queues holds it, as
// graphstore.VersionFacts writes it of the version's owner, read as ReadVersionOwner reads it: a
// query node naming a definition version, at an address inside the store's bound. A version with
// no node that this transaction did not write - one a process of the release before wrote, whose
// node no pass has written yet - is none: the pass that states it may write its node in the same
// transaction (graphcensus.PlanMissingVersionNodes), with no definition_version, and it renders
// from its stored markdown. So is a version written before its document converted, whose node
// holds no definition_version.
func madeQueryVersion(held, defined, queued bool, owner graphstore.VersionOwner, number int) bool {
	switch {
	case held:
		return defined
	case queued:
		return owner.Query && owner.DefinitionVersion != 0 && graphstore.AddressFits(graphstore.VersionID(owner.RefKey, number))
	}
	return false
}

// readCovered reads, through tx, which query's record covers each of versions, versions a pass of
// the ledger's transaction stated (recordStatedVersions), before that transaction's write phase
// sends its first batch: its document's band maps to a query (queryOfBand) and the document has an
// issue, and it is a query version this build's version write made (madeQueryVersion). A document
// the transaction creates is read off the address it was registered under
// (RegisterPendingArtifact), whose row no statement reads before the batch writes it: the store
// holds no version of it and no node, so each of its versions is one the transaction queued, whose
// owner is a query node where the node the transaction writes of the document is one
// (pendingQuery), as ReadVersionOwner reads it. Every other document's versions are read in one
// statement (statedDocumentsStatement), which reads a version this transaction queued as one whose
// row the store does not hold yet; none runs where versions names no stored document. The band it
// reads is the one a read after the batch would: the columns a band is decided by - the issue, the
// kind, is_primary, the role and the slug - are written with the row, and the one write of one of
// them a write phase makes later, an upload's repair of a role left null (api's storeArtifact),
// writes the role the row's slug already gives it (refs.DocumentBandSQL). The owner it reads of a
// version the transaction queued is the one the store holds before the batch, which is the one
// that version's writer read in the read phase (ReadVersionOwner), and so the one its node takes.
func (l *Ledger) readCovered(ctx context.Context, tx pgx.Tx, versions []statedVersion) (map[statedVersion]queryOf, error) {
	covered := map[statedVersion]queryOf{}
	var ids []string
	var numbers []int
	for _, version := range versions {
		address, pending := l.PendingDocument(version.artifactID)
		if !pending {
			ids, numbers = append(ids, version.artifactID), append(numbers, version.number)
			continue
		}
		owner := graphstore.VersionOwner{RefKey: address.RefKey}
		if l.pendingQuery(version.artifactID) {
			owner.Query, owner.DefinitionVersion = true, graphstore.QueryDefinitionVersion
		}
		if of := (queryOf{issue: address.IssueKey, query: queryOfBand[string(address.Band)]}); of.issue != "" && of.query != "" &&
			madeQueryVersion(false, false, true, owner, version.number) {
			covered[version] = of
		}
	}
	if len(ids) == 0 {
		return covered, nil
	}
	rows, err := tx.Query(ctx, statedDocumentsStatement, ids, numbers)
	if err != nil {
		return nil, fmt.Errorf("read which query's record covers the versions the passes stated: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version statedVersion
		var of queryOf
		var band string
		var held, defined, stored bool
		var owner graphstore.VersionOwner
		if err := rows.Scan(append([]any{&version.artifactID, &version.number, &of.issue, &band, &held, &defined, &stored}, owner.Targets()...)...); err != nil {
			return nil, fmt.Errorf("read which query's record covers the versions the passes stated: %w", err)
		}
		if of.query = queryOfBand[band]; of.issue != "" && of.query != "" && madeQueryVersion(held, defined, !stored, owner, version.number) {
			covered[version] = of
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which query's record covers the versions the passes stated: %w", err)
	}
	return covered, nil
}

// readStatedDocuments reads which query's record covers each version a pass of the ledger's
// transaction stated (recordStatedVersions, readCovered), into covered, which the fill reads
// (statedRenders). It runs once the queue is sealed and before the write phase's first batch is
// sent (runQueue), so the read holds no transaction id the batch takes, and a version the
// transaction queued is not stored yet. A transaction whose passes stated no version of a stored
// document runs no statement.
func (l *Ledger) readStatedDocuments(ctx context.Context) error {
	if len(l.stated) == 0 {
		return nil
	}
	tx := l.writer
	if tx == nil {
		tx = l.tx
	}
	covered, err := l.readCovered(ctx, tx, l.stated)
	if err != nil {
		return err
	}
	l.covered = covered
	return nil
}

// cutRecords builds the records of issues' queries at one cut, walking each issue's tree once
// (WalkIssueTree) and building each query's record once (IssueTree.QueryRecord).
type cutRecords struct {
	cut     refs.TreeCut
	trees   map[string]IssueTree
	records map[queryOf]QueryRecord
}

// record is the record of of's query of of's issue at the cut, read through q.
func (c *cutRecords) record(ctx context.Context, q Queryer, of queryOf) (QueryRecord, error) {
	if record, read := c.records[of]; read {
		return record, nil
	}
	tree, walked := c.trees[of.issue]
	if !walked {
		var err error
		if tree, err = WalkIssueTree(ctx, q, of.issue, c.cut); err != nil {
			return QueryRecord{}, err
		}
		if c.trees == nil {
			c.trees = map[string]IssueTree{}
		}
		c.trees[of.issue] = tree
	}
	record, err := tree.QueryRecord(ctx, q, of.query)
	if err != nil {
		return QueryRecord{}, err
	}
	if c.records == nil {
		c.records = map[queryOf]QueryRecord{}
	}
	c.records[of] = record
	return record, nil
}

// writeStatedRenders writes, in the ledger's transaction, the version_renders row of each version
// a pass of it stated (recordStatedVersions) that a query's record covers - a query version this
// build's version write made, of a document of a band a query covers (readCovered, read before the
// write phase's first batch, readStatedDocuments): the digest of that query's record, and the time
// and the seq of the cut it was read at, one cut for the whole transaction, read once its write
// phase has run (refs.HeadCut), so the record and the render as of the row's time hold every tuple
// the passes appended under their events. It runs after the write phase's batch and before the
// commit (runQueue): the record is built in Go from the store as that batch leaves it
// (IssueTree.QueryRecord), which no statement of the batch can read. Where no query's record
// covers a stated version, it runs no statement, and the version answers its stored bytes.
//
// A version a restore carry pins (carryRestored) holds its row already: the digest the approval it
// carries pinned, which the insert trigger of the carry's review took from it. Its record is read
// here as every other one is, by the fill's own walk (cutRecords), and never from that row or from
// artifact_version_text_sha256, both of which answer the pin in this transaction. Where every pin is
// its version's record's digest, the pinned rows take the cut's time and seq and keep their digest
// (queuePinnedRenderCut), and every other row is written as above. Where one is not - or the fill
// states no row of a pinned version at all - it writes no row and answers each restore carry it
// missed, in the order the carries ran, for the write phase to drop (writeRestored). The stated
// versions stay recorded, so the fill of the round after a drop states the same versions again.
func (l *Ledger) writeStatedRenders(ctx context.Context) ([]*restoredCarry, error) {
	pins := map[statedVersion]*restoredCarry{}
	for _, restore := range l.restores {
		if !restore.dropped {
			pins[restore.version] = restore
		}
	}
	tx := l.writer
	if tx == nil {
		tx = l.tx
	}
	renders, err := l.statedRenders(ctx, tx)
	if err != nil {
		return nil, err
	}
	held := map[statedVersion]bool{}
	for _, render := range renders {
		if restore, pinned := pins[render.version]; pinned {
			held[render.version] = render.record.Digest == restore.pin
		}
	}
	var missed []*restoredCarry
	for _, restore := range l.restores {
		if pins[restore.version] == restore && !held[restore.version] {
			missed = append(missed, restore)
		}
	}
	if len(missed) > 0 || len(renders) == 0 {
		return missed, nil
	}
	return nil, store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		for _, render := range renders {
			if _, pinned := pins[render.version]; pinned {
				if err := queuePinnedRenderCut(b, render.version, render.record); err != nil {
					return err
				}
				continue
			}
			if err := QueueVersionRender(b, render.version.artifactID, render.version.number, render.record); err != nil {
				return err
			}
		}
		return nil
	})
}

// statedRender is a version a pass of the transaction stated that a query's record covers, with
// that record as the fill read it.
type statedRender struct {
	version statedVersion
	record  QueryRecord
}

// statedRenders reads, through tx, the record of each version a pass of the ledger's transaction
// stated that a query's record covers (covered, readStatedDocuments), once a version, in the order
// the passes stated them: every record at one cut, each issue's tree walked once (cutRecords). It
// reads no version_renders row, and nothing where no stated version is covered.
func (l *Ledger) statedRenders(ctx context.Context, tx pgx.Tx) ([]statedRender, error) {
	if len(l.covered) == 0 {
		return nil, nil
	}
	cut, err := refs.HeadCut(ctx, tx)
	if err != nil {
		return nil, err
	}
	records := cutRecords{cut: cut}
	var renders []statedRender
	written := map[statedVersion]bool{}
	for _, version := range l.stated {
		of, held := l.covered[version]
		if !held || written[version] {
			continue
		}
		written[version] = true
		record, err := records.record(ctx, tx, of)
		if err != nil {
			return nil, err
		}
		renders = append(renders, statedRender{version: version, record: record})
	}
	return renders, nil
}

// pinVersionRender is the step that writes the version_renders row of version, which a restore
// carry pins (carryRestored), before the carry's review is inserted: the digest pin, the approval's
// it carries, which artifact_version_text_sha256 then answers for the version and the review's
// insert trigger pins (migration 20261105084017), with the transaction's start, at, and seq 0 for
// the time and the seq of a cut no record has been read at yet, which the fill sets
// (queuePinnedRenderCut) before anything reads the row as a version's record.
func pinVersionRender(version statedVersion, pin string, at time.Time) WriteStep {
	return execStep("pin the version_renders row of version "+fmt.Sprint(version.number), `
		insert into version_renders (artifact_id, number, record_sha256, at, read_seq) values ($1, $2, $3, $4, 0)`,
		version.artifactID, version.number, pin, at)
}

// errPinnedRenderGone refuses the fill of a pinned version whose row is not there to take its cut.
var errPinnedRenderGone = errors.New("docs: the version_renders row a restore carry pinned is not there")

// queuePinnedRenderCut queues on b the time and the seq of record's cut on the version_renders row
// version holds with the digest a restore carry pinned (pinVersionRender), which the fill found to
// be record's digest (writeStatedRenders): the row then holds what QueueVersionRender would have
// written, and its digest stays the one the carried review pinned.
func queuePinnedRenderCut(b *store.WriteBatch, version statedVersion, record QueryRecord) error {
	if record.Cut.Dated || record.Cut.At.IsZero() {
		return errVersionRenderCut
	}
	b.Queue("date the pinned version_renders row of version "+fmt.Sprint(version.number), `
		update version_renders set at = $3, read_seq = $4
		where artifact_id = $1 and number = $2 and record_sha256 = $5`,
		version.artifactID, version.number, record.Cut.At, record.Cut.ReadSeq, record.Digest).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("date version %d of %s: %w", version.number, version.artifactID, errPinnedRenderGone)
		}
		return nil
	})
	return nil
}

// rendersInTransaction reports whether version number of artifactID takes its version_renders row
// at the end of the first part of tx's write phase (writeStatedRenders): a pass of tx stated it
// (recordStatedVersions) and a query's record covers it (readCovered), read here, in tx's read
// phase, by the same predicate the fill's read takes before the write phase's first batch, so the
// carry's place and the row agree. A version tx queued is not stored yet in either read.
func rendersInTransaction(ctx context.Context, tx pgx.Tx, artifactID string, number int) (bool, error) {
	ledger := queueFor(ctx, tx)
	version := statedVersion{artifactID: artifactID, number: number}
	if ledger == nil || !slices.Contains(ledger.stated, version) {
		return false, nil
	}
	covered, err := ledger.readCovered(ctx, tx, []statedVersion{version})
	if err != nil {
		return false, err
	}
	_, held := covered[version]
	return held, nil
}

// carryAfterRender runs carry, a carry of an approval to version number of artifactID that a
// writer of tx runs in its read phase, so that every write it makes runs after that version's
// version_renders row where tx writes one (rendersInTransaction): in the second part of the write
// phase, which the ledger sends once the rows stand (runQueue). The review the carry inserts then
// pins the digest the row records, which artifact_version_text_sha256 answers from the row; the
// insert's trigger takes the digest it answers at the insert. A carry that restores the approved
// text is the one exception: its writes keep their place, before the version's artifact.version,
// behind the row it writes itself (carryRestored). A version that takes no row in tx - one a pass
// left to the reconciliation, one no query's record covers (a version of a document of no query's
// band, or of a document whose node was no query node as the version was written), one written in
// a transaction no ledger queues - keeps its carry where its writer runs it, and pins the digest
// of its stored markdown.
func carryAfterRender(ctx context.Context, tx pgx.Tx, artifactID string, number int, carry func() error) error {
	rendered, err := rendersInTransaction(ctx, tx, artifactID, number)
	if err != nil {
		return err
	}
	if !rendered {
		return carry()
	}
	ledger := queueFor(ctx, tx)
	ledger.toRendered = true
	defer func() { ledger.toRendered = false }()
	return carry()
}

// carryRestored runs carry, the carry of newest - the document's newest review, an approval of
// version A - to version number of artifactID, whose text is A's stored text byte for byte
// (restoresApprovedText). A restore carries only because the version is what the person approved,
// so where the version takes its version_renders row in tx (toRendered, which carryAfterRender
// sets for the carry it runs to such a version) its record's digest must be the one newest pinned,
// which is known before the write phase. The carry's writes then keep their place in the queue -
// the review and its artifact.approved before the version's artifact.version, the order the Legion
// daemon reads a restore by - after a step that writes the version's row with that pinned digest
// (pinVersionRender), so the review's insert trigger pins it too. The write phase takes a savepoint
// before the version's insert, reads the version's record by the fill's own walk once the queue has
// run, and drops the carry where its digest is not the pin, writing the version, what it states and
// its row again with no carry (writeRestored): the restore is then no exact one in the model,
// whatever its text. A version that takes no row in tx, a review that pinned no digest, and a
// version this transaction queued no insert of carry as carryAfterRender left them.
func carryRestored(ctx context.Context, tx pgx.Tx, artifactID string, number int, newest model.ArtifactReview, carry func() error) error {
	ledger := queueFor(ctx, tx)
	if ledger == nil || !ledger.toRendered || newest.ContentSHA256 == nil {
		return carry()
	}
	pending, queued := ledger.latest[artifactID]
	if !queued || pending.version.Number != number {
		return carry()
	}
	at, err := ledger.Now(ctx)
	if err != nil {
		return err
	}
	restore := &restoredCarry{version: statedVersion{artifactID: artifactID, number: number}, pin: *newest.ContentSHA256, from: pending.queued}
	ledger.restores = append(ledger.restores, restore)
	ledger.toRendered, ledger.restoring = false, restore
	defer func() { ledger.toRendered, ledger.restoring = true, nil }()
	if err := write(ctx, tx, pinVersionRender(restore.version, restore.pin, at)); err != nil {
		return err
	}
	return carry()
}

// revisedDocumentsStatement is each document of the issue $1 whose versions a query's record covers
// (queryOfBand), where the issue has converted - an issue_trees row holds its tree, whose room each
// such document is edited in (document_rooms) - and nothing for any other issue: each one's id, its
// band, and its newest version's number and markdown with the digest that version's version_renders
// row records, null where it holds no row. They come in id order.
var revisedDocumentsStatement = `
	select d.id::text, ` + refs.DocumentBandSQL("d") + `, v.number, coalesce(v.markdown, ''), r.record_sha256
	from issue_trees t
	join document_rooms m on m.room = t.artifact_id
	join artifacts d on d.id = m.artifact_id and d.issue_key = t.issue_key and d.kind = 'doc' and d.retired_at is null
	join lateral (
		select number, markdown from artifact_versions where artifact_id = d.id order by number desc limit 1
	) v on true
	left join version_renders r on r.artifact_id = d.id and r.number = v.number
	where t.issue_key = $1 and ` + refs.DocumentBandSQL("d") + ` in ('` + string(refs.RankBandSpec) + `', '` +
	string(refs.RankBandVerification) + `', '` + string(refs.RankBandPlan) + `')
	order by d.id`

// lockQueryRoomsStatement takes the lock of the room of the issue $1, its tree's (issue_trees),
// as lockDocumentRoom takes it, and takes nothing for an issue with no tree.
const lockQueryRoomsStatement = `select pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t where t.issue_key = $1`

// LockQueryRooms takes, in tx, the lock of every room ReviseQueries writes a version of issueKey's
// query documents in: the issue's own room, its tree's, whose stored history's head each version
// records (versionRoomHead), and nothing for an issue that has not converted. A writer that calls
// ReviseQueries after writes of its own takes it right after its owner locks and before its first
// write, the order docs/persistence.go sets for a transaction that takes both an owner's lock and
// a room's: the owner's advisory lock, then the room's, then the owner row its writes take (the
// event's). The lock is a transaction advisory lock, so ReviseQueries taking it again later waits
// on nothing.
func LockQueryRooms(ctx context.Context, tx pgx.Tx, issueKey string) error {
	if _, err := tx.Exec(ctx, lockQueryRoomsStatement, issueKey); err != nil {
		return fmt.Errorf("lock the room of %s: %w", issueKey, err)
	}
	return nil
}

// ReviseQueries appends, in tx, a version of each query document of the converted issue issueKey
// whose query's record or whose rendering moved since its newest version, for a write that moved
// a query's record and wrote no version of it: a recorded result, its retraction, a room write
// that moved a query whose document's fragment it did not change. For an issue that has not
// converted - no issue_trees row holds its tree - it does nothing and answers the zero value.
//
// It reads the store as tx holds it, so its caller's writes have run: it is called after them, and
// never between a write phase's planning and its batch. It takes the issue's owner lock, which
// every writer of a version takes first and which its caller holds already, and reads at one cut
// of tx (refs.HeadCut) each query document's record (IssueTree.QueryRecord, the issue's tree walked
// once) and its render as of the cut's time (RenderAt). A document whose newest version holds no
// version_renders row is left as it is: either no pass has stated that version yet, so the tree may
// not hold it (a deferred pass), and the pass that states it records its digest, or it is no query
// version this build's version write made (madeQueryVersion), which no row ever covers and which
// answers its stored bytes for good. A document whose record digests to its newest version's
// digest and whose render is that version's markdown is left too. Each other one takes a version
// whose markdown is the render, which for a record moved alone is the bytes of the version before
// it, written by writer, with its version node
// (WriteVersionNode, whose owner names the query and its definition_version, ReadVersionOwner) and
// its version_renders row holding the record's digest and the cut's time and seq, as a pass records
// them (QueueVersionRender): the census reads it as every rendered version, and never as deferred.
//
// Before it writes, it takes the lock of the issue's room (LockQueryRooms), after every owner lock
// its caller holds: each version records the head of its room's stored history
// (versionRoomHead), which only that lock holds still. A caller that wrote before calling it took
// that lock already, right after its owner locks and before its first write (LockQueryRooms); a
// room settlement holds its own room's lock from its read phase; so the take here waits on nothing
// in either.
//
// What it answers is the version nodes' facts, held for the event the caller writes: the caller
// causes them under that event (graphstore.Caused), as every writer of the store causes its facts.
// It appends no event of its own.
func (s *Service) ReviseQueries(ctx context.Context, tx pgx.Tx, issueKey string, writer model.Actor) (graphstore.Uncaused, error) {
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issueKey)); err != nil {
		return graphstore.Uncaused{}, err
	}
	rows, err := tx.Query(ctx, revisedDocumentsStatement, issueKey)
	if err != nil {
		return graphstore.Uncaused{}, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
	}
	type queryDocument struct {
		id, query, markdown string
		number              int
		digest              *string
	}
	var documents []queryDocument
	for rows.Next() {
		var document queryDocument
		var band string
		if err := rows.Scan(&document.id, &band, &document.number, &document.markdown, &document.digest); err != nil {
			rows.Close()
			return graphstore.Uncaused{}, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
		}
		if document.query = queryOfBand[band]; document.digest != nil {
			documents = append(documents, document)
		}
	}
	if err := rows.Err(); err != nil {
		return graphstore.Uncaused{}, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
	}
	if len(documents) == 0 {
		return graphstore.Uncaused{}, nil
	}
	cut, err := refs.HeadCut(ctx, tx)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	records := cutRecords{cut: cut}
	type revision struct {
		document queryDocument
		markdown string
		record   QueryRecord
	}
	var revisions []revision
	for _, document := range documents {
		record, err := records.record(ctx, tx, queryOf{issue: issueKey, query: document.query})
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		rendering, err := RenderAt(ctx, tx, model.Actor{}, document.id, cut.At)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		if record.Digest != *document.digest || rendering.Markdown != document.markdown {
			revisions = append(revisions, revision{document: document, markdown: rendering.Markdown, record: record})
		}
	}
	if len(revisions) == 0 {
		return graphstore.Uncaused{}, nil
	}
	if err := LockQueryRooms(ctx, tx, issueKey); err != nil {
		return graphstore.Uncaused{}, err
	}
	nodes := make([]graphstore.Uncaused, 0, len(revisions))
	for _, revised := range revisions {
		id := revised.document.id
		version, err := queueVersionInsert(ctx, tx, id, revised.markdown, []model.Actor{writer}, false, nil, nil, RenderStamp{})
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		owner, err := ReadVersionOwner(ctx, tx, id)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		node, err := WriteVersionNode(ctx, tx, owner, version.Number, writer, version.CreatedAt)
		if err != nil {
			return graphstore.Uncaused{}, err
		}
		record := revised.record
		if err := write(ctx, tx, func(b *store.WriteBatch) error {
			return QueueVersionRender(b, id, version.Number, record)
		}); err != nil {
			return graphstore.Uncaused{}, err
		}
		nodes = append(nodes, node)
	}
	return graphstore.Joined(nodes...), nil
}

// versionRenderAt renders the document artifactID as of at for the census of the rendered
// versions: RenderAt's markdown, as the server reads the store.
func versionRenderAt(ctx context.Context, q graphstore.Queryer, artifactID string, at time.Time) (string, error) {
	rendering, err := RenderAt(ctx, q, model.Actor{}, artifactID, at)
	return rendering.Markdown, err
}

// ReadVersionRenderCensus reads, through q, what the census of `census --store` prints of the
// versions version_renders holds (graphcensus.VersionRenderCensus): each one's stored markdown
// held to a fresh render of the tree as of the time its row records, which only docs renders.
func ReadVersionRenderCensus(ctx context.Context, q graphstore.Queryer) (graphcensus.VersionRenderCensus, error) {
	return graphcensus.CompareVersionRenders(ctx, q, versionRenderAt)
}
