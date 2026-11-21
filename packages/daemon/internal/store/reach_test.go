package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reopened is a second store over the database st reads, as a daemon restarted onto the same
// database opens one: whatever it reads back was in the database, not in st's memory.
func reopened(t *testing.T, st *Store) *Store {
	t.Helper()
	second, err := Open(context.Background(), st.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("open a second store over the same database: %v", err)
	}
	t.Cleanup(second.Close)
	return second
}

// The daemon's record of whether Envoy can reach a claim's session survives a restart, because
// three of its facts are needed exactly then (R5): a claim reported before a boot is not reported
// again after it, a second relaunch within 6 hours is refused, and the daemon relaunches its own
// controller at most once in 6 hours. No write a machine makes carries it, a launch's included,
// since a relaunch keeps the session the record speaks of.
func TestTheReachRecordSurvivesARestartAndNoClaimWriteCarriesIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	at := time.Date(2026, 11, 12, 15, 51, 1, 0, time.UTC)
	want := supervise.Reach{
		Session: "ses_implementer", FirstReady: at.Add(-time.Hour), Since: at, LastSeen: at,
		Generation: c.Generation, Reported: true, RelaunchedAt: at.Add(25 * time.Minute), Relaunched: true,
	}
	if known, err := st.PutClaimReach(ctx, c.Token, want); err != nil || !known {
		t.Fatalf("PutClaimReach = %t, %v", known, err)
	}

	// A machine's write of the claim, a launch's that moves the generation among them.
	launched := c
	launched.Generation++
	launched.BootTokenHash = supervise.HashBootToken("the next launch's token")
	if err := st.PutClaim(ctx, launched); err != nil {
		t.Fatalf("put the launch: %v", err)
	}

	read, found, err := reopened(t, st).ClaimByBootTokenHash(ctx, launched.BootTokenHash)
	if err != nil || !found {
		t.Fatalf("read the claim back after a restart: found %t, %v", found, err)
	}
	if read.Reach != want {
		t.Fatalf("reach read back after a launch's write and a restart = %+v, want %+v", read.Reach, want)
	}

	if known, err := st.PutClaimReach(ctx, "legion-LEGION-999-implementer", want); err != nil || known {
		t.Fatalf("PutClaimReach of a claim the store holds none of = %t, %v, want unknown", known, err)
	}
}

// A claim nothing has been read of carries the empty record, which reads as no mark at all.
func TestAClaimNothingHasBeenReadOfCarriesNoReach(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	claims, err := st.Claims(ctx)
	if err != nil || len(claims) != 1 {
		t.Fatalf("Claims = %d claims, %v", len(claims), err)
	}
	if claims[0].Reach != (supervise.Reach{}) || claims[0].Reach.Unreachable() {
		t.Fatalf("reach of a new claim = %+v, want the empty record", claims[0].Reach)
	}
}

// A claim's last relaunch is written by the claim's own write — its launch's at the latest — onto
// the claim's own row (R5): the keeper's write of its record never touches it, and a write of the
// claim that carries an earlier moment, or none, never moves it back, so the 6-hour refusal
// outlives every writer that comes after the relaunch.
func TestAClaimsLastRelaunchIsWrittenWithItsLaunchAndNoLaterWriteMovesItBack(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	at := time.Date(2026, 11, 12, 15, 51, 1, 123456000, time.UTC)
	launched := c
	launched.Generation++
	launched.BootTokenHash = supervise.HashBootToken("the relaunch's token")
	launched.LastRelaunch = at
	if err := st.PutClaim(ctx, launched); err != nil {
		t.Fatalf("put the relaunch's launch: %v", err)
	}
	kept := supervise.Reach{Session: "ses_implementer", Since: at.Add(-time.Hour), Relaunched: true, RelaunchedAt: at}
	if known, err := st.PutClaimReach(ctx, c.Token, kept); err != nil || !known {
		t.Fatalf("PutClaimReach = %t, %v", known, err)
	}
	for _, later := range []time.Time{{}, at.Add(-time.Hour)} {
		written := launched
		written.State = supervise.StateReady
		written.LastRelaunch = later
		if err := st.PutClaim(ctx, written); err != nil {
			t.Fatalf("put the claim with relaunch %v: %v", later, err)
		}
	}

	read, found, err := reopened(t, st).ClaimByBootTokenHash(ctx, launched.BootTokenHash)
	if err != nil || !found {
		t.Fatalf("read the claim back after a restart: found %t, %v", found, err)
	}
	if !read.LastRelaunch.Equal(at) {
		t.Fatalf("the claim's last relaunch read back = %v, want the launch's %v", read.LastRelaunch, at)
	}
	if read.Reach != kept {
		t.Fatalf("reach read back = %+v, want the keeper's record %+v", read.Reach, kept)
	}
}

