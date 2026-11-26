package admit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// passRecordLine is the message of the record line of an admission pass that moves a slot.
const passRecordLine = "admission: a pass moved slots"

// passRecord is one record line of an admission pass, decoded from the JSON the daemon's logger
// writes: the slots in use after the pass, the cap, each slot the pass gave out, each slot it
// took back and each candidate of the line it passed over. An empty list and an absent one read
// alike, as nil.
type passRecord struct {
	Slots  int                 `json:"slots"`
	Cap    int                 `json:"cap"`
	Given  []record.SlotGiven  `json:"given"`
	Taken  []record.SlotTaken  `json:"taken"`
	Passed []record.SlotPassed `json:"passed"`
}

// passRecords is every record line logged holds, in order, with the length in bytes of each line
// as the handler wrote it.
func passRecords(t *testing.T, logged *bytes.Buffer) ([]passRecord, []int) {
	t.Helper()
	var records []passRecord
	var sizes []int
	for _, line := range loggedLines(t, logged, passRecordLine) {
		var pass passRecord
		if err := json.Unmarshal(line, &pass); err != nil {
			t.Fatalf("decode the record line %q: %v", line, err)
		}
		if len(pass.Given) == 0 {
			pass.Given = nil
		}
		if len(pass.Taken) == 0 {
			pass.Taken = nil
		}
		if len(pass.Passed) == 0 {
			pass.Passed = nil
		}
		records = append(records, pass)
		sizes = append(sizes, len(line))
	}
	return records, sizes
}

// loggedLines is every line logged holds whose message is msg, in order, each as the handler
// wrote it, failing t on a line that is no JSON object.
func loggedLines(t *testing.T, logged *bytes.Buffer, msg string) [][]byte {
	t.Helper()
	var lines [][]byte
	for _, line := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		if head.Msg == msg {
			lines = append(lines, line)
		}
	}
	return lines
}

// loggedMessage says whether logged holds a line whose message is msg.
func loggedMessage(t *testing.T, logged *bytes.Buffer, msg string) bool {
	t.Helper()
	return len(loggedLines(t, logged, msg)) > 0
}

// failingHandler fails every fact it is handed. Put after admission in a chain, it rolls back the
// fact's transaction after the pass has run in it.
type failingHandler struct{}

func (failingHandler) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	return intake.Result{}, errors.New("the fact fails after the admission pass ran in its transaction")
}

// tickRolledBack applies one controller tick through the daemon's own chain of handlers with
// failingHandler behind admission, so the pass runs and its fact rolls back, and fails t where
// the fact applied.
func tickRolledBack(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	chain := append(Handlers(record.NewStore(), acmeProject, admission.engine, admission), failingHandler{})
	if _, err := intake.ApplyFact(context.Background(), pool, "controller", "the pass", intake.ControllerTick{}, chain...); err == nil {
		t.Fatalf("the fact applied, want it to fail and roll back after the pass")
	}
}

