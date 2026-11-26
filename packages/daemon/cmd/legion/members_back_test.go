package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// seedMembersBackStore writes on st what a root's departure leaves of the project CAPTURE:
// CAPTURE-10, a root a person set to backlog; CAPTURE-11, a member of its tree in implementing whose
// implementer the daemon suspended; and CAPTURE-12, one whose implementer the operator suspended.
func seedMembersBackStore(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, issue := range []struct{ key, tree, phase, status string }{
		{"CAPTURE-10", "CAPTURE-10", "done", "backlog"},
		{"CAPTURE-11", "CAPTURE-10", "implementing", "in_progress"},
		{"CAPTURE-12", "CAPTURE-10", "implementing", "in_progress"},
	} {
		if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, dispatch_status, rank,
			last_dispatch_seq, repo, default_branch, github_repo) values ($1, $2, 'CAPTURE', $1, $3, 1, $4, $4, $1, 1, 'acme/widgets', 'main', 'acme/widgets')`,
			issue.key, issue.tree, issue.phase, issue.status); err != nil {
			t.Fatalf("record %s: %v", issue.key, err)
		}
	}
	for issue, by := range map[string]string{"CAPTURE-11": "daemon", "CAPTURE-12": "operator"} {
		token, err := legionclaim.NewToken("capture", issue, legionclaim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by, tree_epoch)
			values ($1, 'capture', 'CAPTURE-10', $2, 'implementer', 1, $3, $4, 'suspended', 0, 0, 0, 0, $5, 1)`,
			string(token), issue, "ses-"+string(token), "/sessions/"+string(token)+".jsonl", by); err != nil {
			t.Fatalf("record the claim %s: %v", token, err)
		}
	}
}

// membersBackStoreRows is every row of the tables a conversion writes on st, as text: what the
// listing must leave as it found it.
func membersBackStoreRows(t *testing.T, st *store.Store) string {
	t.Helper()
	var rows string
	if err := st.Pool().QueryRow(context.Background(), `select concat_ws(' | ',
		(select string_agg(concat_ws(' ', key, phase, generation, status), ', ' order by key) from issues),
		(select string_agg(concat_ws(' ', token, state, suspended_by), ', ' order by token) from claims),
		(select count(*)::text from outbox), (select count(*)::text from phases), (select count(*)::text from tree_conversions))`).Scan(&rows); err != nil {
		t.Fatalf("read the store's rows: %v", err)
	}
	return rows
}

// `legion members-back` reads the store its configuration names, in a read-only transaction, and
// prints each member that the project's first boot on this release brings back with its phase and
// how its tree left, in the words an operator reads before the upgrade: CAPTURE-11, whose root
// CAPTURE-10 went to backlog, and not CAPTURE-12, whose implementer the operator suspended. It
// changes no row. On a store whose conversion has ended it prints that the conversion ran, and
// lists no member. No claim token or session id of the store is in what it prints.
func TestMembersBackListsEachMemberTheFirstBootBringsBackAndChangesNothing(t *testing.T) {
	t.Parallel()
	dsn := testpg.Clone(t, "legion_cmd_members_back")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	seedMembersBackStore(t, st)
	dir := t.TempDir()
	config := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(config, []byte("project: CAPTURE\npostgres_dsn: "+dsn+"\nstate_dir: "+filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	membersBack := func() (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"legion", "members-back", "--config", config}, &out, &errb)
		return code, out.String(), errb.String()
	}

	before := membersBackStoreRows(t, st)
	code, out, errb := membersBack()
	want := "CAPTURE: the first boot on this release brings back 1 member, each in the phase it stands in:\n" +
		"CAPTURE-11 implementing: its root CAPTURE-10 went to backlog\n"
	if code != 0 || out != want || errb != "" {
		t.Fatalf("legion members-back = %d, stdout %q, stderr %q; want 0 and stdout %q", code, out, errb, want)
	}
	if strings.Contains(out, "legion-capture-") || strings.Contains(out, "ses-") {
		t.Fatalf("legion members-back printed a claim token or a session id: %q", out)
	}
	if after := membersBackStoreRows(t, st); after != before {
		t.Fatalf("legion members-back changed the store:\nbefore %s\nafter  %s", before, after)
	}
	t.Logf("legion members-back --config %s printed:\n%s", config, out)

	if _, err := st.Pool().Exec(context.Background(), `insert into tree_conversions (project, step, converted_at)
		values ('CAPTURE', 12, '2026-11-24T12:00:00Z')`); err != nil {
		t.Fatalf("record the conversion of CAPTURE: %v", err)
	}
	code, out, errb = membersBack()
	want = "CAPTURE: the first boot's conversion ended at 2026-11-24T12:00:00Z. It brought the members back, and it brings none back again.\n"
	if code != 0 || out != want || errb != "" {
		t.Fatalf("legion members-back on a converted store = %d, stdout %q, stderr %q; want 0 and stdout %q", code, out, errb, want)
	}
}