// The generation a relaunch's launch left the claim at goes with that relaunch's moment: the
// relaunch's write after its launch records it, a write of a later relaunch starts it again as
// not recorded, and a write that carries an earlier relaunch, or none, leaves it as it stands.
func TestTheRelaunchGenerationGoesWithItsRelaunch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	first := time.Date(2026, 11, 12, 15, 51, 1, 0, time.UTC)
	read := func() supervise.Claim {
		t.Helper()
		got, found, err := reopened(t, st).ClaimByBootTokenHash(ctx, c.BootTokenHash)
		if err != nil || !found {
			t.Fatalf("read the claim back: found %t, %v", found, err)
		}
		return got
	}
	write := func(at time.Time, generation uint64) {
		t.Helper()
		c.LastRelaunch, c.LastRelaunchGeneration = at, generation
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("put the claim with relaunch %v at generation %d: %v", at, generation, err)
		}
	}

	write(first, 0)
	write(first, 4)
	if got := read(); !got.LastRelaunch.Equal(first) || got.LastRelaunchGeneration != 4 {
		t.Fatalf("after the relaunch's launch the row reads %v at generation %d, want %v at 4", got.LastRelaunch, got.LastRelaunchGeneration, first)
	}
	write(time.Time{}, 0)
	write(first.Add(-time.Hour), 9)
	if got := read(); !got.LastRelaunch.Equal(first) || got.LastRelaunchGeneration != 4 {
		t.Fatalf("after writes of no relaunch and an earlier one the row reads %v at generation %d, want %v at 4", got.LastRelaunch, got.LastRelaunchGeneration, first)
	}
	second := first.Add(7 * time.Hour)
	write(second, 0)
	if got := read(); !got.LastRelaunch.Equal(second) || got.LastRelaunchGeneration != 0 {
		t.Fatalf("after a later relaunch's first write the row reads %v at generation %d, want %v not recorded", got.LastRelaunch, got.LastRelaunchGeneration, second)
	}
}

// A reach record written in the transaction of a notice that does not commit is not written: a
// notice and the mark that says it was told commit together or not at all.
func TestAReachRecordWrittenInATransactionThatDoesNotCommitIsNotWritten(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	marked := supervise.Reach{Session: "ses_implementer", Since: time.Date(2026, 11, 12, 15, 51, 1, 0, time.UTC), Reported: true}
	refused := errors.New("the notice's own write was refused")
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		if known, err := PutClaimReachTx(ctx, tx, c.Token, marked); err != nil || !known {
			return fmt.Errorf("PutClaimReachTx = %t, %w", known, err)
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("the transaction = %v, want the notice's refusal", err)
	}
	read, _, err := st.ClaimByBootTokenHash(ctx, c.BootTokenHash)
	if err != nil || read.Reach != (supervise.Reach{}) {
		t.Fatalf("reach after a transaction that did not commit = %+v, %v, want none", read.Reach, err)
	}

	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		_, err := PutClaimReachTx(ctx, tx, c.Token, marked)
		return err
	}); err != nil {
		t.Fatalf("the transaction that commits: %v", err)
	}
	if read, _, err := st.ClaimByBootTokenHash(ctx, c.BootTokenHash); err != nil || read.Reach != marked {
		t.Fatalf("reach after a transaction that committed = %+v, %v, want %+v", read.Reach, err, marked)
	}
}

