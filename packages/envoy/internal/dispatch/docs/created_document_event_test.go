package docs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestACreatedDocumentsEventNamesItAndCausesItsNodeAndBlocks checks R1: a project
// document's creation event records its first version and causes its graph writes.
// A seed that needs no settlement leaves none pending.
func TestACreatedDocumentsEventNamesItAndCausesItsNodeAndBlocks(t *testing.T) {
	t.Parallel()
	f := newCreatedDocuments(t)
	ctx := t.Context()
	project := "DOC"
	const heading, paragraph = "A project document", "Its first paragraph."
	const seed = "# " + heading + "\n\n" + paragraph + "\n"
	var artifact model.Artifact
	var written WrittenText
	var eventID int64
	if err := joinedWrite(f.service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := LockProjectArtifacts(ctx, tx, project); err != nil {
			return err
		}
		row := NewArtifact{
			Project: &project, RefPrefix: project, Slug: "created-md", Name: "created.md",
			CreatedBy: model.Actor{Kind: "user", ID: "alice"},
		}
		var err error
		artifact, written, err = CreateDocument(ctx, f.service, row, seed)
		if err != nil {
			return err
		}
		// A project's own document owns its event. Queue the append after the
		// artifact insert so the append can read that owner.
		if err := ledgerFrom(ctx).Queue(func(b *store.WriteBatch) error {
			return f.service.events.QueueAppend(b, model.Event{
				ArtifactID: &artifact.ID, Type: "artifact.created", Actor: row.CreatedBy,
				Payload: ArtifactCreatedEventPayload(artifact, written.Changes),
			}, func(event model.Event) { eventID = event.ID })
		}); err != nil {
			return err
		}
		if err := causeInWritePhase(ctx, tx, written.Node, graphstore.LatestEvent); err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, written.Blocks, graphstore.LatestEvent)
	}); err != nil {
		t.Fatalf("create the project document and record its event: %v", err)
	}

	pool := f.database.Pool
	var namedID string
	var namedVersion int
	if err := pool.QueryRow(ctx, `
		select payload #>> '{artifact,id}', (payload #>> '{artifact,versions,0,number}')::int
		from events where id = $1 and artifact_id = $2 and type = 'artifact.created'
	`, eventID, artifact.ID).Scan(&namedID, &namedVersion); err != nil {
		t.Fatalf("read the artifact.created payload of event %d: %v", eventID, err)
	}
	if namedID != artifact.ID || namedVersion != 1 {
		t.Errorf("event %d names document %q version %d, want %q version 1", eventID, namedID, namedVersion, artifact.ID)
	}

	node, found, err := graphstore.ReadNode(ctx, pool, graphstore.KindArtifact, artifact.RefKey)
	if err != nil {
		t.Fatalf("read the document's node: %v", err)
	}
	if !found || node.State != graphstore.StateLive || node.SourceEvent != eventID {
		t.Errorf("document node %q: found %t, state %q, cause %d; want a live node caused by event %d",
			artifact.RefKey, found, node.State, node.SourceEvent, eventID)
	}
	var statements, otherCauses int
	if err := pool.QueryRow(ctx, `
		select count(*), count(*) filter (where source_event <> $3)
		from graph_tuples where from_kind = $1 and from_id = $2
	`, graphstore.KindArtifact, artifact.RefKey, eventID).Scan(&statements, &otherCauses); err != nil {
		t.Fatalf("read the causes of the document's statements: %v", err)
	}
	if statements == 0 || otherCauses != 0 {
		t.Errorf("document node %q has %d statements, %d with a cause other than event %d", artifact.RefKey, statements, otherCauses, eventID)
	}

	rows, err := pool.Query(ctx, `
		select b.type, b.node_id, b.changed_version, coalesce(n.source_event, 0)
		from document_blocks b left join graph_nodes n on (n.kind, n.id) = (b.node_kind, b.node_address)
		where b.artifact_id = $1 order by b.type
	`, artifact.ID)
	if err != nil {
		t.Fatalf("read the document's block nodes and their causes: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, address string
		var version int
		var cause int64
		if err := rows.Scan(&kind, &address, &version, &cause); err != nil {
			t.Fatalf("scan a block node of %s: %v", artifact.RefKey, err)
		}
		if version != 1 || cause != eventID {
			t.Errorf("block %q is from version %d and caused by event %d, want version 1 and event %d", address, version, cause, eventID)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's block nodes: %v", err)
	}
	if !slices.Equal(kinds, []string{"heading", "paragraph"}) {
		t.Errorf("indexed blocks = %v, want the seeded heading and paragraph", kinds)
	}

	if number, markdown, _ := latestDocumentVersion(t, f.database, artifact.ID); number != 1 || markdown != written.Markdown ||
		!strings.Contains(markdown, "# "+heading) || !strings.Contains(markdown, paragraph) {
		t.Errorf("stored version %d holds %q, want version 1 holding the seed rendered as %q", number, markdown, written.Markdown)
	}
	if f.owed(t, artifact.ID) {
		t.Errorf("the created document %s owes a settlement", artifact.RefKey)
	}
}
