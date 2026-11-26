package holdrecord_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// hubAbortRows are TestTheHoldJournal's rows of hub-restore's hold abort: before and after a done
// prove, after a stopped census, after the proven disk is lost, and after a crash before the
// release. TestTheHoldJournal runs them in its own order of rows. Each row runs on a schema of its
// own.
func hubAbortRows(t *testing.T) {
	ctx := context.Background()
	t.Run("hub-restore's hold abort: before a prove it completes, after one it is excluded, refused it stops", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-abort", holdrecord.WorkflowHubRestore, twoDisks)
		if _, err := c.Admit(ctx, "h-abort", cmd("hold abort"), nil); err != nil {
			t.Fatalf("hold abort before any prove: %v", err)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-abort", "aborted"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-abort", "hold abort", nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-abort"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}

		hold(t, c, "h-abort2", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-abort2", cmd("hold prove"), nil, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		_, err := c.Admit(ctx, "h-abort2", cmd("hold abort"), nil)
		refused(t, "hold abort after a done prove", err, holdrecord.ErrExcluded, "hold prove")
		run(t, c, "h-abort2", cmd("streams census"), nil, nil)
		release(t, c, "h-abort2")

		hold(t, c, "h-abort3", holdrecord.WorkflowHubRestore, twoDisks)
		if _, err := c.Admit(ctx, "h-abort3", cmd("hold abort"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-abort3", "hold abort", "target A's disk is not step 1's"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-abort3", cmd("hold prove"), map[string]string{"restored.A": "disk-7"}); err != nil {
			t.Fatalf("hold prove after a stopped abort: %v", err)
		}
	})

	// After a done prove, a census that stops (UNKNOWN) admits hold abort, its intent carrying the
	// census's record of what it left; hold release stays refused, since hub-restore's completion is
	// its clean census. The abort then releases the row, over the proven disks, which the hold's
	// command compares with the done prove's record outside the journal (t-10's check), and its done
	// entry completes the record.
	t.Run("hub-restore: after a done prove, a stopped census admits hold abort, which releases the row", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-hubstop", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-hubstop", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-7"}, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		if _, err := c.Admit(ctx, "h-hubstop", cmd("streams census"), nil); err != nil {
			t.Fatal(err)
		}
		const left = "UNKNOWN: KV_envoy_roles' info cannot be read"
		if err := c.AppendStopLeft(ctx, "h-hubstop", "streams census", "the run is not clean", left); err != nil {
			t.Fatalf("the census's stop: %v", err)
		}
		_, err := c.Admit(ctx, "h-hubstop", cmd(holdrecord.CommandRelease), nil)
		refused(t, "hold release after the census's stop", err, holdrecord.ErrOutOfOrder, "not complete")
		if _, err := c.Admit(ctx, "h-hubstop", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort after a done prove and the census's stop: %v", err)
		}
		d, _ := journal(t, c, "h-hubstop")
		abort, carried := last(t, d), false
		for _, v := range abort.Inputs {
			carried = carried || v == "streams census: "+left
		}
		if abort.Command != holdrecord.CommandAbort || abort.State != "intent" || !carried {
			t.Fatalf("the journal ends %+v, want hold abort's intent carrying the census's record %q", abort, "streams census: "+left)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-hubstop", "aborted over the proven disks"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-hubstop", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-hubstop"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}
	})

	// Rules (1) and (2): after a failed re-prove, abort's disk check finds the done prove's disk
	// gone and its stop names it; the operator restores onto another disk and proves it, and abort,
	// admitted again after its own stop, releases the row over the new epoch's disk.
	t.Run("hub-restore: an abort that finds the proven disk gone stops, and after a new prove runs again", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-gone", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-gone", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-7"}, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		if _, err := c.Admit(ctx, "h-gone", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-8"}); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStopLeft(ctx, "h-gone", holdrecord.CommandProve, "disk-8 is not attached", holdrecord.LeftUnchanged); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-gone", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("abort after the failed re-prove: %v", err)
		}
		if err := c.AppendStopLeft(ctx, "h-gone", holdrecord.CommandAbort, "the done prove's disk of A is gone: disk-7 answers 404", holdrecord.LeftUnchanged); err != nil {
			t.Fatalf("abort's gone-disk stop: %v", err)
		}
		run(t, c, "h-gone", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-9"}, map[string]string{"disk.A": "disk-9", "disk_id.A": "9"})
		if _, err := c.Admit(ctx, "h-gone", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("abort after its own stop and the new prove: %v", err)
		}
		d, _ := journal(t, c, "h-gone")
		if p, _ := holdrecord.Proven(d); p.DiskIDs["A"] != "9" {
			t.Fatalf("the abort reads the done prove's disk %q, want the new epoch's 9", p.DiskIDs["A"])
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-gone", "aborted over the new epoch's disk"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-gone", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-gone"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}
	})

	// The condition on rule (2): an abort admitted on its own pending intent is a rerun of a crashed
	// abort, which repeats the whole abort from its start (the disk check against the newest done
	// prove, then the reconcile over every store) and trusts nothing the crashed run wrote. Here the
	// abort, admitted on its own stop from before the new prove, crashes between the reconcile's
	// writes and the release; its rerun is admitted, reconciles again and releases.
	t.Run("hub-restore: an abort that crashes before its release is rerun whole and releases", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-rerun", holdrecord.WorkflowHubRestore, twoDisks)
		run(t, c, "h-rerun", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-7"}, map[string]string{"disk.A": "disk-7", "disk_id.A": "7"})
		if _, err := c.Admit(ctx, "h-rerun", cmd("streams census"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStopLeft(ctx, "h-rerun", "streams census", "the run is not clean", "UNKNOWN: KV_envoy_roles' info cannot be read"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-rerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStopLeft(ctx, "h-rerun", holdrecord.CommandAbort, "the done prove's disk of A is gone: disk-7 answers 404", holdrecord.LeftUnchanged); err != nil {
			t.Fatal(err)
		}
		run(t, c, "h-rerun", cmd(holdrecord.CommandProve), map[string]string{"restored.A": "disk-9"}, map[string]string{"disk.A": "disk-9", "disk_id.A": "9"})
		// The abort, admitted on its own stop, runs its disk check and the reconcile's writes, then
		// its process is killed before the release: its intent is its latest entry.
		if _, err := c.Admit(ctx, "h-rerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("abort after its own stop and the new prove: %v", err)
		}
		d, _ := journal(t, c, "h-rerun")
		if e := last(t, d); e.Command != holdrecord.CommandAbort || e.State != "intent" {
			t.Fatalf("after the crash the journal ends %+v, want abort's pending intent", e)
		}
		// The rerun is admitted on its own pending intent, repeats the check and the reconcile, and
		// releases.
		if _, err := c.Admit(ctx, "h-rerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("the crashed abort's rerun: %v", err)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-rerun", "the rerun released"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-rerun", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-rerun"); !d.Complete {
			t.Fatal("the rerun's done entry did not complete the record")
		}
	})
}

// wayOutRows are TestTheHoldJournal's rows of the way out of a command that fails, the buckets
// backup, the records floor and the end of the hold, which it runs last. Each row runs on a schema
// of its own.
func wayOutRows(t *testing.T) {
	ctx := context.Background()
	// A buckets command that fails for good after its intent must not wedge the hold: its stop
	// ends the intent and records what it left, and hold abort ends the hold with that record.
	t.Run("a buckets restore cut after its intent stops on its rerun, and hold abort ends the hold recording what it left", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-wayout", holdrecord.WorkflowBucketsRestore, nil)
		// The first run is cut after its intent: its process is gone, the intent pending.
		if _, err := c.Admit(ctx, "h-wayout", cmd("buckets restore"), nil); err != nil {
			t.Fatal(err)
		}
		_, err := c.Admit(ctx, "h-wayout", cmd(holdrecord.CommandAbort), nil)
		refused(t, "hold abort with the cut restore's intent pending", err, holdrecord.ErrBusy, "buckets restore")
		// Its rerun fails for good after deleting the bucket, and its stop names what it left.
		if _, err := c.Admit(ctx, "h-wayout", cmd("buckets restore"), nil); err != nil {
			t.Fatalf("the restore's rerun: %v", err)
		}
		const left = "changed: envoy_sessions deleted and not restored"
		if err := c.AppendStopLeft(ctx, "h-wayout", "buckets restore", "the snapshot's chunks never arrive", left); err != nil {
			t.Fatalf("the rerun's stop: %v", err)
		}
		d, _ := journal(t, c, "h-wayout")
		if e := last(t, d); e.State != "stop" || e.Outputs["left"] != left || e.Outputs["reason"] != "the snapshot's chunks never arrive" {
			t.Fatalf("the journal ends %+v, want the restore's stop with its reason and what it left", e)
		}
		if _, err := c.Admit(ctx, "h-wayout", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort after the restore's stop: %v", err)
		}
		d, _ = journal(t, c, "h-wayout")
		abort, carried := last(t, d), false
		for _, v := range abort.Inputs {
			carried = carried || v == "buckets restore: "+left
		}
		if abort.Command != holdrecord.CommandAbort || abort.State != "intent" || !carried {
			t.Fatalf("the journal ends %+v, want hold abort's intent carrying the stop's record %q", abort, "buckets restore: "+left)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-wayout", "aborted"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-wayout", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-wayout"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}
		if after := row(t, c); after.Phase != natsgrant.OperatorPhaseReleased {
			t.Fatalf("after hold abort the row is %s", after.Phase)
		}
	})

	t.Run("hold release is refused after a stop that left changes, or that recorded nothing", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		for _, s := range []struct {
			holdID, left string
		}{{"h-changed", "changed: envoy_roles restored in part"}, {"h-unrecorded", ""}} {
			hold(t, c, s.holdID, holdrecord.WorkflowBucketsRestore, nil)
			if _, err := c.Admit(ctx, s.holdID, cmd("buckets restore"), nil); err != nil {
				t.Fatal(err)
			}
			var stop error
			if s.left != "" {
				stop = c.AppendStopLeft(ctx, s.holdID, "buckets restore", "the snapshot's chunks never arrive", s.left)
			} else {
				stop = c.AppendStop(ctx, s.holdID, "buckets restore", "the snapshot's chunks never arrive")
			}
			if stop != nil {
				t.Fatalf("%s: the restore's stop: %v", s.holdID, stop)
			}
			_, err := c.Admit(ctx, s.holdID, cmd(holdrecord.CommandRelease), nil)
			refused(t, s.holdID+": hold release after the stop", err, holdrecord.ErrOutOfOrder, "not complete")
			// The way out is hold abort, which ends the hold.
			if _, err := c.Admit(ctx, s.holdID, cmd(holdrecord.CommandAbort), nil); err != nil {
				t.Fatalf("%s: hold abort after the stop: %v", s.holdID, err)
			}
			current := row(t, c)
			if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, s.holdID, "aborted"); err != nil {
				t.Fatal(err)
			}
			if err := c.AppendDone(ctx, s.holdID, holdrecord.CommandAbort, nil); err != nil {
				t.Fatal(err)
			}
		}
	})

	// envoy-infra buckets backup takes a snapshot of each copied bucket, so it runs under a hold:
	// the first command of buckets-recreate and buckets-restore, Repeatable, and Stoppable with a
	// stop that left nothing changed, since it writes only the archive outside the hub. The
	// recreate, which deletes the bucket, needs the backup's done entry; the restore does not, since
	// a lost bucket cannot be backed up and restores from an archive an earlier hold took.
	t.Run("buckets backup runs first in a buckets-recreate hold, again and again, and the recreate needs it", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-backup-recreate", holdrecord.WorkflowBucketsRecreate, nil)
		run(t, c, "h-backup-recreate", cmd(commandBackup), nil, map[string]string{"archive": "gs://archive/a"})
		run(t, c, "h-backup-recreate", cmd(commandBackup), nil, map[string]string{"archive": "gs://archive/b"})
		if _, err := c.Admit(ctx, "h-backup-recreate", cmd(commandBackup), nil); err != nil {
			t.Fatalf("a third backup: %v", err)
		}
		if err := c.AppendStopLeft(ctx, "h-backup-recreate", commandBackup, "a snapshot chunk timed out", holdrecord.LeftUnchanged); err != nil {
			t.Fatalf("the backup's stop: %v", err)
		}
		run(t, c, "h-backup-recreate", cmd("buckets recreate"), nil, nil)
		if d, _ := journal(t, c, "h-backup-recreate"); !d.Complete {
			t.Fatal("the recreate after the backups did not complete the record")
		}
		release(t, c, "h-backup-recreate")
		// A second hold: the recreate before any backup is refused, and a backup that stops having
		// changed nothing leaves a hold that hold release ends.
		hold(t, c, "h-recreate-first", holdrecord.WorkflowBucketsRecreate, nil)
		_, err := c.Admit(ctx, "h-recreate-first", cmd("buckets recreate"), nil)
		refused(t, "the recreate before any backup", err, holdrecord.ErrOutOfOrder, commandBackup)
		if _, err := c.Admit(ctx, "h-recreate-first", cmd(commandBackup), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStopLeft(ctx, "h-recreate-first", commandBackup, "the bucket's stream cannot be read", holdrecord.LeftUnchanged); err != nil {
			t.Fatal(err)
		}
		release(t, c, "h-recreate-first")
	})

	t.Run("buckets backup runs in a buckets-restore hold, again and again, and the restore runs with or without it", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-backup-restore", holdrecord.WorkflowBucketsRestore, nil)
		run(t, c, "h-backup-restore", cmd(commandBackup), nil, nil)
		run(t, c, "h-backup-restore", cmd(commandBackup), nil, nil)
		if _, err := c.Admit(ctx, "h-backup-restore", cmd(commandBackup), nil); err != nil {
			t.Fatalf("a third backup: %v", err)
		}
		if err := c.AppendStopLeft(ctx, "h-backup-restore", commandBackup, "a snapshot chunk timed out", holdrecord.LeftUnchanged); err != nil {
			t.Fatalf("the backup's stop: %v", err)
		}
		run(t, c, "h-backup-restore", cmd("buckets restore"), nil, nil)
		if d, _ := journal(t, c, "h-backup-restore"); !d.Complete {
			t.Fatal("the restore after the backups did not complete the record")
		}
		release(t, c, "h-backup-restore")
		hold(t, c, "h-restore-alone", holdrecord.WorkflowBucketsRestore, nil)
		run(t, c, "h-restore-alone", cmd("buckets restore"), nil, nil)
		release(t, c, "h-restore-alone")
	})

	// The broker's floor of each record: an older signed journal a database writer puts back reads
	// as unknown, so Admit fails closed, and hold release is not admitted over a command that
	// changed data. A broker that starts again has no floor: the stated residual.
	t.Run("a replayed older journal is unknown, so hold release is refused after a command changed data", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-replay", holdrecord.WorkflowBucketsRecreate, nil)
		var older, olderSig string
		var olderGen int64
		if err := r.Pool.QueryRow(ctx, `select value::text, gen, sig from nats_hold_records where hold_id = 'h-replay' and kind = 'done'`).Scan(&older, &olderGen, &olderSig); err != nil {
			t.Fatal(err)
		}
		recreateStops(t, c, "h-replay", "envoy_sessions deleted")
		_, err := c.Admit(ctx, "h-replay", cmd(holdrecord.CommandRelease), nil)
		refused(t, "hold release after the recreate's stop", err, holdrecord.ErrOutOfOrder, "not complete")
		// A database writer puts back the earlier signed journal: hold set's entries alone.
		if _, err := r.Pool.Exec(ctx, `update nats_hold_records set value = $1::jsonb, gen = $2, sig = $3 where hold_id = 'h-replay' and kind = 'done'`, older, olderGen, olderSig); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-replay", cmd(holdrecord.CommandRelease), nil); !errors.Is(err, holdrecord.ErrUnknown) {
			t.Fatalf("hold release over the put-back gen-%d journal answered %v, want ErrUnknown", olderGen, err)
		}
		// The residual: a broker process that starts again has no floor, and admits the release.
		r.Restart()
		if _, err := c.Admit(ctx, "h-replay", cmd(holdrecord.CommandRelease), nil); err != nil {
			t.Fatalf("after a restart hold release over the put-back journal answered %v; the stated residual is that it is admitted", err)
		}
	})

	// Admit reads the row before the journal, so a process can read the row held, and another end
	// the hold before it reads the journal: the journal's done release ends it, and Decide refuses
	// the command, so no intent is written after the hold ended. Release's done keeps the record's
	// completion.
	t.Run("a command whose row read came before another process's release is refused", func(t *testing.T) {
		r := newRig(t)
		a, b := r.client(t), r.client(t)
		hold(t, a, "h-ended", holdrecord.WorkflowSelectionRecovery, nil)
		run(t, a, "h-ended", cmd("create-bucket --after-loss"), nil, nil)
		journalRead := func(req *http.Request) bool {
			return req.Method == http.MethodGet && req.URL.Path == "/v1/nats-hold/records" && req.URL.Query().Get("kind") == "done"
		}
		fired := r.once(journalRead, func(http.ResponseWriter, *http.Request) bool {
			release(t, a, "h-ended") // between b's row read and its journal read
			return false
		})
		_, err := b.Admit(ctx, "h-ended", cmd("replay"), nil)
		if !fired.Load() {
			t.Fatal("the release between the row read and the journal read did not run")
		}
		refused(t, "replay admitted after another process's release", err, holdrecord.ErrAlreadyDone, "the hold has ended")
		d, _ := journal(t, a, "h-ended")
		if e := last(t, d); e.Command != holdrecord.CommandRelease || e.State != "done" || !d.Complete {
			t.Fatalf("after the release the journal ends %+v with complete %v, want release's done and the completion kept", e, d.Complete)
		}
	})

	// A run cut after its intent leaves what it changed unknown: its rerun's stop cannot record
	// unchanged, which AppendStopLeft refuses naming the rule, and hold release is refused over the
	// stop that names what the runs left; hold abort ends the hold.
	t.Run("a rerun after a cut run cannot stop as unchanged, so hold release stays refused", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		for i, x := range []struct {
			w       holdrecord.Workflow
			command string
		}{{holdrecord.WorkflowCutover, "replay"}, {holdrecord.WorkflowBucketsCreate, "buckets create"}, {holdrecord.WorkflowStreamsUpdate, "streams update"}} {
			holdID := "h-cut-" + strconv.Itoa(i)
			hold(t, c, holdID, x.w, nil)
			if _, err := c.Admit(ctx, holdID, cmd(x.command), nil); err != nil {
				t.Fatal(err)
			}
			// The first run is cut after its intent: no entry follows it. Its rerun is admitted.
			if _, err := c.Admit(ctx, holdID, cmd(x.command), nil); err != nil {
				t.Fatalf("%s: the rerun after the cut run: %v", x.command, err)
			}
			err := c.AppendStopLeft(ctx, holdID, x.command, "its check refuses", holdrecord.LeftUnchanged)
			if err == nil || !strings.Contains(err.Error(), "ended with no entry") {
				t.Fatalf("%s: the rerun's unchanged stop after a cut run answered %v, want a refusal naming the run that ended with no entry", x.command, err)
			}
			const left = "unknown: an earlier run ended with no entry"
			if err := c.AppendStopLeft(ctx, holdID, x.command, "its check refuses", left); err != nil {
				t.Fatalf("%s: the rerun's stop naming what the runs left: %v", x.command, err)
			}
			_, err = c.Admit(ctx, holdID, cmd(holdrecord.CommandRelease), nil)
			refused(t, x.command+": release after the cut run's rerun stopped", err, holdrecord.ErrOutOfOrder, "not complete")
			if _, err := c.Admit(ctx, holdID, cmd(holdrecord.CommandAbort), nil); err != nil {
				t.Fatalf("%s: abort after the rerun's stop: %v", x.command, err)
			}
			current := row(t, c)
			if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, holdID, "aborted"); err != nil {
				t.Fatal(err)
			}
			if err := c.AppendDone(ctx, holdID, holdrecord.CommandAbort, nil); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Not only the buckets commands: every command whose own check can refuse after its intent
	// stops, recording what it left, and hold abort then ends the hold.
	t.Run("a streams update that fails for good stops, and hold abort ends the hold", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-update", holdrecord.WorkflowStreamsUpdate, nil)
		if _, err := c.Admit(ctx, "h-update", cmd("streams update"), nil); err != nil {
			t.Fatal(err)
		}
		const left = "changed: ENVOY_NOTIFICATIONS replicas 3, KV_envoy_roles not updated"
		if err := c.AppendStopLeft(ctx, "h-update", "streams update", "KV_envoy_roles differs otherwise", left); err != nil {
			t.Fatalf("the update's stop: %v", err)
		}
		if _, err := c.Admit(ctx, "h-update", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort after the update's stop: %v", err)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-update", "aborted"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-update", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-update"); !d.Complete {
			t.Fatal("hold abort's done entry did not complete the record")
		}
	})

	t.Run("hold release follows a stop that left nothing changed", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-unchanged", holdrecord.WorkflowBucketsCreate, nil)
		if _, err := c.Admit(ctx, "h-unchanged", cmd("buckets create"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStopLeft(ctx, "h-unchanged", "buckets create", "a stop that names nothing it left", ""); err == nil {
			t.Fatal("AppendStopLeft with an empty left answered no error")
		}
		if err := c.AppendStopLeft(ctx, "h-unchanged", "buckets create", "the bucket's config is not the build's",
			holdrecord.LeftUnchanged); err != nil {
			t.Fatalf("the create's stop: %v", err)
		}
		release(t, c, "h-unchanged")
		if after := row(t, c); after.Phase != natsgrant.OperatorPhaseReleased {
			t.Fatalf("after hold release the row is %s", after.Phase)
		}
	})

	// Outside hub-restore hold abort is the way out of a failed command, not a way to end a
	// workflow that is going well: with no command's latest entry a stop it is refused.
	t.Run("outside hub-restore hold abort is refused until a command's latest entry is a stop", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-nostop", holdrecord.WorkflowCutover, nil)
		abortRefused := func(step string) {
			t.Helper()
			writes := r.RecordWrites()
			_, err := c.Admit(ctx, "h-nostop", cmd(holdrecord.CommandAbort), nil)
			refused(t, "hold abort "+step, err, holdrecord.ErrOutOfOrder, "no command has stopped")
			if r.RecordWrites() != writes {
				t.Fatalf("the abort refused %s wrote %d records", step, r.RecordWrites()-writes)
			}
		}
		abortRefused("with hold set's done entry alone")
		run(t, c, "h-nostop", cmd("create-bucket"), nil, nil)
		abortRefused("after create-bucket's done entry")
		// convert stops, replay gives K a start, and convert's rerun completes: convert's latest
		// entry is then its done, so no command has stopped, and abort is refused again.
		if _, err := c.Admit(ctx, "h-nostop", cmd("convert"), nil); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendStop(ctx, "h-nostop", "convert", "no safe start for K"); err != nil {
			t.Fatal(err)
		}
		d, _ := journal(t, c, "h-nostop")
		if err := holdrecord.Decide(d, cmd(holdrecord.CommandAbort)); err != nil {
			t.Fatalf("hold abort after convert's stop answered %v, want admitted", err)
		}
		run(t, c, "h-nostop", cmd("replay"), map[string]string{"key": "K"}, nil)
		run(t, c, "h-nostop", cmd("convert"), nil, nil)
		abortRefused("after convert's rerun completed it")
	})

	// The model's finding: an abort whose own intent a crash left pending must be admitted again,
	// though the stop it followed has been superseded since, or the hold can never end.
	t.Run("a crashed hold abort runs again after the stop it followed is superseded", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		prepare(t, c, "h-abortrerun", holdrecord.WorkflowCutover, nil)
		if err := c.AppendStop(ctx, "h-abortrerun", holdrecord.CommandSet, "a step of 2 to 6 failed"); err != nil {
			t.Fatal(err)
		}
		// The first abort stops at its row move (the row was restored since its read)...
		if _, err := c.Admit(ctx, "h-abortrerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort after hold set's stop: %v", err)
		}
		if err := c.AppendStop(ctx, "h-abortrerun", holdrecord.CommandAbort, "409 gen_mismatch at the row move"); err != nil {
			t.Fatal(err)
		}
		// ...hold set runs again and finishes, so hold set's latest entry is a done...
		run(t, c, "h-abortrerun", cmd(holdrecord.CommandSet), nil, nil)
		// ...the second abort, admitted on its own stop, crashes after its intent...
		if _, err := c.Admit(ctx, "h-abortrerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort after its own stop: %v", err)
		}
		// ...and its rerun, with no command's latest entry a stop, is admitted and ends the hold.
		if _, err := c.Admit(ctx, "h-abortrerun", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("the crashed abort's rerun: %v", err)
		}
		current := row(t, c)
		if _, err := c.MoveRow(ctx, current.Gen, natsgrant.OperatorPhaseReleased, "h-abortrerun", "aborted"); err != nil {
			t.Fatal(err)
		}
		if err := c.AppendDone(ctx, "h-abortrerun", holdrecord.CommandAbort, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := journal(t, c, "h-abortrerun"); !d.Complete {
			t.Fatal("the rerun's done entry did not complete the record")
		}
	})

	t.Run("hold abort is refused while a command's intent is pending, and admitted in any workflow once it stops", func(t *testing.T) {
		r := newRig(t)
		c := r.client(t)
		hold(t, c, "h-abortbusy", holdrecord.WorkflowCutover, nil)
		run(t, c, "h-abortbusy", cmd("create-bucket"), nil, nil)
		if _, err := c.Admit(ctx, "h-abortbusy", cmd("convert"), nil); err != nil {
			t.Fatal(err)
		}
		writes := r.RecordWrites()
		_, err := c.Admit(ctx, "h-abortbusy", cmd(holdrecord.CommandAbort), nil)
		refused(t, "hold abort with convert's intent pending", err, holdrecord.ErrBusy, "convert")
		if r.RecordWrites() != writes {
			t.Fatalf("the refused abort wrote %d records", r.RecordWrites()-writes)
		}
		if err := c.AppendStop(ctx, "h-abortbusy", "convert", "no safe start for K"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Admit(ctx, "h-abortbusy", cmd(holdrecord.CommandAbort), nil); err != nil {
			t.Fatalf("hold abort in cutover after convert's stop: %v", err)
		}
	})
}
