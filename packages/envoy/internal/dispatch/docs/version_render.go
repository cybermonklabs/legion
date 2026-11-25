package docs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
// The decisions query node holds no member: the one renderer branches on its node's query and
// renders it by the query's answer as of at (renderDecisions), the function its version write
// stores the bytes of (R15).
//
// who is the reader the render answers; the render leaves out no node for a reader, so every
// reader reads one rendering.
func RenderAt(ctx context.Context, q Queryer, who model.Actor, artifactID string, at time.Time) (Rendering, error) {
	var refKey, issueKey string
	var decisions bool
	if err := q.QueryRow(ctx, `select a.ref_key, coalesce(a.issue_key, ''), `+refs.DecisionsQuerySQL("a")+` from artifacts a where a.id = $1`,
		artifactID).Scan(&refKey, &issueKey, &decisions); err != nil {
		return Rendering{}, fmt.Errorf("read the document %s to render: %w", artifactID, err)
	}
	cut := refs.TreeCut{At: at, Dated: true}
	if decisions {
		rendered, err := renderDecisions(ctx, q, issueKey, cut)
		if err != nil {
			return Rendering{}, err
		}
		return Rendering{Markdown: rendered, Source: RenderedFromTree}, nil
	}
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

// renderDecisions is the readable rendering of the answer of the decisions query of the issue
// issueKey at cut (R15): the nodes the query takes (decisionsTakes) in the order of each node's
// made_at, then its address, each compared byte for byte, and not in the tree's order, so a move,
// which changes a node's place and not its made_at, its address or whether the walk reaches it,
// changes no byte of it. Each node prints as the words a reader knows it by: a decision or a
// reading by its text value at the cut, its whole slice of the version its document stored, and
// an ask by its question, as the conversation's loader answers it (refs.ConversationNodes). The
// entries are joined by one blank line and the last line ends; an answer that takes no node
// renders as no bytes. The version write and the renderer both call it: the drain's at the cut it
// records on the version's row (planRevisions), the renderer as of that row's time (RenderAt).
func renderDecisions(ctx context.Context, q Queryer, issueKey string, cut refs.TreeCut) (string, error) {
	tree, err := WalkIssueTree(ctx, q, issueKey, cut)
	if err != nil {
		return "", err
	}
	taken, _ := tree.Takes(graphstore.QueryDecisions)
	nodes := make([]TreeNode, 0, len(taken))
	var blocks []refs.Target
	for _, index := range taken {
		node := tree.Nodes[index]
		nodes = append(nodes, node)
		if node.Kind != graphstore.KindAsk {
			blocks = append(blocks, node.target())
		}
	}
	facts, err := refs.TreeFactsOf(ctx, q, blocks, tree.Cut)
	if err != nil {
		return "", fmt.Errorf("read the decisions of %s to render: %w", issueKey, err)
	}
	slices.SortStableFunc(nodes, func(a, b TreeNode) int {
		return cmp.Or(a.MadeAt.Compare(b.MadeAt), strings.Compare(a.Address(), b.Address()))
	})
	entries := make([]string, 0, len(nodes))
	for _, node := range nodes {
		words := facts[node.target()].Values[graphstore.AttributeText]
		if node.Kind == graphstore.KindAsk {
			words = ""
			if loaded := tree.Conversation[node.target()]; loaded.Excerpt != nil {
				words = loaded.Excerpt.Text
			}
		}
		if words != "" {
			entries = append(entries, words)
		}
	}
	return joinDecisionEntries(entries), nil
}

// joinDecisionEntries joins the entries of a decisions rendering: one blank line between two, a
// line end after the last, and no bytes for none.
func joinDecisionEntries(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	return strings.Join(entries, "\n\n") + "\n"
}

// DecisionsOfSpec is the rendering of the decisions query's answer (renderDecisions) that the
// create of an issue born converted stores as version 1 of the issue's decisions query node, read
// off the spec's tree in the create's read phase, before the batch that writes the spec's block
// nodes: at the create the query takes the spec's decisions and the readings under them, each a
// node the spec's pass writes in that batch with one made_at, the pass's time, so they print in the
// order of their addresses, which share the room's prefix and so follow their block ids, each by
// its text value, its slice of the spec's rendering (refs.IndexBlocks). No ask of the issue has a
// node yet: the spec's settlement opens each ask its blocks hold once the create has committed
// (reconcileAskBlocks). A reading is one whose nearest typed block around it is a decision.
func DecisionsOfSpec(spec *pmdoc.Node) (string, error) {
	markdown, offsets, err := documentMarkdownWithOffsets(spec)
	if err != nil {
		return "", err
	}
	type decisionBlock struct{ id, text string }
	var taken []decisionBlock
	for index, block := range offsets {
		take := block.Type == decisionType
		if block.Type == refs.ReadingSupertag {
			for around := index - 1; around >= 0; around-- {
				outer := offsets[around]
				if outer.From <= block.From && block.To <= outer.To && pmdoc.IsTypedBlock(outer.Type) {
					take = outer.Type == decisionType
					break
				}
			}
		}
		if take && block.ID != "" {
			taken = append(taken, decisionBlock{id: block.ID, text: markdown[block.From:block.To]})
		}
	}
	slices.SortStableFunc(taken, func(a, b decisionBlock) int { return strings.Compare(a.id, b.id) })
	entries := make([]string, 0, len(taken))
	for _, block := range taken {
		entries = append(entries, block.text)
	}
	return joinDecisionEntries(entries), nil
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
// with the version or not at all, and owes no digest. It writes a table of its own and no tuple of
// the store. A version that holds its row with a time keeps it: the first pass that states a
// version records it, and a later pass that states the version again - a room pass stating each
// fragment's newest version, the reconciliation stating a version the drain of a query mark wrote
// (writeRevisions) - records nothing; a row that holds no time yet, and no tree_mismatch, takes
// it.
func QueueVersionRender(b *store.WriteBatch, artifactID string, number int, record QueryRecord) error {
	_, err := queueVersionRender(b, artifactID, number, record)
	return err
}

// queueVersionRender is QueueVersionRender, answering the statement it queued, whose command tag
// says whether the row took its time from it (firstStatedOn).
func queueVersionRender(b *store.WriteBatch, artifactID string, number int, record QueryRecord) (store.Queued, error) {
	if record.Cut.Dated || record.Cut.At.IsZero() {
		return store.Queued{}, errVersionRenderCut
	}
	return b.Queue("write the version_renders row of version "+fmt.Sprint(number), `
		insert into version_renders (artifact_id, number, record_sha256, at, read_seq) values ($1, $2, $3, $4, $5)
		on conflict (artifact_id, number) do update
			set record_sha256 = excluded.record_sha256, at = excluded.at, read_seq = excluded.read_seq, digest_owed = false
			where version_renders.at is null and not version_renders.tree_mismatch`,
		artifactID, number, record.Digest, record.Cut.At, record.Cut.ReadSeq), nil
}

// versionRenderCutStatement writes the version_renders row of version $2 of the document $1 in
// the batch that writes the version (R15): the cut it takes in SQL as the statement runs - the
// database's clock and the newest seq the tuple log holds, as refs.HeadCut reads the head's cut -
// as its time and its read_seq, and $3 as digest_owed, true where a query's record covers the
// version, whose digest the fill after the COMMIT reads at that cut and records
// (fillOwedDigests), false for a version of a document no query record covers, which holds no
// digest for good. It is queued after every step of the batch, so its cut holds every tuple the
// transaction's passes appended. A row that holds a time already keeps it; one that holds none
// and no tree_mismatch takes the cut.
const versionRenderCutStatement = `
	insert into version_renders (artifact_id, number, at, read_seq, digest_owed)
	values ($1, $2, clock_timestamp(), coalesce((select seq from graph_tuples order by seq desc limit 1), 0), $3)
	on conflict (artifact_id, number) do update set at = excluded.at, read_seq = excluded.read_seq, digest_owed = excluded.digest_owed
		where version_renders.at is null and not version_renders.tree_mismatch`

// queueVersionRenderCut queues on b the version_renders row of version with the cut taken in SQL
// (versionRenderCutStatement), its record's digest owed where owed says a query's record covers
// it, and answers the statement, whose command tag says whether the row took its time from it
// (firstStatedOn).
func queueVersionRenderCut(b *store.WriteBatch, version statedVersion, owed bool) store.Queued {
	return b.Queue("write the version_renders row of version "+fmt.Sprint(version.number), versionRenderCutStatement,
		version.artifactID, version.number, owed)
}

// queryOfBand is the query whose record can cover the versions of an issue's document of each band
// (refs.DocumentBandSQL, the one statement of which document plays which part): the spec, the
// Verification document and the plan. A document of any other band - the issue room's own
// fragment, an upload, a project's document - is covered by no query's record, and its versions
// render from the tree with no digest (R15), but for the decisions query node, a document of the
// last band whose node is a query node (rendersFrom). Of a document of one of these bands, a
// query's record covers only a query version this build's version write made (madeQueryVersion);
// every other version answers its stored bytes.
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

// rendersFrom reports whether a version a pass stated renders from the tree, and the issue and
// the query whose record covers it, of the document's issue, its band and the query of its node,
// "" where that node is no query node, and of whether the version is a query version this build's
// version write made (madeQueryVersion). A query version so made renders from the tree with its
// record's digest, the record of its node's query. A version of a document whose node is no query
// node, of a band no query covers - the issue room's own fragment, an upload, a project's document -
// renders from the tree with no digest (R15): of is its issue, if any, with no query. Every other
// version - one of a document of a query's band whose node is no query node, an issue's that has
// not converted, and one of a query node's document that this build's version write did not make -
// renders from its stored bytes, and takes no row.
func rendersFrom(issue, band, query string, made bool) (queryOf, bool) {
	switch {
	case query != "":
		return queryOf{issue: issue, query: query}, issue != "" && made
	case queryOfBand[band] == "":
		return queryOf{issue: issue}, true
	}
	return queryOf{}, false
}

// recordStatedVersions records on the ledger that runs tx's write phase each version of versions a
// pass of tx stated. That ledger writes the version_renders row of each one that renders from the
// tree (readCovered) in its write phase (Ledger.runQueue). A version a pass states in a transaction
// no ledger of ctx knows takes no row and answers its stored bytes.
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
// stored document and a number - what says whether it renders from the tree and which query's
// record covers it (readCovered, rendersFrom): the document's issue and band (refs.DocumentBandSQL,
// queryOfBand), and the query of the document's node where that node is a query node, "" where it
// is none; whether the store holds the version's node present, at the address graphstore.VersionID
// gives it and at any state but gone, since a node held gone is no present node (graphcensus reads
// a row at its address as a later row at a freed address), and whether that node holds
// definition_version; whether the store holds the version's row; and what the version's node
// takes of its document (graphstore.VersionOwnerColumns), as ReadVersionOwner reads it.
var statedDocumentsStatement = `
	select s.id::text, s.number, coalesce(d.issue_key, ''), ` + refs.DocumentBandSQL("d") + `,
	       coalesce((select q.attributes->>'` + graphstore.AttributeQuery + `' from graph_nodes q
	                 where q.kind = '` + graphstore.KindArtifact + `' and q.id = d.ref_key and q.supertag = '` + graphstore.SupertagQuery + `'), ''),
	       n.id is not null, coalesce(n.attributes ? '` + graphstore.AttributeDefinitionVersion + `', false),
	       exists (select 1 from artifact_versions v where v.artifact_id = s.id and v.number = s.number),
	       ` + graphstore.VersionOwnerColumns("s.id") + `
	from unnest($1::uuid[], $2::int[]) as s (id, number)
	join artifacts d on d.id = s.id
	left join graph_nodes n on n.kind = '` + graphstore.KindVersion + `' and n.id = d.ref_key || '@v' || s.number
		and n.state <> '` + graphstore.StateGone + `'`

// queryOf is the issue and the query whose record covers a version; a version of a document no
// query record covers is its issue's, "" for a project's document, with no query.
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

// readCovered reads, through tx, which of versions, versions a pass of the ledger's transaction
// stated (recordStatedVersions), render from the tree and which query's record covers each
// (rendersFrom), before that transaction's write phase sends its first batch. A document the
// transaction creates is read off the address it was registered under (RegisterPendingArtifact),
// whose row no statement reads before the batch writes it: the store holds no version of it and no
// node, so each of its versions is one the transaction queued, whose owner is a query node where
// the node the transaction writes of the document is one (pendingQuery), as ReadVersionOwner reads
// it. Every other document's versions are read in one statement (statedDocumentsStatement), which
// reads a version this transaction queued as one whose row the store does not hold yet; none runs
// where versions names no stored document. The band it reads is the one a read after the batch
// would: the columns a band is decided by - the issue, the kind, is_primary, the role and the slug
// - are written with the row, and the one write of one of them a write phase makes later, an
// upload's repair of a role left null (api's storeArtifact), writes the role the row's slug already
// gives it (refs.DocumentBandSQL). The owner it reads of a version the transaction queued is the
// one the store holds before the batch, which is the one that version's writer read in the read
// phase (ReadVersionOwner), and so the one its node takes.
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
		query := l.pendingQuery(version.artifactID)
		owner := graphstore.VersionOwner{RefKey: address.RefKey}
		if query != "" {
			owner.Query, owner.DefinitionVersion = true, graphstore.QueryDefinitionVersion
		}
		if of, renders := rendersFrom(address.IssueKey, string(address.Band), query, madeQueryVersion(false, false, true, owner, version.number)); renders {
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
		var issue, band, query string
		var held, defined, stored bool
		var owner graphstore.VersionOwner
		if err := rows.Scan(append([]any{&version.artifactID, &version.number, &issue, &band, &query, &held, &defined, &stored}, owner.Targets()...)...); err != nil {
			return nil, fmt.Errorf("read which query's record covers the versions the passes stated: %w", err)
		}
		if of, renders := rendersFrom(issue, band, query, madeQueryVersion(held, defined, !stored, owner, version.number)); renders {
			covered[version] = of
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which query's record covers the versions the passes stated: %w", err)
	}
	return covered, nil
}

// readStatedDocuments reads which versions a pass of the ledger's transaction stated render from
// the tree, and which query's record covers each (recordStatedVersions, readCovered), into
// covered, which the write phase's rows read (queueStatedRenders, statedRenders). It runs once the
// queue is sealed and before the write phase's first batch is sent (runQueue), so the read holds
// no transaction id the batch takes, and a version the transaction queued is not stored yet. A
// transaction whose passes stated no version of a stored document runs no statement.
func (l *Ledger) readStatedDocuments(ctx context.Context) error {
	var deferred []statedVersion
	if l.owesDigests() && len(l.restores) == 0 {
		for _, artifactID := range slices.Sorted(maps.Keys(l.latest)) {
			version := statedVersion{artifactID: artifactID, number: l.latest[artifactID].version.Number}
			if !slices.Contains(l.stated, version) {
				deferred = append(deferred, version)
			}
		}
	}
	if len(l.stated)+len(deferred) == 0 {
		return nil
	}
	tx := l.writer
	if tx == nil {
		tx = l.tx
	}
	covered, err := l.readCovered(ctx, tx, slices.Concat(l.stated, deferred))
	if err != nil {
		return err
	}
	l.covered, l.deferred = covered, nil
	for _, version := range deferred {
		if _, renders := covered[version]; renders {
			l.deferred = append(l.deferred, version)
		}
	}
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

// queueStatedRenders queues on b, last among the write phase's steps, the version_renders row of
// each version a pass of the ledger's transaction stated that renders from the tree (covered,
// readStatedDocuments), once a version, in the order the passes stated them, each with the cut it
// takes in SQL (queueVersionRenderCut): its record's digest owed where a query's record covers it,
// and none where no query record covers its document. It records the issue of each owed digest
// for the fill that runs once the transaction has committed (fills, fillOwedDigests). So the
// write keeps main's window: no read of a record runs between its batch and its COMMIT (R15). It
// then queues the row of each version the transaction queued and no pass of it stated (deferred):
// a row with no time, no seq and no digest, which the first pass that states the version fills
// (QueueVersionRender, versionRenderCutStatement), and which the census counts as deferred until
// then (R15).
func (l *Ledger) queueStatedRenders(b *store.WriteBatch) error {
	queued := map[statedVersion]bool{}
	for _, version := range l.stated {
		of, renders := l.covered[version]
		if !renders || queued[version] {
			continue
		}
		queued[version] = true
		owed := of.query != ""
		queueVersionRenderCut(b, version, owed)
		if owed && !slices.Contains(l.fills, of.issue) {
			l.fills = append(l.fills, of.issue)
		}
	}
	for _, version := range l.deferred {
		b.Queue("write the deferred version_renders row of version "+fmt.Sprint(version.number), `
			insert into version_renders (artifact_id, number) values ($1, $2) on conflict (artifact_id, number) do nothing`,
			version.artifactID, version.number)
	}
	return nil
}

// writeStatedRenders writes, in the ledger's transaction, the version_renders row of each version
// a pass of it stated (recordStatedVersions) that renders from the tree (readCovered, read before
// the write phase's first batch, readStatedDocuments), where the transaction records each digest
// in itself: a background pass's (queueWrites) and a restore's (writeRestored). A version a query's
// record covers takes the digest of that record, and the time and the seq of the cut it was read
// at, one cut for the whole transaction, read once its write phase has run (refs.HeadCut), so the
// record and the render as of the row's time hold every tuple the passes appended under their
// events; one no query record covers takes the cut in SQL and no digest (queueVersionRenderCut). It
// runs after the write phase's batch and before the commit (runQueue): the record is built in Go
// from the store as that batch leaves it (IssueTree.QueryRecord), which no statement of the batch
// can read. Where no stated version renders from the tree, it runs no statement, and each version
// answers its stored bytes.
//
// It notes each version whose row takes its time from this transaction (firstStatedOn), for the
// carry of R14 its caller runs once the rows stand (carryFirstStated), and marks tree_mismatch on
// the row of each version a pass of it found its tree does not render as (recordTreeMismatch,
// queueTreeMismatch), which takes no time, no seq and no digest.
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
	renders, unrecorded, err := l.statedRenders(ctx, tx)
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
	if len(missed) > 0 || len(renders)+len(unrecorded)+len(l.mismatched) == 0 {
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
			queued, err := queueVersionRender(b, render.version.artifactID, render.version.number, render.record)
			if err != nil {
				return err
			}
			l.firstStatedOn(queued, render.version)
		}
		for _, version := range unrecorded {
			l.firstStatedOn(queueVersionRenderCut(b, version, false), version)
		}
		for _, version := range l.mismatched {
			queueTreeMismatch(b, version)
		}
		return nil
	})
}

