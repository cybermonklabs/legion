package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"
)

// specQueryWrites is what a converted spec's query holds that a write of it appends: its
// versions, its reviews, the record digests its versions owe, and the issue's events by type,
// artifact.version and artifact.digest counted for the spec alone.
type specQueryWrites struct {
	versions, reviews, owed int
	events                  map[string]int
}

// A comment anchored in a converted spec's text adds no node to the spec query's answer, so it
// writes no version of the spec, leaves no record digest owed, and appends no artifact.version,
// no artifact.digest and no review: each comment of a run anchored by block_id in the spec's
// requirements appends its comment.created alone (R14; the architect's rule of 2026-11-28). The
// run is written with a settle delay of an hour, so no settlement runs between its comments, as
// none runs between the comments c-tree-read-bound writes eight at a time.
func TestACommentAnchoredInASpecsTextWritesNoVersionDigestOrReviewOfTheSpec(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	const requirements = 3
	var spec strings.Builder
	for index := 1; index <= requirements; index++ {
		fmt.Fprintf(&spec, ":::requirement{#r%d}\nThe requirement r%d holds one paragraph.\n:::\n\n", index, index)
	}
	issue := createDigestIssue(t, handler, database, "CORE", "A spec commented on", spec.String(), false)
	specID := issue.PrimaryArtifactID
	awaitDigestsFilled(t, database, specID)
	read := func() specQueryWrites {
		t.Helper()
		var held specQueryWrites
		if err := database.Pool.QueryRow(ctx, `
			select
				(select count(*) from artifact_versions where artifact_id = $1::uuid),
				(select count(*) from artifact_reviews where artifact_id = $1::uuid),
				(select count(*) from version_renders where artifact_id = $1::uuid and digest_owed)
		`, specID).Scan(&held.versions, &held.reviews, &held.owed); err != nil {
			t.Fatalf("read the spec's versions, reviews and owed digests: %v", err)
		}
		rows, err := database.Pool.Query(ctx, `
			select type, count(*) from events
			where issue_key = $1 and (type not in ('artifact.version', 'artifact.digest') or payload->>'artifact_id' = $2)
			group by type`, issue.Key, specID)
		if err != nil {
			t.Fatalf("read the events of %s: %v", issue.Key, err)
		}
		defer rows.Close()
		held.events = map[string]int{}
		for rows.Next() {
			var kind string
			var count int
			if err := rows.Scan(&kind, &count); err != nil {
				t.Fatalf("read the events of %s: %v", issue.Key, err)
			}
			held.events[kind] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the events of %s: %v", issue.Key, err)
		}
		return held
	}
	before := read()
	const comments = 5
	for index := range comments {
		block := fmt.Sprintf("r%d", index%requirements+1)
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body":   fmt.Sprintf("Comment %d on %s: a note on its words.", index+1, block),
			"anchor": map[string]any{"artifact": specID, "block_id": block, "quote": "The requirement " + block + " holds one paragraph."},
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("comment %d anchored in %s: status=%d body=%s", index+1, block, created.Code, created.Body.String())
		}
	}
	after := read()
	if after.versions != before.versions || after.reviews != before.reviews || after.owed != 0 {
		t.Errorf("%d comments anchored in the spec's text left it %d versions, %d reviews and %d owed digests, want %d versions, %d reviews and none owed",
			comments, after.versions, after.reviews, after.owed, before.versions, before.reviews)
	}
	want := maps.Clone(before.events)
	want["comment.created"] += comments
	if !maps.Equal(after.events, want) {
		t.Errorf("%d comments anchored in the spec's text left the events %v (artifact.version and artifact.digest counted for the spec), want %v: a comment.created each and nothing else",
			comments, after.events, want)
	}
}
