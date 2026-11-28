package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The places the kept-decision model runs an outbox row, by the name each gives the audit
// (keptLaneAudit): the four callers of runOne, the step that runs rows beside each other, and the
// row an interleaving runs inside a catch-up's publish.
const (
	keptSiteRunRow         = "runRow"
	keptSiteTogether       = "runTogether"
	keptSiteRaceCatchUp    = "raceRow's catch-up"
	keptSiteRaceOpRow      = "the race op's row"
	keptSiteHandOverStarts = "the hand-over's start rows"
	keptSiteLeftovers      = "quiesce's leftovers"
)

// keptRowSites is every place the model runs a row, which the inventory of
// TestTheKeptModelRunsOnlyRowsALaneWouldClaim holds to the lanes' claim rule as a class.
var keptRowSites = []string{keptSiteRunRow, keptSiteTogether, keptSiteRaceCatchUp, keptSiteRaceOpRow,
	keptSiteHandOverStarts, keptSiteLeftovers}

// keptLaneAudit holds one kept-decision run to the rule a lane of the daemon claims rows by
// (record's outboxClaimable). Just before a place of the model takes a row's lease or claim,
// before reads, in a transaction of its own, whether a lane could claim that row at that moment:
// whether the row is among those record.Store.DueRows lists for its issue and kind, which is the
// daemon's own rule rather than a copy of it. Once the lease or the claim went through and the row
// runs, ran fails the run for a row no lane could have claimed, with the step, the place, the row
// and the rule the row broke. ran also counts each place that ran a row, for the test's inventory.
// A nil audit, which is every run's but that test's (keptRun.audit), reads and fails nothing.
type keptLaneAudit struct {
	run   *keptRun
	sites *keptAuditedSites
	// read is what before found of each row it read that ran has not taken yet, by row id: "" for
	// a row a lane could claim, and otherwise the rule the row broke.
	read map[int64]string
}

// before reads whether a lane could claim row now, just before the place site takes its lease or
// its claim.
func (a *keptLaneAudit) before(site string, row record.OutboxRow) {
	if a == nil {
		return
	}
	r := a.run
	r.t.Helper()
	now := time.Now().UTC()
	var rule string
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		due, err := r.records.DueRows(r.ctx, tx, "LEGION", row.Issue, []record.OutboxKind{row.Kind}, now)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(due, func(listed record.OutboxRow) bool { return listed.ID == row.ID }) {
			return nil
		}
		rule, err = keptBrokenRule(r.ctx, tx, row, now)
		return err
	}); err != nil {
		r.fatalf("read whether a lane could claim outbox row %d, which %s is about to run: %v", row.ID, site, err)
	}
	a.read[row.ID] = rule
}

// ran says row now runs at site, under the lease or the claim the place just took: a row before
// found no lane could claim fails the run, and so does a row before never read.
func (a *keptLaneAudit) ran(site string, row record.OutboxRow) {
	if a == nil {
		return
	}
	r := a.run
	r.t.Helper()
	a.sites.note(site)
	rule, read := a.read[row.ID]
	delete(a.read, row.ID)
	switch {
	case !read:
		r.fatalf("step %d: %s ran outbox row %d (%s) with no read of the claim rule before its lease", r.step, site, row.ID,
			keptAuditedRow(row))
	case rule != "":
		r.fatalf("step %d: %s ran outbox row %d (%s), which no lane would claim then: %s", r.step, site, row.ID,
			keptAuditedRow(row), rule)
	}
}

