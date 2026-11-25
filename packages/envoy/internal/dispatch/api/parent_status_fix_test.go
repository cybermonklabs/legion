// parent_status_fix_test.go holds the tests of R25's fix (parent_status_fix.go): the one-time
// change, at boot with DISPATCH_PARENT_STATUS_RULE on, of the pairs of a parent and a child that
// break R24. The tests run it over the chains captured from a deployment on 2026-11-20, stored
// with neutral keys (testdata/parent-status-fix/chains.jsonl), and over a few rows beside
// them for the cases the chains lack.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fixIssue is an issue as the fix's tests load it and read it back.
type fixIssue struct {
	Key    string   `json:"key"`
	Status string   `json:"status"`
	Parent *string  `json:"parent"`
	Labels []string `json:"labels"`
}

// fixSyntheticIssues are the rows beside the captured chains, for the two cases the chains lack:
// an open issue (ACME-903) under an open parent (ACME-902) under a done grandparent (ACME-901),
// which stays under its parent while the fix unlinks that parent; and a chain of three
// not-started issues (ACME-911, ACME-912, ACME-913) over an issue in work (ACME-914), which the
// fix moves into work highest first.
var fixSyntheticIssues = []fixIssue{
	{Key: "ACME-901", Status: "done"},
	{Key: "ACME-902", Status: "backlog", Parent: new("ACME-901")},
	{Key: "ACME-903", Status: "triage", Parent: new("ACME-902")},
	{Key: "ACME-911", Status: "icebox"},
	{Key: "ACME-912", Status: "triage", Parent: new("ACME-911")},
	{Key: "ACME-913", Status: "backlog", Parent: new("ACME-912")},
	{Key: "ACME-914", Status: "in_progress", Parent: new("ACME-913")},
}

// fixUnlink is an issue that the fix unlinks, and the done parent it leaves.
type fixUnlink struct{ key, parent string }

// fixUnlinks is each issue that the fix unlinks over the fixture, in the order of its writes,
// which is key order: by project, then by number. ACME-902 is a synthetic row, and the rest are
// the captured chains' eleven.
var fixUnlinks = []fixUnlink{
	{"ACME-79", "ACME-22"},
	{"ACME-902", "ACME-901"},
	{"ZETA-1637", "ZETA-1407"},
	{"ZETA-1638", "ZETA-1407"},
	{"ZETA-1657", "ZETA-1407"},
	{"ZETA-1673", "ZETA-60"},
	{"ZETA-1683", "ZETA-60"},
	{"ZETA-1694", "ZETA-1473"},
	{"ZETA-1695", "ZETA-1473"},
	{"ZETA-1699", "ZETA-60"},
	{"ZETA-1701", "ZETA-60"},
	{"ZETA-1752", "ZETA-60"},
}

// fixMove is an issue that the fix moves to in_progress: its status before, and the issue in work
// under it that its comment names, the first in key order.
type fixMove struct{ key, from, inWork string }

// fixMoves is each issue that the fix moves to in_progress over the fixture, in the order of its
// writes: the highest first, then in key order. ACME-911, ACME-912 and ACME-913 are synthetic
// rows, and the rest are the captured chains' four: ACME-79, ZETA-1638 and ZETA-1694, then
// ZETA-1788, which is under ZETA-1694.
var fixMoves = []fixMove{
	{"ACME-79", "backlog", "ACME-469"},
	{"ACME-911", "icebox", "ACME-914"},
	{"ZETA-1638", "backlog", "ZETA-1672"},
	{"ZETA-1694", "backlog", "ZETA-1789"},
	{"ACME-912", "triage", "ACME-914"},
	{"ZETA-1788", "backlog", "ZETA-1938"},
	{"ACME-913", "backlog", "ACME-914"},
}

// fixPullRequest is the pull request that ACME-79 links, which its unlink keeps.
const fixPullRequest = "https://github.com/acme/widgets/pull/79"

// fixUnlinkComment is the comment of an unlink, as storedComments lists it.
func fixUnlinkComment(unlink fixUnlink) string {
	return unlink.key + " system:parent-status-rule: Dispatch's parent status rule (R24): " + unlink.parent +
		" is done and this issue is not, so Dispatch removed the link. This issue keeps its status, its labels and the issues under it."
}

