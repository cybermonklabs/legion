package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The heads the state events of these tests name, each a full commit id, as a check.state_changed
// event names its head; and the migration whose way back the rollback test runs, as the failures of
// wayBackBlockOfTheHeader name it.
const (
	statesHeadOne   = "1111111111111111111111111111111111111111"
	statesHeadTwo   = "2222222222222222222222222222222222222222"
	statesHeadThree = "3333333333333333333333333333333333333333"

	checkStatesMigrationFile = "internal/store/migrations/20261126110300_check_states.up.sql"
)

// stateLogs is the stand-in Dispatch of the boot's catch-up (awaitingDispatch) holding one event
// log for each issue: EventsAfter serves key's own log past after, as Dispatch's events route does,
// records each read as key>after, and answers a key of refused with that status, as Dispatch
// answers a refusal.
type stateLogs struct {
	awaitingDispatch
	logs    map[string][]json.RawMessage
	refused map[string]int
	reads   []string
}

func (d *stateLogs) EventsAfter(_ context.Context, key string, after int64) ([]json.RawMessage, error) {
	d.reads = append(d.reads, fmt.Sprintf("%s>%d", key, after))
	if status, ok := d.refused[key]; ok {
		return nil, &dispatch.Error{Status: status, Message: http.StatusText(status)}
	}
	var events []json.RawMessage
	for _, raw := range d.logs[key] {
		var event struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		if event.Seq > after {
			events = append(events, raw)
		}
	}
	return events, nil
}

// readsOf is every read of key's event log the stand-in answered, by the sequence it was read after.
func (d *stateLogs) readsOf(key string) []string {
	var reads []string
	for _, read := range d.reads {
		if strings.HasPrefix(read, key+">") {
			reads = append(reads, read)
		}
	}
	return reads
}

// passedAt is the state event at seq of issue's check: yes from the tester at head, at serial, the
// head's set of checks being checks (checkstatetest.Event).
func passedAt(t *testing.T, issue string, seq, serial int64, head, check string, checks ...string) json.RawMessage {
	t.Helper()
	return checkstatetest.Event(t, issue, "acme/widgets", head, seq,
		intake.DispatchCheckState{Check: check, State: "yes", Roles: []string{"tester"}, Checks: checks, Serial: serial})
}

// checkStatesRig is the daemon's workflow runtime over a fresh store of project ACME, against
// client, as the boot's catch-up of check states runs on it: the records scoped to the project
// (projectRecords), as the boot reads them, and the workflow's engine as the handler that holds a
// state and moves a tested issue.
func checkStatesRig(t *testing.T, client dispatch.Client) (*store.Pool, *workflowRuntime) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	log := quietLogger()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "ACME", Repositories: testRepositories, Clock: catchUpClock}, log)
	return pool, &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "ACME"}, dispatch: client,
		dispatchProject: "ACME", engine: engine, handlers: []intake.Handler{engine}, log: log, bootID: "boot-1",
		catchUpRetryDelay: time.Millisecond}
}

// acmeIssue is key, a root of project ACME standing in at with status, of generation 1.
func acmeIssue(key string, at phase.Phase, status string) record.Issue {
	return record.Issue{Key: key, Tree: key, Project: "ACME", Title: key, Phase: at, Generation: 1, Status: status, Rank: "A",
		HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", LastDispatchSeq: 1}
}

// openAt records key's pull request number, open at head, the head its checks settled on.
func openAt(ctx context.Context, tx pgx.Tx, records *record.Postgres, key string, number int, head string) error {
	return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets", Number: number,
		Branch: "legion/" + key, HeadSHA: head, CheckedHead: head, Failing: []string{}, Generation: 1})
}

