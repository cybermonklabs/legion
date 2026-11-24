package graphcensus

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A document whose ref_key is past the store's bound has no node, and no writer writes one: the
// project census counts it on its own past_bound line and in no class, so its absence from the
// store is neither a missing node nor a hidden row. A comment on it is counted the same way.
//
// Its control is the past_bound column read as false: the document then reads as missing.
func TestTheProjectCensusCountsADocumentPastTheBoundAsAClassOfItsOwn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('PAST', 'Past', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PAST-1', 'PAST', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed the project and the issue: %v", err)
	}
	slug := strings.Repeat("s", graphstore.MaxAddressBytes)
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('PAST-1', 'PAST', $1, 'Long', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&document); err != nil {
		t.Fatalf("seed the document past the bound: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body) values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark.')
	`, document); err != nil {
		t.Fatalf("seed the comment on it: %v", err)
	}
	census, err := ProjectCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	// The issue was seeded with no node, so it reads missing; only the two kinds past the bound are
	// held here.
	for _, kind := range census {
		if kind.Kind != graphstore.KindArtifact && kind.Kind != graphstore.KindComment {
			if kind.PastBound != 0 {
				t.Errorf("the project census reads past_bound=%d for %s, want 0", kind.PastBound, kind.Kind)
			}
			continue
		}
		if kind.PastBound != 1 || kind.Findings() != 0 || kind.Rows != 0 {
			t.Errorf("the project census reads %+v for %s, want past_bound=1, no row counted and no finding", kind, kind.Kind)
		}
	}
	incomplete, err := ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	for _, line := range incomplete {
		if strings.HasPrefix(line, "artifact ") || strings.HasPrefix(line, "comment ") {
			t.Errorf("ProjectComplete names %q, want nothing of a row past the bound", line)
		}
	}
}