// fixMoveComment is the comment of a move, as storedComments lists it.
func fixMoveComment(move fixMove) string {
	return move.key + " system:parent-status-rule: Dispatch's parent status rule (R24): " + move.inWork +
		", in work, is under this issue, so Dispatch moved it from `" + move.from + "` to `in_progress`."
}

// fixComments is every comment that the fix leaves over the fixture, in the order of its writes:
// each unlink's, then each move's. ACME-79 has two, its unlink's and its move's.
func fixComments() []string {
	var comments []string
	for _, unlink := range fixUnlinks {
		comments = append(comments, fixUnlinkComment(unlink))
	}
	for _, move := range fixMoves {
		comments = append(comments, fixMoveComment(move))
	}
	return comments
}

// fixRig is a server, with the rule off, whose database holds the fixture as a boot finds it
// before the fix: the captured chains and the synthetic rows, with no record of the fix.
type fixRig struct {
	ruleRig
	// loaded is each issue of the fixture as loaded, by key, and keys is their keys.
	loaded map[string]fixIssue
	keys   []string
}

// newFixRig loads the fixture into a server's database, in one batch: every parent before its
// children, each issue with its key, number, status, parent and labels, the pull request link of
// ACME-79, and each project's next_number past its largest number. Every parent that the fixture
// names is in it.
func newFixRig(t *testing.T) fixRig {
	t.Helper()
	ctx := context.Background()
	rig := newRuleRig(t, false)
	createTestProject(t, rig.handler, "ZETA")
	data, err := os.ReadFile(filepath.Join("testdata", "parent-status-fix", "chains.jsonl"))
	if err != nil {
		t.Fatalf("read the captured chains: %v", err)
	}
	var issues []fixIssue
	for line := range strings.Lines(string(data)) {
		var issue fixIssue
		if err := json.Unmarshal([]byte(line), &issue); err != nil {
			t.Fatalf("decode the captured issue %q: %v", line, err)
		}
		issues = append(issues, issue)
	}
	issues = append(issues, fixSyntheticIssues...)
	loaded := make(map[string]fixIssue, len(issues))
	for _, issue := range issues {
		if issue.Labels == nil {
			issue.Labels = []string{}
		}
		loaded[issue.Key] = issue
	}
	batch := &pgx.Batch{}
	for inserted := map[string]bool{}; len(inserted) < len(issues); {
		progressed := false
		for _, issue := range issues {
			if inserted[issue.Key] || (issue.Parent != nil && !inserted[*issue.Parent]) {
				continue
			}
			project, digits, _ := strings.Cut(issue.Key, "-")
			number, err := strconv.Atoi(digits)
			if err != nil {
				t.Fatalf("the fixture's key %s has no number: %v", issue.Key, err)
			}
			batch.Queue(`
				insert into issues (key, project_key, number, title, status, parent_key, labels, created_by, rank, closed_at)
				values ($1, $2, $3, $4, $5::text, $6, $7, '{"kind":"user","id":"alice"}', $8, case when $5::text = 'done' then now() end)
			`, issue.Key, project, number, "Issue "+issue.Key, issue.Status, issue.Parent, loaded[issue.Key].Labels, fmt.Sprintf("U%06d", number))
			inserted[issue.Key], progressed = true, true
		}
		if !progressed {
			t.Fatalf("the fixture names a parent it does not hold: %d of %d issues loaded", len(inserted), len(issues))
		}
	}
	batch.Queue(`insert into issue_external_links (issue_key, url, kind) values ('ACME-79', '` + fixPullRequest + `', 'github_pr')`)
	batch.Queue(`update projects set next_number = (select max(number) + 1 from issues where project_key = projects.key) where key in ('ACME', 'ZETA')`)
	if err := rig.database.Pool.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	return fixRig{ruleRig: rig, loaded: loaded, keys: slices.Sorted(maps.Keys(loaded))}
}

// fix runs the fix with the setting on, as the boot of one process does, and fails t unless it
// answers the rule on.
func (rig fixRig) fix(t *testing.T) {
	t.Helper()
	rig.fixThrough(t, storetest.Leases(t, rig.database.Pool))
}

