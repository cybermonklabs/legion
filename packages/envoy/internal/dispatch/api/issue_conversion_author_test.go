package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func TestAConvertedNodeKeepsItsAuthor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	handler, database, deps := newTestServer(t, testServerOptions{})
	service := deps.Docs.(*docs.Service)
	exec := func(sql string, args ...any) {
		t.Helper()
		seedExec(t, database, "the former release's rows", sql, args...)
	}
	const key = "AUTHOR-1"
	const spec = key + "/spec"
	exec(`insert into projects (key, name, default_assignee) values ('AUTHOR', 'Block authors', 'alice')`)
	exec(`insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'AUTHOR', 1, 'Authors at conversion', '{"kind":"user","id":"alice"}', 'U')`, key)
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'AUTHOR', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}') returning id::text`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	person, agent, backfilled := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "session", ID: "writer"}, model.Actor{Kind: "user", ID: "bob"}
	const decisionText = "::::decision{#decision}\nChoose the first design.\n\n:::reading{#reading}\nThe record keeps that design.\n:::\n::::\n"
	decisionAt := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)
	unknownAt := decisionAt.Add(24 * time.Hour)
	const decisionVersion = 5
	fixtures := []struct {
		block string
		who   []model.Actor
		at    time.Time
	}{
		{"person", []model.Actor{person}, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"agent", []model.Actor{agent}, time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"both", []model.Actor{person, agent}, time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)},
		{"backfilled", []model.Actor{backfilled}, time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)},
		{"decision", []model.Actor{person}, decisionAt},
		{"unknown", []model.Actor{person}, unknownAt},
	}
	var source strings.Builder
	var room *pmdoc.Node
	for index, fixture := range fixtures {
		body := ":::requirement{#" + fixture.block + "}\nKeep the words of " + fixture.block + ".\n:::\n"
		if fixture.block == "decision" {
			body = decisionText
		}
		source.WriteString(body + "\n")
		var err error
		room, err = pmdoc.ParseRendering(source.String())
		if err != nil {
			t.Fatal(err)
		}
		markdown, err := pmdoc.Render(room)
		if err != nil {
			t.Fatal(err)
		}
		exec(`insert into artifact_versions (artifact_id, number, markdown, authors, created_at) values ($1, $2, $3, $4, $5)`,
			id, index+1, markdown, fixture.who, fixture.at)
		exec(`insert into document_block_texts (artifact_id, block_id, version, text) values ($1, $2, $3, $4)`, id, fixture.block, index+1, body)
		exec(`insert into document_indexed_versions (artifact_id, version) values ($1, $2) on conflict do nothing`, id, index+1)
	}
	// The person wrote both typed blocks in one version. The old typed pass takes their
	// credit from doc_ask_block_authors and states the decision's separate decided_by tie.
	exec(`insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1, 'reading', $2, 'The record keeps that design.')`, id, decisionVersion)
	exec(`insert into doc_ask_block_authors (artifact_id, block_id, actor)
		values ($1, 'decision', $2), ($1, 'reading', $2)`, id, person)
	fixtures = append(fixtures, struct {
		block string
		who   []model.Actor
		at    time.Time
	}{"reading", []model.Actor{person}, decisionAt})
	oldRoom := crdt.New()
	fragment := oldRoom.GetXmlFragment("prosemirror")
	if err := oldRoom.TransactE(func(tx *crdt.Transaction) error { return pmdoc.Update(tx, fragment, room) }); err != nil {
		t.Fatal(err)
	}
	exec(`insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)`, id, crdt.EncodeStateAsUpdateV1(oldRoom, nil))
	// The backfill names a node on this old-shape row. Its system maker is not the block's author.
	exec(`insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1, 'backfilled', $2, 'requirement', repeat('0', 64), 'The former shape.', 4, $3)`,
		id, spec+"#backfilled", fixtures[3].at)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatal(err)
	}
	var oldMaker model.Actor
	if err := database.Pool.QueryRow(ctx, `select made_by from graph_nodes where kind = 'block' and id = $1`, spec+"#backfilled").Scan(&oldMaker); err != nil || oldMaker.Kind != "system" {
		t.Fatalf("the backfill did not create the old node as a system actor: %+v, %v", oldMaker, err)
	}
	if _, err := service.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatal(err)
	}
	// Remove only the proof that "unknown" was first written at v6. Its live node has
	// an authored_by, but there is no indexed version immediately below its first text.
	exec(`delete from document_indexed_versions where artifact_id = $1 and version = $2`, id, decisionVersion)
	const decider = "alice"
	var oldDecider string
	if err := database.Pool.QueryRow(ctx, `select to_id from graph_tuples
		where from_kind = 'block' and from_id = $1 and kind = 'decided_by' and asserted
		order by seq desc limit 1`, spec+"#decision").Scan(&oldDecider); err != nil || oldDecider != decider {
		t.Fatalf("read the original decision's decider: %q, %v", oldDecider, err)
	}

	// blockRef is the reference to block of the issue's document slug as every writer writes one
	// (text.FormatBlockRef): `dispatch://KEY/spec#b-<id>` for the spec and
	// `dispatch://KEY/artifact/<slug>#b-<id>` for any other document.
	blockRef := func(slug, block string) string {
		t.Helper()
		written, ok := text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: key, ID: slug, Block: block})
		if !ok {
			t.Fatalf("no reference names block %s of %s's document %s", block, key, slug)
		}
		return written
	}
	readRef := func(reference string) model.GraphNode {
		t.Helper()
		ref := "/api/v1/references?to=" + url.QueryEscape(reference)
		answer := dispatchRequest(t, handler, http.MethodGet, ref, nil, "alice")
		if answer.Code != http.StatusOK {
			t.Fatalf("read %s: %d %s", reference, answer.Code, answer.Body.String())
		}
		return decodeBody[struct{ Node model.GraphNode }](t, answer).Node
	}
	checkMaker := func(surface, block string, got, want []model.Actor, at *time.Time, wantAt time.Time) {
		t.Helper()
		sameTime := wantAt.IsZero() && at == nil || at != nil && at.Equal(wantAt)
		if !reflect.DeepEqual(got, want) || !sameTime {
			t.Errorf("%s of %s: makers %+v at %v, want %+v at %s", surface, block, got, at, want, wantAt)
		}
	}
	wantTime := func(block string, known time.Time) time.Time {
		if block == "unknown" {
			return time.Time{}
		}
		return known
	}
	for _, fixture := range fixtures {
		node := readRef(blockRef("spec", fixture.block))
		checkMaker("old reference", fixture.block, node.MadeBy, fixture.who, node.MadeAt, wantTime(fixture.block, fixture.at))
	}
	// An upload is not a role document. Compare the complete reference node before and after.
	const note = "An upload keeps its own room."
	other := uploadDocument(t, handler, "/api/v1/issues/"+key, "notes.md", note+"\n").Artifact
	otherBlock := blockIDsByText(t, handler, other.ID)[note]
	if otherBlock == "" {
		t.Fatal("the upload did not index its paragraph")
	}
	otherRef := blockRef(other.Slug, otherBlock)
	before := readRef(otherRef)
	result, err := service.ConvertIssue(ctx, key, docs.IssueRoomGate, false)
	if err != nil || result.Outcome != docs.IssueConverted {
		t.Fatalf("convert %s: %+v, %v", key, result, err)
	}
	if after := readRef(otherRef); !reflect.DeepEqual(before, after) {
		t.Errorf("the separate upload's reference changed: before %+v, after %+v", before, after)
	}
	// The unmarked route walks the tuples. Fill the same issue's head rows to exercise
	// the optimized route without changing its tree.
	unmarkHead(t, database, key)
	walk := treeRead(t, handler, key, "?view=spec", "alice", false)
	walkDecisions := treeRead(t, handler, key, "?view=decisions", "alice", false)
	if _, _, err := graphcensus.FillIssueHeads(ctx, database, graphcensus.HeadFillBatch); err != nil {
		t.Fatal(err)
	}
	if _, marked, err := refs.HeadRows(ctx, database.Pool, person, key, nil); err != nil || !marked {
		t.Fatalf("the head projection is not available: marked=%t, %v", marked, err)
	}
	head := treeRead(t, handler, key, "?view=spec", "alice", false)
	headDecisions := treeRead(t, handler, key, "?view=decisions", "alice", false)
	for _, fixture := range fixtures {
		redirected := readRef(blockRef("spec", fixture.block))
		if redirected.ID == refs.BlockAddress(spec, fixture.block) {
			t.Errorf("the old address of %s did not move", fixture.block)
		}
		wantAt := wantTime(fixture.block, fixture.at)
		checkMaker("redirected reference", fixture.block, redirected.MadeBy, fixture.who, redirected.MadeAt, wantAt)
		walkPage, headPage := walk, head
		if fixture.block == "decision" || fixture.block == "reading" {
			walkPage, headPage = walkDecisions, headDecisions
		}
		for _, reading := range []struct {
			name string
			path treeAnswer
		}{{"walk", walkPage}, {"head", headPage}} {
			node, _ := reading.path.node(t, refs.BlockNodeKind, redirected.ID)
			checkMaker(reading.name, fixture.block, node.Author, fixture.who, node.MadeAt, wantAt)
			if fixture.block == "decision" && !reading.path.holdsTuple(graphstore.TupleDecidedBy,
				treeEnd(refs.BlockNodeKind, redirected.ID), treeEnd(graphstore.KindPerson, oldDecider)) {
				t.Errorf("%s lost the decision's decided_by %s", reading.name, oldDecider)
			}
		}
		var maker []byte
		var at time.Time
		if err := database.Pool.QueryRow(ctx, `select made_by, made_at from graph_nodes where kind = 'block' and id = $1`, redirected.ID).Scan(&maker, &at); err != nil {
			t.Fatal(err)
		}
		var first model.Actor
		if err := json.Unmarshal(maker, &first); err != nil {
			t.Fatal(err)
		}
		if fixture.block == "unknown" {
			if first != (model.Actor{}) || !at.After(fixture.at) {
				t.Errorf("the unknown original recorded %+v at %s, want the zero actor at conversion time", first, at)
			}
			continue
		}
		checkMaker("stored node", fixture.block, []model.Actor{first}, fixture.who[:1], &at, fixture.at)
		var dated int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples
			where from_kind = 'block' and from_id = $1 and kind = 'authored_by' and asserted and at = $2`, redirected.ID, fixture.at).Scan(&dated); err != nil || dated != len(fixture.who) {
			t.Errorf("%s has %d authored_by tuples dated %s, want %d (%v)", fixture.block, dated, fixture.at, len(fixture.who), err)
		}
	}

	editToVersion(t, handler, id, latestVersion(t, database, id).Number+1,
		map[string]any{"op": "insert", "after": "end", "markdown": ":::requirement{#new}\nKeep this new text.\n:::\n"})
	var authors []model.Actor
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `select authors, created_at from artifact_versions
		where artifact_id = $1 order by number desc limit 1`, id).Scan(&authors, &at); err != nil {
		t.Fatal(err)
	}
	if len(authors) != 1 || authors[0].ID != person.ID {
		t.Fatalf("the new room version has authors %+v, want the person who wrote it", authors)
	}
	node := readRef(blockRef("spec", "new"))
	checkMaker("new room block reference", "new", node.MadeBy, authors, node.MadeAt, at)
	if _, marked, err := refs.HeadRows(ctx, database.Pool, person, key, nil); err != nil || !marked {
		t.Fatalf("the edit left no head projection: marked=%t, %v", marked, err)
	}
	head = treeRead(t, handler, key, "?view=spec", "alice", false)
	newHead, _ := head.node(t, refs.BlockNodeKind, node.ID)
	checkMaker("new room block head", "new", newHead.Author, authors, newHead.MadeAt, at)
	unmarkHead(t, database, key)
	walk = treeRead(t, handler, key, "?view=spec", "alice", false)
	newWalk, _ := walk.node(t, refs.BlockNodeKind, node.ID)
	checkMaker("new room block walk", "new", newWalk.Author, authors, newWalk.MadeAt, at)
}