// firstStatedOn notes version among the versions whose row took its time from this transaction
// (firstStated) once queued, the statement that writes its row (queueVersionRender,
// queueVersionRenderCut), has run and written it: a row it inserted, or one that held no time and
// no tree_mismatch, which it updated. A row that held a time keeps it, and the statement writes
// nothing of it.
func (l *Ledger) firstStatedOn(queued store.Queued, version statedVersion) {
	queued.Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() == 1 {
			l.firstStated = append(l.firstStated, version)
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
// the passes stated them: every record at one cut, each issue's tree walked once (cutRecords); and
// it answers beside them each stated version that renders from the tree with no record, once. It
// reads no version_renders row, and nothing where no stated version is covered by a record.
func (l *Ledger) statedRenders(ctx context.Context, tx pgx.Tx) ([]statedRender, []statedVersion, error) {
	var records *cutRecords
	var renders []statedRender
	var unrecorded []statedVersion
	written := map[statedVersion]bool{}
	for _, version := range l.stated {
		of, held := l.covered[version]
		if !held || written[version] {
			continue
		}
		written[version] = true
		if of.query == "" {
			unrecorded = append(unrecorded, version)
			continue
		}
		if records == nil {
			cut, err := refs.HeadCut(ctx, tx)
			if err != nil {
				return nil, nil, err
			}
			records = &cutRecords{cut: cut}
		}
		record, err := records.record(ctx, tx, of)
		if err != nil {
			return nil, nil, err
		}
		renders = append(renders, statedRender{version: version, record: record})
	}
	return renders, unrecorded, nil
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

// rendersInTransaction reports whether version number of artifactID takes, in tx, a version_renders
// row that holds its record's digest: a pass of tx stated it (recordStatedVersions) and a query's
// record covers it (readCovered), read here, in tx's read phase, by the same predicate the write
// phase's read takes before its first batch, so the restore's pin and the row agree. A version tx
// queued is not stored yet in either read.
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
	return covered[version].query != "", nil
}

// recordPinStatement is the record's digest the review $1, an approval of version $3 of the
// document $2 that pinned the digest $4, stands on: the digest the fill repinned the review to
// where it is a carry whose own row of artifact_review_carries holds one (fillOwedDigests), and
// else $4, where it is the digest the version_renders row of the approved version records; null
// where the review pinned the digest of the approved version's stored markdown alone - a review
// taken before the cutover, before the pass that stated its version or before the fill of its
// digest (R14) - which no record digests to.
const recordPinStatement = `
	select coalesce(
		(select c.record_sha256 from artifact_review_carries c where c.review_id = $1::uuid),
		(select r.record_sha256 from version_renders r where r.artifact_id = $2::uuid and r.number = $3 and r.record_sha256 = $4))`

// recordPin is the record's digest newest, an approval, stands on (recordPinStatement), nil where
// it stands on no record's digest. newest read off the ledger, a review this transaction queued,
// names no row of artifact_review_carries yet.
func recordPin(ctx context.Context, tx pgx.Tx, artifactID string, newest model.ArtifactReview) (*string, error) {
	var review *string
	if newest.ID != "" {
		review = &newest.ID
	}
	var pin *string
	if err := tx.QueryRow(ctx, recordPinStatement, review, artifactID, newest.Version, newest.ContentSHA256).Scan(&pin); err != nil {
		return nil, fmt.Errorf("read the record digest the approval of version %d of %s stands on: %w", newest.Version, artifactID, err)
	}
	return pin, nil
}

// carryRestored runs carry, the carry of newest - the document's newest review, an approval of
// version A - to version number of artifactID, whose text is A's stored text byte for byte
// (restoresApprovedText). A restore carries only because the version is what the person approved,
// so where the version takes its version_renders row with its record's digest in tx
// (rendersInTransaction) and newest stands on a record's digest (recordPin), the version's record's
// digest must be that one, which is known before the write phase. The carry's writes then keep
// their place in the queue - the review and its artifact.approved before the version's
// artifact.version, the order the Legion daemon reads a restore by - after a step that writes the
// version's row with that pinned digest (pinVersionRender), so the review's insert trigger pins it
// too. The write phase takes a savepoint before the version's insert, reads the version's record
// by the fill's own walk once the queue has run, and drops the carry where its digest is not the
// pin, writing the version, what it states and its row again with no carry (writeRestored): the
// restore is then no exact one in the model, whatever its text. These are the windows the owner
// approved as root R29's second exception (R14). A version that takes no such row in tx, a review
// that stands on no record's digest, and a version this transaction queued no insert of, carry as
// any other carry does, in their place.
func carryRestored(ctx context.Context, tx pgx.Tx, artifactID string, number int, newest model.ArtifactReview, carry func() error) error {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return carry()
	}
	pending, queued := ledger.latest[artifactID]
	if !queued || pending.version.Number != number {
		return carry()
	}
	rendered, err := rendersInTransaction(ctx, tx, artifactID, number)
	if err != nil {
		return err
	}
	if !rendered {
		return carry()
	}
	pin, err := recordPin(ctx, tx, artifactID, newest)
	if err != nil {
		return err
	}
	if pin == nil {
		return carry()
	}
	at, err := ledger.Now(ctx)
	if err != nil {
		return err
	}
	restore := &restoredCarry{version: statedVersion{artifactID: artifactID, number: number}, pin: *pin, from: pending.queued}
	ledger.restores = append(ledger.restores, restore)
	ledger.restoring = restore
	defer func() { ledger.restoring = nil }()
	if err := write(ctx, tx, pinVersionRender(restore.version, restore.pin, at)); err != nil {
		return err
	}
	return carry()
}

// The fill of a record digest a write left owed (R15). A version write keeps main's window: its
// batch inserts the version's version_renders row with the cut it takes in SQL and, where a query's
// record covers the version, its digest owed (queueStatedRenders), so a review its batch inserts -
// the carried review of R14's order - pins the digest of the version's stored markdown, which
// artifact_version_text_sha256 answers until the fill. Right after the COMMIT, and before the
// server publishes the write's events, the fill reads each owed version's record at its row's cut
// and records its digest (fillOwedDigests), in a transaction of its own whose window is its reads,
// then one batch and its COMMIT. The drain of an issue's query marks runs the fill of the issue
// first (drainQueryMark), and the block-index tick repairs a fill a request lost
// (fillLostDigests).

// owedDigestsStatement is each version of a document of the issue $1 whose record digest is owed
// (version_renders.digest_owed), by document and number: its document and number, the cut its row
// records, the query of its document's node, whose record covers it, and its stored markdown.
var owedDigestsStatement = `
	select r.artifact_id::text, r.number, r.at, r.read_seq, n.attributes->>'` + graphstore.AttributeQuery + `', coalesce(v.markdown, '')
	from artifacts a
	join version_renders r on r.artifact_id = a.id and r.digest_owed
	join graph_nodes n on n.kind = '` + graphstore.KindArtifact + `' and n.id = a.ref_key and n.supertag = '` + graphstore.SupertagQuery + `'
	join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.number
	where a.issue_key = $1
	order by r.artifact_id, r.number`

// owedCarriesStatement is each carried review of version $2 of the document $1 whose row of
// artifact_review_carries holds no record digest yet: the carry the version's own write queued in
// its batch (R14), which pinned the digest of the version's stored markdown.
const owedCarriesStatement = `
	select c.review_id::text
	from artifact_reviews r
	join artifact_review_carries c on c.review_id = r.id
	where r.artifact_id = $1 and r.version = $2 and c.record_sha256 is null
	order by c.review_id`

// lostDigestsStatement lists the issues a version of whose documents still owes its record digest
// though the write that left it owed has committed - a fill a request lost to a failure or a stop
// - at most $1 of them. A row whose write has not committed is no row of it.
const lostDigestsStatement = `
	select distinct a.issue_key
	from version_renders r
	join artifacts a on a.id = r.artifact_id
	where r.digest_owed and a.issue_key is not null
	limit $1`

// owedDigest is one version whose record digest is owed, as owedDigestsStatement reads it, with
// the record the fill read at its row's cut and the carried reviews of its write's batch the fill
// repins.
type owedDigest struct {
	version  statedVersion
	cut      refs.TreeCut
	query    string
	markdown string
	record   QueryRecord
	carries  []string
}

// readOwedDigests reads, through tx, each version of a document of the issue issueKey whose record
// digest is owed (owedDigestsStatement).
func readOwedDigests(ctx context.Context, tx pgx.Tx, issueKey string) ([]owedDigest, error) {
	rows, err := tx.Query(ctx, owedDigestsStatement, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the record digests %s owes: %w", issueKey, err)
	}
	owed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDigest, error) {
		var version owedDigest
		err := row.Scan(&version.version.artifactID, &version.version.number, &version.cut.At, &version.cut.ReadSeq, &version.query, &version.markdown)
		return version, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the record digests %s owes: %w", issueKey, err)
	}
	return owed, nil
}

