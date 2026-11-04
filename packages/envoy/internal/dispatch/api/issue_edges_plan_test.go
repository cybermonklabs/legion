package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func TestIssueEdgePlansOnIssueAndCitationVolume(t *testing.T) {
	_, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('VOL','Volume');
 insert into issues (key,project_key,number,title,created_by,rank,parent_key)
 select 'VOL-'||n,'VOL',n,'Topic '||n,'{"kind":"user","id":"alice"}',n::text,case when n>1 then 'VOL-1' end
 from generate_series(1,20000) n;
 insert into artifacts (id,issue_key,project_key,slug,name,kind,is_primary,created_by)
 select md5(n::text)::uuid,'VOL-'||n,'VOL','spec','spec.md','doc',true,'{"kind":"user","id":"alice"}' from generate_series(1,20000) n;
 insert into refs (from_kind,from_id,kind,to_kind,to_id)
 select 'artifact',md5(n::text)::uuid::text,'mentions','issue','VOL-'||(20000-x) from generate_series(1,20000) n cross join generate_series(1,5) x;
 analyze issues; analyze artifacts; analyze refs;
 `); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass,true)`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql string
		args      []any
	}{
		{"legacy-point", `select * from graph_edges where from_kind='issue' and from_id=$1`, []any{"VOL-2"}},
		{"named-links", `select * from ` + refs.AllEdgesSQL + ` g where from_kind='issue' and from_id=$1`, []any{"VOL-2"}},
		{"neighbourhood", relatedGraphSQL, []any{"VOL", []string{"issue"}, []string{"VOL-19999"}, "VOL-2"}},
		{"shadow", issueLinksShadowSQL, []any{"VOL-2"}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var raw []byte
			if err := database.Pool.QueryRow(ctx, `explain (analyze,buffers,format json) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct {
				Execution float64 `json:"Execution Time"`
				Planning  float64 `json:"Planning Time"`
				Plan      map[string]any
			}
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan) != 1 {
				t.Fatalf("plan=%s", raw)
			}
			t.Logf("20000 issues, 20000 specs, 100000 citations: planning=%.3fms execution=%.3fms rows=%v", plan[0].Planning, plan[0].Execution, plan[0].Plan["Actual Rows"])
			if query.name == "neighbourhood" && plan[0].Plan["Actual Rows"] != float64(3) {
				t.Fatalf("missing nonempty volume result: %s", raw)
			}
		})
	}
	var after string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass,true)`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != definition {
		t.Fatal("named graph reads changed the legacy view")
	}
}