// A launch keeps the boot token of the launch it replaces, with that launch's generation, so a
// shim of the older generation whose hello reaches a daemon restarted since is known for a launch
// the claim has left (R3), where the claim row alone knows only its current launch. Every previous
// launch is kept, however many there were: R3 grants no launch an exception, and a shim isolated
// through any number of later launches is still told to stop.
func TestALaunchKeepsTheBootTokenOfTheLaunchItReplacesAcrossARestart(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.Generation = 1
	c.BootTokenHash = supervise.HashBootToken("generation 1")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	// A write that does not move the generation keeps nothing.
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim again: %v", err)
	}
	for generation := uint64(2); generation <= 11; generation++ {
		c.Generation = generation
		c.BootTokenHash = supervise.HashBootToken(fmt.Sprintf("generation %d", generation))
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("put launch %d: %v", generation, err)
		}
		if generation == 2 {
			second := reopened(t, st)
			read, launched, found, err := second.ClaimByPriorBootTokenHash(ctx, supervise.HashBootToken("generation 1"))
			if err != nil || !found {
				t.Fatalf("the first launch's token after a restart: found %t, %v", found, err)
			}
			if read.Token != c.Token || launched != 1 || read.Generation != 2 {
				t.Fatalf("the first launch's token resolves to %s at generation %d (claim at %d), want %s at 1 with the claim at 2",
					read.Token, launched, read.Generation, c.Token)
			}
			if _, _, found, err := second.ClaimByPriorBootTokenHash(ctx, c.BootTokenHash); err != nil || found {
				t.Fatalf("the current launch's token resolved as a previous one: found %t, %v", found, err)
			}
		}
	}
	var kept int
	if err := st.Pool().QueryRow(ctx, `select count(*) from claim_boot_tokens where claim_token = $1`, string(c.Token)).Scan(&kept); err != nil {
		t.Fatalf("count the kept tokens: %v", err)
	}
	if kept != 10 {
		t.Fatalf("kept %d previous boot tokens after ten launches, want all 10", kept)
	}
	read, launched, found, err := reopened(t, st).ClaimByPriorBootTokenHash(ctx, supervise.HashBootToken("generation 1"))
	if err != nil || !found || read.Token != c.Token || launched != 1 {
		t.Fatalf("the first launch's token after ten launches and a restart: %s at generation %d, found %t, %v; want %s at 1",
			read.Token, launched, found, err, c.Token)
	}
	if _, _, found, err := st.ClaimByPriorBootTokenHash(ctx, supervise.HashBootToken("a token no launch minted")); err != nil || found {
		t.Fatalf("a token no launch minted resolved: found %t, %v", found, err)
	}
}

// Every writer of a claim's launch keeps the hash it replaces, whatever binary it is: a daemon of
// an earlier release, which a rollback runs, writes a launch with statements of its own and none of
// this release's code (R3). The database keeps it, so a roll forward still knows the shim of every
// launch that release made. The write below is such a statement: the claim's own columns, and
// nothing that names the table the hash is kept in.
func TestALaunchWrittenByNoCodeOfThisReleaseStillKeepsTheTokenItReplaces(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.Generation = 4
	c.BootTokenHash = supervise.HashBootToken("generation 4")
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	for generation := 5; generation <= 6; generation++ {
		if _, err := st.Pool().Exec(ctx, `update claims set generation = $2, boot_token_hash = $3, state = 'launching', updated_at = now()
			where token = $1`, string(c.Token), generation, supervise.HashBootToken(fmt.Sprintf("generation %d", generation))); err != nil {
			t.Fatalf("write launch %d as an earlier release would: %v", generation, err)
		}
	}
	for _, generation := range []uint64{4, 5} {
		read, launched, found, err := reopened(t, st).ClaimByPriorBootTokenHash(ctx, supervise.HashBootToken(fmt.Sprintf("generation %d", generation)))
		if err != nil || !found || read.Token != c.Token || launched != generation || read.Generation != 6 {
			t.Fatalf("generation %d's token after launches no code of this release wrote: %s at generation %d (claim at %d), found %t, %v; want %s at %d with the claim at 6",
				generation, read.Token, launched, read.Generation, found, err, c.Token, generation)
		}
	}
	if _, _, found, err := st.ClaimByPriorBootTokenHash(ctx, supervise.HashBootToken("generation 6")); err != nil || found {
		t.Fatalf("the current launch's token resolved as a previous one: found %t, %v", found, err)
	}
	// A write of the row that leaves the hash where it is keeps nothing.
	if _, err := st.Pool().Exec(ctx, `update claims set state = 'ready' where token = $1`, string(c.Token)); err != nil {
		t.Fatalf("write the claim's state: %v", err)
	}
	var kept int
	if err := st.Pool().QueryRow(ctx, `select count(*) from claim_boot_tokens where claim_token = $1`, string(c.Token)).Scan(&kept); err != nil {
		t.Fatalf("count the kept tokens: %v", err)
	}
	if kept != 2 {
		t.Fatalf("kept %d previous boot tokens after two launches, want 2", kept)
	}
}