// fillOwedDigests fills, in a transaction of its own, the record digest of each version of a
// document of the issue issueKey whose digest is owed, and answers the events it appended. It
// takes the issue's owner lock (store.LockOwner), which every writer of a version of the issue's
// documents takes, and reads each owed version (readOwedDigests), its query's record at the cut its
// row records (cutRecords), and the carried reviews of its write's batch (owedCarriesStatement),
// none of which gives the transaction an id. Its one batch then records each record's digest on
// its version's row, clearing digest_owed, and repins each such carried review to it on its own row
// of artifact_review_carries, with no event and no second review row (R14): the review's
// content_sha256 cannot move, since the update trigger of artifact_reviews refuses it. A person's
// approval of such a version given between the write's COMMIT and the fill is no carry: the fill
// carries it at that version (carryApprovalBeforeFill), after the rows, so the carried review's
// insert trigger pins the record's digest, and that carry appends its artifact.approved after the
// version's artifact.version, because the approval itself came after it. The COMMIT follows the
// batch, so the fill's window is main's batched shape. An issue that owes no digest runs nothing
// past its reads.
func (s *Service) fillOwedDigests(ctx context.Context, issueKey string) ([]model.Event, error) {
	if hold := s.fillHold.Load(); hold != nil {
		(*hold)(issueKey)
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the fill of the record digests %s owes: %w", issueKey, err)
	}
	defer tx.Rollback(ctx)
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issueKey)); err != nil {
		return nil, err
	}
	owed, err := readOwedDigests(ctx, tx, issueKey)
	if err != nil || len(owed) == 0 {
		return nil, err
	}
	if err := refs.TurnJITOff(ctx, tx); err != nil {
		return nil, err
	}
	for index := range owed {
		version := &owed[index]
		records := cutRecords{cut: version.cut}
		if version.record, err = records.record(ctx, tx, queryOf{issue: issueKey, query: version.query}); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, owedCarriesStatement, version.version.artifactID, version.version.number)
		if err != nil {
			return nil, fmt.Errorf("read the carried reviews of version %d of %s: %w", version.version.number, version.version.artifactID, err)
		}
		if version.carries, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, fmt.Errorf("read the carried reviews of version %d of %s: %w", version.version.number, version.version.artifactID, err)
		}
	}
	ctx, ledger := queueWrites(ctx, tx)
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		for _, version := range owed {
			queueFilledDigest(b, version)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, version := range owed {
		if err := s.carryApprovalBeforeFill(ctx, tx, version); err != nil {
			return nil, err
		}
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the fill of the record digests %s owes: %w", issueKey, err)
	}
	return ledger.events, nil
}

