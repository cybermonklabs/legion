package docs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// renderWho writes the render rig's tuples and reads its renders.
var renderWho = model.Actor{Kind: "user", ID: "alice"}

// The blocks of the render rig's spec, each as its text value holds it: the whole slice of the
// block, its children included and the blank line after it left out (D9 of this branch's plan).
// The memo is the issue's first child; the two criteria are authored nodes at the top; the two
// requirements are authored nodes under the first criterion in the tree, ranked 1a and 1B there,
// and in the text they stand after the second criterion, at the text ranks 1na and 1nB, which byte
// order takes B first; the box is a callout whose text holds the inner callout, a node placed by
// the text under it; the closing paragraph is the last top-level block. The check is a block of
// the Verification document under the first requirement, which no render of the spec takes. The
// intruder is a paragraph a test dates before the version written before the cutover.
const (
	renderMemo     = ":::callout{#memo kind=\"note\" title=\"\"}\nThe memo says why.\n:::"
	renderK1       = ":::criterion{#k1}\nI want the urgent list.\n:::"
	renderR1       = ":::requirement{#r1}\nAn urgent ask comes first.\n:::"
	renderR1Edited = ":::requirement{#r1}\nAn urgent ask comes first, always.\n:::"
	renderR2       = ":::requirement{#r2}\nAn ask keeps its urgency.\n:::"
	renderK2       = ":::criterion{#k2}\nI want it quick.\n:::"
	renderInner    = ":::callout{#inner kind=\"note\" title=\"\"}\nThe inner words.\n:::"
	renderBox      = "::::callout{#box kind=\"note\" title=\"\"}\nThe box holds:\n\n" + renderInner + "\n::::"
	renderP1       = "A closing paragraph."
	renderCheck    = ":::check{#c1}\nThe list reads the urgent asks first.\n:::"
	renderIntruder = "A paragraph dated before the cutover."
	// renderBefore is the version of the spec written before the cutover, which holds no row.
	renderBefore = "The words of the spec before the cutover.\n"
)

// joinRenderings is the rendering of the top-level renderings given, each joined to the next by
// one blank line, and the document's last line ended.
func joinRenderings(renderings ...string) string { return strings.Join(renderings, "\n\n") + "\n" }

// renderRig is the store the render steps share: DOC-1, converted into its room, whose spec a
// real settlement writes; DOC-2, converted too, whose spec's blocks are written as tuples straight
// into the store, version 2 of it written before the cutover and version 3 after, from the tree;
// and DOC-9, an issue that has not converted, whose blocks stand as the release before this one
// stores them.
type renderRig struct {
	database *store.Store
	settled  issueRoom
	room     issueRoom
	// tree is the ref_key of DOC-2's tree artifact, which the blocks of its room are addressed
	// under, and spec the ref_key of its spec, which each block's in_document names.
	tree, spec string
	// before is when version 2, the version before the cutover, was written; first the bytes the
	// first render of version 3 answered, and digest the digest of its record.
	before        time.Time
	first, digest string
	// settledRows is how many version_renders rows DOC-1's settlement left: the rows its room pass
	// recorded for the newest version of each of DOC-1's query documents, which the census of the
	// rendered versions reads beside DOC-2's.
	settledRows int64
}

// address is the store address of block id of DOC-2's room.
func (r *renderRig) address(id string) string { return r.tree + "#" + id }

// now is the database's clock, clock_timestamp(), which dates every tuple the rig writes, so the
// order of the rig's writes and of the versions' times is the database's own.
func (r *renderRig) now(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	if err := r.database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&at); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return at
}

// write appends tuples straight into the store, in one transaction.
func (r *renderRig) write(t *testing.T, tuples ...graphstore.Tuple) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(context.Background(), tx, nil, tuples)
	}); err != nil {
		t.Fatalf("write %+v: %v", tuples, err)
	}
}

// relation is a tuple out of block id of DOC-2's room, at.
func (r *renderRig) relation(id, kind, toKind, toID string, asserted bool, at time.Time) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: r.address(id), Kind: kind, ToKind: toKind, ToID: toID,
		At: at, Who: renderWho, SourceEvent: graphstore.NoEvent, Asserted: asserted,
	}
}

// value is the value tuple of attribute on block id of DOC-2's room, at.
func (r *renderRig) value(id, attribute, value string, at time.Time) graphstore.Tuple {
	return r.relation(id, attribute, graphstore.KindValue, value, true, at)
}

// block is every tuple a pass states of block id of document, at: its in_document, its part_of
// to parent, its rank, its text_parent to textParent, its text_rank, its text and its placement.
func (r *renderRig) block(id, document string, parent, textParent refs.Target, rank, textRank, text, placement string, at time.Time) []graphstore.Tuple {
	return []graphstore.Tuple{
		r.relation(id, refs.TupleInDocument, graphstore.KindArtifact, document, true, at),
		r.relation(id, graphstore.TuplePartOf, parent.Kind, parent.ID, true, at),
		r.value(id, refs.AttributeRank, rank, at),
		r.relation(id, refs.TupleTextParent, textParent.Kind, textParent.ID, true, at),
		r.value(id, refs.AttributeTextRank, textRank, at),
		r.value(id, graphstore.AttributeText, text, at),
		r.value(id, refs.AttributePlacement, placement, at),
	}
}

// document is the node of DOC-2's spec, the text parent of each of its top-level blocks.
func (r *renderRig) document() refs.Target {
	return refs.Target{Kind: graphstore.KindArtifact, ID: r.spec}
}

// issue is DOC-2's node, and under the block id of its room.
func (r *renderRig) issue() refs.Target {
	return refs.Target{Kind: graphstore.KindIssue, ID: r.room.issue}
}
func (r *renderRig) under(id string) refs.Target {
	return refs.Target{Kind: refs.BlockNodeKind, ID: r.address(id)}
}

// renderVersion is RenderVersion of version number of artifactID.
func (r *renderRig) renderVersion(t *testing.T, artifactID string, number int) Rendering {
	t.Helper()
	rendering, err := RenderVersion(context.Background(), r.database.Pool, renderWho, artifactID, number)
	if err != nil {
		t.Fatalf("render version %d of %s: %v", number, artifactID, err)
	}
	return rendering
}

// renderAt is RenderAt of artifactID as of at.
func (r *renderRig) renderAt(t *testing.T, artifactID string, at time.Time) Rendering {
	t.Helper()
	rendering, err := RenderAt(context.Background(), r.database.Pool, renderWho, artifactID, at)
	if err != nil {
		t.Fatalf("render %s as of %s: %v", artifactID, at, err)
	}
	return rendering
}

// requireVersionFromTree fails unless version 3 of the spec, which stores its first render, renders
// from the tree as want, naming the first byte that differs.
func (r *renderRig) requireVersionFromTree(t *testing.T, what, want string) {
	t.Helper()
	if got := r.renderVersion(t, r.room.spec, 3); got.Source != RenderedFromTree || got.Markdown != want {
		t.Errorf("the render of %s@v3 differs from its stored bytes from byte %d %s: it renders %q from %q, want %q from the tree",
			r.spec, firstDifference(got.Markdown, want), what, got.Markdown, got.Source, want)
	}
}