// fixThrough is fix with the lease pool leases.
func (rig fixRig) fixThrough(t *testing.T, leases duty.Leases) {
	t.Helper()
	rule, err := RunParentStatusFixAtBoot(t.Context(), rig.deps, leases, true)
	if err != nil || !rule.On() {
		t.Fatalf("the fix with the setting on answered the rule on=%t and %v, want the rule on and no error", rule.On(), err)
	}
}

// leaseStatementsOfAFix runs the fix as fix does, through a lease pool of its own whose statements
// it counts, and answers how many statements the fix sent through that pool. The fix takes its
// lease on that pool (duty.Exclusive), so a start that takes the lease sends some, and a start
// that takes no lease sends none.
func (rig fixRig) leaseStatementsOfAFix(t *testing.T) int64 {
	t.Helper()
	counter := &storetest.StatementCounter{}
	leases := storetest.Leases(t, storetest.TracedStore(t, rig.database, counter).Pool)
	sent := counter.Count()
	rig.fixThrough(t, leases)
	return counter.Count() - sent
}

// stored is each issue of the fixture as the database holds it now.
func (rig fixRig) stored(t *testing.T) map[string]fixIssue {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `select key, status, parent_key, labels from issues where key = any($1)`, rig.keys)
	if err != nil {
		t.Fatalf("read the fixture's issues: %v", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowToStructByPos[fixIssue])
	if err != nil {
		t.Fatalf("read the fixture's issues: %v", err)
	}
	stored := make(map[string]fixIssue, len(issues))
	for _, issue := range issues {
		stored[issue.Key] = issue
	}
	return stored
}

// fixed is the fixture as the fix leaves it: each issue of fixUnlinks a root, each issue of
// fixMoves in in_progress, and every other link, every other status and every label as loaded.
func (rig fixRig) fixed() map[string]fixIssue {
	fixed := maps.Clone(rig.loaded)
	for _, unlink := range fixUnlinks {
		issue := fixed[unlink.key]
		issue.Parent = nil
		fixed[unlink.key] = issue
	}
	for _, move := range fixMoves {
		issue := fixed[move.key]
		issue.Status = "in_progress"
		fixed[move.key] = issue
	}
	return fixed
}

// requireIssues fails t for each issue of the fixture that the database does not hold as want
// holds it: its status, its parent and its labels.
func (rig fixRig) requireIssues(t *testing.T, want map[string]fixIssue) {
	t.Helper()
	stored := rig.stored(t)
	for _, key := range rig.keys {
		got, wanted := stored[key], want[key]
		if got.Status != wanted.Status || !stringPointersEqual(got.Parent, wanted.Parent) || !slices.Equal(got.Labels, wanted.Labels) {
			t.Errorf("%s is %s under %s with labels %q, want %s under %s with labels %q",
				key, got.Status, fixParentName(got.Parent), got.Labels, wanted.Status, fixParentName(wanted.Parent), wanted.Labels)
		}
	}
}

// fixParentName is a parent as a failure names it.
func fixParentName(parent *string) string {
	if parent == nil {
		return "no parent"
	}
	return *parent
}

// requireRecord fails t unless the database holds exactly one record of the fix, with unlinked
// and moved.
func (rig fixRig) requireRecord(t *testing.T, unlinked, moved int) {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `select unlinked || ' ' || moved from parent_status_fix`)
	if err != nil {
		t.Fatalf("read the records of the fix: %v", err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the records of the fix: %v", err)
	}
	if want := []string{fmt.Sprintf("%d %d", unlinked, moved)}; !slices.Equal(records, want) {
		t.Errorf("the records of the fix (unlinked, moved) are %q, want %q", records, want)
	}
}

