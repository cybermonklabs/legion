package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ciIssue is the issue of the CI news rows here, and ciHeadX and ciHeadY two heads of its pull
// request: the one the rows are at, and the one a push moves it to.
const (
	ciIssue = "LEGION-208"
	ciHeadX = "c1c1000000000000000000000000000000000001"
	ciHeadY = "c1c1000000000000000000000000000000000002"
)

// ciRig is an outbox runner over an isolated database holding ciIssue in a phase, its tree admitted
// and holding its slot, its open pull request #42 at ciHeadX, and a supervisor with no claim on it
// yet, as runnerIn builds it. Its listener is holderPublisher's, and its log is kept. The rows it
// queues are due at start, and each attempt runs the outbox once on the runner's clock set past it.
type ciRig struct {
	t         *testing.T
	runner    *outbox
	sup       *supervisor
	pool      *store.Pool
	publisher *holderPublisher
	logs      *syncBuffer
	start     time.Time
}

func newCIRig(t *testing.T, at phase.Phase) *ciRig {
	t.Helper()
	publisher := &holderPublisher{}
	logs := &syncBuffer{}
	runner, sup, pool := runnerIn(t, at, "in_progress", publisher, slog.New(slog.NewJSONHandler(logs, nil)))
	holdSlot(t, pool, ciIssue)
	r := &ciRig{t: t, runner: runner, sup: sup, pool: pool, publisher: publisher, logs: logs, start: time.Now()}
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return runner.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: ciIssue, Repo: "acme/widgets", Number: 42,
			Branch: "legion/" + ciIssue, HeadSHA: ciHeadX, State: "open"})
	})
	return r
}

// tx writes the rig's records in one transaction.
func (r *ciRig) tx(write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.pool, write); err != nil {
		r.t.Fatalf("write the rig's records: %v", err)
	}
}

// exec runs statement on the rig's database.
func (r *ciRig) exec(statement string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), statement, args...); err != nil {
		r.t.Fatalf("%s: %v", statement, err)
	}
}

// entry raises role's phases-row entry to entry.
func (r *ciRig) entry(role claim.Role, entry int64) {
	r.t.Helper()
	token := mustClaimToken(r.t, ciIssue, role)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		return r.runner.records.PutPhase(ctx, tx, record.PhaseRow{Issue: ciIssue, Role: role, Claim: token, Entry: entry,
			EntryTask: "work", EntryRaisedAt: time.Now().UTC()})
	})
}

// runningFix records fix 1 of the task default running at the implementer's entry entry.
func (r *ciRig) runningFix(entry int64) {
	r.t.Helper()
	r.entry(claim.RoleImplementer, entry)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		fix := record.TaskFix{Issue: ciIssue, Generation: 1, Task: "default", Fix: 1, State: record.FixReading, Check: "k-ci", Head: ciHeadX, Place: 1}
		if err := r.runner.records.PutFix(ctx, tx, fix); err != nil {
			return err
		}
		fix.State, fix.Entry = record.FixRunning, entry
		return r.runner.records.UpdateFix(ctx, tx, fix)
	})
}

// queue queues payload for ciIssue, due at the rig's start, and answers its row's id.
func (r *ciRig) queue(payload record.OutboxPayload) int64 {
	r.t.Helper()
	enqueueOutbox(r.t, r.pool, r.runner.records, mustOutboxRow(r.t, ciIssue, payload, r.start))
	var id int64
	if err := r.pool.QueryRow(context.Background(), `select max(id) from outbox`).Scan(&id); err != nil {
		r.t.Fatalf("read the queued row's id: %v", err)
	}
	return id
}

// attempt runs the outbox once with its clock at the rig's start and after.
func (r *ciRig) attempt(after time.Duration) {
	r.t.Helper()
	at := r.start.Add(after)
	r.runner.now = func() time.Time { return at }
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("run the outbox at +%s: %v", after, err)
	}
}