// queueFilledDigest queues on b the fill of version's record digest: the digest on its
// version_renders row, only while that row still owes it, and the same digest on the row of
// artifact_review_carries of each carried review of its write's batch.
func queueFilledDigest(b *store.WriteBatch, version owedDigest) {
	b.Queue(fmt.Sprintf("fill the record digest of version %d", version.version.number), `
		update version_renders set record_sha256 = $3, digest_owed = false
		where artifact_id = $1 and number = $2 and digest_owed`,
		version.version.artifactID, version.version.number, version.record.Digest)
	if len(version.carries) > 0 {
		b.Queue(fmt.Sprintf("repin the carried reviews of version %d", version.version.number), `
			update artifact_review_carries set record_sha256 = $2
			where review_id = any($1::uuid[]) and record_sha256 is null`,
			version.carries, version.record.Digest)
	}
}

// carryApprovalBeforeFill carries, in the fill's transaction, the newest review of version's
// document to version where it is an approval of that same version that no carry wrote: a person's
// own, or a plan review's, given between the version's COMMIT and the fill, which pinned the digest
// of the version's stored markdown. It carries it through carryNewestApproval, on that function's
// own conditions, as R14's pass carries an approval of a deferred version, and the carry's reason
// names the version itself (restoredReason). A carry the version's own write queued is repinned
// instead (queueFilledDigest), and an approval of another version carries nothing here: the
// version's own write weighed it.
func (s *Service) carryApprovalBeforeFill(ctx context.Context, tx pgx.Tx, version owedDigest) error {
	id, number := version.version.artifactID, version.version.number
	newest, err := NewestReview(ctx, tx, id)
	if err != nil || newest == nil || newest.State != "approved" || newest.Version != number || newest.CarriedFromVersion != nil {
		return err
	}
	tree, err := pmdoc.ParseRendering(version.markdown)
	if err != nil {
		return skipUnreadableVersion(id, number, err)
	}
	return carryNewestApproval(ctx, tx, s.events, id, number, version.markdown, tree, *newest, s.serverURL, decisionStepCases{}, eventSink(ctx))
}

// HoldFills sets hold to run at the start of every fill of the record digests a write left owed
// (fillOwedDigests), once that write has committed and before the fill's transaction begins, and
// nil takes it off again. A test pauses the fill there to read the committed row that still owes
// its digest, and to move the head record before the fill reads its own cut.
func (s *Service) HoldFills(hold func(issueKey string)) {
	if hold == nil {
		s.fillHold.Store(nil)
		return
	}
	s.fillHold.Store(&hold)
}

// fillOwed runs the fill of each of issues (fillOwedDigests), each in a transaction of its own,
// once the write that left their digests owed has committed, and answers the events the fills
// appended, in order, for the write's caller to publish after its own. A fill that fails is
// logged, and its digests stay owed for the block-index tick (fillLostDigests); the write stands
// committed either way. A caller still holding a pooled connection fills nothing, since the fill's
// transaction would be a second connection taken while it holds one (store.HoldsPooledConnection),
// and the tick fills those digests too.
func (s *Service) fillOwed(ctx context.Context, issues []string) []model.Event {
	if len(issues) == 0 || store.HoldsPooledConnection(ctx) {
		return nil
	}
	var appended []model.Event
	for _, issue := range issues {
		filled, err := s.fillOwedDigests(ctx, issue)
		if err != nil {
			slog.Error("dispatch: fill the record digests a write left owed", "issue", issue, "error", err)
			continue
		}
		appended = append(appended, filled...)
	}
	return appended
}

// fillLostDigests is the block-index tick's repair of the fills requests lost: it fills the owed
// digests of up to queryMarkBatch issues (lostDigestsStatement), each in a transaction of its own
// (fillOwedDigests), and publishes what each appended once it has committed. An issue it cannot
// fill is logged and left for the next tick.
func (s *Service) fillLostDigests(ctx context.Context) error {
	rows, err := s.store.Pool.Query(ctx, lostDigestsStatement, queryMarkBatch)
	if err != nil {
		return fmt.Errorf("list the issues that owe a record digest: %w", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the issues that owe a record digest: %w", err)
	}
	for _, issue := range issues {
		filled, err := s.fillOwedDigests(ctx, issue)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("dispatch: block index could not fill the record digests an issue owes", "issue", issue, "error", err)
			continue
		}
		for _, event := range filled {
			s.events.Publish(event)
		}
	}
	return nil
}