// fixWrites is the key of each issue whose field the fix's issue.updated events changed, in the
// order of their ids.
func (rig fixRig) fixWrites(t *testing.T, field string) []string {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select issue_key from events
		where type = 'issue.updated' and actor ->> 'kind' = 'system' and actor ->> 'id' = 'parent-status-rule'
		  and payload -> 'changed' ? $1
		order by id
	`, field)
	if err != nil {
		t.Fatalf("read the fix's writes of %s: %v", field, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the fix's writes of %s: %v", field, err)
	}
	return keys
}

// TestTheFixUnlinksEachOpenIssueFromItsDoneParentOnly: the fix's first step unlinks each open
// issue from its done parent, the captured chains' eleven among them (ACME-79 from ACME-22), in
// key order. An unlinked issue keeps its status (ACME-79 moves only in the second step), its
// labels, its pull request and the issues under it: ACME-469 stays under ACME-79, ZETA-1672 under
// ZETA-1638, and ZETA-1789 and ZETA-1788 under ZETA-1694. An open issue whose parent is open
// stays under it although its grandparent is done (ACME-903), and every other link stays as it
// was captured. Each unlinked issue has the unlink's comment, which names the done parent.
func TestTheFixUnlinksEachOpenIssueFromItsDoneParentOnly(t *testing.T) {
	rig := newFixRig(t)
	rig.fix(t)

	stored := rig.stored(t)
	unlinked := map[string]bool{}
	order := make([]string, 0, len(fixUnlinks))
	for _, unlink := range fixUnlinks {
		unlinked[unlink.key] = true
		order = append(order, unlink.key)
		if issue := stored[unlink.key]; issue.Parent != nil {
			t.Errorf("%s is still under %s, want it unlinked from its done parent %s", unlink.key, *issue.Parent, unlink.parent)
		}
	}
	for _, key := range rig.keys {
		if loaded := rig.loaded[key]; !unlinked[key] && !stringPointersEqual(stored[key].Parent, loaded.Parent) {
			t.Errorf("%s is under %s, want it under %s as captured", key, fixParentName(stored[key].Parent), fixParentName(loaded.Parent))
		}
	}
	for child, parent := range map[string]string{
		"ACME-469": "ACME-79", "ZETA-1672": "ZETA-1638", "ZETA-1789": "ZETA-1694", "ZETA-1788": "ZETA-1694", "ACME-903": "ACME-902",
	} {
		if got := stored[child].Parent; got == nil || *got != parent {
			t.Errorf("%s is under %s, want it under %s", child, fixParentName(got), parent)
		}
	}
	if got := rig.fixWrites(t, "parent"); !slices.Equal(got, order) {
		t.Errorf("the fix unlinked %q in that order, want %q", got, order)
	}
	var links []string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(url order by url), '{}') from issue_external_links where issue_key = 'ACME-79'
	`).Scan(&links); err != nil {
		t.Fatalf("read the links of ACME-79: %v", err)
	}
	if !slices.Equal(links, []string{fixPullRequest}) {
		t.Errorf("ACME-79 links %q, want its pull request %s kept", links, fixPullRequest)
	}
	rig.requireIssues(t, rig.fixed())
	requireComments(t, rig.database, rig.keys, fixComments()...)
	rig.requireRecord(t, len(fixUnlinks), len(fixMoves))
}

// TestTheFixMovesNotStartedAncestorsIntoWorkOverTheLinksLeft: the fix's second step, over the
// links the first step leaves, moves each not-started ancestor of an issue in work to
// in_progress, the highest first and then in key order: ACME-79 (whose done parent the first step
// unlinked), ZETA-1638 and ZETA-1694 before ZETA-1788, which is under ZETA-1694, and the
// synthetic chain ACME-911, ACME-912, ACME-913 from its top. No done issue is reopened: over the
// links left, each ancestor of an issue in work is in work or not started. Each moved issue has
// the move's comment, which names the issue in work under it, first in key order, and its status
// before. After the fix no pair of the fixture breaks the rule.
func TestTheFixMovesNotStartedAncestorsIntoWorkOverTheLinksLeft(t *testing.T) {
	rig := newFixRig(t)
	rig.fix(t)

	want := make([]string, 0, len(fixMoves))
	for _, move := range fixMoves {
		want = append(want, move.key)
	}
	if got := rig.fixWrites(t, "status"); !slices.Equal(got, want) {
		t.Errorf("the fix moved %q to in_progress in that order, want %q", got, want)
	}
	stored := rig.stored(t)
	states := make(map[string]ruleState, len(stored))
	for key, issue := range stored {
		states[key] = ruleState{issue.Status, issue.Parent}
	}
	if broken := breaches(states); len(broken) > 0 {
		t.Errorf("after the fix these pairs break the rule: %s", strings.Join(broken, ", "))
	}
	rig.requireIssues(t, rig.fixed())
	requireComments(t, rig.database, rig.keys, fixComments()...)
	rig.requireRecord(t, len(fixUnlinks), len(fixMoves))
}

