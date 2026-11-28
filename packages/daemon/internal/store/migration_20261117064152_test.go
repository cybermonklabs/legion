package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// stepRecordsRelease names the release whose way back the roll forward's header
// (migrations.StepRecordsRollForward) carries, as a failure to read that block names it.
const stepRecordsRelease = "the roll forward of the release that records each step"

// wayBackOf is the block the header of the migration version carries, read by the one reader of a
// migration's way back (migrations.WayBack) from the embedded migration, which is that file's
// bytes as the binary ships them; what names the release a failure to read it is about. A header
// with no way back stops the test by name: a block this cannot find is a block it would pass
// while running nothing. Where internal/textcheck holds a block to the one docs/kubernetes.md
// gives, the block an operator runs is the one these tests run.
func wayBackOf(t *testing.T, version int, what string) string {
	t.Helper()
	block, err := migrations.WayBack(version)
	if err != nil {
		t.Fatalf("read the way back of %s: %v", what, err)
	}
	return block
}

// rowsAsText is each row query answers on store, as text, in the query's order: what a test holds
// a table to before and after a statement that must leave it as it was.
func rowsAsText(t *testing.T, store *Store, query string) []string {
	t.Helper()
	rows, err := store.Pool().Query(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return all
}

// rollBackStore is a store migrated to this release, on which a test seeds issues and their
// step_starts rows as this release leaves them, runs the way back, makes the earlier release's
// writes by hand, and rolls forward.
type rollBackStore struct {
	t     *testing.T
	store *Store
}

func newRollBackStore(t *testing.T) rollBackStore {
	t.Helper()
	store := emptyStore(t)
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return rollBackStore{t: t, store: store}
}

func (s rollBackStore) exec(sql string) {
	s.t.Helper()
	if _, err := s.store.Pool().Exec(s.t.Context(), sql); err != nil {
		s.t.Fatalf("%s: %v", sql, err)
	}
}

// issue is key in tree, standing in phase (held from heldFrom where that is not ""), at
// generation, its implementer at pass 1.
func (s rollBackStore) issue(key, tree, phase, heldFrom string, generation int) {
	s.t.Helper()
	held := "null"
	if heldFrom != "" {
		held = "'" + heldFrom + "'"
	}
	s.exec(fmt.Sprintf(`insert into issues (key, tree, project, title, phase, held_from, generation, status, rank, last_dispatch_seq)
		values ('%s', '%s', 'LEGION', '%s', '%s', %s, %d, 'in_progress', 'A', 1)`, key, tree, key, phase, held, generation))
	s.exec(fmt.Sprintf(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, pass)
		values ('%s', 'implementer', '', '', 0, '', 1)`, key))
}

// entry raises the entry of role on key, as each start that hands a phase worker its work does
// (workflow's enter), every release's.
func (s rollBackStore) entry(key, role string) {
	s.t.Helper()
	s.exec(fmt.Sprintf(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, pass, entry)
		values ('%s', '%s', '', '', 0, '', 0, 1)
		on conflict (issue, role) do update set entry = phases.entry + 1`, key, role))
}

// pullRequest is key's pull request, #number, in state.
func (s rollBackStore) pullRequest(key string, number int, state record.PullRequestState) {
	s.t.Helper()
	if err := Transact(s.t.Context(), s.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(s.t.Context(), tx, record.PullRequest{Issue: key, Repo: "sjawhar/legion", Number: number,
			Branch: "legion/" + key, HeadSHA: "head-" + key, State: state, MergeState: record.MergeStateClean, Failing: []string{}})
	}); err != nil {
		s.t.Fatalf("put the pull request of %s: %v", key, err)
	}
}

// stepRole and stepType are the role and the type of the record of each place (workflow's
// stepOfWalk); a retro's and a merge's type is "".
var (
	stepRole = map[string]string{"step 1": "planner", "step 2": "implementer", "step 3": "tester", "step 4": "reviewer",
		"step 5": "acceptance", "retro 0": "implementer", "merge 0": "merger"}
	stepType = map[string]string{"step 1": "planning", "step 2": "implementation", "step 3": "testing", "step 4": "review",
		"step 5": "review"}
)