// A version whose tree does not render as its stored markdown (R15). A reconciliation's pass over
// a document, or over an issue room, states the tree it reads; where the rendering of that tree, or
// of a fragment of the room, is not the stored markdown of the version it states, byte for byte -
// a version whose writer wrote a tree that does not render as the markdown it stored - the pass
// records no time, no seq and no digest on that version's row and marks it tree_mismatch
// (recordTreeMismatch), so the version answers its stored bytes, artifact_version_text_sha256 the
// digest of its stored markdown, an approval of that digest stands with nothing carried, and the
// census counts the row in a class of its own and refuses while it does. Each tick of the
// reconciliation begins with the repair of the rows already marked (repairTreeMismatches), which
// restates such a version from its stored bytes, or closes its mark where a later version's pass
// has moved its document's blocks past it. A pass that gives a version's row its time first runs
// the carry of R14 to it once the row stands (carryFirstStated), so an approval of a deferred or
// a repaired version, taken before the pass, stands on that version's record.

// recordTreeMismatch records on the ledger that runs tx's write phase that the pass of tx that
// stated versions stated a tree that does not render as each version's stored markdown: each one
// a pass of tx stated (recordStatedVersions) leaves the versions whose rows the write phase dates
// and is marked tree_mismatch instead (Ledger.mismatched, queueTreeMismatch). A version no pass of
// tx stated - one whose pass lost the revision fence and stated nothing - is no version of it.
func recordTreeMismatch(ctx context.Context, tx pgx.Tx, versions refs.Versions) {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer != tx && ledger.tx != tx {
		return
	}
	for _, artifactID := range slices.Sorted(maps.Keys(versions)) {
		version := statedVersion{artifactID: artifactID, number: versions[artifactID].Number}
		if !slices.Contains(ledger.stated, version) {
			continue
		}
		ledger.stated = slices.DeleteFunc(ledger.stated, func(stated statedVersion) bool { return stated == version })
		ledger.mismatched = append(ledger.mismatched, version)
	}
}

// queueTreeMismatch queues on b the mark tree_mismatch on the version_renders row of version, a
// version a pass of the transaction found its tree does not render as (recordTreeMismatch): only
// on a row that holds no time, which its writer inserted for the first pass to state (R15). A row
// that holds a time keeps it and takes no mark, and a version with no row answers its stored bytes
// already.
func queueTreeMismatch(b *store.WriteBatch, version statedVersion) {
	b.Queue("mark the version_renders row of version "+fmt.Sprint(version.number)+" tree_mismatch", `
		update version_renders set tree_mismatch = true
		where artifact_id = $1 and number = $2 and at is null`,
		version.artifactID, version.number)
}

// carryFirstStated runs, in tx, the carry of R14 to each of versions, the versions whose
// version_renders row a reconciliation's pass of tx gave its time and its record's digest first
// (Ledger.firstStated), once those rows stand: the insert trigger of artifact_reviews pins the
// digest artifact_version_text_sha256 answers at the insert, which is the record's from the row
// on. It carries only where the document's newest review is an approval of that same version - a
// person's or a plan review's, or a carry the version's own write made to it - and through
// carryNewestApproval, on that function's own conditions: every ask block of the version agrees
// with its node, and then the render as of the row's time (RenderVersion) is compared with the
// stored markdown the approval approved, the version's own (restoresApprovedText), so a render
// equal to it carries with the restore's reason (restoredReason), naming the version the person
// approved (approvalOrigin), and one that differs carries only by carryVerdict's cases. An approval
// of another version carries nothing here: the version's own write weighed it. Its writes go on a
// write phase of their own in tx (queueWrites), and it answers the events they appended, for its
// caller to publish once tx has committed.
func (s *Service) carryFirstStated(ctx context.Context, tx pgx.Tx, versions []statedVersion) ([]model.Event, error) {
	if len(versions) == 0 {
		return nil, nil
	}
	ctx, ledger := queueWrites(ctx, tx)
	for _, version := range versions {
		id, number := version.artifactID, version.number
		newest, err := NewestReview(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if newest == nil || newest.State != "approved" || newest.Version != number {
			continue
		}
		rendered, err := RenderVersion(ctx, tx, model.Actor{}, id, number)
		if err != nil {
			return nil, err
		}
		tree, err := pmdoc.ParseRendering(rendered.Markdown)
		if err != nil {
			if err := skipUnreadableVersion(id, number, err); err != nil {
				return nil, err
			}
			continue
		}
		if err := carryNewestApproval(ctx, tx, s.events, id, number, rendered.Markdown, tree, *newest, s.serverURL, decisionStepCases{}, eventSink(ctx)); err != nil {
			return nil, err
		}
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return nil, err
	}
	return ledger.events, nil
}

// commitPass ends the transaction tx of a reconciliation's pass (indexOwedDocument, indexOwedRoom,
// the repair's restate): the write phase of ledger, the pass's (runQueue), which writes the row of
// each version the pass stated and marks each it found mismatched; then, in tx and after those
// rows, the carry of R14 to each version whose row took its time from it (carryFirstStated); then
// beforeCommit, where it is given, and the commit, after which it publishes the events the carry
// appended.
func (s *Service) commitPass(ctx context.Context, tx pgx.Tx, ledger *Ledger, beforeCommit func() error) error {
	if err := ledger.runQueue(ctx, nil); err != nil {
		return err
	}
	carried, err := s.carryFirstStated(ctx, tx, ledger.firstStated)
	if err != nil {
		return err
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, event := range carried {
		s.events.Publish(event)
	}
	return nil
}

// The two ways the repair of a version marked tree_mismatch takes, which its log line names.
const (
	treeMismatchRestated = "restated"
	treeMismatchClosed   = "closed"
)

// markedVersion is a version whose version_renders row a pass marked tree_mismatch, as the repair
// lists it (treeMismatchesStatement): the version, its reference, <ref_key>@v<n>, which the repair's
// log line names, and the issue room its document is edited in, "" for a document that is its own.
type markedVersion struct {
	version statedVersion
	ref     string
	room    string
}

// treeMismatchesStatement lists at most $1 versions marked tree_mismatch, by document and number
// (the partial index version_renders_tree_mismatch): each one's document, number and reference,
// and the issue room its document is edited in - its document_rooms row's room, or the document
// itself where it is a room's tree artifact (issue_trees), as owedDocuments lists a document's
// room - and "" for any other document.
const treeMismatchesStatement = `
	select r.artifact_id::text, r.number, a.ref_key || '@v' || r.number,
	       coalesce((select d.room::text from document_rooms d where d.artifact_id = r.artifact_id),
	                (select t.artifact_id::text from issue_trees t where t.artifact_id = r.artifact_id), '')
	from version_renders r
	join artifacts a on a.id = r.artifact_id
	where r.tree_mismatch
	order by r.artifact_id, r.number
	limit $1`

// clearTreeMismatchStatement clears the mark of version $2 of the document $1 and sets
// tree_mismatch_repaired_at to the time of the repair's transaction, either way the repair takes:
// where it closes the mark the row keeps no time, no digest and no seq, and the version answers its
// stored bytes for good; where it restates the version the statement runs in the write phase
// before the pass's rows are written (writeStatedRenders), which then give the row its time, its
// seq and its record's digest.
const clearTreeMismatchStatement = `
	update version_renders set tree_mismatch = false, tree_mismatch_repaired_at = now()
	where artifact_id = $1 and number = $2 and tree_mismatch`

// repairTreeMismatches is the repair each tick of the block-index reconciliation begins with
// (ReconcileBlockIndex, R15), over at most blockIndexDocumentBatch of the rows marked tree_mismatch
// when the tick begins (treeMismatchesStatement), so a mark is repaired one tick after the pass
// that set it at the earliest. Each one's document, or for a document of an issue room its room, is
// repaired in a transaction of its own under the owner lock (repairTreeMismatch). A version it
// cannot repair is logged and keeps its mark for a later tick, and holds back none of the others.
func (s *Service) repairTreeMismatches(ctx context.Context) error {
	rows, err := s.store.Pool.Query(ctx, treeMismatchesStatement, blockIndexDocumentBatch)
	if err != nil {
		return fmt.Errorf("list the versions marked tree_mismatch: %w", err)
	}
	marked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (markedVersion, error) {
		var version markedVersion
		err := row.Scan(&version.version.artifactID, &version.version.number, &version.ref, &version.room)
		return version, err
	})
	if err != nil {
		return fmt.Errorf("list the versions marked tree_mismatch: %w", err)
	}
	for _, version := range marked {
		err := s.repairTreeMismatch(ctx, version)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			slog.Error("dispatch: block index could not repair a version marked tree_mismatch; its mark stands",
				"version", version.ref, "error", err)
		}
	}
	return nil
}