// requireHead fails unless the spec, rendered as of the database's clock now, reads want.
func (r *renderRig) requireHead(t *testing.T, what, want string) {
	t.Helper()
	if got := r.renderAt(t, r.room.spec, r.now(t)); got.Source != RenderedFromTree || got.Markdown != want {
		t.Errorf("%s: the head renders %q from %q, want %q from the tree", what, got.Markdown, got.Source, want)
	}
}

// versionDigest is artifact_version_text_sha256 of version number of artifactID.
func (r *renderRig) versionDigest(t *testing.T, artifactID string, number int) string {
	t.Helper()
	var digest *string
	if err := r.database.Pool.QueryRow(context.Background(), `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, number).Scan(&digest); err != nil {
		t.Fatalf("read the digest of version %d of %s: %v", number, artifactID, err)
	}
	if digest == nil {
		return ""
	}
	return *digest
}

// renderSum is the lower-case hex sha256 of text.
func renderSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// writeVersionFromTree writes version number of the spec in tx as this child's writer writes one:
// the spec rendered as of the head's cut, stored as the version's markdown, and its version_renders
// row holding the digest of the spec's record read at that cut. It answers the rendering and the
// record.
func (r *renderRig) writeVersionFromTree(t *testing.T, tx pgx.Tx, number int) (Rendering, QueryRecord) {
	t.Helper()
	ctx := context.Background()
	cut, err := refs.HeadCut(ctx, tx)
	if err != nil {
		t.Fatalf("read the head's cut: %v", err)
	}
	rendering, err := RenderAt(ctx, tx, renderWho, r.room.spec, cut.At)
	if err != nil {
		t.Fatalf("render the spec at the head: %v", err)
	}
	record, err := ReadQueryRecord(ctx, tx, r.room.issue, graphstore.QuerySpec, cut)
	if err != nil {
		t.Fatalf("read the spec's record at the head: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')`, r.room.spec, number, rendering.Markdown); err != nil {
		t.Fatalf("write version %d of the spec: %v", number, err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		return QueueVersionRender(b, r.room.spec, number, record)
	}); err != nil {
		t.Fatalf("write the version_renders row of version %d: %v", number, err)
	}
	return rendering, record
}

// versionRows is how many artifact_versions and version_renders rows version number of the spec
// holds.
func (r *renderRig) versionRows(t *testing.T, q Queryer, number int) (versions, renders int) {
	t.Helper()
	if err := q.QueryRow(context.Background(), `
		select (select count(*) from artifact_versions where artifact_id = $1 and number = $2),
		       (select count(*) from version_renders where artifact_id = $1 and number = $2)`,
		r.room.spec, number).Scan(&versions, &renders); err != nil {
		t.Fatalf("count version %d's rows: %v", number, err)
	}
	return versions, renders
}

// writeUnconvertedIssue writes by SQL, as the release before this one stores them, the spec of
// DOC-9, an issue with no issue_trees row and no document_rooms row: one version, the rows of its
// two blocks in document_blocks and document_block_texts, and their nodes, which hold no text
// value; the document indexed at that version by a pass of rule 1, so it owes no index, and stated
// by no pass of rule 2. It answers how many block rows it wrote.
func writeUnconvertedIssue(t *testing.T, database *store.Store) int {
	t.Helper()
	ctx := context.Background()
	words := []string{"The first words of an issue that has not converted.", "Its second words."}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('DOC-9', 'DOC', 9, 'An issue that has not converted', '{"kind":"user","id":"alice"}', 'W')`); err != nil {
			return err
		}
		var id, refKey string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DOC-9', 'DOC', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key`).Scan(&id, &refKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')`, id, joinRenderings(words...)); err != nil {
			return err
		}
		for index, text := range words {
			block := "u" + string(rune('1'+index))
			address := refKey + "#" + block
			if _, err := tx.Exec(ctx, `
				insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
				values ('block', $1, 'block', 'project', 'live', now(), '{"kind":"user","id":"alice"}', 0)`, address); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, node_kind, node_address)
				values ($1, $2, $3, 'paragraph', $4, $5, 1, now(), 'block', $3)`, id, block, address, renderSum(text), text); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				insert into document_block_texts (artifact_id, block_id, version, text) values ($1, $2, 1, $3)`, id, block, text); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			insert into document_block_versions (artifact_id, version, store_version) values ($1, 1, 1)
			on conflict (artifact_id) do update
			set version = 1, store_version = 1, store_owed_at = null, stated_version = null, stated_rule = null`, id)
		return err
	}); err != nil {
		t.Fatalf("write the issue that has not converted: %v", err)
	}
	return len(words)
}

// writeQueryNode writes the node of the document slug of issue, in project, as the query node of
// query an issue born converted holds (graphstore.QueryNode, as api's create and upload write it),
// part of the issue, and names it on the document's row where that row stands: a version this
// build then writes of the document is a query version, holding its query's definition_version,
// which a query's record covers. A test that writes a document by SQL, which writes no node, calls
// it before the document's first version that should take a version_renders row.
func writeQueryNode(t *testing.T, database *store.Store, project, issue, slug, query string) {
	t.Helper()
	ctx := context.Background()
	refKey := graphstore.ArtifactRefKey(issue, project, "", slug)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		at, err := graphstore.StatementTime(ctx, tx, graphstore.KindArtifact, refKey)
		if err != nil {
			return err
		}
		node := graphstore.Created(graphstore.QueryNode(refKey, project, query, renderWho, at))
		facts := graphstore.AttributeFacts([]graphstore.Tuple{graphstore.ArtifactPartOf(refKey, issue, renderWho, at)}, node)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, facts); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update artifacts set node_kind = $2, node_address = $1 where ref_key = $1`, refKey, graphstore.KindArtifact)
		return err
	}); err != nil {
		t.Fatalf("write %s as the %s query's node: %v", refKey, query, err)
	}
}

// writeRoomQueryNodes writes the node of each of room's spec, Verification document and plan as
// the query node of its query (writeQueryNode), as an issue born converted holds them:
// createIssueRoom writes the rows by SQL with no node.
func writeRoomQueryNodes(t *testing.T, database *store.Store, room issueRoom) {
	t.Helper()
	writeQueryNode(t, database, "DOC", room.issue, "spec", graphstore.QuerySpec)
	writeQueryNode(t, database, "DOC", room.issue, model.ArtifactSlugVerification, graphstore.QueryVerification)
	writeQueryNode(t, database, "DOC", room.issue, model.ArtifactSlugPlan, graphstore.QueryPlan)
}

