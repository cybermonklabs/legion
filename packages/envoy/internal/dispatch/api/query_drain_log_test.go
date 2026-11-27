package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// R7 for a drain that meets another write's owed digest. A write's drain runs its fill, then reads
// the mark under the issue's owner lock and the lock of its room; a write of the issue that commits
// between the two leaves a version whose record digest is owed until that write's own fill, and the
// drain refuses to clear the mark over it (docs' errQueryDigestOwed). That is no failure - the room's
// settlement an answer schedules commits beside the answer's own drain - so the drain logs it at
// Info, never at Error, and leaves the mark, which the next drain takes once the digest is filled.
// Here a write marks the issue's queries owed with a value on the spec's requirement, which moves
// the spec's record, and the drain runs while the room's lock is held, so it waits past its fill;
// the holder then leaves the spec's newest version owing its digest, as another write's COMMIT
// does, and lets the drain go on. Not parallel: it reads the default logger's lines.
func TestADrainRefusedOverAnotherWritesOwedDigestLogsNoError(t *testing.T) {
	logs := captureLogs(t)
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	ctx := context.Background()
	awaitDigestsFilled(t, rig.database, rig.spec.id)
	requireNoQueryMark(t, rig.database, "once the issue's create answered", key)
	standing := newestVersionOf(t, rig.database, rig.spec.id)

	rig.stateMarked(t, key, blockValue(rig.block("r1"), "probe", "marked"))

	holder, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of the room's lock: %v", err)
	}
	defer holder.Rollback(ctx)
	var rooms, holderPID int
	if err := holder.QueryRow(ctx, `
		select count(*), pg_backend_pid() from (
			select pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t where t.issue_key = $1
		) held`, key).Scan(&rooms, &holderPID); err != nil || rooms != 1 {
		t.Fatalf("hold the lock of the room of %s: %d rooms, %v", key, rooms, err)
	}
	drained := make(chan []model.Event, 1)
	go func() { drained <- rig.documents.DrainOwedQueries(ctx, key) }()
	waiting := blockedStatement(t, rig.database, holderPID, "the drain of "+key+" to wait for the room's lock")
	if !strings.Contains(waiting, "pg_advisory_xact_lock") {
		t.Fatalf("the drain of %s waits at %q, want its lock of the room", key, waiting)
	}
	if owed, err := holder.Exec(ctx, `update version_renders set digest_owed = true where artifact_id = $1::uuid and number = $2`,
		rig.spec.id, standing); err != nil || owed.RowsAffected() != 1 {
		t.Fatalf("leave version %d of the spec owing its record digest: %d rows, %v", standing, owed.RowsAffected(), err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the drain go on: %v", err)
	}
	if appended := testwait.Receive(t, "the drain of "+key, drained); len(appended) != 0 {
		t.Errorf("the refused drain of %s appended %d events, want none", key, len(appended))
	}

	if _, held := readQueryMarkRow(t, rig.database, key); !held {
		t.Errorf("the refused drain of %s cleared its query mark", key)
	}
	logged := logs.Lines("drain the queries an issue owes")
	if !strings.Contains(logged, "level=INFO") || !strings.Contains(logged, "issue="+key) || strings.Contains(logged, "level=ERROR") {
		t.Errorf("the drain of %s refused over an owed digest logged %q, want one line at INFO and none at ERROR", key, logged)
	}
	rig.documents.DrainOwedQueries(ctx, key)
	requireNoQueryMark(t, rig.database, "once the drain ran again", key)
	if newest := newestVersionOf(t, rig.database, rig.spec.id); newest != standing+1 {
		t.Errorf("the drain run again left version %d of the spec newest, want %d", newest, standing+1)
	}
}