// repairTreeMismatch repairs marked, a version marked tree_mismatch, in one of two ways, or leaves
// its mark where neither applies yet: for a document of an issue room over its room
// (repairRoomMismatch), and for any other document over the document alone
// (repairDocumentMismatch). A room no document has joined yet is its tree artifact alone, repaired
// as a document, as indexOwedRoom indexes one.
func (s *Service) repairTreeMismatch(ctx context.Context, marked markedVersion) error {
	if marked.room == "" {
		return s.repairDocumentMismatch(ctx, marked)
	}
	read, err := readDocumentRoom(ctx, s.store.Pool, marked.room)
	if err != nil {
		return err
	}
	if len(read.layout) == 0 {
		return s.repairDocumentMismatch(ctx, marked)
	}
	return s.repairRoomMismatch(ctx, marked, read.layout)
}

// logTreeMismatchRepaired logs the one line the repair of a version marked tree_mismatch writes,
// with the version's reference and the way it took (treeMismatchRestated, treeMismatchClosed).
func logTreeMismatchRepaired(ref, way string) {
	slog.Info("dispatch: block index repaired a version marked tree_mismatch", "version", ref, "way", way)
}

// documentMismatchStatement reads, under the document owner's lock, what the repair of version $2
// of the document $1 decides on (repairDocumentMismatch): whether its row still holds
// tree_mismatch; the version the document's blocks stand at (document_block_versions.version),
// null where no pass has stood them or a pass is owed; whether a pass is owed by either mark;
// whether the document's pending settlement stands (doc_settlements_pending); and the version's
// stored markdown and created_at.
const documentMismatchStatement = `
	select r.tree_mismatch, b.version, b.version is null or b.store_owed_at is not null,
	       exists (select 1 from doc_settlements_pending p where p.artifact_id = r.artifact_id),
	       v.markdown, v.created_at
	from version_renders r
	join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.number
	left join document_block_versions b on b.artifact_id = r.artifact_id
	where r.artifact_id = $1::uuid and r.number = $2`

// repairDocumentMismatch repairs marked, a version marked tree_mismatch of a document that is its
// own room, in a transaction holding the document owner's lock (lockArtifactOwner). It reads the
// document the server serves (TextWithBlocks), whose blocks lend their ids to the version's, then
// the drain and the count test of the passes (roomHoldsUnstoredUpdate), with no transaction held,
// then, under the lock, the stand it tests (documentMismatchStatement). Where a later version's
// pass has moved the document's blocks past the version it closes the mark (closeTreeMismatch): the
// revision fence refuses a pass at a version older than the one the blocks stand at, so no pass can
// state it again. Where the blocks still stand at the version, no pass is owed, no pending row
// stands and the room holds no update in flight, it restates the version from its stored markdown
// (restatedVersion), and only then owes the document its index (oweDocumentIndex), so the pass,
// which reads the stand after that, wins the fence at the version the blocks stand at: a repair
// that owed first would test no stand, since the fence admits a pass at any version over a null
// one. The pass is the reconciliation's (restateDocumentVersion), in the same transaction, after the
// statement that clears the mark, and it records the row's time, its seq and its record's digest
// and runs the carry of R14 (commitPass). Where neither way applies - an update in flight, a
// pending row, a pass owed, or a stored markdown that does not read back as its own bytes - it
// changes nothing, and the mark stands for a later tick.
func (s *Service) repairDocumentMismatch(ctx context.Context, marked markedVersion) error {
	id, number := marked.version.artifactID, marked.version.number
	live, liveBlocks, err := s.TextWithBlocks(ctx, id)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		live, liveBlocks = "", nil
	case err != nil:
		return err
	}
	inFlight := s.roomHoldsUnstoredUpdate(id)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	owner, _, err := lockArtifactOwner(queued, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var still, owed, pending bool
	var stand *int
	var stored *string
	var versionAt time.Time
	err = tx.QueryRow(queued, documentMismatchStatement, id, number).Scan(&still, &stand, &owed, &pending, &stored, &versionAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the version %s marked tree_mismatch: %w", marked.ref, err)
	}
	switch {
	case !still:
		return nil
	case stand != nil && *stand > number:
		return closeTreeMismatch(queued, tx, ledger, marked)
	case inFlight || pending || owed || stand == nil || *stand != number || stored == nil:
		return nil
	}
	indexed, err := indexedBlocks(queued, tx, id)
	if err != nil {
		return err
	}
	tree, markdown, offsets, err := restatedVersion(*stored, live, liveBlocks, indexed)
	if err != nil {
		slog.Warn("dispatch: block index cannot restate a version marked tree_mismatch: its stored markdown does not read back as its own bytes; the mark stands",
			"version", marked.ref, "error", err)
		return nil
	}
	if err := oweDocumentIndex(queued, tx, id, s.markingRule); err != nil {
		return err
	}
	if err := writeExec(queued, tx, "clear the tree_mismatch mark of "+marked.ref, clearTreeMismatchStatement, id, number); err != nil {
		return err
	}
	if err := s.restateDocumentVersion(queued, tx, owner, id, number, versionAt, markdown, offsetBlocks(offsets, nil), tree); err != nil {
		return err
	}
	if err := s.commitPass(ctx, tx, ledger, nil); err != nil {
		return err
	}
	logTreeMismatchRepaired(marked.ref, treeMismatchRestated)
	return nil
}