// A version this child's writer wrote renders again from the tree, in the text order its author
// wrote. The test writes a
// subtree of DOC-2's spec straight into the store, writes version 3 from it - its rendering and its
// version_renders row, holding the digest of the spec's record, in one transaction - and then, one
// change at a time, edits a requirement's text, re-parents another, re-orders the text of two
// top-level siblings and the tree ranks of two others, and retracts a paragraph's in_document,
// each dated after the version: the version renders its first bytes after each, the head renders
// each change that moves the text and none that moves only the tree, and two renders of the
// version at two times read one byte string. The render prints the document's text children, each
// once, by its text value, in increasing text_rank, a text_rank of a and one of B taking B first,
// byte for byte; it refuses to run where select 'a' < 'B' is false. A transaction rolled back
// leaves neither the version nor its row. The digest of a version with a row is its record's, and
// of one without its stored markdown's. A tuple dated before the version written before the
// cutover moves nothing of it, though the tree at its time holds that tuple; it moves the version
// that holds a row, which the census of the versions then reads as a difference; and with that row
// deleted the version answers its stored bytes and says so. A version whose markdown is null
// says it holds no bytes. The head of a
// spec a real settlement wrote renders as that version's stored markdown, and that settlement's
// room pass recorded the version's row, holding its record's digest, which the census reads beside
// DOC-2's with no difference. The census of document_block_texts reads no difference in either
// direction over the converted issues, and prints the rows of DOC-9 as the class of unstated rows
// of an issue that has not converted, with their count; and document_block_texts keeps the columns
// 0121_block_graph.up.sql creates.
//
// Then the versions the real version path writes, each holding the row its pass recorded with its
// record's digest. Five versions of the spec of CORE-1 each render their stored bytes
// (renderStepTextOrder): a requirement under the criterion before the paragraph in the tree prints
// after the paragraph, where its text stands; each member prints once at its place in the text,
// whether its parent is a block of another document, an ask, a criterion the version dropped or
// the callout holding it; two members of one text each print at their own place; and a member's
// place in the text is read as of the row's time. And the versions an issue room's settlement
// writes each render their stored bytes, each block's text value being its slice of its own
// fragment's rendering (renderStepFragments).
func TestTheTreeRendersAPostCutoverVersionFromItsTuples(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'a' < 'B'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of a and B: %v", err)
	}
	if !collated {
		t.Fatal("this database orders 'B' before 'a' by its own collation, so it cannot tell a render that orders ranks byte for byte from one that forgot to: run this test where select 'a' < 'B' is true")
	}
	service, server := newTestServiceInstance(t, database)
	rig := &renderRig{database: database, settled: createIssueRoom(t, database, 1), room: createIssueRoom(t, database, 2)}
	if err := database.Pool.QueryRow(ctx, `
		select (select ref_key from artifacts where id = $1), (select ref_key from artifacts where id = $2)`,
		rig.room.tree, rig.room.spec).Scan(&rig.tree, &rig.spec); err != nil {
		t.Fatalf("read DOC-2's ref_keys: %v", err)
	}

	// DOC-1's spec, written by a real settlement of its room once the peer's update has landed. Its
	// documents are query nodes, as a converted issue's are, so the version the settlement writes
	// is a query version, which takes the row its room pass records.
	writeRoomQueryNodes(t, database, rig.settled)
	peer := connectPeer(t, server.URL, rig.settled.spec, "alice")
	stored := roomUpdateCount(t, database, rig.settled.tree)
	if _, err := peer.Send(writeParagraph(peer.Doc, rig.settled.spec, "Spec words.")); err != nil {
		t.Fatalf("write into DOC-1's spec: %v", err)
	}
	if err := peer.WaitForText(fragmentMarkdown(rig.settled.spec), "Spec words.\n", roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of DOC-1's room: %v", err)
	}
	waitForStoredUpdates(t, database, rig.settled.tree, stored)
	if err := service.waitForDurableAppends(ctx, rig.settled.tree); err != nil {
		t.Fatalf("wait for DOC-1's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, rig.settled.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle DOC-1's room: %v", err)
	}
	// The room pass of DOC-1's settlement records the row of the newest version of each of its query
	// documents, the spec's among them, and the census reads each as its stored bytes.
	settledSpec := newestVersionNumber(t, database, rig.settled.spec)
	requireStatedRow(t, database, rig.settled.issue, graphstore.QuerySpec, rig.settled.spec, settledSpec)
	settledRenders := rig.readRenderCensus(t)
	if settledRenders.Versions == 0 || settledRenders.Differences != 0 {
		t.Fatalf("after DOC-1's settlement the census of the rendered versions compares %d and reads %d differences (%+v), want DOC-1's versions and none",
			settledRenders.Versions, settledRenders.Differences, settledRenders.Reported)
	}
	rig.settledRows = settledRenders.Versions
	unconverted := writeUnconvertedIssue(t, database)

	// DOC-2: version 2 of the spec, written before the cutover, and then the spec's subtree.
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
		values ($1, 2, $2, '[{"kind":"user","id":"alice"}]', clock_timestamp()) returning created_at`,
		rig.room.spec, renderBefore).Scan(&rig.before); err != nil {
		t.Fatalf("write the version before the cutover: %v", err)
	}
	at := rig.now(t)
	var subtree []graphstore.Tuple
	spec := rig.document()
	for _, block := range []struct {
		id, document       string
		parent, textParent refs.Target
		rank, textRank     string
		text, how          string
	}{
		{"memo", rig.spec, rig.issue(), spec, "1c", "1c", renderMemo, refs.PlacementText},
		{"k1", rig.spec, rig.issue(), spec, "1g", "1g", renderK1, refs.PlacementAuthored},
		{"r1", rig.spec, rig.under("k1"), spec, "1a", "1na", renderR1, refs.PlacementAuthored},
		{"r2", rig.spec, rig.under("k1"), spec, "1B", "1nB", renderR2, refs.PlacementAuthored},
		{"k2", rig.spec, rig.issue(), spec, "1m", "1m", renderK2, refs.PlacementAuthored},
		{"box", rig.spec, rig.issue(), spec, "1p", "1p", renderBox, refs.PlacementText},
		{"inner", rig.spec, rig.under("box"), rig.under("box"), "1q", "1q", renderInner, refs.PlacementText},
		{"p1", rig.spec, rig.issue(), spec, "1t", "1t", renderP1, refs.PlacementText},
		{"c1", rig.room.issue + "/verification-md", rig.under("r1"),
			refs.Target{Kind: graphstore.KindArtifact, ID: rig.room.issue + "/verification-md"}, "3a", "3a", renderCheck, refs.PlacementAuthored},
	} {
		subtree = append(subtree, rig.block(block.id, block.document, block.parent, block.textParent, block.rank, block.textRank, block.text, block.how, at)...)
	}
	rig.write(t, subtree...)

	for _, step := range []struct {
		name string
		run  func(t *testing.T, rig *renderRig)
	}{
		{"a version written from the tree holds its row, written in the version's own transaction", renderStepVersionRow},
		{"a text edit after the version leaves the version's text as it stood", renderStepText},
		{"a re-parent after the version moves no byte of a render", renderStepParent},
		{"a re-order after the version leaves the version's text ranks as they stood", renderStepRanks},
		{"an in_document retracted after the version leaves the node in it", renderStepRetraction},
		{"two renders of the version at two times read one byte string, and the head is the renderer at now", renderStepTwoTimes},
		{"the digest of a version with a row is its record's, and of one without its stored markdown's", renderStepDigests},
		{"the census reads the converted issues' texts both ways and the unconverted issue's rows as a class", func(t *testing.T, rig *renderRig) {
			renderStepCensus(t, rig, unconverted)
		}},
		{"a tuple dated before the version written before the cutover moves nothing of it", renderStepBoundary},
		{"with its row deleted a version answers its stored bytes and says so", renderStepRowDeleted},
		{"a version whose markdown is null says it holds no bytes", renderStepNoBytes},
		{"document_block_texts keeps the columns 0121_block_graph creates", renderStepTextColumns},
		{"each version the real path writes renders in the text order its author wrote", func(t *testing.T, rig *renderRig) {
			renderStepTextOrder(t, rig, service)
		}},
		{"each version an issue room's settlement writes renders its own fragment's bytes", func(t *testing.T, rig *renderRig) {
			renderStepFragments(t, rig, service, server.URL)
		}},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(t, rig) })
	}
}

// renderFirst is the rendering of version 3, the spec's subtree as written, in the order of the
// text ranks: the requirements after the second criterion, the one of text rank 1nB before the one
// of 1na, and the inner callout once, inside the box. The tree's order would print the two
// requirements under the first criterion, before the second.
var renderFirst = joinRenderings(renderMemo, renderK1, renderK2, renderR2, renderR1, renderBox, renderP1)

func renderStepVersionRow(t *testing.T, rig *renderRig) {
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	rendering, record := rig.writeVersionFromTree(t, tx, 3)
	if rendering.Source != RenderedFromTree || rendering.Markdown != renderFirst {
		t.Fatalf("the head renders %q from %q, want %q from the tree", rendering.Markdown, rendering.Source, renderFirst)
	}
	if strings.Index(rendering.Markdown, renderR2) > strings.Index(rendering.Markdown, renderR1) {
		t.Errorf("the render takes r1 (text rank 1na) before r2 (text rank 1nB), want B first, byte for byte")
	}
	if strings.Count(rendering.Markdown, renderInner) != 1 {
		t.Errorf("the render holds the inner callout %d times, want once, inside its box", strings.Count(rendering.Markdown, renderInner))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit version 3: %v", err)
	}
	rig.digest = record.Digest
	var digest string
	var at time.Time
	var readSeq int64
	if err := rig.database.Pool.QueryRow(ctx, `
		select record_sha256, at, read_seq from version_renders where artifact_id = $1 and number = 3`, rig.room.spec).Scan(&digest, &at, &readSeq); err != nil {
		t.Fatalf("read version 3's row: %v", err)
	}
	if digest != record.Digest || !at.Equal(record.Cut.At) || readSeq != record.Cut.ReadSeq {
		t.Errorf("version 3's row holds %s at %s and seq %d, want the record's %s at %s and seq %d",
			digest, at, readSeq, record.Digest, record.Cut.At, record.Cut.ReadSeq)
	}
	if renderSum(renderFirst) == record.Digest {
		t.Errorf("the record's digest is the sha256 of the version's markdown, %s", record.Digest)
	}

	// The same write rolled back leaves neither the version nor its row.
	rolled, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rolled back version's transaction: %v", err)
	}
	rig.writeVersionFromTree(t, rolled, 4)
	if versions, renders := rig.versionRows(t, rolled, 4); versions != 1 || renders != 1 {
		t.Errorf("inside its transaction version 4 holds %d versions and %d rows, want one of each", versions, renders)
	}
	if err := rolled.Rollback(ctx); err != nil {
		t.Fatalf("roll version 4 back: %v", err)
	}
	if versions, renders := rig.versionRows(t, rig.database.Pool, 4); versions != 0 || renders != 0 {
		t.Errorf("after its rollback version 4 holds %d versions and %d rows, want neither", versions, renders)
	}
	rig.first = rig.renderVersion(t, rig.room.spec, 3).Markdown
	rig.requireVersionFromTree(t, "as written", renderFirst)
}

func renderStepText(t *testing.T, rig *renderRig) {
	rig.write(t, rig.value("r1", graphstore.AttributeText, renderR1Edited, rig.now(t)))
	rig.requireVersionFromTree(t, "after r1's text edit", renderFirst)
	rig.requireHead(t, "after r1's text edit", joinRenderings(renderMemo, renderK1, renderK2, renderR2, renderR1Edited, renderBox, renderP1))
}

// The render of a document reads no part_of: a node moved under another parent stands at one
// place in the text in both renders.
func renderStepParent(t *testing.T, rig *renderRig) {
	at := rig.now(t)
	rig.write(t,
		rig.relation("r2", graphstore.TuplePartOf, refs.BlockNodeKind, rig.address("k1"), false, at),
		rig.relation("r2", graphstore.TuplePartOf, refs.BlockNodeKind, rig.address("k2"), true, at))
	rig.requireVersionFromTree(t, "after r2 moved under k2", renderFirst)
	rig.requireHead(t, "after r2 moved under k2", joinRenderings(renderMemo, renderK1, renderK2, renderR2, renderR1Edited, renderBox, renderP1))
}

// A re-order of the text moves the head and leaves the version as it stood, whose text ranks are
// the ones its members held at its row's time; a re-order of the tree's ranks moves neither.
func renderStepRanks(t *testing.T, rig *renderRig) {
	ctx := context.Background()
	at := rig.now(t)
	rig.write(t,
		rig.value("box", refs.AttributeTextRank, "1t", at), rig.value("p1", refs.AttributeTextRank, "1p", at),
		rig.value("memo", refs.AttributeRank, "1g", at), rig.value("k1", refs.AttributeRank, "1c", at))
	rig.requireVersionFromTree(t, "after the box and the closing paragraph swapped text ranks", renderFirst)
	rig.requireHead(t, "after the box and the closing paragraph swapped text ranks",
		joinRenderings(renderMemo, renderK1, renderK2, renderR2, renderR1Edited, renderP1, renderBox))
	var rendered time.Time
	if err := rig.database.Pool.QueryRow(ctx, `select at from version_renders where artifact_id = $1 and number = 3`, rig.room.spec).Scan(&rendered); err != nil {
		t.Fatalf("read version 3's row: %v", err)
	}
	box, p1 := rig.under("box"), rig.under("p1")
	facts, err := refs.TreeFactsOf(ctx, rig.database.Pool, []refs.Target{box, p1}, refs.TreeCut{At: rendered, Dated: true})
	if err != nil {
		t.Fatalf("read the box and the closing paragraph as of version 3: %v", err)
	}
	if held := []string{facts[box].Values[refs.AttributeTextRank], facts[p1].Values[refs.AttributeTextRank]}; !slices.Equal(held, []string{"1p", "1t"}) {
		t.Errorf("as of version 3's row the box and the closing paragraph hold the text ranks %v, want the 1p and 1t they held then", held)
	}
}

// An in_document retracted after the version leaves the member in the version, and takes it out
// of the head.
func renderStepRetraction(t *testing.T, rig *renderRig) {
	rig.write(t, rig.relation("p1", refs.TupleInDocument, graphstore.KindArtifact, rig.spec, false, rig.now(t)))
	rig.requireVersionFromTree(t, "after the closing paragraph's in_document was retracted", renderFirst)
	rig.requireHead(t, "after the closing paragraph's in_document was retracted",
		joinRenderings(renderMemo, renderK1, renderK2, renderR2, renderR1Edited, renderBox))
}

func renderStepTwoTimes(t *testing.T, rig *renderRig) {
	if again := rig.renderVersion(t, rig.room.spec, 3).Markdown; again != rig.first || rig.first != renderFirst {
		t.Errorf("version 3 rendered after every change reads %q, and first read %q, want one byte string, %q", again, rig.first, renderFirst)
	}
	var stored string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1 and number = 3`, rig.room.spec).Scan(&stored); err != nil {
		t.Fatalf("read version 3's markdown: %v", err)
	}
	if stored != rig.first {
		t.Errorf("version 3 stores %q, want its own render, %q", stored, rig.first)
	}
	// The head of DOC-1's spec, which a real settlement indexed, is that version's markdown.
	var settled string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`, rig.settled.spec).Scan(&settled); err != nil {
		t.Fatalf("read DOC-1's newest spec version: %v", err)
	}
	if settled != "Spec words.\n" {
		t.Fatalf("DOC-1's newest spec version holds %q, want the settlement's \"Spec words.\\n\"", settled)
	}
	if head := rig.renderAt(t, rig.settled.spec, rig.now(t)); head.Source != RenderedFromTree || head.Markdown != settled {
		t.Errorf("the head of DOC-1's spec renders %q from %q, want its settled version's %q from the tree", head.Markdown, head.Source, settled)
	}
}

func renderStepDigests(t *testing.T, rig *renderRig) {
	ctx := context.Background()
	if got := rig.versionDigest(t, rig.room.spec, 2); got != renderSum(renderBefore) {
		t.Errorf("the version before the cutover digests to %s, want the sha256 of its stored markdown, %s", got, renderSum(renderBefore))
	}
	if got := rig.versionDigest(t, rig.room.spec, 3); got != rig.digest {
		t.Errorf("version 3 digests to %s, want its record's digest, %s", got, rig.digest)
	}
	for number, want := range map[int]string{2: renderSum(renderBefore), 3: rig.digest} {
		var pinned string
		if err := rig.database.Pool.QueryRow(ctx, `
			insert into artifact_reviews (artifact_id, version, state, actor)
			values ($1, $2, 'approved', '{"kind":"user","id":"alice"}') returning content_sha256`, rig.room.spec, number).Scan(&pinned); err != nil {
			t.Fatalf("approve version %d: %v", number, err)
		}
		if pinned != want {
			t.Errorf("an approval of version %d pins %s, want %s", number, pinned, want)
		}
	}
}

func renderStepCensus(t *testing.T, rig *renderRig, unconverted int) {
	census := treeCensus(t, rig.database)
	texts := treeCensusKind(t, census, "block_text")
	for _, difference := range slices.Concat(texts.ReportedRows, texts.ReportedNodes) {
		t.Errorf("the census reads a block_text difference at %s: %s", difference.At, difference.Cause)
	}
	if texts.RowDifferences != 0 || texts.NodeDifferences != 0 || texts.Rows == 0 || texts.Nodes == 0 {
		t.Errorf("the census reads %d rows and %d nodes of block_text with %d and %d differences, want the converted issues' blocks and none",
			texts.Rows, texts.Nodes, texts.RowDifferences, texts.NodeDifferences)
	}
	if !texts.CountsUnstated || texts.Unstated != int64(unconverted) {
		t.Errorf("the census counts %d unstated block_text rows of an issue that has not converted (held %t), want DOC-9's %d, and no other",
			texts.Unstated, texts.CountsUnstated, unconverted)
	}
	var printed bytes.Buffer
	census.Write(&printed)
	if line := "census: store kind=block_text unstated_rows=" + strconv.Itoa(unconverted) + " "; !strings.Contains(printed.String(), line) {
		t.Errorf("the census prints no line %q:\n%s", line, printed.String())
	}
	renders, err := ReadVersionRenderCensus(context.Background(), rig.database.Pool)
	if err != nil {
		t.Fatalf("read the census of the versions version_renders holds: %v", err)
	}
	if renders.Versions != rig.settledRows+1 || renders.Differences != 0 {
		t.Errorf("the census of the rendered versions compares %d and reads %d differences (%+v), want version 3 beside DOC-1's %d and none",
			renders.Versions, renders.Differences, renders.Reported, rig.settledRows)
	}
}

func renderStepBoundary(t *testing.T, rig *renderRig) {
	ctx := context.Background()
	rig.write(t, rig.block("intruder", rig.spec, rig.issue(), rig.document(), "1d", "1d", renderIntruder, refs.PlacementText, rig.before.Add(-time.Minute))...)
	if tree := rig.renderAt(t, rig.room.spec, rig.before); !strings.Contains(tree.Markdown, renderIntruder) {
		t.Fatalf("the tree as of the version before the cutover renders %q, without the paragraph dated before it, so this case proves nothing", tree.Markdown)
	}
	if got := rig.renderVersion(t, rig.room.spec, 2); got.Source != StoredBytes || got.Markdown != renderBefore {
		t.Errorf("the version before the cutover renders %q from %q, want its stored %q, the stored bytes", got.Markdown, got.Source, renderBefore)
	}
	// The version that holds a row renders from the tree, which now holds the paragraph dated
	// before it: the census of the rendered versions reads it.
	if got := rig.renderVersion(t, rig.room.spec, 3); got.Source != RenderedFromTree || !strings.Contains(got.Markdown, renderIntruder) {
		t.Errorf("version 3 renders %q from %q, want the tree's, with the paragraph dated before it", got.Markdown, got.Source)
	}
	renders, err := ReadVersionRenderCensus(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the census of the versions version_renders holds: %v", err)
	}
	if renders.Versions != rig.settledRows+1 || renders.Differences != 1 || len(renders.Reported) != 1 || !strings.Contains(renders.Reported[0].At, rig.spec+"@v3") {
		t.Errorf("the census of the rendered versions compares %d and reads %d differences (%+v), want version 3's beside DOC-1's %d versions", renders.Versions, renders.Differences, renders.Reported, rig.settledRows)
	}
}

func renderStepRowDeleted(t *testing.T, rig *renderRig) {
	ctx := context.Background()
	if _, err := rig.database.Pool.Exec(ctx, `delete from version_renders where artifact_id = $1 and number = 3`, rig.room.spec); err != nil {
		t.Fatalf("delete version 3's row: %v", err)
	}
	if got := rig.renderVersion(t, rig.room.spec, 3); got.Source != StoredBytes || got.Markdown != renderFirst {
		t.Errorf("version 3 with no row renders %q from %q, want its stored %q, the stored bytes", got.Markdown, got.Source, renderFirst)
	}
	if got := rig.versionDigest(t, rig.room.spec, 3); got != renderSum(renderFirst) {
		t.Errorf("version 3 with no row digests to %s, want the sha256 of its stored markdown, %s", got, renderSum(renderFirst))
	}
	renders, err := ReadVersionRenderCensus(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the census of the versions version_renders holds: %v", err)
	}
	if renders.Versions != rig.settledRows || renders.Differences != 0 {
		t.Errorf("the census of the rendered versions compares %d and reads %d differences, want DOC-1's %d and none: no version of DOC-2 holds a row", renders.Versions, renders.Differences, rig.settledRows)
	}
}

// readRenderCensus is the census of the versions version_renders holds.
func (r *renderRig) readRenderCensus(t *testing.T) graphcensus.VersionRenderCensus {
	t.Helper()
	renders, err := ReadVersionRenderCensus(context.Background(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the census of the versions version_renders holds: %v", err)
	}
	return renders
}

// The texts of the spec of CORE-1 that renderStepTextOrder writes through the real version path,
// each as its member's text value holds it: the first criterion, the paragraph after it, and the
// requirement after the paragraph whose parent attribute names the criterion; the second
// criterion, a requirement under the callout of the issue's Verification document, a box holding
// a requirement under the first criterion, and a requirement under the second criterion; and the
// paragraph and the rule a version repeats. orderR3 is the requirement under an ask of the issue.
const (
	orderK1        = ":::criterion{#k1}\nI want the urgent list.\n:::"
	orderParagraph = "A paragraph between the two."
	orderR1        = ":::requirement{#r1 parent=\"dispatch://CORE-1/spec#b-k1\"}\nAn urgent ask comes first.\n:::"
	orderK2        = ":::criterion{#k2}\nI want it quick.\n:::"
	orderR2        = ":::requirement{#r2 parent=\"dispatch://CORE-1/artifact/verification-md#b-c1\"}\nThe proof holds the list.\n:::"
	orderR4        = ":::requirement{#r4 parent=\"dispatch://CORE-1/spec#b-k1\"}\nA boxed ask comes first too.\n:::"
	orderBox       = "::::callout{#box kind=\"note\" title=\"\"}\nThe box holds:\n\n" + orderR4 + "\n::::"
	orderR5        = ":::requirement{#r5 parent=\"dispatch://CORE-1/spec#b-k2\"}\nA quick ask is answered first.\n:::"
	orderSame      = "Same words."
	orderRule      = "---"
	// orderProof is the issue's Verification document, whose callout r2 stands under.
	orderProof = ":::callout{#c1 kind=\"note\" title=\"\"}\nThe proof of the urgent list.\n:::\n"
)

func orderR3(ask string) string {
	return ":::requirement{#r3 parent=\"dispatch://CORE-1/ask/" + ask + "\"}\nThe asked ask comes first.\n:::"
}

// firstDifference is the first byte at which got and want differ, the shorter one's length where
// one is the other's prefix.
func firstDifference(got, want string) int {
	for index := range min(len(got), len(want)) {
		if got[index] != want[index] {
			return index
		}
	}
	return min(len(got), len(want))
}

// requireStatedRow fails unless version number of the document artifactID, a document of issue
// whose query is query, holds the version_renders row the pass that stated it wrote: the time and
// the seq of a cut, and the digest of the query's record read at that cut.
func requireStatedRow(t *testing.T, database *store.Store, issue, query, artifactID string, number int) {
	t.Helper()
	ctx := context.Background()
	var digest string
	var cut refs.TreeCut
	if err := database.Pool.QueryRow(ctx, `
		select record_sha256, at, read_seq from version_renders where artifact_id = $1 and number = $2`,
		artifactID, number).Scan(&digest, &cut.At, &cut.ReadSeq); err != nil {
		t.Fatalf("read the version_renders row the pass that stated version %d of %s wrote: %v", number, artifactID, err)
	}
	record, err := ReadQueryRecord(ctx, database.Pool, issue, query, cut)
	if err != nil {
		t.Fatalf("read the %s record of %s at the cut of version %d of %s: %v", query, issue, number, artifactID, err)
	}
	if digest != record.Digest {
		t.Errorf("the pass that stated version %d of %s recorded the digest %s, want its %s record's, %s", number, artifactID, digest, query, record.Digest)
	}
}

// newestVersionNumber is the number of the newest version of the document artifactID.
func newestVersionNumber(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return number
}

// requireRenderedAsStored fails, naming the first byte that differs, unless version number of the
// document artifactID, whose ref_key is refKey, renders from the tree as the bytes it stores, and
// answers the render.
func (r *renderRig) requireRenderedAsStored(t *testing.T, artifactID, refKey string, number int) string {
	t.Helper()
	stored := storedVersionMarkdown(t, r.database, artifactID, number)
	got := r.renderVersion(t, artifactID, number)
	if got.Source != RenderedFromTree || got.Markdown != stored {
		t.Errorf("the render of %s@v%d differs from its stored bytes from byte %d: it renders %q from %s, and stores %q",
			refKey, number, firstDifference(got.Markdown, stored), got.Markdown, got.Source, stored)
	}
	return got.Markdown
}

// requireOnceInPlace fails unless each of texts is held once by the render of version number of
// refKey, at the byte offset its stored bytes hold it.
func requireOnceInPlace(t *testing.T, refKey string, number int, rendered, stored string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if count := strings.Count(rendered, text); count != 1 {
			t.Errorf("the render of %s@v%d holds the text of %q %d times, want once", refKey, number, text, count)
		} else if at, want := strings.Index(rendered, text), strings.Index(stored, text); at != want {
			t.Errorf("the render of %s@v%d holds the text of %q at byte %d, want the byte %d its stored bytes hold it at", refKey, number, text, at, want)
		}
	}
}

// A version of a document renders in the text order its author wrote (R15), each member once, at
// its place in the text, wherever its parent stands (R1). The spec of CORE-1 is written through the
// real version path, version by version, each holding the version_renders row the pass that
// states it recorded, and each renders its stored bytes, character for character:
//   - version 1, 165 bytes: a criterion, a paragraph, and a requirement whose parent attribute
//     names the criterion, which the tree holds under the criterion, before the paragraph; the
//     text of each once, at its place; and the census of the rendered versions reads no difference
//     at that version;
//   - version 2, a second criterion and four requirements more, under a block of the issue's
//     Verification document, under an ask of the issue, inside a callout under the first criterion,
//     and under the second criterion: each once, at its place;
//   - version 3, which drops the second criterion, stated gone, and keeps the requirement under
//     it: that requirement once and no text of the criterion, and version 2 renders as it did;
//   - version 4, two paragraphs of one text and two rules: each at its own place;
//   - version 5, which moves the boxed requirement out of the callout and the first of those
//     paragraphs into it, their text parents the document's node and the callout: version 4
//     renders as it did.
func renderStepTextOrder(t *testing.T, rig *renderRig, service *Service) {
	ctx := context.Background()
	database := rig.database
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice') on conflict (key) do nothing;
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	// The spec and the Verification document are query nodes before their first versions, as a
	// converted issue's are, so each version the real path writes of them is a query version,
	// which takes the row the pass that states it records. Their rows, written after, name no node.
	writeQueryNode(t, database, "CORE", "CORE-1", "spec", graphstore.QuerySpec)
	writeQueryNode(t, database, "CORE", "CORE-1", model.ArtifactSlugVerification, graphstore.QueryVerification)
	treeDocument(t, database, service, "CORE-1", "verification-md", false, model.ArtifactRoleVerification, orderProof)
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options)
		values ('CORE-1', '{"kind":"session","id":"asker"}', 'Which list comes first?', '[{"label":"urgent"}]') returning id::text`).Scan(&ask); err != nil {
		t.Fatalf("open an ask of CORE-1: %v", err)
	}

	first := joinRenderings(orderK1, orderParagraph, orderR1)
	if len(first) != 165 {
		t.Fatalf("version 1 of the spec is %d bytes, want the 165 the case is written for", len(first))
	}
	spec, written := treeDocument(t, database, service, "CORE-1", "spec", true, "", first)
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, spec).Scan(&refKey); err != nil {
		t.Fatalf("read the spec's ref_key: %v", err)
	}
	address := func(id string) string { return refKey + "#" + id }
	version := func(markdown string) int {
		t.Helper()
		if _, err := joinedReplaceText(service, spec, markdown, alice); err != nil {
			t.Fatalf("write the spec's next text: %v", err)
		}
		result, _ := writeVersionThroughTheRealPath(t, service, spec, "The next version")
		settle(t, database, spec)
		return result.Version.Number
	}
	stated := func(number int) string {
		t.Helper()
		requireStatedRow(t, database, "CORE-1", graphstore.QuerySpec, spec, number)
		return storedVersionMarkdown(t, database, spec, number)
	}

	one := written.Version.Number
	if stored := stated(one); stored != first {
		t.Fatalf("version %d of the spec stores %q, want the %q written", one, stored, first)
	}
	if parent := currentPartOf(t, database, address("r1")); parent.ToID != address("k1") {
		t.Fatalf("the requirement of version 1 is part of %s, want the criterion %s, before the paragraph in the tree", parent.ToID, address("k1"))
	}
	rendered := rig.requireRenderedAsStored(t, spec, refKey, one)
	requireOnceInPlace(t, refKey, one, rendered, first, orderK1, orderParagraph, orderR1)
	renders := rig.readRenderCensus(t)
	for _, difference := range renders.Reported {
		if strings.Contains(difference.At, refKey+"@v"+strconv.Itoa(one)) {
			t.Errorf("the census reads a difference at %s: %s", difference.At, difference.Cause)
		}
	}

	second := joinRenderings(orderK1, orderParagraph, orderR1, orderK2, orderR2, orderR3(ask), orderBox, orderR5)
	two := version(second)
	stored := stated(two)
	for id, parent := range map[string]string{
		"r2": "CORE-1/verification-md#c1", "r3": ask, "r4": address("k1"), "r5": address("k2"),
	} {
		if held := currentPartOf(t, database, address(id)); held.ToID != parent {
			t.Errorf("version 2's %s is part of %s/%s, want %s", id, held.ToKind, held.ToID, parent)
		}
	}
	renderedTwo := rig.requireRenderedAsStored(t, spec, refKey, two)
	requireOnceInPlace(t, refKey, two, renderedTwo, stored, orderR2, orderR3(ask), orderR4, orderR5)

	third := joinRenderings(orderK1, orderParagraph, orderR1, orderR2, orderR3(ask), orderBox, orderR5)
	three := version(third)
	stored = stated(three)
	if node, found, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, address("k2")); err != nil || !found || node.State != graphstore.StateGone {
		t.Errorf("after version 3 the second criterion's node reads %+v (found %t, %v), want it gone", node, found, err)
	}
	rendered = rig.requireRenderedAsStored(t, spec, refKey, three)
	requireOnceInPlace(t, refKey, three, rendered, stored, orderR5)
	if strings.Contains(rendered, orderK2) {
		t.Errorf("the render of %s@v%d holds the text of %s, a block whose state is gone at that version", refKey, three, address("k2"))
	}
	if again := rig.requireRenderedAsStored(t, spec, refKey, two); again != renderedTwo {
		t.Errorf("version %d renders %q after the second criterion went gone, want the %q it rendered before", two, again, renderedTwo)
	}

	fourth := joinRenderings(orderK1, orderParagraph, orderR1, orderR2, orderR3(ask), orderBox, orderR5, orderSame, orderRule, orderSame, orderRule)
	four := version(fourth)
	stated(four)
	renderedFour := rig.requireRenderedAsStored(t, spec, refKey, four)

	fifth := joinRenderings(orderK1, orderParagraph, orderR1, orderR2, orderR3(ask),
		":::callout{#box kind=\"note\" title=\"\"}\nThe box holds:\n\n"+orderSame+"\n:::", orderR4, orderR5, orderRule, orderSame, orderRule)
	five := version(fifth)
	stated(five)
	rig.requireRenderedAsStored(t, spec, refKey, five)
	if held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address("r4"), []string{refs.TupleTextParent}); err != nil ||
		len(held) != 1 || held[0].ToKind != graphstore.KindArtifact || held[0].ToID != refKey {
		t.Errorf("after version 5 the boxed requirement's text parents are %+v (%v), want the document's node", held, err)
	}
	members, err := refs.TreeMembers(ctx, database.Pool, refKey, refs.TreeCut{At: rig.now(t), Dated: true})
	if err != nil {
		t.Fatalf("read the members of the spec: %v", err)
	}
	facts, err := refs.TreeFactsOf(ctx, database.Pool, members, refs.TreeCut{At: rig.now(t), Dated: true})
	if err != nil {
		t.Fatalf("read the members of the spec: %v", err)
	}
	boxed := 0
	for _, member := range members {
		if facts[member].Values[graphstore.AttributeText] != orderSame {
			continue
		}
		for _, relation := range facts[member].Relations {
			if relation.Kind == refs.TupleTextParent && relation.To.ID == address("box") {
				boxed++
			}
		}
	}
	if boxed != 1 {
		t.Errorf("after version 5 %d paragraphs reading %q name the callout as their text parent, want the one moved into it", boxed, orderSame)
	}
	if again := rig.requireRenderedAsStored(t, spec, refKey, four); again != renderedFour {
		t.Errorf("version %d renders %q after version 5, want the %q it rendered before", four, again, renderedFour)
	}
}