// row is key's step_starts row at kind and place, at the count 2, whose record names a uuid of its
// own, as StepID's does: md5 of the issue, the kind and the place (recordID).
func (s rollBackStore) row(key, kind string, place int, run string) {
	s.t.Helper()
	at := fmt.Sprintf("%s %d", kind, place)
	s.exec(fmt.Sprintf(`insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
		values ('%[1]s', 'default', '%[2]s', %[3]d, 2, 'agent of %[1]s', '2026-11-17T01:00:00Z', '%[4]s',
		jsonb_strip_nulls(jsonb_build_object('id', md5('%[1]s %[2]s %[3]d')::uuid, 'task', 'default', 'step', %[3]d, 'kind', '%[2]s',
			'role', '%[5]s', 'type', nullif('%[6]s', ''), 'agent', 'agent of %[1]s', 'generation', 1, 'started_at', '2026-11-17T01:00:00Z')))`,
		key, kind, place, run, stepRole[at], stepType[at]))
}

// recordID is the id of the record row kept for place ("KEY kind place").
func (s rollBackStore) recordID(place string) string {
	s.t.Helper()
	var id string
	if err := s.store.Pool().QueryRow(s.t.Context(), `select md5($1)::uuid::text`, place).Scan(&id); err != nil {
		s.t.Fatalf("compute the id of %s: %v", place, err)
	}
	return id
}

// wayBack runs the way back an operator runs with the daemon stopped.
func (s rollBackStore) wayBack() {
	s.t.Helper()
	s.exec(wayBackOf(s.t, migrations.StepRecordsRollForward, stepRecordsRelease))
}

// rollForward is the newer image's Migrate, which applies the roll forward the way back re-armed.
func (s rollBackStore) rollForward() {
	s.t.Helper()
	if applied, err := s.store.Migrate(s.t.Context()); err != nil || applied != 1 {
		s.t.Fatalf("the roll forward's Migrate applied %d migrations, %v; want the one the way back re-armed", applied, err)
	}
}

// keptStart is what step_starts holds of one place: its run, its count and its record's id.
type keptStart struct {
	run    string
	starts int64
	id     string
}

// starts is each row of step_starts, by "KEY kind place".
func (s rollBackStore) starts() map[string]keptStart {
	s.t.Helper()
	rows, err := s.store.Pool().Query(s.t.Context(), `select issue || ' ' || kind || ' ' || place, run, starts, coalesce(record->>'id', '') from step_starts`)
	if err != nil {
		s.t.Fatalf("read step_starts: %v", err)
	}
	defer rows.Close()
	all := map[string]keptStart{}
	for rows.Next() {
		var place string
		var start keptStart
		if err := rows.Scan(&place, &start.run, &start.starts, &start.id); err != nil {
			s.t.Fatalf("scan step_starts: %v", err)
		}
		all[place] = start
	}
	if err := rows.Err(); err != nil {
		s.t.Fatalf("read step_starts: %v", err)
	}
	return all
}

// ends is every step record row of the outbox, by issue, decoded as the daemon reads it.
func (s rollBackStore) ends() map[string][]record.StepRecord {
	s.t.Helper()
	outbox, err := s.store.Pool().Query(s.t.Context(), `select issue, payload from outbox where kind = 'step_record' order by id`)
	if err != nil {
		s.t.Fatalf("read the outbox: %v", err)
	}
	defer outbox.Close()
	all := map[string][]record.StepRecord{}
	for outbox.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindStepRecord}
		if err := outbox.Scan(&row.Issue, &row.Payload); err != nil {
			s.t.Fatalf("scan the outbox: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			s.t.Fatalf("the roll forward's step record row of %s does not decode as the daemon reads it: %v", row.Issue, err)
		}
		all[row.Issue] = append(all[row.Issue], payload.(record.StepRecord))
	}
	if err := outbox.Err(); err != nil {
		s.t.Fatalf("read the outbox: %v", err)
	}
	return all
}