// closeTreeMismatch closes the mark of marked in tx, the repair's transaction under the owner
// lock, whose write phase ledger runs (clearTreeMismatchStatement), and commits.
func closeTreeMismatch(ctx context.Context, tx pgx.Tx, ledger *Ledger, marked markedVersion) error {
	if err := writeExec(ctx, tx, "close the tree_mismatch mark of "+marked.ref, clearTreeMismatchStatement, marked.version.artifactID, marked.version.number); err != nil {
		return err
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	logTreeMismatchRepaired(marked.ref, treeMismatchClosed)
	return nil
}

// roomMismatchStatement reads, under the issue room $1's owner lock, what its repair of version $3
// of its document $2 decides on beside the stands (repairRoomMismatch): the documents the room's
// layout holds, as indexOwedRoom re-reads them; whether that version's row still holds
// tree_mismatch; and whether the room's pending settlement stands, keyed by its tree artifact.
const roomMismatchStatement = `
	select array(select d.artifact_id::text from document_rooms d join artifacts a on a.id = d.artifact_id
	             where d.room = $1::uuid and a.retired_at is null order by 1),
	       coalesce((select r.tree_mismatch from version_renders r where r.artifact_id = $2::uuid and r.number = $3), false),
	       exists (select 1 from doc_settlements_pending p where p.artifact_id = $1::uuid)`

// roomStandsStatement reads, for each document the array $1 names, in its order, the stand its
// room's repair tests (repairRoomMismatch): its ref_key; whether it holds a document_block_versions
// row, the version that row's blocks stand at and whether a pass is owed it by either mark; whether
// it holds any markdown version; the stored markdown and created_at of the version its blocks stand
// at; and whether that version's row holds tree_mismatch.
const roomStandsStatement = `
	select a.ref_key, b.artifact_id is not null, b.version,
	       b.artifact_id is not null and (b.version is null or b.store_owed_at is not null),
	       exists (select 1 from artifact_versions h where h.artifact_id = s.id and h.markdown is not null),
	       v.markdown, v.created_at,
	       coalesce((select r.tree_mismatch from version_renders r where r.artifact_id = s.id and r.number = b.version), false)
	from unnest($1::uuid[]) with ordinality as s (id, position)
	join artifacts a on a.id = s.id
	left join document_block_versions b on b.artifact_id = s.id
	left join artifact_versions v on v.artifact_id = s.id and v.number = b.version
	order by s.position`

// roomStand is one document of an issue room as roomStandsStatement reads it.
type roomStand struct {
	refKey    string
	held      bool
	version   *int
	owed      bool
	versioned bool
	markdown  *string
	at        *time.Time
	marked    bool
}

// repairRoomMismatch repairs marked, a version marked tree_mismatch of a document of the issue
// room marked.room, of layout, over the room whole, in a transaction holding the room's owner lock
// (lockArtifactOwner on its tree artifact), as indexOwedRoom reconciles it. It reads the room the
// server serves (roomTree), each fragment of which lends its blocks' ids to the version's, then the
// drain and the count test of the passes (roomHoldsUnstoredUpdate), with no transaction held, then,
// under the lock, the stand of every document of the room (roomStandsStatement), before it owes any
// of them anything. Where a later version's pass has moved the marked document's blocks past the
// version it closes the mark (closeTreeMismatch). Where those blocks still stand at the version, no
// document of the room is owed a pass, no pending row stands and the room holds no update in
// flight, it restates the room: every fragment from the stored markdown of its document's version
// at its stand - the marked document's the marked version - read back with its blocks' ids
// (restatedVersion), a fragment whose document holds no version as empty, as indexDocumentInRoom
// takes one, and only where every one of those reads back as its own stored bytes; it then owes
// every document of the room its index (oweDocumentIndex), since the room's pass states every
// fragment and the fence admits a pass at the version a document's blocks stand at only while the
// document holds the mark, clears the mark of each document whose version at its stand is marked,
// the marked one's among them, and runs the reconciliation's pass over the room (stateRoom), which
// records each row's time, seq and digest and runs the carry of R14 (commitPass). Where neither way
// applies, or a stored markdown does not read back as its bytes, it changes nothing: it states no
// block of the room, marks no other version, and the mark stands.
func (s *Service) repairRoomMismatch(ctx context.Context, marked markedVersion, layout roomLayout) error {
	room := marked.room
	documents := layout.documents(room)
	live := make([]string, len(documents))
	liveBlocks := make([][]model.ArtifactBlock, len(documents))
	tree, counts, err := s.roomTree(ctx, room, layout)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
	case err != nil:
		return err
	default:
		for index, part := range layout.parts(tree, counts) {
			if markdown, offsets, err := renderFragment(part); err == nil {
				live[index], liveBlocks[index] = markdown, offsetBlocks(offsets, nil)
			}
		}
	}
	inFlight := s.roomHoldsUnstoredUpdate(room)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	owner, _, err := lockArtifactOwner(queued, tx, room)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var held []string
	var still, pending bool
	if err := tx.QueryRow(queued, roomMismatchStatement, room, marked.version.artifactID, marked.version.number).Scan(&held, &still, &pending); err != nil {
		return fmt.Errorf("read the room of the version %s marked tree_mismatch: %w", marked.ref, err)
	}
	at := slices.Index(documents, marked.version.artifactID)
	if !still || at < 0 || !slices.Equal(held, slices.Sorted(slices.Values(documents[1:]))) {
		return nil
	}
	rows, err := tx.Query(queued, roomStandsStatement, documents)
	if err != nil {
		return fmt.Errorf("read the stands of room %s: %w", room, err)
	}
	stands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (roomStand, error) {
		var stand roomStand
		err := row.Scan(&stand.refKey, &stand.held, &stand.version, &stand.owed, &stand.versioned, &stand.markdown, &stand.at, &stand.marked)
		return stand, err
	})
	if err != nil {
		return fmt.Errorf("read the stands of room %s: %w", room, err)
	}
	if len(stands) != len(documents) {
		return nil
	}
	if version := stands[at].version; version != nil && *version > marked.version.number {
		return closeTreeMismatch(queued, tx, ledger, marked)
	}
	if inFlight || pending || stands[at].version == nil || *stands[at].version != marked.version.number {
		return nil
	}
	versions := refs.Versions{}
	parts := make([]*pmdoc.Node, len(documents))
	markdowns := make([]string, len(documents))
	offsets := make([][]pmdoc.BlockOffset, len(documents))
	var restated []markedVersion
	for index, id := range documents {
		stand := stands[index]
		switch {
		case stand.owed, !stand.held && stand.versioned, stand.held && stand.version != nil && *stand.version > 0 && (stand.markdown == nil || stand.at == nil):
			return nil
		case !stand.held || *stand.version == 0:
			parts[index] = &pmdoc.Node{Type: "doc"}
			continue
		}
		number := *stand.version
		versions[id] = refs.Version{Number: number, At: *stand.at}
		if stand.marked {
			restated = append(restated, markedVersion{version: statedVersion{artifactID: id, number: number}, ref: stand.refKey + "@v" + fmt.Sprint(number), room: room})
		}
		if *stand.markdown == "" {
			parts[index] = &pmdoc.Node{Type: "doc"}
			continue
		}
		indexed, err := indexedBlocks(queued, tx, id)
		if err != nil {
			return err
		}
		if parts[index], markdowns[index], offsets[index], err = restatedVersion(*stand.markdown, live[index], liveBlocks[index], indexed); err != nil {
			slog.Warn("dispatch: block index cannot restate a version marked tree_mismatch: a fragment of its issue room does not read back as its own stored bytes; the mark stands",
				"version", marked.ref, "room", room, "fragment", id, "error", err)
			return nil
		}
	}
	for _, id := range documents {
		if err := oweDocumentIndex(queued, tx, id, s.markingRule); err != nil {
			return err
		}
	}
	for _, version := range restated {
		if err := writeExec(queued, tx, "clear the tree_mismatch mark of "+version.ref, clearTreeMismatchStatement, version.version.artifactID, version.version.number); err != nil {
			return err
		}
	}
	if err := s.stateRoom(queued, tx, owner, room, layout, documents, versions, parts, markdowns, offsets); err != nil {
		return err
	}
	if err := s.commitPass(ctx, tx, ledger, nil); err != nil {
		return err
	}
	for _, version := range restated {
		logTreeMismatchRepaired(version.ref, treeMismatchRestated)
	}
	return nil
}

// errRestatedOtherwise refuses the restate of a version whose stored markdown, read back, renders
// otherwise than its own bytes.
var errRestatedOtherwise = errors.New("docs: the version's stored markdown, read back, renders otherwise than its own bytes")

// blockSpan is a block of a rendering by its type and its range in it, which two parses of one
// markdown give the same block and no two blocks of one rendering share.
type blockSpan struct {
	typ      string
	from, to int
}

// restatedVersion is the tree the repair of a version marked tree_mismatch restates, with its
// rendering and its blocks' ranges in it (R15): stored, the version's stored markdown, read back
// (pmdoc.ParseRendering) and rendered with its blocks' offsets (documentMarkdownWithOffsets), each
// block under the id of the index row or the live block that accounts for it, as
// closedVersionBlocks gives them - live and liveBlocks the document, or the fragment, as the server
// serves it, indexed its index's rows - and every other block under the new id the read-back
// minted. Where stored is a writer's defect its blocks match no row and no live block, and each
// takes a new id. It refuses a markdown that does not read back, and one whose rendering is not its
// own bytes, byte for byte (errRestatedOtherwise).
func restatedVersion(stored, live string, liveBlocks []model.ArtifactBlock, indexed []indexedBlock) (*pmdoc.Node, string, []pmdoc.BlockOffset, error) {
	_, accounted, err := closedVersionBlocks(stored, live, liveBlocks, indexed)
	if err != nil {
		return nil, "", nil, err
	}
	tree, err := pmdoc.ParseRendering(stored)
	if err != nil {
		return nil, "", nil, err
	}
	markdown, offsets, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		return nil, "", nil, err
	}
	if markdown != stored {
		return nil, "", nil, errRestatedOtherwise
	}
	ids := make(map[blockSpan]string, len(accounted))
	for _, block := range accounted {
		ids[blockSpan{typ: block.Type, from: block.From, to: block.To}] = block.ID
	}
	renamed := make(map[string]string, len(accounted))
	for _, offset := range offsets {
		if id, held := ids[blockSpan{typ: offset.Type, from: offset.From, to: offset.To}]; held && id != offset.ID {
			renamed[offset.ID] = id
		}
	}
	if len(renamed) == 0 {
		return tree, markdown, offsets, nil
	}
	renameBlockIDs(tree, renamed)
	if markdown, offsets, err = documentMarkdownWithOffsets(tree); err != nil {
		return nil, "", nil, err
	}
	if markdown != stored {
		return nil, "", nil, errRestatedOtherwise
	}
	return tree, markdown, offsets, nil
}

// renameBlockIDs gives each block of tree whose id renamed names the id it maps it to.
func renameBlockIDs(tree *pmdoc.Node, renamed map[string]string) {
	if id, held := tree.Attrs[pmdoc.BlockIDAttr].(string); held {
		if to, named := renamed[id]; named {
			tree.Attrs[pmdoc.BlockIDAttr] = to
		}
	}
	for _, child := range tree.Children {
		renameBlockIDs(child, renamed)
	}
}