// testerCompletes applies the tester's completion of key's testing at head, its test record
// naming serial, through the rig's handlers, as the API route applies it.
func testerCompletes(t *testing.T, w *workflowRuntime, key, head string, serial int64) {
	t.Helper()
	applyFact(t, w.pool, "api", "test-complete:"+key, intake.HandoffComplete{Generation: 1, Issue: key, Role: claim.RoleTester,
		Claim: "test-claim", Summary: "tested", Commit: head, HandoffRecord: "test-" + key, HandoffVersion: 1, StatesSerial: serial},
		w.handlers...)
}

// textRows is every row query answers over pool, each as the text of its one column.
func textRows(t *testing.T, pool *store.Pool, query string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("run %q: %v", query, err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return texts
}

// heldStates is each check and its state the store holds of key at head, by check.
func heldStates(t *testing.T, pool *store.Pool, key, head string) []string {
	t.Helper()
	return textRows(t, pool, `select check_id || '=' || state from check_states where issue = $1 and head = $2 order by check_id`, key, head)
}

// heldSerials is every serial of key's state events the store holds, ascending.
func heldSerials(t *testing.T, pool *store.Pool, key string) []string {
	t.Helper()
	return textRows(t, pool, `select serial::text from check_state_serials where issue = $1 order by serial`, key)
}

// statesCursor is key's cursor of the boot's catch-up of check states, issues.check_states_seq.
func statesCursor(t *testing.T, pool *store.Pool, key string) int64 {
	t.Helper()
	var cursor int64
	if err := pool.QueryRow(context.Background(), `select check_states_seq from issues where key = $1`, key).Scan(&cursor); err != nil {
		t.Fatalf("read the check states cursor of %s: %v", key, err)
	}
	return cursor
}

// ACME-1 is in testing, and its tester completed at its head while the daemon ran, its test record
// naming serial 2. The daemon goes down, and Dispatch appends the state events of k1 and k2 at that
// head, both yes from the tester, which no consumer delivers. At the boot, the catch-up reads
// ACME-1's log after its cursor and applies both, so the move reaches its fence and ACME-1 goes to
// reviewing, and the cursor moves to the last sequence the read went through, past an event that
// is no state. ACME-2, in planning, gets its state events at the same boot, so its later testing
// move reaches its fence at once. ACME-3, done, is not read. Before this catch-up the boot read
// only the roots that wait on a person, so ACME-1 stayed in testing.
func TestTheBootCatchUpTakesTheStateEventsTheDaemonMissed(t *testing.T) {
	ctx := context.Background()
	client := &stateLogs{logs: map[string][]json.RawMessage{
		"ACME-1": {passedAt(t, "ACME-1", 10, 1, statesHeadOne, "k1", "k1", "k2"), passedAt(t, "ACME-1", 11, 2, statesHeadOne, "k2", "k1", "k2"),
			issueEvent("ACME-1", 12, "comment.created", alice, map[string]any{"id": "comment-1"})},
		"ACME-2": {passedAt(t, "ACME-2", 20, 1, statesHeadTwo, "k1", "k1", "k2"), passedAt(t, "ACME-2", 21, 2, statesHeadTwo, "k2", "k1", "k2")},
		"ACME-3": {passedAt(t, "ACME-3", 30, 1, statesHeadThree, "k1", "k1")},
	}}
	pool, w := checkStatesRig(t, client)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		done := acmeIssue("ACME-3", phase.Done, "done")
		for _, issue := range []record.Issue{acmeIssue("ACME-1", phase.Testing, "in_progress"), acmeIssue("ACME-2", phase.Planning, "in_progress"), done} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return openAt(ctx, tx, records, "ACME-1", 70, statesHeadOne)
	})
	testerCompletes(t, w, "ACME-1", statesHeadOne, 2)
	if got := readIssue(t, pool, "ACME-1").Phase; got != phase.Testing {
		t.Fatalf("ACME-1 stands in %s after its tester's completion with no state held, want testing: the case's own premise", got)
	}

	if err := w.catchUpCheckStates(ctx); err != nil {
		t.Fatalf("the boot's catch-up of check states: %v", err)
	}

	if got, want := heldStates(t, pool, "ACME-1", statesHeadOne), []string{"k1=yes", "k2=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states ACME-1 holds at its head after the boot = %v, want %v", got, want)
	}
	if got := readIssue(t, pool, "ACME-1").Phase; got != phase.Reviewing {
		t.Fatalf("ACME-1 stands in %s after the boot, want reviewing: both checks read yes at the tested head", got)
	}
	if got := statesCursor(t, pool, "ACME-1"); got != 12 {
		t.Fatalf("ACME-1's cursor after the boot = %d, want 12, the last sequence the read went through", got)
	}
	if got, want := heldStates(t, pool, "ACME-2", statesHeadTwo), []string{"k1=yes", "k2=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states ACME-2 holds after the boot = %v, want %v: an issue in planning takes its state events too", got, want)
	}
	if got, want := heldSerials(t, pool, "ACME-2"), []string{"1", "2"}; !slices.Equal(got, want) {
		t.Fatalf("the serials ACME-2 holds after the boot = %v, want %v", got, want)
	}
	if got := readIssue(t, pool, "ACME-2").Phase; got != phase.Planning {
		t.Fatalf("ACME-2 stands in %s after the boot, want planning: a state moves no issue outside testing", got)
	}
	if got := statesCursor(t, pool, "ACME-2"); got != 21 {
		t.Fatalf("ACME-2's cursor after the boot = %d, want 21", got)
	}
	if got := client.readsOf("ACME-3"); len(got) != 0 {
		t.Fatalf("the boot read ACME-3's log %v, want no read of a done issue", got)
	}
	if got := heldStates(t, pool, "ACME-3", statesHeadThree); len(got) != 0 || statesCursor(t, pool, "ACME-3") != 0 {
		t.Fatalf("done ACME-3 holds the states %v at cursor %d after the boot, want none at 0", got, statesCursor(t, pool, "ACME-3"))
	}
	if got, want := client.reads, []string{"ACME-1>0", "ACME-2>0"}; !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Fatalf("the boot read the logs %v, want %v: each issue in work once, from its cursor", got, want)
	}

	// ACME-2's planning and implementing rounds end, standing in for which the record moves it to
	// testing at its pull request's head, and its tester completes there naming serial 2.
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, acmeIssue("ACME-2", phase.Testing, "in_progress")); err != nil {
			return err
		}
		return openAt(ctx, tx, records, "ACME-2", 71, statesHeadTwo)
	})
	testerCompletes(t, w, "ACME-2", statesHeadTwo, 2)
	if got := readIssue(t, pool, "ACME-2").Phase; got != phase.Reviewing {
		t.Fatalf("ACME-2 stands in %s after its tester's completion naming serial 2, want reviewing: the boot held every serial through it", got)
	}
}