// TestTheFixClosesNothingAndChangesNoChildsStatus: the fix closes no issue, and an unlink changes
// no status: every status but the moves' to in_progress is the one captured, and each unlink's
// issue.updated changed the parent alone.
func TestTheFixClosesNothingAndChangesNoChildsStatus(t *testing.T) {
	rig := newFixRig(t)
	rig.fix(t)

	moved := map[string]bool{}
	for _, move := range fixMoves {
		moved[move.key] = true
	}
	stored := rig.stored(t)
	for _, key := range rig.keys {
		loaded, got := rig.loaded[key], stored[key]
		switch {
		case got.Status == "done" && loaded.Status != "done":
			t.Errorf("the fix closed %s, which was %s", key, loaded.Status)
		case !moved[key] && got.Status != loaded.Status:
			t.Errorf("the fix moved %s from %s to %s, want its status kept", key, loaded.Status, got.Status)
		}
	}
	var closes int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from events where type = 'issue.closed'`).Scan(&closes); err != nil {
		t.Fatalf("count the closes: %v", err)
	}
	if closes != 0 {
		t.Errorf("the fix appended %d issue.closed events, want none", closes)
	}
	rows, err := rig.database.Pool.Query(context.Background(), `
		select issue_key || ' ' || (payload -> 'changed')::text from events
		where type = 'issue.updated' and actor ->> 'id' = 'parent-status-rule' and payload -> 'changed' ? 'parent'
		order by id
	`)
	if err != nil {
		t.Fatalf("read the unlinks' events: %v", err)
	}
	changed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the unlinks' events: %v", err)
	}
	want := make([]string, 0, len(fixUnlinks))
	for _, unlink := range fixUnlinks {
		want = append(want, unlink.key+` ["parent"]`)
	}
	if !slices.Equal(changed, want) {
		t.Errorf("the unlinks changed\n%s\nwant\n%s", strings.Join(changed, "\n"), strings.Join(want, "\n"))
	}
	rig.requireIssues(t, rig.fixed())
}

// TestTheFixRunsBeforeAnyOtherWriteAndOnlyWithItsSetting: with the setting off, the boot's call
// answers the rule off and sends no statement through the store or its lease pool, so it reads
// nothing and writes no row, event, comment or record. With the setting on, it answers only once
// every change of the fix and its record are committed, as another session reads them, and it
// answers the rule on: a server built with that rule refuses a write that breaks it.
func TestTheFixRunsBeforeAnyOtherWriteAndOnlyWithItsSetting(t *testing.T) {
	rig := newFixRig(t)

	t.Run("off", func(t *testing.T) {
		before := databaseFingerprint(t, rig.database)
		counter := &storetest.StatementCounter{}
		deps := rig.deps
		deps.Store = storetest.TracedStore(t, rig.database, counter)
		leases := storetest.Leases(t, deps.Store.Pool)
		sent := counter.Count()
		rule, err := RunParentStatusFixAtBoot(t.Context(), deps, leases, false)
		if err != nil || rule.On() {
			t.Fatalf("the fix with the setting off answered the rule on=%t and %v, want the rule off and no error", rule.On(), err)
		}
		if made := counter.Count() - sent; made != 0 {
			t.Errorf("the fix with the setting off sent %d statements, want none", made)
		}
		assertUnchanged(t, "the fix with the setting off", before, databaseFingerprint(t, rig.database))
		rig.requireIssues(t, rig.loaded)
	})

	t.Run("on", func(t *testing.T) {
		rule, err := RunParentStatusFixAtBoot(t.Context(), rig.deps, storetest.Leases(t, rig.database.Pool), true)
		if err != nil || !rule.On() {
			t.Fatalf("the fix with the setting on answered the rule on=%t and %v, want the rule on and no error", rule.On(), err)
		}
		// Another session reads what the fix committed before it answered.
		other := fixRig{ruleRig: ruleRig{database: storetest.SecondProcess(t, rig.database)}, loaded: rig.loaded, keys: rig.keys}
		other.requireIssues(t, rig.fixed())
		other.requireRecord(t, len(fixUnlinks), len(fixMoves))
		requireComments(t, other.database, rig.keys, fixComments()...)

		deps := rig.deps
		deps.ParentStatusRule = rule
		mux := http.NewServeMux()
		served := register(mux, t.Context(), deps)
		t.Cleanup(func() { waitBackgroundOf(t, served) })
		// ZETA-1638 is in work now, over ZETA-1672 in needs_review: its move back to backlog
		// breaks the rule.
		write := patchOf("ZETA-1638", map[string]any{"status": "backlog"})
		requireParentStatusConflict(t, write.String(), dispatchRequest(t, mux, write.method, write.path, write.body, "alice"), "ZETA-1672")
		if status, _ := rig.issue(t, "ZETA-1638"); status != "in_progress" {
			t.Errorf("the refused write left ZETA-1638 %s, want in_progress", status)
		}
	})
}

// TestASecondStartOfTheFixChangesNothing: a boot that finds the fix's record runs nothing: it
// sends no statement through its lease pool, so it takes no lease, and it writes no row, event,
// comment or record. A boot that stopped after the fix's last step and before its record plans no
// step the next time, and writes only the record, with nothing unlinked and nothing moved. That
// start takes the lease, and its lease pool counts its statements, so the count can see a lease.
func TestASecondStartOfTheFixChangesNothing(t *testing.T) {
	rig := newFixRig(t)
	rig.fix(t)
	awaitSettled(t, rig.database)
	fixed := databaseFingerprint(t, rig.database)

	if sent := rig.leaseStatementsOfAFix(t); sent != 0 {
		t.Errorf("a second start of the fix sent %d statements through its lease pool, want none: it takes no lease", sent)
	}
	assertUnchanged(t, "a second start of the fix", fixed, databaseFingerprint(t, rig.database))
	rig.requireRecord(t, len(fixUnlinks), len(fixMoves))

	if _, err := rig.database.Pool.Exec(context.Background(), `delete from parent_status_fix`); err != nil {
		t.Fatalf("remove the record, as a boot that stopped before it leaves the database: %v", err)
	}
	if sent := rig.leaseStatementsOfAFix(t); sent == 0 {
		t.Errorf("a start of the fix after a boot that stopped before its record sent no statement through its lease pool, want the lease taken")
	}
	after := databaseFingerprint(t, rig.database)
	delete(fixed, "parent_status_fix")
	delete(after, "parent_status_fix")
	assertUnchanged(t, "a start of the fix after a boot that stopped before its record", fixed, after)
	rig.requireRecord(t, 0, 0)
	rig.requireIssues(t, rig.fixed())
	requireComments(t, rig.database, rig.keys, fixComments()...)
}

// TestTheFixRunsOnceUnderItsLeaseBesideASecondProcess: two processes boot together, each with a
// store pool and a lease pool of its own on the one database: the lease is a session-level
// advisory lock, so two pools stand in for two processes. One of them runs the fix under the
// lease, and the other waits. Each answers the rule on only once the record exists, so the
// database holds one record, each change once and one comment for each step.
func TestTheFixRunsOnceUnderItsLeaseBesideASecondProcess(t *testing.T) {
	rig := newFixRig(t)
	second := rig.deps
	second.Store = storetest.SecondProcess(t, rig.database)

	type boot struct {
		name     string
		deps     Deps
		on       bool
		err      error
		recorded bool
	}
	boots := []*boot{{name: "the first process", deps: rig.deps}, {name: "the second process", deps: second}}
	start := make(chan struct{})
	var booted sync.WaitGroup
	for _, process := range boots {
		pool := storetest.Leases(t, process.deps.Store.Pool)
		booted.Go(func() {
			<-start
			rule, err := RunParentStatusFixAtBoot(t.Context(), process.deps, pool, true)
			process.on, process.err = rule.On(), err
			// What the process reads the moment it answers: the record is there.
			process.recorded, process.err = readRecorded(process.deps.Store, process.err)
		})
	}
	close(start)
	booted.Wait()
	for _, process := range boots {
		if process.err != nil || !process.on {
			t.Errorf("%s answered the rule on=%t and %v, want the rule on and no error", process.name, process.on, process.err)
		}
		if !process.recorded {
			t.Errorf("%s answered before the record of the fix existed", process.name)
		}
	}
	if got := rig.fixWrites(t, "parent"); len(got) != len(fixUnlinks) {
		t.Errorf("the two processes unlinked %q, want each issue of fixUnlinks once", got)
	}
	if got := rig.fixWrites(t, "status"); len(got) != len(fixMoves) {
		t.Errorf("the two processes moved %q, want each issue of fixMoves once", got)
	}
	rig.requireRecord(t, len(fixUnlinks), len(fixMoves))
	rig.requireIssues(t, rig.fixed())
	requireComments(t, rig.database, rig.keys, fixComments()...)
}

// readRecorded reads whether database holds the fix's record, after a boot that answered err: a
// boot that failed reads nothing and keeps its error.
func readRecorded(database *store.Store, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	return parentStatusFixRecorded(context.Background(), database.Pool)
}

// TestTheFixWritesEventsTheDaemonReads: each change of the fix is an issue.updated event of the
// issue it changes, written as Dispatch's system actor and left for the outbox to publish, whose
// payload holds the fields the daemon's decodeDispatchEvent reads (packages/daemon/internal/intake/
// decode.go): the key, which is the event's issue, the status, the parent and what changed. An
// unlink also appends child.removed on the done parent it leaves, and a move of an issue that
// has a parent appends child.status on that parent, as the PATCH's write does.
func TestTheFixWritesEventsTheDaemonReads(t *testing.T) {
	rig := newFixRig(t)
	rig.fix(t)

	var want []string
	for _, unlink := range fixUnlinks {
		want = append(want,
			fmt.Sprintf(`issue.updated %s key="%s" status="%s" parent=null changed=["parent"]`, unlink.key, unlink.key, rig.loaded[unlink.key].Status),
			fmt.Sprintf(`child.removed %s child_key="%s"`, unlink.parent, unlink.key))
	}
	fixed := rig.fixed()
	for _, move := range fixMoves {
		parent := "null"
		if above := fixed[move.key].Parent; above != nil {
			parent = strconv.Quote(*above)
		}
		want = append(want, fmt.Sprintf(`issue.updated %s key="%s" status="in_progress" parent=%s changed=["status"]`, move.key, move.key, parent))
		if above := fixed[move.key].Parent; above != nil {
			want = append(want, fmt.Sprintf(`child.status %s child_key="%s" from="%s" to="in_progress"`, *above, move.key, move.from))
		}
	}

	rows, err := rig.database.Pool.Query(context.Background(), `
		select type, issue_key, actor ->> 'kind', actor ->> 'id', published_at is null, payload from events
		where type in ('issue.updated', 'child.removed', 'child.status') and issue_key = any($1)
		order by id
	`, rig.keys)
	if err != nil {
		t.Fatalf("read the fix's events: %v", err)
	}
	type storedEvent struct {
		kind, issue, actorKind, actorID string
		unpublished                     bool
		payload                         map[string]json.RawMessage
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedEvent, error) {
		var event storedEvent
		err := row.Scan(&event.kind, &event.issue, &event.actorKind, &event.actorID, &event.unpublished, &event.payload)
		return event, err
	})
	if err != nil {
		t.Fatalf("read the fix's events: %v", err)
	}
	got := make([]string, 0, len(stored))
	for _, event := range stored {
		if event.actorKind != parentStatusRuleActor.Kind || event.actorID != parentStatusRuleActor.ID {
			t.Errorf("the %s of %s was written by %s:%s, want Dispatch's system actor %s:%s",
				event.kind, event.issue, event.actorKind, event.actorID, parentStatusRuleActor.Kind, parentStatusRuleActor.ID)
		}
		if !event.unpublished {
			t.Errorf("the %s of %s is published, want it left for the outbox", event.kind, event.issue)
		}
		fields := []string{"key", "status", "parent", "changed"}
		switch event.kind {
		case "child.removed":
			fields = []string{"child_key"}
		case "child.status":
			fields = []string{"child_key", "from", "to"}
		}
		line := event.kind + " " + event.issue
		for _, field := range fields {
			line += " " + field + "=" + string(event.payload[field])
		}
		got = append(got, line)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the fix's events are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