// left is how many rows of kind the outbox still holds.
func (r *ciRig) left(kind record.OutboxKind) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1`, string(kind)).Scan(&n); err != nil {
		r.t.Fatalf("count the %s rows: %v", kind, err)
	}
	return n
}

// prompts is every ci_news row the outbox holds, decoded, in id order.
func (r *ciRig) prompts() []record.CINews {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = 'ci_news' order by id`)
	if err != nil {
		r.t.Fatalf("read the CI news rows: %v", err)
	}
	collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var o record.OutboxRow
		err := row.Scan(&o.ID, &o.Kind, &o.Issue, &o.Payload)
		return o, err
	})
	if err != nil {
		r.t.Fatalf("read the CI news rows: %v", err)
	}
	var news []record.CINews
	for _, row := range collected {
		decoded, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode CI news row %d: %v", row.ID, err)
		}
		news = append(news, decoded.(record.CINews))
	}
	return news
}

// resumes is how many resumes the record keeps for ciIssue: the wake reservations its rows took.
func (r *ciRig) resumes() int { return podResumes(r.t, r.pool, ciIssue) }

// lines is each line of the rig's log that holds text.
func (r *ciRig) lines(text string) []map[string]any { return loggedLines(r.t, r.logs.String(), text) }

// ciTopic is the role topic of role's claim on ciIssue.
func ciTopic(t *testing.T, role claim.Role) string {
	t.Helper()
	return notify.RoleTopicPrefix + string(mustClaimToken(t, ciIssue, role))
}

// ciPrompt is a prompt to read Dispatch for role's reader of the task default at generation and
// head, replacing the head's set of checks with checks.
func ciPrompt(t *testing.T, role claim.Role, generation uint64, head string, reader record.CIReader, checks ...string) record.CINews {
	t.Helper()
	set := checks
	return record.CINews{Purpose: record.CINewsState, Role: string(mustClaimToken(t, ciIssue, role)), Generation: generation, Task: "default",
		Repo: "acme/widgets", PullRequest: 42, Head: head, Reader: &reader, Checks: &set, Body: record.ReadPromptBody(ciIssue, head, checks)}
}

// staleLine is the line a row for work no longer current finishes with.
const staleLine = "outbox CI news finished undelivered: the work it is for is no longer current"