// keptBrokenRule names the rule of outboxClaimable that keeps a lane from claiming row at now, by
// three reads of what the model's schedules can leave in its way: a live lease on the row itself,
// which only a simulated crash leaves standing (runRow, noteCrash); a live lease on another row of
// its issue's lifecycle lane, which outboxLaneFree waits on; and, for a supervise start, an older
// issue_branch row of its issue, which outboxInOrder holds the start behind. A row none of the
// three holds back breaks another rule of outboxClaimable, and is named so.
func keptBrokenRule(ctx context.Context, tx pgx.Tx, row record.OutboxRow, now time.Time) (string, error) {
	var leased bool
	if err := tx.QueryRow(ctx, "select coalesce(lease_until > $2, false) from outbox where id = $1", row.ID, now).
		Scan(&leased); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if leased {
		return "a crash's lease is live", nil
	}
	if record.LaneOf(row.Kind) == record.LaneLifecycle {
		running, err := keptRunningLifecycleRow(ctx, tx, row, now)
		if err != nil {
			return "", err
		}
		if running != 0 {
			return fmt.Sprintf("the issue's lifecycle lane runs row %d", running), nil
		}
	}
	if payload, err := record.DecodeOutboxPayload(row); err == nil {
		if request, ok := payload.(record.SuperviseRequest); ok && request.Op == "start" {
			var branch int64
			err := tx.QueryRow(ctx, "select id from outbox where issue = $1 and kind = $2 and id < $3 order by id limit 1",
				row.Issue, string(record.OutboxKindIssueBranch), row.ID).Scan(&branch)
			if err == nil {
				return fmt.Sprintf("an older issue_branch row of its issue, row %d", branch), nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return "", err
			}
		}
	}
	return "another rule of outboxClaimable", nil
}

// keptRunningLifecycleRow is the oldest other row of row's issue whose kind its lifecycle lane
// runs (record.LaneOf) and that holds a live lease at now, and 0 for none.
func keptRunningLifecycleRow(ctx context.Context, tx pgx.Tx, row record.OutboxRow, now time.Time) (int64, error) {
	rows, err := tx.Query(ctx, "select id, kind from outbox where issue = $1 and id <> $2 and lease_until > $3 order by id",
		row.Issue, row.ID, now)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return 0, err
		}
		if record.LaneOf(record.OutboxKind(kind)) == record.LaneLifecycle {
			return id, nil
		}
	}
	return 0, rows.Err()
}

// keptAuditedRow names an audited row for a failure: its kind, its payload and its issue.
func keptAuditedRow(row record.OutboxRow) string {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Sprintf("%s of %s", row.Kind, row.Issue)
	}
	return fmt.Sprintf("%s %s of %s", row.Kind, keptRowWhat(payload), row.Issue)
}

// keptAuditedSites is what the runs of one test's audits ran, which the runs of its parallel
// subtests count beside each other: how many rows each place ran, and how many of the test's
// subtests started.
type keptAuditedSites struct {
	mu      sync.Mutex
	ran     map[string]int
	started atomic.Int32
}

// audit is an audit of run that counts what it ran here.
func (s *keptAuditedSites) audit(run *keptRun) *keptLaneAudit {
	return &keptLaneAudit{run: run, sites: s, read: map[int64]string{}}
}

func (s *keptAuditedSites) note(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ran[site]++
}

// missed is every place of keptRowSites at which no audited row ran, in that list's order.
func (s *keptAuditedSites) missed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var missed []string
	for _, site := range keptRowSites {
		if s.ran[site] == 0 {
			missed = append(missed, site)
		}
	}
	return missed
}

// counts is how many audited rows each place of keptRowSites ran, for the test's log.
func (s *keptAuditedSites) counts() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make([]string, 0, len(keptRowSites))
	for _, site := range keptRowSites {
		counts = append(counts, fmt.Sprintf("%s %d", site, s.ran[site]))
	}
	return strings.Join(counts, ", ")
}

