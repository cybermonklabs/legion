package record

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A defect of the change is recorded, read back while it is open, withdrawn by the owner with a
// reason, and carried by the change of a generation: the carry keeps it open until that change
// merges, and the merge ends it. A withdrawal needs a reason, ends only a reported defect, and ends
// it once. A check's reason cannot be withdrawn: only a new approved version of the Verification
// document changes a check. Another issue's defects are never read.
func TestADefectRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	issue := issueFixture("ACME-1")
	other := issueFixture("ACME-2")
	head := strings.Repeat("a", 40)
	reported := Defect{Issue: issue.Key, Generation: 3, Kind: DefectKindDefect, Text: "the retry never stops", Role: claim.RoleMerger,
		Head: head, Check: "c-retry", ReportedAt: at}
	failed := Defect{Issue: issue.Key, Generation: 3, Kind: DefectKindCheck, Text: "c-done failed at its merge", Check: "c-done",
		Result: "result-7", ReportedAt: at.Add(time.Minute)}
	var reportedID, failedID int64
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutIssue(ctx, tx, other))
		var err error
		reportedID, err = records.PutDefect(ctx, tx, reported)
		must(t, err)
		failedID, err = records.PutDefect(ctx, tx, failed)
		must(t, err)
		_, err = records.PutDefect(ctx, tx, Defect{Issue: other.Key, Generation: 3, Kind: DefectKindDefect, Text: "another issue's",
			Role: claim.RoleImplementer, Head: head, ReportedAt: at})
		must(t, err)
	})
	reported.ID, failed.ID = reportedID, failedID
	open := func() []Defect {
		t.Helper()
		var got []Defect
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.OpenDefects(ctx, tx, issue.Key)
			must(t, err)
		})
		return got
	}
	if got := open(); !reflect.DeepEqual(got, []Defect{reported, failed}) {
		t.Fatalf("the open defects = %+v, want the reported defect and the check's reason as they were put: %+v", got, []Defect{reported, failed})
	}

	inTx(t, st, func(tx pgx.Tx) {
		if _, err := records.WithdrawDefect(ctx, tx, issue.Key, reportedID, "  ", at); err == nil {
			t.Fatal("a withdrawal with no reason was taken, want it refused")
		}
		if withdrawn, err := records.WithdrawDefect(ctx, tx, issue.Key, failedID, "it is wrong", at); err != nil || withdrawn {
			t.Fatalf("the withdrawal of a check's reason = %t, %v, want it refused: only an approved version changes a check", withdrawn, err)
		}
		if withdrawn, err := records.WithdrawDefect(ctx, tx, other.Key, reportedID, "same as 9", at); err != nil || withdrawn {
			t.Fatalf("the withdrawal of %d named under %s = %t, %v, want nothing withdrawn: it is another issue's", reportedID, other.Key, withdrawn, err)
		}
	})
	withdrawnAt := at.Add(time.Hour)
	inTx(t, st, func(tx pgx.Tx) {
		withdrawn, err := records.WithdrawDefect(ctx, tx, issue.Key, reportedID, "same as 9", withdrawnAt)
		if err != nil || !withdrawn {
			t.Fatalf("the owner's withdrawal of %d = %t, %v, want it taken", reportedID, withdrawn, err)
		}
		if again, err := records.WithdrawDefect(ctx, tx, issue.Key, reportedID, "same as 9", withdrawnAt); err != nil || again {
			t.Fatalf("a second withdrawal of %d = %t, %v, want nothing: it ended already", reportedID, again, err)
		}
	})
	if got := open(); !reflect.DeepEqual(got, []Defect{failed}) {
		t.Fatalf("the open defects after the withdrawal = %+v, want the check's reason alone", got)
	}
	var row struct {
		at     time.Time
		reason string
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, tx.QueryRow(ctx, "select withdrawn_at, withdraw_reason from defects where id = $1", reportedID).Scan(&row.at, &row.reason))
	})
	if !row.at.Equal(withdrawnAt) || row.reason != "same as 9" {
		t.Fatalf("the withdrawn defect holds %v and %q, want the withdrawal's time and reason", row.at, row.reason)
	}

	// The change of generation 4 carries the check's reason: it stays open until that change
	// merges, and an end of another generation's change ends nothing of it.
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.CarryDefects(ctx, tx, issue.Key, 4, []int64{failedID}))
		must(t, records.MergeDefects(ctx, tx, issue.Key, 5, at.Add(2*time.Hour)))
	})
	failed.CarriedBy = 4
	if got := open(); !reflect.DeepEqual(got, []Defect{failed}) {
		t.Fatalf("the open defects once generation 4 carries the reason = %+v, want it still open, carried by 4", got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.MergeDefects(ctx, tx, issue.Key, 4, at.Add(3*time.Hour)))
	})
	if got := open(); len(got) != 0 {
		t.Fatalf("the open defects once the change that carries the reason merged = %+v, want none", got)
	}
}

// A read of the checks' states is the issue's newest read: a later read replaces it, and a read
// that began before the stored one, which finished after it, does not. A read holds Dispatch's
// answer or says why it failed.
func TestTheNewestCheckReadStands(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	issue := issueFixture("ACME-1")
	answer := json.RawMessage(`{"checks": [{"id": "c-done", "answer": "yes"}]}`)
	newest := CheckRead{Issue: issue.Key, Generation: 3, ReadAt: at.Add(time.Minute), Answer: answer, RecordedAt: at.Add(2 * time.Minute)}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		if read, err := records.NewestCheckRead(ctx, tx, issue.Key); err != nil || read != nil {
			t.Fatalf("the newest read before any = %+v, %v, want none", read, err)
		}
		if stored, err := records.PutCheckRead(ctx, tx, newest); err != nil || !stored {
			t.Fatalf("store the read = %t, %v, want it stored", stored, err)
		}
		late := CheckRead{Issue: issue.Key, Generation: 3, ReadAt: at, Failure: "502 BAD_GATEWAY", RecordedAt: at.Add(3 * time.Minute)}
		if stored, err := records.PutCheckRead(ctx, tx, late); err != nil || stored {
			t.Fatalf("store a read that began before the stored one = %t, %v, want it left out", stored, err)
		}
		if _, err := records.PutCheckRead(ctx, tx, CheckRead{Issue: issue.Key, ReadAt: at, RecordedAt: at}); err == nil {
			t.Fatal("a read with neither an answer nor a failure was stored, want it refused")
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		read, err := records.NewestCheckRead(ctx, tx, issue.Key)
		must(t, err)
		var got, want map[string]any
		must(t, json.Unmarshal(read.Answer, &got))
		must(t, json.Unmarshal(answer, &want))
		read.Answer = nil
		stored := newest
		stored.Answer = nil
		if !reflect.DeepEqual(*read, stored) || !reflect.DeepEqual(got, want) {
			t.Fatalf("the newest read = %+v with %v, want %+v with %v", *read, got, stored, want)
		}
	})
}