// A row of CI news for a current reader whose pod was released wakes that pod and waits for it:
// the reviewer, a running fix and the merger in merging each get one wake reservation, no publish
// while no session holds the role's topic, and then one publish under the row's own key, after
// which the row is finished.
//
// The control, a kept subtest: delivered as a role message (the seam ciNewsAsRoleMessage), the row
// to the released reviewer finishes undelivered, with no wake.
func TestACINewsRowWakesItsReleasedReader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		role   claim.Role
		reader record.CIReader
		seed   func(*ciRig)
	}{
		{"a released reviewer", phase.Reviewing, claim.RoleReviewer, record.CIReader{Phase: phase.Reviewing, Entry: 4},
			func(r *ciRig) { r.entry(claim.RoleReviewer, 4) }},
		{"a released fix", phase.Reviewing, claim.RoleImplementer, record.CIReader{Fix: 1, Entry: 5},
			func(r *ciRig) { r.runningFix(5) }},
		{"a released merger in merging", phase.Merging, claim.RoleMerger, record.CIReader{Phase: phase.Merging, Entry: 6},
			func(r *ciRig) { r.entry(claim.RoleMerger, 6) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCIRig(t, tc.at)
			tc.seed(r)
			topic := ciTopic(t, tc.role)
			r.publisher.setAbsent(topic)
			machine := releasedClaim(t, r.sup, tc.role)
			id := r.queue(ciPrompt(t, tc.role, 1, ciHeadX, tc.reader, "k-ci"))

			r.attempt(time.Minute)
			if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 1 || r.resumes() != 1 {
				t.Fatalf("the first attempt: %d publishes, %d rows left, %d wakes; want none, the row, one wake", attempts, r.left(record.OutboxKindCINews), r.resumes())
			}
			if held := machine.Claim(); held.Released() {
				t.Fatalf("the %s after the first attempt is %s by %s, want the wake to have resumed it", tc.role, held.State, held.SuspendedBy)
			}
			r.attempt(2 * time.Minute)
			if attempts, delivered := r.publisher.snapshot(); attempts != 1 || len(delivered) != 0 || r.left(record.OutboxKindCINews) != 1 || r.resumes() != 1 {
				t.Fatalf("the attempt before the role's topic is held: %d publishes, %d delivered, %d rows, %d wakes; want one refused publish, the row kept, one wake",
					attempts, len(delivered), r.left(record.OutboxKindCINews), r.resumes())
			}
			r.publisher.setAbsent()
			r.attempt(5 * time.Minute)
			_, delivered := r.publisher.snapshot()
			if len(delivered) != 1 || delivered[0].topic != topic || delivered[0].key != record.OutboxKey(id) {
				t.Fatalf("delivered = %+v, want one publish to %s under the row's key %s", delivered, topic, record.OutboxKey(id))
			}
			if r.left(record.OutboxKindCINews) != 0 || r.resumes() != 1 {
				t.Fatalf("after the delivery: %d rows, %d wakes; want the row finished and the one wake", r.left(record.OutboxKindCINews), r.resumes())
			}
		})
	}
	t.Run("control: ciNewsAsRoleMessage finishes the row undelivered with no wake", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		r.runner.ciNewsAsRoleMessage = true
		releasedClaim(t, r.sup, claim.RoleReviewer)
		r.queue(ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: 4}, "k-ci"))
		r.attempt(time.Minute)
		if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 0 || r.resumes() != 0 {
			t.Fatalf("the row as a role message: %d publishes, %d rows, %d wakes; want it finished undelivered with no wake, the fault the reader's delivery takes out",
				attempts, r.left(record.OutboxKindCINews), r.resumes())
		}
	})
}

// A release between the executor's read of the claim and its publish is a publish the listener
// answers with no holder: the row stays, and its retry publishes once, under its key.
//
// The control, a kept subtest: with no holder taken as delivered (the seam noHolderDelivers), the
// row finishes unpublished, which the subtest asserts.
func TestAReleaseDuringDeliveryKeepsTheRow(t *testing.T) {
	t.Parallel()
	play := func(t *testing.T, noHolderDelivers bool) (*ciRig, int64) {
		t.Helper()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		r.runner.noHolderDelivers = noHolderDelivers
		r.publisher.setAbsent(ciTopic(t, claim.RoleReviewer))
		id := r.queue(ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: 4}, "k-ci"))
		r.attempt(time.Minute)
		return r, id
	}
	t.Run("the row stays and its retry publishes once", func(t *testing.T) {
		t.Parallel()
		r, id := play(t, false)
		if attempts, delivered := r.publisher.snapshot(); attempts != 1 || len(delivered) != 0 || r.left(record.OutboxKindCINews) != 1 {
			t.Fatalf("the publish no holder took: %d attempts, %d delivered, %d rows; want the row kept", attempts, len(delivered), r.left(record.OutboxKindCINews))
		}
		r.publisher.setAbsent()
		r.attempt(3 * time.Minute)
		_, delivered := r.publisher.snapshot()
		if len(delivered) != 1 || delivered[0].key != record.OutboxKey(id) || r.left(record.OutboxKindCINews) != 0 || r.resumes() != 0 {
			t.Fatalf("the retry: delivered %+v, %d rows, %d wakes; want one publish under %s, the row finished, no wake",
				delivered, r.left(record.OutboxKindCINews), r.resumes(), record.OutboxKey(id))
		}
	})
	t.Run("control: noHolderDelivers finishes the row unpublished", func(t *testing.T) {
		t.Parallel()
		r, _ := play(t, true)
		if attempts, delivered := r.publisher.snapshot(); attempts != 1 || len(delivered) != 0 || r.left(record.OutboxKindCINews) != 0 {
			t.Fatalf("with no holder taken as delivered: %d attempts, %d delivered, %d rows; want the row finished unpublished, the fault the wait takes out",
				attempts, len(delivered), r.left(record.OutboxKindCINews))
		}
	})
}