// The kept-decision model runs a row only where a lane of the daemon could claim it. A lane lists
// the rows it may run (record.Store.DueRows, under the rule record's outboxClaimable states) and
// claims each by id before it runs it (ClaimRow). A model that runs a row no lane would claim
// drives a schedule the daemon never makes, and a decision the model then tells twice is the
// model's fault, not the daemon's (dispatch://INFRA-2836, told-twice-cause-md section 8). The
// audit (keptLaneAudit) reads the rule just before each place of the model takes a row's lease or
// claim, and fails the step that then runs a row the rule refuses, naming the place and the rule.
//
// Three rows hold the three ways the model departed from a lane. Each builds a small state on
// newKeptRun with the model's own steps and drives the one step that meets it: a step that runs
// two lifecycle rows of one issue at once, which outboxLaneFree forbids the lanes; a row whose
// simulated crash left its lease, run again while that lease is live, where a lane waits for the
// lease to lapse; and quiesce running a start that outboxInOrder holds behind an older
// issue_branch row of its issue. The fourth row drives the hand-over seeds under the audit, as
// TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild runs them.
//
// Every place the model runs a row is held to the rule as a class: once every subtest has ended,
// the test fails naming each place of keptRowSites at which no audited row ran, since nothing here
// holds a place no row reached. That inventory reads the rows of every subtest, so a -run that
// leaves one out, as a failure's replay line does, does not read it.
func TestTheKeptModelRunsOnlyRowsALaneWouldClaim(t *testing.T) {
	t.Parallel()
	subtests := []struct {
		name string
		run  func(t *testing.T, sites *keptAuditedSites)
	}{
		{"two lifecycle rows of one issue", keptAuditTwoLifecycleRows},
		{"a crashed row before its lapse", keptAuditCrashedRow},
		{"a start behind an older issue_branch row at quiesce", keptAuditStartBehindABranch},
		{"the hand-over seeds", keptAuditHandOverSeeds},
	}
	sites := &keptAuditedSites{ran: map[string]int{}}
	// A test's cleanup runs once every subtest of it has ended, parallel ones included, so this is
	// the inventory over all of them.
	t.Cleanup(func() {
		t.Logf("audited rows by place: %s", sites.counts())
		if started := int(sites.started.Load()); started < len(subtests) {
			t.Logf("the inventory reads the rows of all %d subtests and %d started, so it is not read", len(subtests), started)
			return
		}
		if missed := sites.missed(); len(missed) > 0 {
			t.Errorf("no audited row ran at %s over the rows and seeds of this test: every place the model runs a row is "+
				"held to the lanes' claim rule, and a place no row reached is not held", strings.Join(missed, ", "))
		}
	})
	for _, subtest := range subtests {
		t.Run(subtest.name, func(t *testing.T) {
			t.Parallel()
			sites.started.Add(1)
			subtest.run(t, sites)
		})
	}
}

// keptAuditTwoLifecycleRows drives a runTogether step that has two due lifecycle rows of the root.
// The run opens with the root architect's start (newKeptRun), and a second start of the root makes
// two due rows of the root's lifecycle lane and nothing else due, so the step lists both starts
// (claimable) and its count is two. The step claims the first in its shuffled order,
// and ClaimRow refuses the second, since the first's claim holds the root's lifecycle lane
// (outboxLaneFree): the step runs the one row and traces the other not claimed, as a lane holds it
// back while the first's claim is live. The case can fail only on a model that takes both rows, in
// whichever order its shuffle puts them, as the model before cybermonklabs/legion#5874 (main at
// 8868205a) did: it leased each row with leaseOutboxRow and did not claim it.
func keptAuditTwoLifecycleRows(t *testing.T, sites *keptAuditedSites) {
	run := newKeptRun(t, 1, 1)
	run.audit = sites.audit(run)
	run.enqueueStart()
	due := run.rows(true)
	if len(due) != 2 || slices.ContainsFunc(due, func(row record.OutboxRow) bool {
		return row.Issue != keptRoot || row.Kind != record.OutboxKindSupervise
	}) {
		run.fatalf("the case's state is not reached: the due rows are %s, want the root's two start rows alone", keptAuditedRows(due))
	}
	run.step++
	run.runTogether()
}