// A fragment of an issue room renders its own bytes (R1). The room renders its own fragment first,
// then the Verification document, the plan and the spec: through the room of DOC-3 and its
// settlement a peer writes a Verification document ending with a `-` list, a plan opening with a
// `-` list, which the room's joined rendering writes with the other marker, and a spec opening
// with a rule and holding a second one later, whose first the joined rendering writes `---` and
// the spec's own rendering `***`. The settlement writes a version of each from its own fragment's
// rendering, each holding the version_renders row its room pass recorded, and each renders its
// stored bytes; the text value of the plan's and the spec's first block is its slice of its own
// document's stored bytes. The Verification document's blocks read alike in both renderings.
func renderStepFragments(t *testing.T, rig *renderRig, service *Service, serverURL string) {
	ctx := context.Background()
	database := rig.database
	room := createIssueRoom(t, database, 3)
	writeRoomQueryNodes(t, database, room)
	documents := []struct {
		id, query, text string
	}{
		{room.verification, graphstore.QueryVerification, "A proof of the list.\n\n- the first proof\n- the second proof\n"},
		{room.plan, graphstore.QueryPlan, "- the first step\n- the second step\n\nThe plan's closing words.\n"},
		{room.spec, graphstore.QuerySpec, "***\n\nThe spec's words.\n\n***\n\nThe spec's last words.\n"},
	}
	peer := connectPeer(t, serverURL, room.spec, "alice")
	// The peer writes over the plan's fragment only once its copy holds the room's stored seed,
	// else the seed arrives after the write and stands beside it.
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of DOC-3's room: %v", err)
	}
	stored := roomUpdateCount(t, database, room.tree)
	for _, document := range documents {
		tree, err := pmdoc.Parse(document.text)
		if err != nil {
			t.Fatalf("parse %q: %v", document.text, err)
		}
		want, err := pmdoc.Render(tree)
		if err != nil {
			t.Fatalf("render %q: %v", document.text, err)
		}
		fragment := peer.Doc.GetXmlFragment(document.id)
		var updated error
		if _, err := peer.Send(func(txn *crdt.Transaction) { updated = pmdoc.Update(txn, fragment, tree) }); err != nil || updated != nil {
			t.Fatalf("write %q into the fragment of %s: %v, %v", document.text, document.id, err, updated)
		}
		if err := peer.WaitForText(fragmentMarkdown(document.id), want, roomWait(t)); err != nil {
			t.Fatalf("a peer's copy of DOC-3's room: %v", err)
		}
	}
	waitForStoredUpdates(t, database, room.tree, stored)
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for DOC-3's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle DOC-3's room: %v", err)
	}
	for _, document := range documents {
		var refKey string
		var number int
		if err := database.Pool.QueryRow(ctx, `
			select a.ref_key, (select max(v.number) from artifact_versions v where v.artifact_id = a.id)
			from artifacts a where a.id = $1`, document.id).Scan(&refKey, &number); err != nil {
			t.Fatalf("read the newest version of %s: %v", document.id, err)
		}
		requireStatedRow(t, database, room.issue, document.query, document.id, number)
		rig.requireRenderedAsStored(t, document.id, refKey, number)
		if document.id == room.verification {
			continue
		}
		var rendered time.Time
		if err := database.Pool.QueryRow(ctx, `select at from version_renders where artifact_id = $1 and number = $2`, document.id, number).Scan(&rendered); err != nil {
			t.Fatalf("read the row of %s@v%d: %v", refKey, number, err)
		}
		cut := refs.TreeCut{At: rendered, Dated: true}
		members, err := refs.TreeMembers(ctx, database.Pool, refKey, cut)
		if err != nil {
			t.Fatalf("read the members of %s: %v", refKey, err)
		}
		facts, err := refs.TreeFactsOf(ctx, database.Pool, members, cut)
		if err != nil {
			t.Fatalf("read the members of %s: %v", refKey, err)
		}
		var firstBlock refs.Target
		var firstRank string
		for _, member := range members {
			for _, relation := range facts[member].Relations {
				if relation.Kind != refs.TupleTextParent || relation.To.Kind != graphstore.KindArtifact || relation.To.ID != refKey {
					continue
				}
				if held := facts[member].Values[refs.AttributeTextRank]; firstBlock.ID == "" || held < firstRank {
					firstBlock, firstRank = member, held
				}
			}
		}
		text := facts[firstBlock].Values[graphstore.AttributeText]
		versioned := storedVersionMarkdown(t, database, document.id, number)
		if text == "" || !strings.HasPrefix(versioned, text) {
			t.Errorf("the text value of %s is %q, want its slice of %s@v%d, %q", firstBlock.ID, text, refKey, number, versioned[:min(len(text), len(versioned))])
		}
	}
}