// A row for work no longer current wakes nothing: the reader's entry raised past the row's, the
// issue's generation moved, and the pull request's head moved each finish the row with one INFO
// line, no wake and no publish. A stale prompt of the owner of the generation before, beside the
// current owner's own prompt at the same head and entry 0, wakes nobody, and the current prompt
// reaches the owner once.
//
// The control, a kept subtest: with the fence taken out (the seam noCurrentWorkFence), the row
// whose reader's entry was raised wakes the claim, which the subtest asserts.
func TestARowForWorkNoLongerCurrentWakesNothing(t *testing.T) {
	t.Parallel()
	reviewer := record.CIReader{Phase: phase.Reviewing, Entry: 4}
	for _, tc := range []struct {
		name   string
		change func(*ciRig)
	}{
		{"the reader's entry raised past the row's", func(r *ciRig) { r.entry(claim.RoleReviewer, 5) }},
		{"the generation moved", func(r *ciRig) { r.exec(`update issues set generation = 2 where key = $1`, ciIssue) }},
		{"the head moved", func(r *ciRig) { r.exec(`update pull_requests set head_sha = $2 where issue = $1`, ciIssue, ciHeadY) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCIRig(t, phase.Reviewing)
			r.entry(claim.RoleReviewer, 4)
			releasedClaim(t, r.sup, claim.RoleReviewer)
			r.queue(ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reviewer, "k-ci"))
			tc.change(r)
			r.attempt(time.Minute)
			if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 0 || r.resumes() != 0 {
				t.Fatalf("%s: %d publishes, %d rows, %d wakes; want the row finished with no wake", tc.name, attempts, r.left(record.OutboxKindCINews), r.resumes())
			}
			if lines := r.lines(staleLine); len(lines) != 1 || lines[0]["level"] != "INFO" {
				t.Fatalf("the stale row's lines = %v, want one at INFO", lines)
			}
		})
	}
	t.Run("a stale owner's prompt beside the current one", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Accepting)
		releasedClaim(t, r.sup, claim.RoleArchitect)
		r.exec(`update issues set generation = 2 where key = $1`, ciIssue)
		owner := record.CIReader{Phase: phase.Accepting}
		r.tx(func(ctx context.Context, tx pgx.Tx) error {
			for _, generation := range []uint64{1, 2} {
				prompt := ciPrompt(t, claim.RoleArchitect, generation, ciHeadX, owner)
				prompt.Checks = nil
				prompt.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: int64(generation)}}
				if joined, err := r.runner.records.EnqueueReadPrompt(ctx, tx, ciIssue, prompt, r.start); err != nil || joined {
					return fmt.Errorf("the prompt of generation %d = joined %t, %v; want a row of its own", generation, joined, err)
				}
			}
			return nil
		})
		r.attempt(time.Minute)
		r.attempt(3 * time.Minute)
		r.attempt(6 * time.Minute)
		_, delivered := r.publisher.snapshot()
		if len(delivered) != 1 || r.left(record.OutboxKindCINews) != 0 || r.resumes() != 1 {
			t.Fatalf("the two prompts: %d delivered, %d rows, %d wakes; want the current one delivered once, both finished, one wake",
				len(delivered), r.left(record.OutboxKindCINews), r.resumes())
		}
		if lines := r.lines(staleLine); len(lines) != 1 || lines[0]["generation"] != float64(1) {
			t.Fatalf("the stale lines = %v, want one, of the generation before's row", lines)
		}
	})
	t.Run("control: noCurrentWorkFence wakes the claim of a raised entry", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		r.runner.noCurrentWorkFence = true
		releasedClaim(t, r.sup, claim.RoleReviewer)
		r.queue(ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reviewer, "k-ci"))
		r.entry(claim.RoleReviewer, 5)
		r.attempt(time.Minute)
		if r.resumes() != 1 || r.left(record.OutboxKindCINews) != 1 {
			t.Fatalf("with the fence taken out: %d wakes, %d rows; want the claim woken for work no longer current, the fault the fence takes out",
				r.resumes(), r.left(record.OutboxKindCINews))
		}
	})
}