// assertRollForwardEnd fails unless end is the end the roll forward writes of place's record: the
// identity the row kept, stopped at the moment of the rollback.
func (s rollBackStore) assertRollForwardEnd(place string, end record.StepRecord, rolledBackAt time.Time) {
	s.t.Helper()
	issue, kindPlace, _ := strings.Cut(place, " ")
	if id := s.recordID(place); end.ID != id || end.Agent != "agent of "+issue || end.Role != stepRole[kindPlace] || end.Generation != 1 ||
		end.Result == nil || *end.Result != "stopped" || end.EndedAt == nil || !end.EndedAt.Equal(rolledBackAt) {
		s.t.Errorf("the roll forward's end of %s = %+v, want the record %s the row kept, stopped at the rollback, %s", place, end, id, rolledBackAt)
	}
}

// A rollback hands the store to an earlier release, which moves issues and starts and ends their
// steps without a row of step_starts. The way back keeps each issue's witness and the moment of the
// rollback, and re-arms the roll forward, which the newer image's Migrate then applies again: each
// run a row held open at the rollback, which the earlier release ended, gets the end of its
// record, stopped at that moment, under the identity the row kept; each place where that release
// may have started a run reads 'uncounted', a run it ended at a place it started none reads
// 'ended', and every other row stands as it was. Here the earlier release: raises a root's pass
// (its tester failed and ran again), moves another into the phase whose counted run had ended,
// re-admits a tree's root (its member's step starts again), lingers a tree's root (its member's
// step ends, but not a member's merge that awaits its pull request), releases a held issue into
// the phase it was held from, and re-admits a root of its own; it does nothing to the last root. A
// second run of the way back, before any roll forward, keeps the witness and the moment of the
// first, which the earlier release has moved since.
func TestTheRollForwardMarksUncountedEachIssueTheEarlierReleaseMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newRollBackStore(t)
	s.issue("LEGION-201", "LEGION-201", "testing", "", 1)
	s.row("LEGION-201", "step", 3, "open")
	s.row("LEGION-201", "step", 2, "ended")
	s.issue("LEGION-202", "LEGION-202", "testing", "", 1)
	s.row("LEGION-202", "step", 3, "open")
	s.row("LEGION-202", "step", 2, "ended")
	s.issue("LEGION-203", "LEGION-203", "implementing", "", 1)
	s.row("LEGION-203", "step", 3, "ended")
	s.issue("LEGION-210", "LEGION-210", "done", "", 1)
	s.issue("LEGION-211", "LEGION-210", "testing", "", 4)
	s.row("LEGION-211", "step", 3, "open")
	s.issue("LEGION-220", "LEGION-220", "done", "", 1)
	s.issue("LEGION-221", "LEGION-220", "testing", "", 4)
	s.row("LEGION-221", "step", 3, "open")
	s.issue("LEGION-222", "LEGION-220", "held", "merging", 4)
	s.row("LEGION-222", "merge", 0, "open")
	s.issue("LEGION-230", "LEGION-230", "held", "testing", 1)
	s.row("LEGION-230", "step", 3, "open")
	s.issue("LEGION-240", "LEGION-240", "reviewing", "", 1)
	s.row("LEGION-240", "step", 4, "open")

	s.wayBack()
	var recorded bool
	if err := s.store.Pool().QueryRow(ctx, `select exists (select 1 from schema_version where version = $1)`, migrations.StepRecordsRollForward).Scan(&recorded); err != nil || recorded {
		t.Fatalf("after the way back the roll forward is recorded = %t, %v; want its row deleted, so the roll forward applies it again", recorded, err)
	}
	var rolledBackAt time.Time
	if err := s.store.Pool().QueryRow(ctx, `select taken_at from step_starts_rollback where issue = 'LEGION-202'`).Scan(&rolledBackAt); err != nil {
		t.Fatalf("read the moment the way back kept: %v", err)
	}
	// The earlier release, whose every start of a phase worker raises its role's entry.
	s.exec(`update phases set pass = pass + 1 where issue = 'LEGION-202' and role = 'implementer'`)
	s.entry("LEGION-202", "implementer")
	s.entry("LEGION-202", "tester")
	s.exec(`update issues set phase = 'testing' where key = 'LEGION-203'`)
	s.entry("LEGION-203", "tester")
	s.wayBack()
	s.exec(`update issues set generation = 2 where key = 'LEGION-210'`)
	s.exec(`update issues set linger_until = now() + interval '1 hour' where key = 'LEGION-220'`)
	s.exec(`update issues set phase = 'testing', held_from = null where key = 'LEGION-230'`)
	s.exec(`update issues set generation = 2 where key = 'LEGION-240'`)

	s.rollForward()
	runs := s.starts()
	want := map[string]string{
		"LEGION-201 step 3": "open", "LEGION-201 step 2": "ended",
		"LEGION-202 step 3": "uncounted", "LEGION-202 step 2": "uncounted",
		"LEGION-203 step 3": "uncounted",
		"LEGION-211 step 3": "uncounted",
		// The linger ended LEGION-221's run and started none: nothing runs there to count.
		"LEGION-221 step 3": "ended", "LEGION-222 merge 0": "open",
		"LEGION-230 step 3": "open",
		"LEGION-240 step 4": "uncounted",
	}
	for place, run := range want {
		if runs[place].run != run {
			t.Errorf("after the roll forward %s reads %q, want %q", place, runs[place].run, run)
		}
	}
	if len(runs) != len(want) {
		t.Errorf("step_starts holds %d rows after the roll forward, want the %d it held: %v", len(runs), len(want), runs)
	}
	var kept int
	if err := s.store.Pool().QueryRow(ctx, `select count(*) from step_starts_rollback`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("step_starts_rollback holds %d rows after the roll forward, %v; want none, so a later rollback keeps its own", kept, err)
	}
	ends := s.ends()
	ended := []string{"LEGION-202 step 3", "LEGION-211 step 3", "LEGION-221 step 3", "LEGION-240 step 4"}
	if len(ends) != len(ended) {
		t.Errorf("the roll forward wrote the ends of %v, want those of the runs open at the rollback that the earlier release ended: %v", ends, ended)
	}
	for _, place := range ended {
		issue, _, _ := strings.Cut(place, " ")
		if len(ends[issue]) != 1 {
			t.Errorf("the roll forward wrote %d ends of %s, want one", len(ends[issue]), place)
			continue
		}
		s.assertRollForwardEnd(place, ends[issue][0], rolledBackAt)
	}
}

