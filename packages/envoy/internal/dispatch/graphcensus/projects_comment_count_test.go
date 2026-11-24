package graphcensus

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An issue's comment has no owner document and often no anchor, so the census's test of whether its
// relations name a document past the store's bound reads two absent addresses. Absent is not past
// the bound: the comment is counted among the rows, read against its node, and here, having none,
// named missing, with nothing past the bound.
//
// Its control is the test written without its null guards, as it was once: an absent address
// then makes past_bound null, which drops the comment from rows and skips its node.
func TestAnIssueCommentWithNoAnchorIsCountedAmongTheRows(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CNT', 'Count', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CNT-1', 'CNT', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into comments (issue_key, author, body) values ('CNT-1', '{"kind":"user","id":"alice"}', 'A remark.');
	`); err != nil {
		t.Fatalf("seed the project, the issue and the comment: %v", err)
	}
	census, err := ProjectCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	for _, kind := range census {
		if kind.Kind != graphstore.KindComment {
			continue
		}
		if kind.Rows != 1 || kind.Missing != 1 || kind.PastBound != 0 {
			t.Errorf("the project census reads %+v for comments, want rows=1 missing=1 past_bound=0", kind)
		}
		return
	}
	t.Fatal("the project census printed no comment kind")
}