// A row of CI news with no reader is a role message: to a released claim it finishes undelivered
// with main's line and wakes nothing, and to a holder it is published once.
func TestAnInformationalRowIsDeliveredAsARoleMessage(t *testing.T) {
	t.Parallel()
	news := func(t *testing.T) record.CINews {
		return record.CINews{Purpose: record.CINewsRuns, Role: string(mustClaimToken(t, ciIssue, claim.RoleReviewer)), Generation: 1, Task: "default",
			Repo: "acme/widgets", PullRequest: 42, Head: ciHeadX, Next: record.CINextEndHoldCI,
			Runs: []record.CIRun{{Name: "unit", ID: 9, Head: ciHeadX, Conclusion: "success"}}, Body: "Legion holds the task before its retro."}
	}
	t.Run("to a released claim", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Reviewing)
		releasedClaim(t, r.sup, claim.RoleReviewer)
		r.queue(news(t))
		r.attempt(time.Minute)
		if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 0 || r.resumes() != 0 {
			t.Fatalf("the informational row to a released claim: %d publishes, %d rows, %d wakes; want it finished undelivered with no wake",
				attempts, r.left(record.OutboxKindCINews), r.resumes())
		}
		if lines := r.lines("outbox role message finished undelivered: its role's pod is released"); len(lines) != 1 {
			t.Fatalf("the role message's lines = %v, want main's one", lines)
		}
	})
	t.Run("to a holder", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Reviewing)
		id := r.queue(news(t))
		r.attempt(time.Minute)
		if _, delivered := r.publisher.snapshot(); len(delivered) != 1 || delivered[0].key != record.OutboxKey(id) || r.left(record.OutboxKindCINews) != 0 {
			t.Fatalf("the informational row to a holder: delivered %+v, %d rows; want one publish under its key", delivered, r.left(record.OutboxKindCINews))
		}
	})
}

// Ten checks of one run that change before the prompt is delivered give one row, holding the ten,
// one wake of the released reviewer and one publish.
func TestTenChecksOfOneRunGiveOneWake(t *testing.T) {
	t.Parallel()
	r := newCIRig(t, phase.Reviewing)
	r.entry(claim.RoleReviewer, 4)
	releasedClaim(t, r.sup, claim.RoleReviewer)
	r.tx(func(ctx context.Context, tx pgx.Tx) error {
		for i := 1; i <= 10; i++ {
			prompt := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: 4})
			prompt.Checks = nil
			prompt.States = []record.CIStateSnapshot{{Check: fmt.Sprintf("k-%02d", i), State: "no", Seq: int64(i)}}
			joined, err := r.runner.records.EnqueueReadPrompt(ctx, tx, ciIssue, prompt, r.start)
			if err != nil || joined != (i > 1) {
				return fmt.Errorf("prompt %d = joined %t, %v; want joined from the second on", i, joined, err)
			}
		}
		return nil
	})
	if prompts := r.prompts(); len(prompts) != 1 || len(prompts[0].States) != 10 {
		t.Fatalf("the queued prompts = %+v, want one row holding the ten checks", prompts)
	}
	r.attempt(time.Minute)
	r.attempt(3 * time.Minute)
	if _, delivered := r.publisher.snapshot(); len(delivered) != 1 || r.resumes() != 1 || r.left(record.OutboxKindCINews) != 0 {
		t.Fatalf("the ten checks: %d delivered, %d wakes, %d rows; want one publish, one wake, the row finished", len(delivered), r.resumes(), r.left(record.OutboxKindCINews))
	}
}