// A root's re-admission is not its members' runs ending: a member awaiting its merge keeps its
// merge step open through the linger (workflow's beginLinger) and the root's readmit, and only the
// promotion that starts its merger again ends that run (admit's promotion), which raises
// the merger's entry. So where the earlier release re-admits the root and starts no merger, the
// member's run stands open at the roll forward, with its count and its record, for its next end
// to end: a merger the promotion does not start, and a held merger, whose hold starts none. Where
// the promotion started the merger again, the run the row held is over, and its record gets its
// end.
func TestTheRollForwardKeepsAMembersMergeOpenUntilItsMergerStartsAgain(t *testing.T) {
	t.Parallel()
	s := newRollBackStore(t)
	for _, tree := range []struct {
		root, member, held string
	}{{root: "LEGION-250", member: "LEGION-251"}, {root: "LEGION-260", member: "LEGION-261"}, {root: "LEGION-270", member: "LEGION-271", held: "merging"}} {
		s.issue(tree.root, tree.root, "done", "", 1)
		s.exec(fmt.Sprintf(`update issues set linger_until = now() + interval '1 hour' where key = '%s'`, tree.root))
		phase := "merging"
		if tree.held != "" {
			phase = "held"
		}
		s.issue(tree.member, tree.root, phase, tree.held, 4)
		s.entry(tree.member, "merger")
		s.row(tree.member, "merge", 0, "open")
	}
	s.wayBack()
	var rolledBackAt time.Time
	if err := s.store.Pool().QueryRow(t.Context(), `select taken_at from step_starts_rollback where issue = 'LEGION-261'`).Scan(&rolledBackAt); err != nil {
		t.Fatalf("read the moment the way back kept: %v", err)
	}
	// The earlier release re-admits each root; the promotion of LEGION-260's tree starts
	// LEGION-261's merger again.
	s.exec(`update issues set generation = 2, linger_until = null, phase = 'admitted' where key in ('LEGION-250', 'LEGION-260', 'LEGION-270')`)
	s.entry("LEGION-261", "merger")
	s.rollForward()

	starts, ends := s.starts(), s.ends()
	for _, place := range []string{"LEGION-251 merge 0", "LEGION-271 merge 0"} {
		issue, _, _ := strings.Cut(place, " ")
		if got := starts[place]; got != (keptStart{run: "open", starts: 2, id: s.recordID(place)}) {
			t.Errorf("after the roll forward %s = %+v, want the run open at the count 2 under its record %s", place, got, s.recordID(place))
		}
		if len(ends[issue]) != 0 {
			t.Errorf("the roll forward ended %s, whose merge still runs: %+v", place, ends[issue])
		}
	}
	if got := starts["LEGION-261 merge 0"].run; got != "uncounted" {
		t.Errorf("after the roll forward LEGION-261 merge 0 reads %q, want uncounted: its merger started again", got)
	}
	if len(ends["LEGION-261"]) != 1 {
		t.Fatalf("the roll forward wrote %+v for LEGION-261, want one end of the merge its merger's new start ended", ends["LEGION-261"])
	}
	s.assertRollForwardEnd("LEGION-261 merge 0", ends["LEGION-261"][0], rolledBackAt)
}