func renderStepNoBytes(t *testing.T, rig *renderRig) {
	var file string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		with file as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, 'DOC', 'diagram-bin', 'diagram.bin', 'file', false, '{"kind":"user","id":"alice"}')
			returning id
		), version as (
			insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
			select id, 1, '\x0001'::bytea, 'application/octet-stream', 2, $2, '[{"kind":"user","id":"alice"}]' from file
		)
		select id::text from file`, rig.room.issue, renderSum("\x00\x01")).Scan(&file); err != nil {
		t.Fatalf("write a file: %v", err)
	}
	if got := rig.renderVersion(t, file, 1); got.Source != NoBytes || got.Markdown != "" {
		t.Errorf("a version whose markdown is null renders %q from %q, want no bytes", got.Markdown, got.Source)
	}
	// An empty markdown is a document: the spec's first version holds one.
	if got := rig.renderVersion(t, rig.room.spec, 1); got.Source != StoredBytes || got.Markdown != "" {
		t.Errorf("the spec's empty first version renders %q from %q, want its stored empty markdown", got.Markdown, got.Source)
	}
}

func renderStepTextColumns(t *testing.T, rig *renderRig) {
	rows, err := rig.database.Pool.Query(context.Background(), `
		select column_name || ' ' || data_type || ' ' || is_nullable
		from information_schema.columns
		where table_schema = 'public' and table_name = 'document_block_texts'
		order by ordinal_position`)
	if err != nil {
		t.Fatalf("read document_block_texts' columns: %v", err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read document_block_texts' columns: %v", err)
	}
	want := []string{"artifact_id uuid NO", "block_id text NO", "version integer NO", "text text YES"}
	if !slices.Equal(columns, want) {
		t.Errorf("document_block_texts holds the columns %v, want %v as 0121_block_graph.up.sql creates them", columns, want)
	}
}

// TestTheDecisionsQueryTakesItsNodesAndBuildsNoRecord: over a walk holding an answered ask's node
// with a reading under it, the block that shows that ask in the spec, an ask nobody has answered
// with a reading under its block, a decision with a reading under it, and a requirement, the
// decisions query takes each ask's own node, the decision, the reading under the decision and the
// reading under the answered ask's node, and nothing else: neither block of an ask nor the reading
// in the ask nobody has answered, which is no reading yet; and a record of the decisions query is
// refused, through ReadQueryRecord and through the walk's own builder, with the error a query no
// definition names is refused with, so no version records its digest.
func TestTheDecisionsQueryTakesItsNodesAndBuildsNoRecord(t *testing.T) {
	issue := refs.Target{Kind: graphstore.KindIssue, ID: "CORE-1"}
	ask := refs.Target{Kind: graphstore.KindAsk, ID: "5d1f6a3e-0c4b-4f7e-9b1a-2c3d4e5f6a7b"}
	open := refs.Target{Kind: graphstore.KindAsk, ID: "6e2a7b4f-1d5c-4a8f-8c2b-3d4e5f6a7b8c"}
	openBlock := refs.Target{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#a2"}
	decision := refs.Target{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#d1"}
	nodes := []TreeNode{
		{Kind: issue.Kind, ID: issue.ID, Type: graphstore.SupertagIssue},
		{Kind: ask.Kind, ID: ask.ID, Parent: issue, Type: graphstore.SupertagAsk},
		{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#a1", Parent: issue, Type: graphstore.SupertagAsk},
		{Kind: decision.Kind, ID: decision.ID, Parent: issue, Type: refs.DecisionSupertag},
		{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#g1", Parent: decision, Type: refs.ReadingSupertag},
		{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#g2", Parent: ask, Type: refs.ReadingSupertag},
		{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#r1", Parent: issue, Type: "requirement"},
		{Kind: open.Kind, ID: open.ID, Parent: issue, Type: graphstore.SupertagAsk},
		{Kind: openBlock.Kind, ID: openBlock.ID, Parent: issue, Type: graphstore.SupertagAsk},
		{Kind: refs.BlockNodeKind, ID: "CORE-1/tree#g3", Parent: openBlock, Type: refs.ReadingSupertag},
	}
	tree := IssueTree{Issue: issue.ID, Nodes: nodes, Memo: -1, index: map[refs.Target]int{}}
	for index, node := range nodes {
		tree.index[node.target()] = index
	}
	taken, defined := tree.Takes(graphstore.QueryDecisions)
	var addresses []string
	for _, index := range taken {
		addresses = append(addresses, nodes[index].Address())
	}
	want := []string{nodes[1].Address(), nodes[3].Address(), nodes[4].Address(), nodes[5].Address(), nodes[7].Address()}
	if !defined || !slices.Equal(addresses, want) {
		t.Errorf("the decisions query takes %v (defined %t), want %v: each ask's own node, the decision, the reading under it and the reading under the answered ask",
			addresses, defined, want)
	}

	const refused = `the query "decisions" has no record`
	if _, err := ReadQueryRecord(context.Background(), nil, issue.ID, graphstore.QueryDecisions, refs.TreeCut{}); err == nil || err.Error() != refused {
		t.Errorf("ReadQueryRecord of the decisions query answers %v, want it refused: %s", err, refused)
	}
	if _, err := tree.QueryRecord(context.Background(), nil, graphstore.QueryDecisions); err == nil || err.Error() != refused {
		t.Errorf("the walk's record of the decisions query answers %v, want it refused: %s", err, refused)
	}
}