// keptAuditCrashedRow drives a runRow step that has due a row whose simulated crash left its lease
// live. Seed 1's generator crashes the first run of a row (runRow's one draw in six). The first
// step runs the root architect's opening start, the one row due, and its finish is lost to the
// crash, so the row keeps its lease (noteCrash). The architect that start launched then comes up
// and the workflow is told its ready, which queues its catch-up: a board row a lane can claim while
// the crash's lease holds the root's lifecycle lane. The fourth step's runRow picks
// from claimable, and record.Store.DueRows leaves out the start under its live crash lease, so the
// step runs the catch-up, as a lane leaves the start until its lease lapses. The case can fail only
// on a model that picks among every due row, the crashed start and the catch-up, as the model
// before cybermonklabs/legion#5874 (main at 8868205a) did, and there seed 1 picks the start.
func keptAuditCrashedRow(t *testing.T, sites *keptAuditedSites) {
	run := newKeptRun(t, 1, 4)
	run.audit = sites.audit(run)
	opening := run.rows(true)
	if len(opening) != 1 {
		run.fatalf("the case's state is not reached: the due rows are %s, want the root's opening start alone", keptAuditedRows(opening))
	}
	start := opening[0]
	run.step++
	run.runRow()
	crash, crashed := run.crashed[start.ID]
	if !crashed {
		run.fatalf("the case's state is not reached: the first run of outbox row %d did not crash", start.ID)
	}
	run.step++
	run.ready(run.root)
	run.step++
	run.readyFact(run.root)
	due := run.rows(true)
	if len(due) != 2 || due[0].ID != start.ID || !crash.until.After(time.Now()) {
		run.fatalf("the case's state is not reached: the due rows are %s and the crash's lease lapses at %s, want the crashed "+
			"start %d under its live lease and the catch-up after it", keptAuditedRows(due), crash.until, start.ID)
	}
	run.step++
	run.runRow()
}

// keptAuditStartBehindABranch runs, at quiesce, a root's start that its issue's older issue_branch
// row holds back. The root's opening start runs first, as a leftover, so that once quiesce's
// promotion (unblockAndPromote) admits the second root, whose start admission queues behind a row
// that creates its issue branch, that start is the one row left to pick (the issue_branch row is
// none of the rows the run drives). Quiesce delivers such rows at the start of each of its steps
// (deliverRowsTheRunDoesNotDrive), so the start never meets the branch row there; the case builds
// by hand the schedule quiesce made before it did, calling runLeftoverRow directly after
// unblockAndPromote, while the branch row stands.
func keptAuditStartBehindABranch(t *testing.T, sites *keptAuditedSites) {
	run := newKeptRun(t, 1, 0)
	run.audit = sites.audit(run)
	run.step++
	run.runLeftoverRow()
	run.unblockAndPromote()
	left := run.rows(false)
	var branch int64
	if len(left) == 1 && left[0].Issue == keptQueued {
		if err := run.pool.QueryRow(run.ctx, "select coalesce(min(id), 0) from outbox where issue = $1 and kind = $2 and id < $3",
			keptQueued, string(record.OutboxKindIssueBranch), left[0].ID).Scan(&branch); err != nil {
			run.fatalf("read the issue_branch rows of %s: %v", keptQueued, err)
		}
	}
	if branch == 0 {
		run.fatalf("the case's state is not reached: the rows left are %s, want the start of %s alone, behind an "+
			"issue_branch row of its issue", keptAuditedRows(left), keptQueued)
	}
	run.step++
	run.runLeftoverRow()
}

// keptAuditHandOverSeeds drives the seeds of
// TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild (keptHandOverSeeds) at 300
// steps with the seek that test sets, through the drive, quiesce and the verdict, under the audit.
func keptAuditHandOverSeeds(t *testing.T, sites *keptAuditedSites) {
	for _, seed := range keptHandOverSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			run := newKeptRun(t, seed, 300)
			run.seekHeldHandOver = true
			run.audit = sites.audit(run)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
}

// keptAuditedRows names rows for a case whose state was not reached.
func keptAuditedRows(rows []record.OutboxRow) string {
	if len(rows) == 0 {
		return "none"
	}
	named := make([]string, 0, len(rows))
	for _, row := range rows {
		named = append(named, fmt.Sprintf("%d (%s)", row.ID, keptAuditedRow(row)))
	}
	return strings.Join(named, ", ")
}