// The roll forward uncounts only a place where the earlier release may have started a run: one
// whose role's entry it raised, the two starts that raise none (accepting, and a merge GitHub had
// made already) where the issue moved on to them or back, and every place of an issue whose run or
// whose root's run is a new one. A forward move starts nothing behind it: an issue signed off
// from accepting into the retro keeps its review's counted run ended, for a late completion of
// that review to write its report on (workflow's reportEndedStep), and its acceptance, which no
// move back brought it to again, reads ended once its open run gets its end. The retro and the
// merge it may have reached since read uncounted, and so does every place an architect's move
// back may have run again.
func TestTheRollForwardUncountsOnlyThePlacesTheEarlierReleaseMayHaveStarted(t *testing.T) {
	t.Parallel()
	s := newRollBackStore(t)
	for _, key := range []string{"LEGION-280", "LEGION-281", "LEGION-282"} {
		s.issue(key, key, "accepting", "", 1)
		s.entry(key, "reviewer")
		s.row(key, "step", 4, "ended")
		s.row(key, "step", 5, "open")
	}
	// LEGION-281 ran its retro and its merge once before, and a move back brought it to
	// accepting again.
	s.row("LEGION-281", "retro", 0, "ended")
	s.row("LEGION-281", "merge", 0, "ended")
	s.wayBack()
	var rolledBackAt time.Time
	if err := s.store.Pool().QueryRow(s.t.Context(), `select taken_at from step_starts_rollback where issue = 'LEGION-280'`).Scan(&rolledBackAt); err != nil {
		t.Fatalf("read the moment the way back kept: %v", err)
	}
	// The earlier release signs LEGION-280 and LEGION-281 off into the retro, whose start raises
	// the implementer's entry, and moves LEGION-282 back to reviewing, raising its pass and its
	// reviewer's entry.
	s.exec(`update issues set phase = 'retro' where key in ('LEGION-280', 'LEGION-281')`)
	s.entry("LEGION-280", "implementer")
	s.entry("LEGION-281", "implementer")
	s.exec(`update issues set phase = 'reviewing' where key = 'LEGION-282'`)
	s.exec(`update phases set pass = pass + 1 where issue = 'LEGION-282' and role = 'implementer'`)
	s.entry("LEGION-282", "reviewer")
	s.rollForward()

	starts, ends := s.starts(), s.ends()
	want := map[string]string{
		"LEGION-280 step 4": "ended", "LEGION-280 step 5": "ended",
		"LEGION-281 step 4": "ended", "LEGION-281 step 5": "ended", "LEGION-281 retro 0": "uncounted", "LEGION-281 merge 0": "uncounted",
		"LEGION-282 step 4": "uncounted", "LEGION-282 step 5": "uncounted",
	}
	for place, run := range want {
		if got := starts[place]; got.run != run || got.starts != 2 {
			t.Errorf("after the roll forward %s = %+v, want %q at the count 2", place, got, run)
		}
	}
	if len(starts) != len(want) {
		t.Errorf("step_starts holds %v after the roll forward, want the %d rows it held", starts, len(want))
	}
	for _, key := range []string{"LEGION-280", "LEGION-281", "LEGION-282"} {
		if len(ends[key]) != 1 {
			t.Errorf("the roll forward wrote %+v for %s, want the one end of its acceptance, which the move ended", ends[key], key)
			continue
		}
		s.assertRollForwardEnd(key+" step 5", ends[key][0], rolledBackAt)
	}
}