// readPrompting queues a prompt to read Dispatch inside the fact's transaction, as the workflow's
// checkState does (through record.Store.EnqueueReadPrompt), and keeps whether it joined a row.
type readPrompting struct {
	records record.Store
	prompt  record.CINews
	at      time.Time
	joined  *bool
}

func (h readPrompting) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	joined, err := h.records.EnqueueReadPrompt(ctx, tx, ciIssue, h.prompt, h.at)
	*h.joined = joined
	return intake.Result{}, err
}

// A prompt to read Dispatch is delivered as soon as the fact that queued it commits, as every
// other row is. The workflow queues it through record.Store.EnqueueReadPrompt, whose new row the
// record writes with its own Enqueue, so the workflow's store (projectRecords) wakes the issue's
// role-message lane there: without that wake the prompt waits for the next sweep, or for another
// row of the lane to wake it. The lanes run on a held clock, so no sweep fires: the commit wakes
// the lane, and the reviewer is published the prompt once on that wake alone.
//
// The control, a kept subtest: the same prompt queued past the workflow's store wakes no lane and
// stays queued, published to nobody, until the test fires the sweep, which then delivers it.
func TestAReadPromptRunsOnItsLaneWithoutASweep(t *testing.T) {
	t.Parallel()
	play := func(t *testing.T, workflowStore bool) (*ciRig, *heldClock, []string) {
		t.Helper()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		clock := microClock()
		var mu sync.Mutex
		var woken []string
		records := r.runner.records
		if workflowStore {
			records = projectRecords{Store: r.runner.records, project: "LEGION", log: quietLogger(), wake: func(issue string, kind record.OutboxKind) {
				mu.Lock()
				woken = append(woken, issue+":"+string(kind))
				mu.Unlock()
				r.runner.wake(issue, kind)
			}}
		}
		runLanes(t, r.runner, clock)
		prompt := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, record.CIReader{Phase: phase.Reviewing, Entry: 4})
		prompt.Checks = nil
		prompt.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 1}}
		var joined bool
		if _, err := intake.ApplyFact(context.Background(), r.pool, "lanes-test", "read-prompt", intake.DaemonBooted{Boot: 1},
			readPrompting{records: records, prompt: prompt, at: clock.Now(), joined: &joined}); err != nil || joined {
			t.Fatalf("queue the prompt = joined %t, %v; want a row of its own", joined, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return r, clock, slices.Clone(woken)
	}
	delivered := func(t *testing.T, r *ciRig) {
		t.Helper()
		testwait.Eventually(t, "the prompt published to the reviewer and its row finished", func() bool {
			_, delivered := r.publisher.snapshot()
			return len(delivered) == 1 && r.left(record.OutboxKindCINews) == 0
		})
		if _, delivered := r.publisher.snapshot(); delivered[0].topic != ciTopic(t, claim.RoleReviewer) {
			t.Fatalf("the prompt was published to %s, want the reviewer's topic %s", delivered[0].topic, ciTopic(t, claim.RoleReviewer))
		}
	}
	t.Run("the commit wakes the lane, which delivers the prompt", func(t *testing.T) {
		t.Parallel()
		r, clock, woken := play(t, true)
		if want := ciIssue + ":" + string(record.OutboxKindCINews); !slices.Equal(woken, []string{want}) {
			t.Fatalf("the commit of the prompt woke the lanes %v, want %s's: the prompt waits for the sweep", woken, want)
		}
		delivered(t, r)
		if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
			t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired", sweeps)
		}
	})
	t.Run("control: queued past the workflow's store, the prompt waits for the sweep", func(t *testing.T) {
		t.Parallel()
		r, clock, woken := play(t, false)
		if len(woken) != 0 {
			t.Fatalf("the bare store woke %v, want nothing", woken)
		}
		if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.left(record.OutboxKindCINews) != 1 {
			t.Fatalf("before the sweep: %d publishes, %d rows; want none and the prompt still queued", attempts, r.left(record.OutboxKindCINews))
		}
		fireSweep(t, clock)
		delivered(t, r)
	})
}

