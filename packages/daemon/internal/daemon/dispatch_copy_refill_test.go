package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// copyRefillLine is the line the boot that refills its project's rows of the copy of Dispatch's
// issues logs, once, at info.
const copyRefillLine = "workflow: the first boot on this release refilled the copy of Dispatch's issues from the listing"

// The first boot of a project on this release empties that project's rows of the copy of
// Dispatch's issues and fills them from its listing, in the boot's own transaction, and records
// that it did; a later boot refills nothing (dispatch://INFRA-3320/spec R16).
//
// The database is the one the release before leaves, seeded as that release's daemon left the
// copy: <P>-1 under <P>-OLD at 5, a parent 20261103183833_issue_slots took from the daemon's
// records, which no reading of Dispatch wrote; <P>-2 at 3, which the first listing does not hold;
// and OTHER-1 of project OTHER at 9, a row the periodic turn wrote for a blocker of another
// project. The boot's Store.Migrate gives each value of those rows its row's sequence and marks
// both projects. The first boot lists <P>-1 at 5 under <P>-NEW: a listing at the parent's own
// sequence replaces nothing (record.Store.PutDispatchIssue), so only the refill puts <P>-NEW in
// the copy, and only the refill takes out <P>-2. Neither listed issue is Legion's, so admission
// asks Dispatch nothing more. The second boot lists <P>-3 alone and finds the mark stamped: it
// removes nothing and stamps nothing.
func TestTheFirstBootRefillsTheCopyFromAListingOnce(t *testing.T) {
	var listing atomic.Pointer[[]map[string]any]
	cfg := workflowConfigOn(t, workflowNATS(t), func(t *testing.T, project string) string {
		return dispatchBoard(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case servedBootCall(t, w, r, project):
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(*listing.Load()); err != nil {
					t.Errorf("write Dispatch issues: %v", err)
				}
			default:
				t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			}
		})
	})
	// The database as the release before dispatchValueSequencesMigration leaves it, so the boot's
	// own Store.Migrate applies that one over the rows seeded here.
	st, dsn := storeWithholding(t, "legion_copy_refill", dispatchValueSequencesMigration)
	cfg.PostgresDSN = dsn
	pool := st.Pool()
	project := cfg.Project
	first, second, third := project+"-1", project+"-2", project+"-3"
	if _, err := pool.Exec(context.Background(), `insert into dispatch_issues (key, project, status, blockers, dispatch_seq, parent)
		values ($1, $3, 'backlog', '{}', 5, $4), ($2, $3, 'backlog', '{}', 3, null), ('OTHER-1', 'OTHER', 'in_progress', '{}', 9, null)`,
		first, second, project, project+"-OLD"); err != nil {
		t.Fatalf("seed the copy as the release before left it: %v", err)
	}
	listing.Store(&[]map[string]any{listedBacklog(first, project+"-NEW", 5), listedBacklog(third, "", 6)})
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}

	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, o, jsonLogger(logs))
	want := map[string]copyRow{
		first:     {status: "backlog", parent: project + "-NEW", blockers: []string{}, parentSeq: 5, seq: 5},
		third:     {status: "backlog", blockers: []string{}, parentSeq: 6, seq: 6},
		"OTHER-1": {status: "in_progress", blockers: []string{}, parentSeq: 9, seq: 9},
	}
	if got := copyRows(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after the first boot = %+v, want %+v: %s under the listing's parent, %s gone, %s listed, and OTHER-1 as seeded",
			got, want, first, second, third)
	}
	marks := refillMarks(t, pool)
	if len(marks) != 2 || marks[project] == "" || marks["OTHER"] != "" {
		t.Fatalf("the refill marks after the first boot = %q, want %s stamped and OTHER marked and not stamped", marks, project)
	}
	stamped := marks[project]
	lines := loggedMessages(t, logs.String(), copyRefillLine)
	if len(lines) != 1 {
		t.Fatalf("the first boot logged %d refill lines, want 1\nlog:\n%s", len(lines), logs.String())
	}
	line := lines[0]
	if line["level"] != "INFO" || line["project"] != project || line["removed"] != float64(2) || line["written"] != float64(2) ||
		!reflect.DeepEqual(line["removedUnlisted"], []any{second}) {
		t.Fatalf("the refill line = %v, want it at INFO naming %s, 2 rows removed, 2 written, and %s removed and not listed", line, project, second)
	}

	d.stop()
	rebindHeldPorts(t, &cfg)
	listing.Store(&[]map[string]any{listedBacklog(third, "", 6)})
	again := &syncBuffer{}
	startDaemonLogging(t, cfg, o, jsonLogger(again))
	if got := copyRows(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy after the second boot = %+v, want %+v: a boot whose mark is stamped removes nothing", got, want)
	}
	marks = refillMarks(t, pool)
	if len(marks) != 2 || marks[project] != stamped || marks["OTHER"] != "" {
		t.Fatalf("the refill marks after the second boot = %q, want %s stamped at the first boot's %s and OTHER not stamped",
			marks, project, stamped)
	}
	if lines := loggedMessages(t, again.String(), copyRefillLine); len(lines) != 0 {
		t.Fatalf("the second boot logged the refill line %v, want none", lines)
	}
}

// listedBacklog is one summary of a Dispatch listing: key in backlog at seq under parent ("" for
// none), with no label and no claim, which admission takes nothing of.
func listedBacklog(key, parent string, seq int64) map[string]any {
	summary := map[string]any{"key": key, "title": "The copy's " + key, "status": "backlog", "rank": "a0",
		"labels": []string{}, "last_seq": seq, "blocked_by": []string{}}
	if parent != "" {
		summary["parent"] = parent
	}
	return summary
}

// copyRow is a row of the copy of Dispatch's issues as this test reads it: its status, its
// parent ("" for none), its blockers, the sequence of its parent and the row's own sequence.
type copyRow struct {
	status, parent string
	blockers       []string
	parentSeq, seq int64
}

// copyRows is every row of the copy, by key.
func copyRows(t *testing.T, pool *store.Pool) map[string]copyRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select key, status, coalesce(parent, ''), blockers, parent_seq, dispatch_seq
		from dispatch_issues order by key`)
	if err != nil {
		t.Fatalf("read the copy: %v", err)
	}
	defer rows.Close()
	held := map[string]copyRow{}
	for rows.Next() {
		var key string
		var row copyRow
		if err := rows.Scan(&key, &row.status, &row.parent, &row.blockers, &row.parentSeq, &row.seq); err != nil {
			t.Fatalf("scan a row of the copy: %v", err)
		}
		held[key] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the copy: %v", err)
	}
	return held
}

// refillMarks is the refilled_at of every project's refill mark, as Postgres spells it, by
// project, and "" for a mark not stamped.
func refillMarks(t *testing.T, pool *store.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select project, coalesce(refilled_at::text, '') from dispatch_issue_refills`)
	if err != nil {
		t.Fatalf("read the refill marks: %v", err)
	}
	defer rows.Close()
	marks := map[string]string{}
	for rows.Next() {
		var project, refilled string
		if err := rows.Scan(&project, &refilled); err != nil {
			t.Fatalf("scan a refill mark: %v", err)
		}
		marks[project] = refilled
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the refill marks: %v", err)
	}
	return marks
}