// The close of a parked tree's linger ends the merge of a member it does not hand over whose pull
// request can no longer land, and the unmerged close of that pull request after it ends one the
// close left open (workflow's lingerExpired and closed). An earlier release makes the close and
// writes no end, and nothing of the member's witness moves, so the roll forward reads the close
// where it is durable: the processed fact of the linger's expiry at the root's generation, no
// hand-over of the member by that close, and a pull request that is not open. It ends that run
// once, stopped, at the moment of the rollback: a held merger's and an unheld one's whose pull
// request closed, and a held merger's the close left open before the rollback, whose pull request
// closed under the earlier release. The controls stay open: a linger whose deadline passed with no
// close applied, a linger still running, a member the close handed over, and a held merger whose
// pull request is open, which the close leaves open for its merge. A second rollback and roll
// forward write no second end.
func TestTheRollForwardEndsTheMergeALingersCloseRetired(t *testing.T) {
	t.Parallel()
	s := newRollBackStore(t)
	// lingerAt is where a tree's linger stands: due, its deadline passed and no close applied;
	// running, its deadline not passed; closedEarlier, its close applied by the earlier release
	// after the rollback; closedBefore, its close applied by this release before the rollback.
	type lingerAt int
	const (
		lingerDue lingerAt = iota
		lingerRunning
		lingerClosedEarlier
		lingerClosedBefore
	)
	type tree struct {
		root, member string
		held         bool
		pr           record.PullRequestState
		linger       lingerAt
		// handed is the earlier release's hand-over of the member, closesAfter the pull request's
		// unmerged close under it.
		handed, closesAfter bool
		ends                bool
	}
	trees := []tree{
		{root: "LEGION-290", member: "LEGION-291", held: true, pr: record.PullRequestClosed, linger: lingerClosedEarlier, ends: true},
		{root: "LEGION-300", member: "LEGION-301", pr: record.PullRequestClosed, linger: lingerClosedEarlier, ends: true},
		{root: "LEGION-350", member: "LEGION-351", held: true, pr: record.PullRequestOpen, linger: lingerClosedBefore, closesAfter: true, ends: true},
		{root: "LEGION-310", member: "LEGION-311", held: true, pr: record.PullRequestClosed, linger: lingerDue},
		{root: "LEGION-320", member: "LEGION-321", held: true, pr: record.PullRequestClosed, linger: lingerRunning},
		{root: "LEGION-330", member: "LEGION-331", pr: record.PullRequestOpen, linger: lingerClosedEarlier, handed: true},
		{root: "LEGION-340", member: "LEGION-341", held: true, pr: record.PullRequestOpen, linger: lingerClosedEarlier},
	}
	expire := func(tr tree) {
		s.exec(fmt.Sprintf(`insert into processed_events (source, event_id) values ('outbox', 'linger:%s:1')`, tr.root))
	}
	for i, tr := range trees {
		deadline := "now() - interval '1 minute'"
		if tr.linger == lingerRunning {
			deadline = "now() + interval '1 hour'"
		}
		s.issue(tr.root, tr.root, "done", "", 1)
		s.exec(fmt.Sprintf(`update issues set status = 'backlog', linger_until = %s where key = '%s'`, deadline, tr.root))
		phase, held := "merging", ""
		if tr.held {
			phase, held = "held", "merging"
		}
		s.issue(tr.member, tr.root, phase, held, 4)
		s.entry(tr.member, "merger")
		s.pullRequest(tr.member, 40+i, tr.pr)
		s.row(tr.member, "merge", 0, "open")
		if tr.linger == lingerClosedBefore {
			expire(tr)
		}
	}
	s.wayBack()
	var rolledBackAt time.Time
	if err := s.store.Pool().QueryRow(s.t.Context(), `select taken_at from step_starts_rollback where issue = 'LEGION-291'`).Scan(&rolledBackAt); err != nil {
		t.Fatalf("read the moment the way back kept: %v", err)
	}
	// The earlier release: the close of each linger it applies retires the tree's claims and
	// workspaces, hands over what it hands over, and marks the fact processed, writing no end.
	for _, tr := range trees {
		if tr.linger == lingerClosedEarlier {
			expire(tr)
		}
		if tr.handed {
			s.exec(fmt.Sprintf(`insert into handovers (issue, tree, linger_generation) values ('%s', '%s', 1)`, tr.member, tr.root))
		}
		if tr.closesAfter {
			s.exec(fmt.Sprintf(`update pull_requests set state = 'closed' where issue = '%s'`, tr.member))
		}
	}
	s.rollForward()
	check := func(when string) {
		t.Helper()
		starts, ends := s.starts(), s.ends()
		for _, tr := range trees {
			place := tr.member + " merge 0"
			want := keptStart{run: "open", starts: 2, id: s.recordID(place)}
			if tr.ends {
				want.run = "ended"
			}
			if got := starts[place]; got != want {
				t.Errorf("%s %s = %+v, want %+v", when, place, got, want)
			}
			switch {
			case !tr.ends && len(ends[tr.member]) != 0:
				t.Errorf("%s the roll forward ended %s, whose merge the close left open: %+v", when, place, ends[tr.member])
			case tr.ends && len(ends[tr.member]) != 1:
				t.Errorf("%s the roll forward wrote %+v for %s, want the one end of the merge the close retired", when, ends[tr.member], place)
			case tr.ends:
				s.assertRollForwardEnd(place, ends[tr.member][0], rolledBackAt)
			}
		}
	}
	check("after the roll forward")
	s.wayBack()
	s.rollForward()
	check("after a second rollback and roll forward")
}

