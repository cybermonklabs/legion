package grapherase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// R5: an event of the erased block's document is about the block only where it records a version
// the block stood in, and each type is tested against the versions its payload names. The inner
// block reads `Ship it on Friday.` at version 2 alone: at version 1 the outer list item reads those
// words as its own, and version 3 drops the inner block, leaving the outer item a line of its own.
// The erase of the inner block redacts an artifact.version event of version 3 whose diff is taken
// from version 2, an artifact.human_edited event from version 2 to version 3, a request for
// changes on version 2, and an artifact.version event that names no version, which cannot be
// narrowed to one. It leaves an artifact.version event of version 3 that carries no diff, an
// artifact.human_edited event from version 0 to version 1 and an approval of version 1, each
// holding the words where the inner block did not stand, and lists each as a row it leaves.
func TestTheEraseTestsEachDocumentEventAgainstTheVersionsItNames(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const words = "Ship it on Friday."
	nested := "Plan for the week:\n  - " + words
	artifact := seeded.document.artifactID
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]'), ($1::uuid, 3, $3, '[{"kind":"user","id":"alice"}]')`,
		artifact, "- "+nested, "- Plan for the week:"); err != nil {
		t.Fatalf("write versions 2 and 3 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1::uuid, 'outer', 1, $2), ($1::uuid, 'outer', 2, $3), ($1::uuid, 'inner', 2, $2),
		       ($1::uuid, 'outer', 3, 'Plan for the week:'), ($1::uuid, 'inner', 3, null)`,
		artifact, words, nested); err != nil {
		t.Fatalf("write the blocks' texts at versions 1 to 3: %v", err)
	}
	dropped := "--- previous\n+++ current\n@@ -1,2 +1 @@\n - Plan for the week:\n-  - " + words + "\n"
	written := "--- previous\n+++ current\n@@ -0,0 +1 @@\n+- " + words + "\n"
	alice := map[string]any{"kind": "user", "id": "alice"}
	type documentEvent struct {
		kind     string
		payload  map[string]any
		redacted bool
		why      string
		id       int64
	}
	events := []documentEvent{
		{kind: "artifact.version", redacted: true,
			why: "its diff is taken from version 2, where the inner block stood, and holds its line as a removed one",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md", "diff": dropped,
				"version": map[string]any{"number": 3, "named": true, "authors": []any{alice}}}},
		{kind: "artifact.version", redacted: false,
			why: "it carries no diff, so it holds nothing of version 2, and the inner block did not stand in version 3",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md",
				"version": map[string]any{"number": 3, "named": false, "summary": words, "authors": []any{alice}}}},
		{kind: "artifact.human_edited", redacted: true,
			why: "its diff is taken between versions 2 and 3, and the inner block stood in version 2",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md", "from_version": 2, "to_version": 3,
				"editors": []any{"alice"}, "diff": dropped, "diff_truncated": false}},
		{kind: "artifact.human_edited", redacted: false,
			why: "its diff is taken between versions 0 and 1, neither of which the inner block stood in",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md", "from_version": 0, "to_version": 1,
				"editors": []any{"alice"}, "diff": written, "diff_truncated": false}},
		{kind: "artifact.approved", redacted: false,
			why: "it is pinned to version 1, which the inner block did not stand in",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md", "version": 1, "kind": "person",
				"actor": alice, "reason": words, "ask_id": nil}},
		{kind: "artifact.changes_requested", redacted: true,
			why: "it is pinned to version 2, where the inner block stood",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md", "version": 2, "kind": "person",
				"actor": alice, "reason": words, "ask_id": nil}},
		{kind: "artifact.version", redacted: true,
			why: "it names no version, so it cannot be narrowed to one the inner block did not stand in",
			payload: map[string]any{"artifact_id": artifact, "name": "spec.md",
				"diff": "--- previous\n+++ current\n@@ -1 +1,2 @@\n - Plan for the week:\n+  - " + words + "\n"}},
	}
	for index := range events {
		payload, err := json.Marshal(events[index].payload)
		if err != nil {
			t.Fatalf("encode the %s payload: %v", events[index].kind, err)
		}
		events[index].id = appendEvent(t, database, "CORE-1", events[index].kind, string(payload))
	}

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.inner, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing: %q", seeded.inner, erasure.Blockers)
	}
	for _, event := range events {
		_, raw := eventPayload(t, database, event.id)
		redactedLine := fmt.Sprintf("events %d %s", event.id, event.kind)
		leftLine := []string{"leave public.events", fmt.Sprintf("(id)=(%d)", event.id), "[" + event.kind + "]", "payload"}
		if event.redacted {
			if strings.Contains(raw, words) {
				t.Errorf("after the erase of %s the %s event %d reads %s, want %q redacted: %s",
					seeded.inner, event.kind, event.id, raw, words, event.why)
			}
			if !listed(erasure, redactedLine) {
				t.Errorf("the listing does not name the %s event %d as one the run redacts, and %s:\n%s",
					event.kind, event.id, event.why, strings.Join(erasure.Listing, "\n"))
			}
			continue
		}
		if !strings.Contains(raw, words) {
			t.Errorf("after the erase of %s the %s event %d reads %s, want it as written, holding %q: %s",
				seeded.inner, event.kind, event.id, raw, words, event.why)
		}
		if listed(erasure, redactedLine) {
			t.Errorf("the listing names the %s event %d as one the run redacts, but %s:\n%s",
				event.kind, event.id, event.why, strings.Join(erasure.Listing, "\n"))
		}
		if !listed(erasure, leftLine...) {
			t.Errorf("the listing does not name the %s event %d as a row the run leaves holding the words, and %s:\n%s",
				event.kind, event.id, event.why, strings.Join(erasure.Listing, "\n"))
		}
	}
}

// R5: the erase deletes the display tree of every text it removes or rewrites, and of no other
// text. The outer list item reads `Ship it on Friday. Then rest.` at version 1, as its own words;
// version 2 nests the inner block, reading `Ship it on Friday.`, under a line of the outer
// block's own. The erase of the inner block rewrites the outer block's text at version 2 and
// deletes that text's tree, and leaves the outer block's text at version 1, which the inner block
// did not stand in, with the tree of that text.
func TestTheEraseLeavesTheDisplayTreeOfAnAncestorsTextItLeaves(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const words = "Ship it on Friday."
	own := words + " Then rest."
	nested := "Plan for the week:\n  - " + words
	artifact := seeded.document.artifactID
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')`, artifact, "- "+nested); err != nil {
		t.Fatalf("write version 2 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		values ($1::uuid, 'outer', 1, $2), ($1::uuid, 'outer', 2, $3), ($1::uuid, 'inner', 2, $4)`,
		artifact, own, nested, words); err != nil {
		t.Fatalf("write the blocks' texts at versions 1 and 2: %v", err)
	}
	sum := func(text string) []byte {
		digest := sha256.Sum256([]byte(text))
		return digest[:]
	}
	for _, text := range []string{own, nested} {
		if _, err := database.Pool.Exec(ctx, `
			insert into text_trees (sum, mode, version, tree) values ($1, 0, 1, convert_to('a tree', 'UTF8'))`,
			sum(text)); err != nil {
			t.Fatalf("store the display tree of %q: %v", text, err)
		}
	}
	trees := func(text string) int {
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from text_trees where sum = $1`, sum(text)).Scan(&count); err != nil {
			t.Fatalf("count the display trees of %q: %v", text, err)
		}
		return count
	}

	erasure, err := confirmedErase(t, database, refs.BlockNodeKind, seeded.inner, time.Millisecond)
	if err != nil {
		t.Fatalf("erase %s: %v", seeded.inner, err)
	}
	if !erasure.Applied {
		t.Fatalf("the erase of %s removed nothing: %q", seeded.inner, erasure.Blockers)
	}
	if got := trees(nested); got != 0 {
		t.Errorf("after the erase of %s %d display tree(s) of the outer block's text at version 2 stand, want none: the run rewrote that text",
			seeded.inner, got)
	}
	if got := trees(own); got != 1 {
		t.Errorf("after the erase of %s %d display tree(s) of the outer block's text at version 1 stand, want 1: the inner block did not stand in version 1, so the run leaves that text, and its tree is that text's",
			seeded.inner, got)
	}
}