// revisedDocumentsStatement is each query document of the issue $1 - each document of the issue
// whose node is a query node (graphstore.QueryNode): its spec, Verification document and plan,
// which its room's fragments hold, and its decisions query node, which no room holds - where the
// issue has converted, an issue_trees row holding its tree, and nothing for any other issue: each
// one's id, the query of its node, its name, its newest version's number and markdown with the
// digest that version's version_renders row records, null where it holds no row or holds none yet,
// whether that row owes its digest, and whether the document's block index stands at that version:
// its document_block_versions row at that number, holding no mark of this build's
// (store_owed_at). They come in id order.
var revisedDocumentsStatement = `
	select d.id::text, n.attributes->>'` + graphstore.AttributeQuery + `', d.name, v.number, coalesce(v.markdown, ''), r.record_sha256,
	       coalesce(r.digest_owed, false), coalesce(b.version = v.number and b.store_owed_at is null, false)
	from issue_trees t
	join artifacts d on d.issue_key = t.issue_key and d.kind = 'doc' and d.retired_at is null
	join graph_nodes n on n.kind = '` + graphstore.KindArtifact + `' and n.id = d.ref_key and n.supertag = '` + graphstore.SupertagQuery + `'
	join lateral (
		select number, markdown from artifact_versions where artifact_id = d.id order by number desc limit 1
	) v on true
	left join version_renders r on r.artifact_id = d.id and r.number = v.number
	left join document_block_versions b on b.artifact_id = d.id
	where t.issue_key = $1
	order by d.id`

// errQueryDigestOwed refuses the drain of an issue one of whose query versions still owes its
// record digest (R7): the drain's fill runs first and did not fill it, and a drain past it would
// clear the mark over a version whose record no row holds yet. The mark stays for the tick, whose
// repair of lost fills runs before its drain (drainOwedQueryMarks).
var errQueryDigestOwed = errors.New("docs: a query version of the issue still owes its record digest, so its query mark stays")

// lockQueryRoomsStatement takes the lock of the room of the issue $1, its tree's (issue_trees),
// as lockDocumentRoom takes it, and takes nothing for an issue with no tree.
const lockQueryRoomsStatement = `select pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t where t.issue_key = $1`

// LockQueryRooms takes, in tx, the lock of every room a version of issueKey's query documents is
// written in (writeRevisions): the issue's own room, its tree's, whose stored history's head each
// version records (versionRoomHead), and nothing for an issue that has not converted. The drain
// takes it right after the issue's owner lock and before its first write (drainQueryMark), the
// order docs/persistence.go sets for a transaction that takes both an owner's lock and a room's:
// the owner's advisory lock, then the room's, then the owner row its writes take (an event's).
func LockQueryRooms(ctx context.Context, tx pgx.Tx, issueKey string) error {
	if _, err := tx.Exec(ctx, lockQueryRoomsStatement, issueKey); err != nil {
		return fmt.Errorf("lock the room of %s: %w", issueKey, err)
	}
	return nil
}

// queryDocument is one query document of a converted issue as revisedDocumentsStatement reads it:
// its id, the query whose record covers it, its name, its newest version's number and markdown
// with the digest that version's version_renders row records, whether that row owes it, and
// whether its block index stands at that version.
type queryDocument struct {
	id, query, name, markdown string
	number                    int
	digest                    *string
	owed                      bool
	indexed                   bool
}

// queryRevision is a version a query document is owed (planRevisions): the document as its newest
// version leaves it, the markdown the new version stores - the document's render at the cut, which
// for a record moved alone is the bytes of the version before it - and the query's record read at
// that cut; and, once the version is written (writeRevisions), its row and its node's facts.
type queryRevision struct {
	document queryDocument
	markdown string
	record   QueryRecord
	version  model.Version
	node     graphstore.Uncaused
}

// planRevisions reads, through tx, the version each query document of the converted issue
// issueKey is owed, and nothing for an issue that has not converted: the drain's read
// (drainMark), for a write that moved a query's record and wrote no version of it - a recorded
// result, its retraction, a move or a delete of a node, an issue room's settlement that moved a
// query whose document's fragment it did not change. It refuses while a query version of the issue
// owes its record digest (errQueryDigestOwed). It reads the store as the transaction holds it, at
// one cut of tx (refs.HeadCut): each query document's record (IssueTree.QueryRecord, the issue's
// tree walked once) and its render as of the cut's time (RenderAt; for the decisions query, the
// rendering of its answer, renderDecisions). A document whose newest version holds no
// version_renders row is left as it is: either no pass has stated that version yet, so the tree
// may not hold it (a deferred pass), and the pass that states it records its digest, or it is no
// query version this build's version write made (madeQueryVersion), which no row ever covers and
// which answers its stored bytes for good. A document whose record digests to its newest version's
// digest and whose render is that version's markdown is left too. Each other one is owed a version
// whose markdown is the render. It writes nothing.
func planRevisions(ctx context.Context, tx pgx.Tx, issueKey string) ([]queryRevision, error) {
	rows, err := tx.Query(ctx, revisedDocumentsStatement, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
	}
	var documents []queryDocument
	owed := false
	for rows.Next() {
		var document queryDocument
		if err := rows.Scan(&document.id, &document.query, &document.name, &document.number, &document.markdown, &document.digest, &document.owed, &document.indexed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
		}
		owed = owed || document.owed
		if document.digest != nil {
			documents = append(documents, document)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the query documents of %s: %w", issueKey, err)
	}
	if owed {
		return nil, fmt.Errorf("drain the queries of %s: %w", issueKey, errQueryDigestOwed)
	}
	if len(documents) == 0 {
		return nil, nil
	}
	cut, err := refs.HeadCut(ctx, tx)
	if err != nil {
		return nil, err
	}
	records := cutRecords{cut: cut}
	var revisions []queryRevision
	for _, document := range documents {
		record, err := records.record(ctx, tx, queryOf{issue: issueKey, query: document.query})
		if err != nil {
			return nil, err
		}
		rendering, err := RenderAt(ctx, tx, model.Actor{}, document.id, cut.At)
		if err != nil {
			return nil, err
		}
		if record.Digest != *document.digest || rendering.Markdown != document.markdown {
			revisions = append(revisions, queryRevision{document: document, markdown: rendering.Markdown, record: record})
		}
	}
	return revisions, nil
}

// writeRevisions writes, in tx, each of revisions (planRevisions) as a version by writer: its row,
// whose markdown is the render (queueVersionInsert), its version node (WriteVersionNode, whose owner
// names the query and its definition_version, ReadVersionOwner) and its version_renders row holding
// the record's digest and the cut's time and seq, as a pass records them (QueueVersionRender), so
// the census reads it as every rendered version, and never as deferred. A version that stores the
// bytes of the version before it, where the document's block index stood at that version, stands
// the index at the new version in the same write (queueRevisionStand). Each write is queued on the
// drain's ledger (drainQueryMark, write), which sends them all in its write phase. It records each
// version's row and its node's facts on its revision, for the drain to cause.
func (s *Service) writeRevisions(ctx context.Context, tx pgx.Tx, revisions []queryRevision, writer model.Actor) error {
	for index := range revisions {
		revised := &revisions[index]
		id := revised.document.id
		version, err := queueVersionInsert(ctx, tx, id, revised.markdown, []model.Actor{writer}, false, nil, nil, RenderStamp{})
		if err != nil {
			return err
		}
		owner, err := ReadVersionOwner(ctx, tx, id)
		if err != nil {
			return err
		}
		node, err := WriteVersionNode(ctx, tx, owner, version.Number, writer, version.CreatedAt)
		if err != nil {
			return err
		}
		record := revised.record
		stands := revised.document.indexed && revised.markdown == revised.document.markdown
		if err := write(ctx, tx, func(b *store.WriteBatch) error {
			if err := QueueVersionRender(b, id, version.Number, record); err != nil {
				return err
			}
			if stands {
				queueRevisionStand(b, id, version.Number, s.markingRule)
			}
			return nil
		}); err != nil {
			return err
		}
		revised.version, revised.node = version, node
	}
	return nil
}

// queueRevisionStand queues on b the stand of the block index of the document artifactID at
// version number, a version that stores the bytes of the version before it, whose block index
// stood at that version (writeRevisions): the revision fence's own statement
// (refs.RevisionFenceStatement) at the marking rule rule, as the index pass of one document stands
// its row (refs' BlockIndexing.queueStand). The version's insert, queued before it, owes the
// document its index (document_block_versions_owe, migration 0122), and the fence admits a stand
// over that owed row. The blocks the version before stood at are the new version's, byte for
// byte, so no pass is owed it, and the coverage read lists no document owed for it (api's
// owedDocumentsStatement) and still draws each missing check. A version whose rendering moved stays
// owed for the reconciliation. A stand the fence refuses fails the write, as the pass's does.
func queueRevisionStand(b *store.WriteBatch, artifactID string, number, rule int) {
	b.Queue(fmt.Sprintf("stand the blocks of %s indexed at version %d", artifactID, number),
		refs.RevisionFenceStatement, artifactID, number, rule).QueryRow(func(row pgx.Row) error {
		// The one column says the row was written; nothing reads its value.
		if err := row.Scan(nil); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return fmt.Errorf("the revision fence refused the stand of the blocks of %s at version %d, which stores the bytes of the version before it",
			artifactID, number)
	})
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