// A rollback to a release before 20261111225552_phase_entry drops the phase entries
// (docs/kubernetes.md, the phase entry's block: the eleven statements of the rollback to contract
// 87), and that migration numbers them again at the roll forward, from 1, so an entry then is no
// count of the starts the earlier release made. The roll forward reads no such entry: a running
// step of an issue the earlier release did not move stands open, and where the issue or its root
// moved, every place of it reads as one that release may have started again, as it reads where no
// entry says otherwise.
func TestTheRollForwardReadsNoEntryARollbackPastTheEntriesNumberedAgain(t *testing.T) {
	t.Parallel()
	s := newRollBackStore(t)
	s.issue("LEGION-361", "LEGION-361", "testing", "", 1)
	s.entry("LEGION-361", "tester")
	s.entry("LEGION-361", "tester")
	s.row("LEGION-361", "step", 3, "open")
	s.issue("LEGION-362", "LEGION-362", "accepting", "", 1)
	s.entry("LEGION-362", "reviewer")
	s.entry("LEGION-362", "reviewer")
	s.row("LEGION-362", "step", 4, "ended")
	s.wayBack()
	// The earlier release, before the entries: the rollback's block drops them, and LEGION-362
	// moves on to the retro. The roll forward's 20261111225552 adds them again, numbering every
	// role handed work 1.
	s.exec(`update issues set phase = 'retro' where key = 'LEGION-362'`)
	s.exec(`update phases set entry = 1 where entry > 0`)
	s.exec(`update schema_version set applied_at = now() + interval '1 minute' where version = 20261111225552`)
	s.rollForward()
	starts, ends := s.starts(), s.ends()
	if got := starts["LEGION-361 step 3"]; got.run != "open" || len(ends["LEGION-361"]) != 0 {
		t.Errorf("after the roll forward LEGION-361 step 3 = %+v with ends %+v, want its run open: the earlier release did not move it", got, ends["LEGION-361"])
	}
	if got := starts["LEGION-362 step 4"]; got.run != "uncounted" {
		t.Errorf("after the roll forward LEGION-362 step 4 = %+v, want uncounted: no entry says the earlier release ran no review", got)
	}
}

