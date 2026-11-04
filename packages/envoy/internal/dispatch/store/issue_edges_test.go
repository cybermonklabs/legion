package store

import (
	"context"
	"testing"
)

func TestIssueEdgeMigrationKeepsLegacyViewAndOldInsert(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261104090146)
	var before, after string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("issue migration changed the legacy graph view")
	}
	if _, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('OLD','Old writer');
 insert into issues (key,project_key,number,title,created_by,rank) values ('OLD-1','OLD',1,'Old shape','{"kind":"user","id":"alice"}','U');
 insert into issues (key,project_key,number,title,created_by,rank,status,closed_at) values ('OLD-2','OLD',2,'Duplicate','{"kind":"user","id":"alice"}','V','done',now()+interval '1 second');
 insert into comments (issue_key,author,body) values ('OLD-2','{"kind":"user","id":"alice"}','Duplicate of dispatch://OLD-1.');
 `); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_issue_edges where from_id='OLD-2' and kind='duplicate_of' and to_id='OLD-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("comment duplicate count=%d", count)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set status='backlog',closed_at=null where key='OLD-2'`); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_issue_edges where from_id='OLD-2'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reopening retained the duplicate edge")
	}
	var compatible bool
	if err := database.Pool.QueryRow(ctx, `select array(select attname||':'||atttypid::text from pg_attribute where attrelid='graph_edges'::regclass and attnum>0 order by attnum)=array(select attname||':'||atttypid::text from pg_attribute where attrelid='graph_issue_edges'::regclass and attnum>0 order by attnum)`).Scan(&compatible); err != nil {
		t.Fatal(err)
	}
	if !compatible {
		t.Fatal("issue edge columns differ from the graph")
	}
}