// failingState fails the state event of serial, as a handler that refuses one event would.
type failingState struct{ serial int64 }

func (f failingState) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if state, ok := fact.(intake.DispatchCheckState); ok && state.Serial == f.serial {
		return intake.Result{}, errors.New("this event fails to apply")
	}
	return intake.Result{}, nil
}

// A state event whose fact fails to apply is logged, and the rest of the log is still applied, but
// the cursor stops just below it, so the next boot reads it again, as the live consumer retries it,
// and holds its state then.
func TestTheCheckStatesCatchUpReadsAnEventThatFailedAgainAtTheNextBoot(t *testing.T) {
	ctx := context.Background()
	client := &stateLogs{logs: map[string][]json.RawMessage{"ACME-1": {
		passedAt(t, "ACME-1", 10, 1, statesHeadOne, "k1", "k1", "k2", "k3"), passedAt(t, "ACME-1", 11, 2, statesHeadOne, "k2", "k1", "k2", "k3"),
		passedAt(t, "ACME-1", 12, 3, statesHeadOne, "k3", "k1", "k2", "k3"),
	}}}
	pool, w := checkStatesRig(t, client)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		return records.PutIssue(ctx, tx, acmeIssue("ACME-1", phase.Implementing, "in_progress"))
	})
	healthy := w.handlers
	w.handlers = append([]intake.Handler{failingState{serial: 2}}, healthy...)
	if err := w.catchUpCheckStates(ctx); err != nil {
		t.Fatalf("the first boot's catch-up: %v", err)
	}
	if got, want := heldStates(t, pool, "ACME-1", statesHeadOne), []string{"k1=yes", "k3=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states after the first boot = %v, want %v: the events around the failing one applied", got, want)
	}
	if got := statesCursor(t, pool, "ACME-1"); got != 10 {
		t.Fatalf("the cursor after the first boot = %d, want 10, just below the failing event at 11", got)
	}

	w.handlers, w.checkStatesCaughtUp, w.bootID = healthy, nil, "boot-2"
	if err := w.catchUpCheckStates(ctx); err != nil {
		t.Fatalf("the second boot's catch-up: %v", err)
	}
	if got, want := heldStates(t, pool, "ACME-1", statesHeadOne), []string{"k1=yes", "k2=yes", "k3=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states after the second boot = %v, want %v", got, want)
	}
	if got := statesCursor(t, pool, "ACME-1"); got != 12 {
		t.Fatalf("the cursor after the second boot = %d, want 12", got)
	}
	if got, want := client.readsOf("ACME-1"), []string{"ACME-1>0", "ACME-1>10"}; !slices.Equal(got, want) {
		t.Fatalf("the reads of ACME-1's log = %v, want %v", got, want)
	}
}

