package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R5: a mention "runs from the comment, issue message or document block that holds it to the
// identity it points at". R9: a version a release before this one writes takes its tuples once,
// from the backfill or the block-index tick's catch-up.
//
// The store's own release states a document's blocks and their revision fence and no identity
// mention (migration 20261122100124). Three documents of CORE-1, each with one paragraph naming
// dave: the first indexed and stated by that release; the second by that release too, whose
// paragraph already mentions dave (a later pass of this release stated it); the third by this
// release. The backfill's block identity pass marks the first two owed this release's block index
// (store_owed_at, at the marking rule), which states the mention at the version each stands at,
// and sets identities_stated on both; the third holds it from its insert. The second is marked
// though its paragraph's node mentions dave already: the pass reads the text alone
// (blockNamesIdentity), since a version that release writes can name another identity beside one
// a mention stands to, and the index it owes appends no mention that stands. A second run reads no
// row. Its control is the pass taken out of inboxPasses: the first document stays unmarked, and no
// pass of this release would read it again.
func TestTheBackfillMarksADocumentAnOlderBuildStatedWithoutItsIdentityMentions(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed CORE-1: %v", err)
	}
	const paragraph = "Over to [the copy owner](dispatch://person/dave@example.com) for the notes."
	document := func(slug string, older, mentioned bool) string {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if older {
			if _, err := tx.Exec(ctx, `select set_config($1, 'off', true)`, store.StoreMarkerWriterSetting); err != nil {
				t.Fatalf("take this build's marker writer setting off: %v", err)
			}
		}
		var id, refKey string
		var at time.Time
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key, now()
		`, slug).Scan(&id, &refKey, &at); err != nil {
			t.Fatalf("create document %s: %v", slug, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, '# Notes' || chr(10) || chr(10) || $2, '[{"kind":"user","id":"alice"}]')
		`, id, paragraph); err != nil {
			t.Fatalf("write version 1 of %s: %v", slug, err)
		}
		address := refKey + "#b1"
		if err := graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: graphstore.KindBlock, ID: address, Supertag: graphstore.KindBlock, Visibility: graphstore.VisibilityProject,
			State: graphstore.StateLive, Project: "CORE", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}}, nil); err != nil {
			t.Fatalf("state the paragraph's node of %s: %v", slug, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, node_kind, node_address)
			values ($1::uuid, 'b1', $2, 'paragraph', '', $3, 1, now(), $4, $2)
		`, id, address, paragraph, graphstore.KindBlock); err != nil {
			t.Fatalf("write the paragraph of %s: %v", slug, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, 'b1', 1, $2)
		`, id, paragraph); err != nil {
			t.Fatalf("write the paragraph's text of %s: %v", slug, err)
		}
		if mentioned {
			if err := graphstore.ToPerson(ctx, tx, []graphstore.Tuple{{
				FromKind: graphstore.KindBlock, FromID: address, Kind: graphstore.TupleMentions,
				ToKind: graphstore.KindPerson, ToID: "dave@example.com", At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
			}}); err != nil {
				t.Fatalf("state the paragraph's mention of dave in %s: %v", slug, err)
			}
		}
		// The revision fence, as the pass of the build that indexed the version writes it.
		if tag, err := tx.Exec(ctx, `
			update document_block_versions set version = 1, store_version = 1 where artifact_id = $1::uuid
		`, id); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("stand the blocks of %s at version 1: %d rows, %v", slug, tag.RowsAffected(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	older := document("older", true, false)
	stated := document("stated", true, true)
	here := document("here", false, false)

	type stand struct{ owed, identities bool }
	read := func(id string) stand {
		t.Helper()
		var held stand
		if err := database.Pool.QueryRow(ctx, `
			select store_owed_at is not null, identities_stated from document_block_versions where artifact_id = $1::uuid
		`, id).Scan(&held.owed, &held.identities); err != nil {
			t.Fatalf("read the block index row of %s: %v", id, err)
		}
		return held
	}
	for name, want := range map[string]struct {
		id   string
		want stand
	}{"the older build's document": {older, stand{false, false}}, "the stated document": {stated, stand{false, false}}, "this build's document": {here, stand{false, true}}} {
		if got := read(want.id); got != want.want {
			t.Fatalf("before the backfill %s reads %+v, want %+v: this test proves nothing", name, got, want.want)
		}
	}

	report := backfillInboxOnce(t, database)
	if done := reportedKind(t, report, blockIdentitiesKind); done.Rows != 2 {
		t.Errorf("the block identity pass reports %#v, want the two documents the older build stated read", done)
	}
	for name, want := range map[string]struct {
		id   string
		want stand
	}{"the older build's document": {older, stand{true, true}}, "the stated document": {stated, stand{true, true}}, "this build's document": {here, stand{false, true}}} {
		if got := read(want.id); got != want.want {
			t.Errorf("after the backfill %s reads owed=%t identities_stated=%t, want owed=%t identities_stated=%t",
				name, got.owed, got.identities, want.want.owed, want.want.identities)
		}
	}
	var rule int
	if err := database.Pool.QueryRow(ctx, `select store_owed_rule from document_block_versions where artifact_id = $1::uuid`, older).Scan(&rule); err != nil {
		t.Fatalf("read the rule of the older build's document's mark: %v", err)
	}
	if rule != refs.BlockIndexMarkingRule {
		t.Errorf("the older build's document is marked at rule %d, want this build's %d", rule, refs.BlockIndexMarkingRule)
	}
	requireASecondRunAppendsNothing(t, database)

	// A later write of the row by that release - its window arm's, which leaves a mark of a higher
	// rule than its own standing, as its fence does - says it again: the next run reads it.
	updateAsAnOlderBuild(t, database, "write the row as the release before's window arm does", `
		update document_block_versions set windows_owed_at = null where artifact_id = $1::uuid
	`, older)
	if got := read(older); got.identities {
		t.Fatalf("the older build's write of the row left identities_stated true, want the trigger to say it again")
	}
	backfillInboxOnce(t, database)
	if got := read(older); !got.owed || !got.identities {
		t.Errorf("after the older build drained the mark the next run reads owed=%t identities_stated=%t, want it marked again", got.owed, got.identities)
	}
}