// The way back an operator runs keeps what the roll forward needs and touches nothing else: every
// pending row of the outbox — the step records' own, which fill their records at the roll forward,
// and the issue's other work — and every row of step_starts stand as they stood, and the one
// schema row it deletes is the roll forward's. Run twice, as a second rollback would before any roll
// forward, it leaves the same store.
func TestTheWayBackOfTheStepRecordsKeepsEveryOtherRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'testing', 'A', 1)`,
		`insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
			values ('LEGION-208', 'default', 'step', 3, 4, 'legion-legion-legion-208-tester', '2026-11-17T01:00:00Z', 'open', '{"id": "kept"}'),
			('LEGION-208', 'default', 'step', 2, 2, 'legion-legion-legion-208-implementer', '2026-11-17T00:00:00Z', 'ended', null)`,
		`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values
			('step_record', 'LEGION-208', '{"id": "a start not yet written"}', 3, now() + interval '1 minute', 'Dispatch answered 503'),
			('supervise', 'LEGION-208', '{"op": "start"}', 0, now(), ''),
			('dispatch_status', 'LEGION-209', '{"status": "testing"}', 1, now() + interval '2 minutes', 'timeout')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	read := func(query string) []string { return rowsAsText(t, store, query) }
	const (
		outbox   = `select row(id, kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)::text from outbox order by id`
		starts   = `select row(s.*)::text from step_starts s order by issue, kind, place`
		versions = `select version::text from schema_version order by version`
	)
	outboxBefore, startsBefore, versionsBefore := read(outbox), read(starts), read(versions)
	if len(outboxBefore) != 3 || len(startsBefore) != 2 {
		t.Fatalf("seeded %d outbox rows and %d step_starts rows, want 3 and 2", len(outboxBefore), len(startsBefore))
	}
	for run := 1; run <= 2; run++ {
		if _, err := store.Pool().Exec(ctx, wayBackOf(t, migrations.StepRecordsRollForward, stepRecordsRelease)); err != nil {
			t.Fatalf("run %d of the way back: %v", run, err)
		}
		if got := read(outbox); !reflect.DeepEqual(got, outboxBefore) {
			t.Errorf("after run %d of the way back the outbox holds %v, want every pending row as it stood: %v", run, got, outboxBefore)
		}
		if got := read(starts); !reflect.DeepEqual(got, startsBefore) {
			t.Errorf("after run %d of the way back step_starts holds %v, want every count as it stood: %v", run, got, startsBefore)
		}
		var want []string
		for _, version := range versionsBefore {
			if version != fmt.Sprint(migrations.StepRecordsRollForward) {
				want = append(want, version)
			}
		}
		if got := read(versions); !reflect.DeepEqual(got, want) {
			t.Errorf("after run %d of the way back schema_version lost %d rows, want the roll forward's alone", run, len(versionsBefore)-len(got))
		}
	}
}