// An issue whose log Dispatch keeps refusing does not stop the boot: the catch-up reads it again
// after an error Dispatch may stop giving (a 5xx) up to catchUpAttempts times, after a credential
// refusal (401) not again, then passes over it with its cursor where it stood, and the next issue
// is caught up all the same.
func TestTheCheckStatesCatchUpSkipsAnIssueDispatchKeepsRefusing(t *testing.T) {
	for _, tc := range []struct {
		status int
		reads  int
	}{
		{status: http.StatusBadGateway, reads: catchUpAttempts},
		{status: http.StatusUnauthorized, reads: 1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			client := &stateLogs{refused: map[string]int{"ACME-1": tc.status},
				logs: map[string][]json.RawMessage{"ACME-2": {passedAt(t, "ACME-2", 20, 1, statesHeadTwo, "k1", "k1")}}}
			pool, w := checkStatesRig(t, client)
			seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
				if err := records.PutIssue(ctx, tx, acmeIssue("ACME-1", phase.Implementing, "in_progress")); err != nil {
					return err
				}
				return records.PutIssue(ctx, tx, acmeIssue("ACME-2", phase.Implementing, "in_progress"))
			})
			if err := w.catchUpCheckStates(context.Background()); err != nil {
				t.Fatalf("the catch-up with an issue Dispatch refuses = %v, want the issue passed over", err)
			}
			if got := len(client.readsOf("ACME-1")); got != tc.reads {
				t.Fatalf("reads of the refused issue = %d, want %d", got, tc.reads)
			}
			if got := statesCursor(t, pool, "ACME-1"); got != 0 {
				t.Fatalf("the refused issue's cursor = %d, want 0, where it stood", got)
			}
			if got, want := heldStates(t, pool, "ACME-2", statesHeadTwo), []string{"k1=yes"}; !slices.Equal(got, want) || statesCursor(t, pool, "ACME-2") != 20 {
				t.Fatalf("the next issue holds %v at cursor %d, want %v at 20", got, statesCursor(t, pool, "ACME-2"), want)
			}
		})
	}
}