// A prompt whose set of checks is replaced by no checks, as the removal of a head's last check
// leaves it, keeps that empty set through its encoding, its strict decoding and the store, keeps
// it through a later change of a check's state alone that joins it, and is delivered to its
// current reader once. A prompt with neither a snapshot nor a set is refused.
//
// The control, a kept subtest: the slice encoding (omitEmptyCheckSet), a set of no checks left out
// as an empty slice is, either fails the strict decoding where the row holds no snapshot or loses
// the replacement where it holds one, which the subtest asserts.
func TestAnEmptyReplacementSetSurvivesTheOutbox(t *testing.T) {
	t.Parallel()
	reader := record.CIReader{Phase: phase.Reviewing, Entry: 4}
	t.Run("the replacement of no checks is kept, joined and delivered once", func(t *testing.T) {
		t.Parallel()
		r := newCIRig(t, phase.Reviewing)
		r.entry(claim.RoleReviewer, 4)
		r.tx(func(ctx context.Context, tx pgx.Tx) error {
			if joined, err := r.runner.records.EnqueueReadPrompt(ctx, tx, ciIssue, ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader), r.start); err != nil || joined {
				return fmt.Errorf("the empty set's prompt = joined %t, %v; want a row of its own", joined, err)
			}
			change := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
			change.Checks = nil
			change.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 3}}
			if joined, err := r.runner.records.EnqueueReadPrompt(ctx, tx, ciIssue, change, r.start); err != nil || !joined {
				return fmt.Errorf("the change of a state alone = joined %t, %v; want it joined", joined, err)
			}
			return nil
		})
		prompts := r.prompts()
		if len(prompts) != 1 || prompts[0].Checks == nil || len(*prompts[0].Checks) != 0 || len(prompts[0].States) != 1 {
			t.Fatalf("the stored prompt = %+v, want one row with a present empty set and the one snapshot", prompts)
		}
		r.attempt(time.Minute)
		if _, delivered := r.publisher.snapshot(); len(delivered) != 1 || r.left(record.OutboxKindCINews) != 0 {
			t.Fatalf("the delivery: %d delivered, %d rows; want one publish and the row finished", len(delivered), r.left(record.OutboxKindCINews))
		}
	})
	t.Run("a prompt with neither a snapshot nor a set is refused", func(t *testing.T) {
		t.Parallel()
		prompt := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		prompt.Checks = nil
		if _, err := record.NewOutboxRow(ciIssue, prompt, time.Now()); err == nil {
			t.Fatal("a prompt with neither a snapshot nor a set was taken, want it refused")
		}
	})
	t.Run("control: omitEmptyCheckSet loses the replacement", func(t *testing.T) {
		t.Parallel()
		sliceEncoded := func(n record.CINews) []byte {
			t.Helper()
			encoded, err := json.Marshal(n)
			if err != nil {
				t.Fatalf("encode the prompt: %v", err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &members); err != nil {
				t.Fatalf("read the prompt's members: %v", err)
			}
			if string(members["checks"]) == "[]" {
				delete(members, "checks")
			}
			out, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("encode the prompt again: %v", err)
			}
			return out
		}
		alone := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		if _, err := record.DecodeOutboxPayload(record.OutboxRow{ID: 1, Kind: record.OutboxKindCINews, Issue: ciIssue, Payload: sliceEncoded(alone)}); err == nil ||
			!strings.Contains(err.Error(), "set of checks") {
			t.Fatalf("the slice encoding of a set of no checks decodes = %v, want it refused for want of a set", err)
		}
		joined := ciPrompt(t, claim.RoleReviewer, 1, ciHeadX, reader)
		joined.States = []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 3}}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: 2, Kind: record.OutboxKindCINews, Issue: ciIssue, Payload: sliceEncoded(joined)})
		if err != nil || decoded.(record.CINews).Checks != nil {
			t.Fatalf("the slice encoding beside a snapshot decodes = %+v, %v; want the replacement lost", decoded, err)
		}
	})
}