// An admission pass that gives out or takes back a slot writes one record line once its fact
// commits (R9): the slots in use after the pass, the cap, each slot it gave out with the kind of
// entry its issue stood as, each slot it took back with the path that took it, and each candidate
// of the line it passed over with its kind, its place in the line and why. The line is written
// through the daemon's own JSON handler, as cmd/legion's daemon logger writes it, so what a reader
// of a window decodes is what this decodes. A pass that moves no slot writes none, and so does a
// pass whose transaction rolls back, whose slots never moved. A pass over a thousand candidates
// still writes one line, under the 256 KiB a log entry may hold.
func TestAPassThatMovesASlotWritesOneRecordLine(t *testing.T) {
	cases := []struct {
		name string
		cap  int
		seed func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
		want []passRecord
	}{
		{name: "a pass that gives out one slot", cap: 1,
			seed: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				acmeQueuedRoot(t, pool, "ACME-1", "a")
			},
			want: []passRecord{{Slots: 1, Cap: 1, Given: []record.SlotGiven{{Issue: "ACME-1", Kind: record.LineQueuedRoot.String()}}}},
		},
		{name: "a pass that takes one back", cap: 1,
			seed: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				seedSlottedInPhase(t, pool, acmeProject, "ACME-2", "a", phase.Accepting, func(issue *record.Issue) {
					issue.Status, issue.DispatchStatus = "needs_review", "needs_review"
				})
			},
			want: []passRecord{{Slots: 0, Cap: 1, Taken: []record.SlotTaken{{Issue: "ACME-2", Reason: record.TakenPastReview}}}},
		},
		{name: "a pass that takes one back and gives it out", cap: 1,
			seed: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				seedSlottedInPhase(t, pool, acmeProject, "ACME-3", "a", phase.Accepting, func(issue *record.Issue) {
					issue.Status, issue.DispatchStatus = "needs_review", "needs_review"
				})
				acmeQueuedRoot(t, pool, "ACME-4", "b")
			},
			want: []passRecord{{Slots: 1, Cap: 1,
				Given: []record.SlotGiven{{Issue: "ACME-4", Kind: record.LineQueuedRoot.String()}},
				Taken: []record.SlotTaken{{Issue: "ACME-3", Reason: record.TakenPastReview}}}},
		},
		{name: "a pass that passes over candidates for the cap, for a wait and for a held key", cap: 1,
			seed: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				// The line is the four queued roots by rank: the first takes the cap's one slot, the
				// second waits on an issue Dispatch has not finished, the third finds the cap full,
				// and the fourth is a key the boot's catch-up still holds, which stops the pass.
				acmeQueuedRoot(t, pool, "ACME-5", "a")
				seedWaitingForProject(t, pool, acmeProject, "ACME-6", "b")
				seedMirrorIn(t, pool, acmeProject, "ACME-6", "todo", "ACME-9")
				seedMirrorIn(t, pool, acmeProject, "ACME-9", "in_progress")
				acmeQueuedRoot(t, pool, "ACME-7", "c")
				acmeQueuedRoot(t, pool, "ACME-8", "d")
				admission.HoldForCatchUp("ACME-8")
			},
			want: []passRecord{{Slots: 1, Cap: 1,
				Given: []record.SlotGiven{{Issue: "ACME-5", Kind: record.LineQueuedRoot.String()}},
				Passed: []record.SlotPassed{
					{Issue: "ACME-6", Kind: record.LineQueuedRoot.String(), Place: 2, Reason: record.PassedWaits},
					{Issue: "ACME-7", Kind: record.LineQueuedRoot.String(), Place: 3, Reason: record.PassedCap},
					{Issue: "ACME-8", Kind: record.LineQueuedRoot.String(), Place: 4, Reason: record.PassedHeldKey},
				}}},
		},
		{name: "a pass that moves no slot", cap: 1,
			seed: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				// The holder keeps the cap's one slot and the queued root finds the cap full: the
				// pass passes it over and moves nothing, so it writes no record.
				seedSlottedInPhase(t, pool, acmeProject, "ACME-10", "a", phase.Implementing, nil)
				acmeQueuedRoot(t, pool, "ACME-11", "b")
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			var logged bytes.Buffer
			admission := ruledAdmission(t, tc.cap, record.MainRule, slog.New(slog.NewJSONHandler(&logged, nil)))
			tc.seed(t, pool, admission)
			tickThrough(t, pool, admission, "the pass")
			if got, _ := passRecords(t, &logged); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the record lines of the pass = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("a pass whose transaction rolls back", func(t *testing.T) {
		pool := migratedPool(t)
		var logged bytes.Buffer
		admission := ruledAdmission(t, 1, record.MainRule, slog.New(slog.NewJSONHandler(&logged, nil)))
		acmeQueuedRoot(t, pool, "ACME-12", "a")
		tickRolledBack(t, pool, admission)
		assertSlots(t, pool, nil)
		if got, _ := passRecords(t, &logged); got != nil {
			t.Fatalf("the record lines of a pass that rolled back = %+v, want none: nothing it moved was kept", got)
		}
		if loggedMessage(t, &logged, "admission: slot taken") {
			t.Fatalf("a pass that rolled back logged its slot taken: the log of the slots runs on commit, and nothing committed")
		}
	})

	t.Run("a pass over a thousand candidates", func(t *testing.T) {
		const candidates = 1000
		pool := migratedPool(t)
		var logged bytes.Buffer
		admission := ruledAdmission(t, 1, record.MainRule, slog.New(slog.NewJSONHandler(&logged, nil)))
		key := func(i int) string { return fmt.Sprintf("ACME-%d", 1000+i) }
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for i := range candidates {
				if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key(i), Repo: "acme/widgets", DefaultBranch: "main",
					GitHubRepo: "acme/widgets", Project: acmeProject, Title: key(i), Tree: key(i), Phase: phase.Admitted, Generation: 1,
					Status: "todo", Rank: fmt.Sprintf("%04d", i), HandedOver: true}); err != nil {
					t.Fatalf("put the waiting root %s: %v", key(i), err)
				}
			}
		})
		want := passRecord{Slots: 1, Cap: 1, Given: []record.SlotGiven{{Issue: key(0), Kind: record.LineQueuedRoot.String()}}}
		for i := 1; i < candidates; i++ {
			want.Passed = append(want.Passed, record.SlotPassed{Issue: key(i), Kind: record.LineQueuedRoot.String(), Place: i + 1,
				Reason: record.PassedCap})
		}

		tickThrough(t, pool, admission, "the pass")

		got, sizes := passRecords(t, &logged)
		if len(got) != 1 {
			t.Fatalf("the pass wrote %d record lines, want one", len(got))
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Fatalf("the record line = %d given, %d taken, %d passed, slots %d of %d; want the first root given and the %d behind it passed for the cap, in their places",
				len(got[0].Given), len(got[0].Taken), len(got[0].Passed), got[0].Slots, got[0].Cap, candidates-1)
		}
		if limit := 256 << 10; sizes[0] >= limit {
			t.Fatalf("the record line is %d bytes, want it under %d, the most one log entry holds", sizes[0], limit)
		}
	})
}
