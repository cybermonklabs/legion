package main

import (
	"bytes"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestBlockIDCensusCountsOnlySharedIDsOfOneIssueRoom(t *testing.T) {
	database := storetest.Open(t)
	read := func(want string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := census(t.Context(), []string{"--block-ids"}, database.Pool.Config().ConnString(), &out, &errOut)
		if code != 0 || errOut.Len() != 0 || out.String() != want {
			t.Fatalf("census --block-ids exited %d: stderr %q, stdout:\n%s\nwant stdout:\n%s", code, errOut.String(), out.String(), want)
		}
	}
	read("census: block_ids issues=0 shared=0\n")
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee) values ('CENSUS', 'Census', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
		  ('CENSUS-1', 'CENSUS', 1, 'Shared', '{"kind":"user","id":"alice"}', 'U'),
		  ('CENSUS-2', 'CENSUS', 2, 'Not shared', '{"kind":"user","id":"alice"}', 'V');
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		select issue, 'CENSUS', slug, slug, kind, primary_doc, role, '{"kind":"user","id":"alice"}'::jsonb
		from (values
		  ('CENSUS-1', 'spec', 'doc', true, null),
		  ('CENSUS-1', 'verification-md', 'doc', false, 'verification'),
		  ('CENSUS-1', 'plan-md', 'doc', false, null),
		  ('CENSUS-1', 'notes-md', 'doc', false, null),
		  ('CENSUS-2', 'spec', 'doc', true, null),
		  ('CENSUS-2', 'plan-md', 'file', false, null),
		  (null, 'plan-md', 'doc', false, null)
		) d(issue, slug, kind, primary_doc, role);
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select a.id, b.block, a.ref_key || '#' || b.block, 'paragraph', '', '', 1, now()
		from artifacts a cross join (values ('r1'), ('r2'), ('r3')) b(block)
		where a.project_key = 'CENSUS'
		  and case b.block
		      when 'r1' then true
		      when 'r2' then a.issue_key = 'CENSUS-1' and a.slug in ('spec', 'plan-md')
		      else a.slug in ('spec', 'notes-md') or a.issue_key is null
		      end;
	`); err != nil {
		t.Fatalf("seed role documents and their blocks: %v", err)
	}
	read("census: block_ids issues=2 shared=2\n" +
		`census: shared_block_id {"issue":"CENSUS-1","block":"r1","documents":["plan-md","spec","verification-md"]}` + "\n" +
		`census: shared_block_id {"issue":"CENSUS-1","block":"r2","documents":["plan-md","spec"]}` + "\n")
}