// The way back of the check states' migration drops the three tables and the cursor column, and
// the roll-forward creates them again empty, every cursor at 0. processed_events is not dropped: it
// still holds the id of every state event the daemon applied before the way back, both the id the
// live consumer applied one under (dispatch-<event id>) and the id an earlier catch-up applied one
// under. The roll-forward's first catch-up reads each log from the start and applies each state
// event it reads whatever processed_events records of it, so every state is held again, as the
// migration's header says.
func TestTheCheckStatesCatchUpHoldsTheStatesAgainAfterTheWayBackAndTheRollForward(t *testing.T) {
	ctx := context.Background()
	client := &stateLogs{logs: map[string][]json.RawMessage{"ACME-1": {
		passedAt(t, "ACME-1", 10, 1, statesHeadOne, "k1", "k1", "k2"), passedAt(t, "ACME-1", 11, 2, statesHeadOne, "k2", "k1", "k2"),
	}}}
	pool, w := checkStatesRig(t, client)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, acmeIssue("ACME-1", phase.Implementing, "in_progress")); err != nil {
			return err
		}
		return openAt(ctx, tx, records, "ACME-1", 70, statesHeadOne)
	})
	// k1's event reached the daemon live, under the id the live consumer applies a Dispatch event
	// under, and the first boot's catch-up took both.
	live, unread, err := intake.DecodeDispatchEvent("ACME", "ACME-1", client.logs["ACME-1"][0])
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode k1's event: %v, unread %q", err, unread)
	}
	applyFact(t, pool, "dispatch", "dispatch-5010", live, w.handlers...)
	if err := w.catchUpCheckStates(ctx); err != nil {
		t.Fatalf("the first boot's catch-up: %v", err)
	}
	if got, want := heldStates(t, pool, "ACME-1", statesHeadOne), []string{"k1=yes", "k2=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states before the way back = %v, want %v: the case's own premise", got, want)
	}
	recorded := textRows(t, pool, `select source || ':' || event_id from processed_events order by 1`)

	// The way back, as an operator runs it with the daemon stopped, then the roll-forward's Migrate.
	block := strings.Split(strings.TrimSuffix(wayBackBlockOfTheHeader(t, checkStatesMigrationFile), "\n"), "\n")
	if _, err := pool.Exec(ctx, strings.Join(block[1:len(block)-1], "\n"), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back's block of %s as the one transaction an operator runs: %v", checkStatesMigrationFile, err)
	}
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if applied, err := st.Migrate(ctx); err != nil || applied != 1 {
		t.Fatalf("the roll-forward's migrate applied %d, %v; want the check states' migration alone", applied, err)
	}
	if got := heldStates(t, pool, "ACME-1", statesHeadOne); len(got) != 0 || statesCursor(t, pool, "ACME-1") != 0 {
		t.Fatalf("after the roll-forward ACME-1 holds %v at cursor %d, want nothing at 0: the case's own premise", got, statesCursor(t, pool, "ACME-1"))
	}
	if got := textRows(t, pool, `select source || ':' || event_id from processed_events order by 1`); !slices.Equal(got, recorded) ||
		!slices.Contains(got, "dispatch:dispatch-5010") {
		t.Fatalf("processed_events after the roll-forward = %v, want every id applied before the way back, %v, the live one among them", got, recorded)
	}

	w.bootID, w.checkStatesCaughtUp = "boot-2", nil
	if err := w.catchUpCheckStates(ctx); err != nil {
		t.Fatalf("the roll-forward's first boot's catch-up: %v", err)
	}
	if got, want := heldStates(t, pool, "ACME-1", statesHeadOne), []string{"k1=yes", "k2=yes"}; !slices.Equal(got, want) {
		t.Fatalf("the states after the roll-forward's first boot = %v, want %v: each state event held again", got, want)
	}
	if got, want := heldSerials(t, pool, "ACME-1"), []string{"1", "2"}; !slices.Equal(got, want) {
		t.Fatalf("the serials after the roll-forward's first boot = %v, want %v", got, want)
	}
	if got := statesCursor(t, pool, "ACME-1"); got != 11 {
		t.Fatalf("the cursor after the roll-forward's first boot = %d, want 11", got)
	}
}
