package legionfacts

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestAPrunedCursorChangeComputesNothingWholeAndAVanishedYoungOneDoes: the change the change
// cursor rests on passes its 24 hours and is pruned (delivery.PruneChanges), as the prune takes it
// after a quiet day, while every pass read the change log to its end within the retention: that is
// no loss, and the pass after the prune computes no project whole. The change the cursor rests on
// leaving the log younger than its retention, as a reset log or a delete by hand takes it, is a
// loss no one can name, and the pass after it computes every project whole. So is a change pruned
// after its retention when no pass had read the change log to its end for longer than the
// retention: while the consumer stood still the prune may have taken changes after the cursor that
// no pass read. A prune that empties the log is no loss, since the next change continues the
// sequence above the cursor; a log whose sequence restarted below the cursor (TRUNCATE ... RESTART
// IDENTITY, a restore), holding rows below it and none at it, is a loss whatever the age of the
// change the cursor rested on, since a cursor read on would read nothing until the new seqs passed
// it.
func TestAPrunedCursorChangeComputesNothingWholeAndAVanishedYoungOneDoes(t *testing.T) {
	run := newConsumer(t, 2)
	project := run.fixture.Project
	run.pass(t)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	// restsOn logs one change recorded age before the database's clock, the clock the prune cuts
	// by, has a pass read it, and answers its seq, which the change cursor then rests on.
	restsOn := func(ref string, age time.Duration) int64 {
		t.Helper()
		var seq int64
		if err := run.database.Pool.QueryRow(t.Context(), `
			insert into delivery_changes (kind, repo, ref, at)
			values ('session_group', null, $1, now() - make_interval(secs => $2))
			returning seq
		`, ref, age.Seconds()).Scan(&seq); err != nil {
			t.Fatalf("log the change %s: %v", ref, err)
		}
		run.at = run.at.Add(time.Minute)
		run.pass(t)
		if resting := run.cursors(t)[changesCursor].position; resting != seq {
			t.Fatalf("after the pass that read the change %s at %d the change cursor rests at %d, so this test examined nothing", ref, seq, resting)
		}
		return seq
	}
	gone := func(seq int64) {
		t.Helper()
		var held bool
		if err := run.database.Pool.QueryRow(t.Context(), `select exists (select from delivery_changes where seq = $1)`, seq).Scan(&held); err != nil {
			t.Fatalf("read the change at %d: %v", seq, err)
		}
		if held {
			t.Fatalf("the change at %d, which the cursor rests on, is still in the log, so this test examined nothing", seq)
		}
	}
	// wholeAfter is whether the next pass, at the consumer's clock, computed the project whole.
	wholeAfter := func() bool {
		t.Helper()
		before := computedWholeAt(t, run)[project]
		run.pass(t)
		after := computedWholeAt(t, run)[project]
		return !after.Equal(before) && after.Equal(run.at)
	}
	// prunedThrough ages every change at or below seq past its retention, as the day's clock
	// leaves a log whose changes are recorded in the order of their seqs, and prunes the log.
	prunedThrough := func(seq int64) {
		t.Helper()
		run.exec(t, "age the log through the cursor past its retention",
			`update delivery_changes set at = now() - interval '25 hours' where seq <= $1`, seq)
		if _, err := delivery.PruneChanges(t.Context(), run.database.Pool); err != nil {
			t.Fatalf("prune the change log: %v", err)
		}
		gone(seq)
	}
	// logged logs one change no pass has read yet, recorded now, and answers its seq.
	logged := func(ref string) int64 {
		t.Helper()
		var seq int64
		if err := run.database.Pool.QueryRow(t.Context(), `
			insert into delivery_changes (kind, repo, ref) values ('session_group', null, $1) returning seq
		`, ref).Scan(&seq); err != nil {
			t.Fatalf("log the change %s: %v", ref, err)
		}
		return seq
	}

	// The change the cursor rests on and every change below it pass their 24 hours and are
	// pruned, and a newer change arrives, which wakes the pass: it reads on from the cursor.
	quiet := restsOn("quiet", 25*time.Hour)
	prunedThrough(quiet)
	newer := logged("newer")
	run.at = run.at.Add(time.Minute)
	quietWhole := wholeAfter()
	if quietWhole {
		t.Errorf("the pass after the change at the cursor, %d, was pruned after its 24 hours computed %s whole, with every pass committed within the retention: a routine prune is no loss", quiet, project)
	}
	if resting := run.cursors(t)[changesCursor].position; resting != newer {
		t.Errorf("after the pass that read a pruned change as no loss the change cursor rests at %d, want %d, the change after it", resting, newer)
	}

	// The change the cursor rests on is deleted younger than its 24 hours, with a newer change
	// still in the log.
	young := restsOn("young", 0)
	after := logged("after the young one")
	run.exec(t, "delete the young change by hand", `delete from delivery_changes where seq = $1`, young)
	gone(young)
	run.at = run.at.Add(time.Minute)
	youngWhole := wholeAfter()
	if !youngWhole {
		t.Errorf("the pass after the change at the cursor, %d, left the log younger than its 24 hours (%d stands after it) computed %s whole as of no new moment: that is a loss", young, after, project)
	}

	stalled := restsOn("stalled", 25*time.Hour)
	prunedThrough(stalled)
	run.at = run.at.Add(25 * time.Hour)
	stalledWhole := wholeAfter()
	if !stalledWhole {
		t.Errorf("the pass 25 hours after the last one, the change at its cursor, %d, pruned meanwhile, computed %s whole as of no new moment: that is a loss", stalled, project)
	}

	emptied := restsOn("emptied", 25*time.Hour)
	prunedThrough(emptied)
	if head, err := delivery.ChangesHead(t.Context(), run.database.Pool); err != nil || head.Seq != 0 {
		t.Fatalf("the change log's newest seq after the prune is %d (%v), want 0, an empty log, so this case examined nothing", head.Seq, err)
	}
	run.at = run.at.Add(time.Minute)
	emptiedWhole := wholeAfter()
	if emptiedWhole {
		t.Errorf("the pass after a prune emptied the log, the change at the cursor, %d, pruned after its 24 hours, computed %s whole: an empty log continues the sequence above the cursor and is no loss", emptied, project)
	}

	run.exec(t, "restart the change log's sequence", `truncate delivery_changes restart identity`)
	var restarted int64
	if err := run.database.Pool.QueryRow(t.Context(), `
		insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'restarted') returning seq
	`).Scan(&restarted); err != nil {
		t.Fatalf("log a change of the restarted log: %v", err)
	}
	if restarted >= emptied {
		t.Fatalf("the restarted log's first change has the seq %d, not below the cursor's %d, so this case examined nothing", restarted, emptied)
	}
	run.at = run.at.Add(time.Minute)
	restartedWhole := wholeAfter()
	if !restartedWhole {
		t.Errorf("the pass after the log's sequence restarted below the cursor, %d, at %d computed %s whole as of no new moment: that is a loss", emptied, restarted, project)
	}
	if resting := run.cursors(t)[changesCursor].position; resting != restarted {
		t.Errorf("after the pass that met the restarted log the change cursor rests at %d, want %d, the restarted log's head", resting, restarted)
	}
	t.Logf("%s computed whole after the change at the cursor was pruned in its time (%d): %t; deleted young (%d): %t; pruned after a stall of 25 hours (%d): %t; pruned out of an emptied log (%d): %t; the sequence restarted below the cursor at %d: %t",
		project, quiet, quietWhole, young, youngWhole, stalled, stalledWhole, emptied, emptiedWhole, restarted, restartedWhole)
}
