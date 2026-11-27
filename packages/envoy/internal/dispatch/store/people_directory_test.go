package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// directoryRow is what people holds of the Directory confirmation for one row, as the table
// stores it.
type directoryRow struct {
	confirmedAt, checkedAt, notHeldUntil, heldSince *time.Time
	userID                                          *string
}

// stamp is a stored time as a message prints it: null, or the instant.
func stamp(at *time.Time) string {
	if at == nil {
		return "null"
	}
	return at.Format(time.RFC3339Nano)
}

// String names each column of the row with what it holds.
func (row directoryRow) String() string {
	userID := "null"
	if row.userID != nil {
		userID = strconv.Quote(*row.userID)
	}
	return fmt.Sprintf("{directory_confirmed_at %s, directory_checked_at %s, not_held_until %s, held_since %s, directory_user_id %s}",
		stamp(row.confirmedAt), stamp(row.checkedAt), stamp(row.notHeldUntil), stamp(row.heldSince), userID)
}

// directoryMembership is what Membership read of the Directory columns, as a message prints it.
func directoryMembership(membership auth.PersonMembership) string {
	return fmt.Sprintf("{DirectoryHeld %t, DirectoryConfirmedAt %s, NotHeldUntil %s, DirectoryUserID %q}",
		membership.DirectoryHeld, stamp(membership.DirectoryConfirmedAt), stamp(membership.NotHeldUntil), membership.DirectoryUserID)
}

// readDirectoryRow reads email's row as directoryRow.
func readDirectoryRow(t *testing.T, database *Store, email string) directoryRow {
	t.Helper()
	var row directoryRow
	if err := database.Pool.QueryRow(context.Background(), `
		select directory_confirmed_at, directory_checked_at, not_held_until, held_since, directory_user_id
		from people where email = $1
	`, email).Scan(&row.confirmedAt, &row.checkedAt, &row.notHeldUntil, &row.heldSince, &row.userID); err != nil {
		t.Fatalf("read the Directory columns of %s: %v", email, err)
	}
	return row
}

// rowLeavingOut is email's whole people row as JSON, the named columns left out, so a write that
// may move only those columns is shown to move nothing else.
func rowLeavingOut(t *testing.T, database *Store, email string, columns ...string) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(context.Background(),
		`select (to_jsonb(p) - $2::text[])::text from people p where p.email = $1`, email, columns).Scan(&row); err != nil {
		t.Fatalf("read the people row of %s: %v", email, err)
	}
	return row
}

// sameInstant reports whether a stored time is set and is want.
func sameInstant(got *time.Time, want time.Time) bool {
	return got != nil && got.Equal(want)
}

// TestPgPeopleStoreRecordsEachDirectoryOutcome (c-iap-directory, K10): each Directory outcome's
// write, and Membership's read of what they store, on a full member's row with no refresh token,
// as IAP keeps none.
//
// An Active outcome sets directory_confirmed_at and directory_checked_at to one time, fixes
// not_held_until 24 hours after it, answers that deadline and clears held_since; the first one
// stores the Directory user's id, and a later one keeps it. A Held outcome sets
// directory_checked_at and records a hold at one time, and a second moves both to its own time, so
// held_since is the latest Held outcome's; it leaves the confirmation and the deadline as they
// are. A Failed outcome moves directory_checked_at and
// nothing else. RecordDirectoryRebound replaces a stored id with the new account's, confirms with a
// new deadline and releases a hold, and moves no other column. No outcome writes a row for a login
// with none. Membership reads DirectoryHeld true for a hold on a row with no confirmation and for a
// hold later than the confirmation, and false after a later Active or a rebind.
//
// Controls (verify step): a Failed write that also sets directory_confirmed_at fails the Failed
// outcome's check that it moved nothing but directory_checked_at; a rebind that keeps the stored id
// (coalesce, as RecordDirectoryActive writes it) fails the rebind's check of the new id.
func TestPgPeopleStoreRecordsEachDirectoryOutcome(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const ada, bob, nobody = "ada@example.com", "bob@example.com", "nobody@example.com"
	for _, email := range []string{ada, bob} {
		if err := people.Record(ctx, email); err != nil {
			t.Fatal(err)
		}
	}

	// No outcome writes a row for a login with none.
	if until, err := people.RecordDirectoryActive(ctx, nobody, "directory-nobody", databaseNow(t, database)); err != nil || !until.IsZero() {
		t.Fatalf("an Active outcome for a login with no row answered %s, %v; want the zero time and no error", until, err)
	}
	if err := people.RecordDirectoryHeld(ctx, nobody); err != nil {
		t.Fatalf("a Held outcome for a login with no row: %v", err)
	}
	if err := people.RecordDirectoryFailed(ctx, nobody); err != nil {
		t.Fatalf("a Failed outcome for a login with no row: %v", err)
	}
	if until, err := people.RecordDirectoryRebound(ctx, nobody, "directory-nobody", databaseNow(t, database)); err != nil || !until.IsZero() {
		t.Fatalf("a rebind for a login with no row answered %s, %v; want the zero time and no error", until, err)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from people where email = $1`, nobody).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("people holds %d rows for %s (%v); want none: no outcome makes a row", rows, nobody, err)
	}
	if _, found, err := people.Membership(ctx, nobody); err != nil || found {
		t.Fatalf("Membership of %s answered found %t, %v; want not found", nobody, found, err)
	}

	// A first-ever Held outcome records a hold on a row with no confirmation.
	before := databaseNow(t, database)
	if err := people.RecordDirectoryHeld(ctx, ada); err != nil {
		t.Fatal(err)
	}
	after := databaseNow(t, database)
	firstHeld := readDirectoryRow(t, database, ada)
	if firstHeld.heldSince == nil || firstHeld.heldSince.Before(before) || firstHeld.heldSince.After(after) ||
		!sameInstant(firstHeld.checkedAt, *firstHeld.heldSince) {
		t.Fatalf("after a first Held outcome the row reads %+v; want held_since and directory_checked_at one time between %s and %s", firstHeld, before, after)
	}
	if firstHeld.confirmedAt != nil || firstHeld.notHeldUntil != nil || firstHeld.userID != nil {
		t.Fatalf("a Held outcome on a row never confirmed wrote a confirmation, a deadline or an id: %+v", firstHeld)
	}
	membership, found, err := people.Membership(ctx, ada)
	if err != nil || !found {
		t.Fatalf("Membership of %s: found %t, %v", ada, found, err)
	}
	if !membership.DirectoryHeld || membership.DirectoryConfirmedAt != nil || membership.NotHeldUntil != nil || membership.DirectoryUserID != "" {
		t.Fatalf("Membership after a first Held outcome reads %s; want DirectoryHeld for a hold with no confirmation, and no confirmation, deadline or id", directoryMembership(membership))
	}

	// A second Held outcome moves the hold and the check to its own time, so the guard of a later
	// Active write compares with the latest Held outcome (TestPgPeopleStoreKeepsAHoldRecordedSinceTheActiveRead).
	if err := people.RecordDirectoryHeld(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if again := readDirectoryRow(t, database, ada); again.heldSince == nil || !again.heldSince.After(*firstHeld.heldSince) || !sameInstant(again.checkedAt, *again.heldSince) {
		t.Fatalf("after a second Held outcome the row reads %+v; want held_since and directory_checked_at one time, later than the first Held outcome's %s", again, *firstHeld.heldSince)
	}

	// An Active outcome confirms, fixes the deadline 24 hours later, stores the id and releases.
	before = databaseNow(t, database)
	until, err := people.RecordDirectoryActive(ctx, ada, "directory-ada", before)
	if err != nil {
		t.Fatal(err)
	}
	after = databaseNow(t, database)
	active := readDirectoryRow(t, database, ada)
	if active.confirmedAt == nil || active.confirmedAt.Before(before) || active.confirmedAt.After(after) || !sameInstant(active.checkedAt, *active.confirmedAt) {
		t.Fatalf("after an Active outcome the row reads %+v; want directory_confirmed_at and directory_checked_at one time between %s and %s", active, before, after)
	}
	if !sameInstant(active.notHeldUntil, active.confirmedAt.Add(24*time.Hour)) || !until.Equal(*active.notHeldUntil) {
		t.Fatalf("after an Active outcome at %s, not_held_until reads %s and the write answered %s; want both 24 hours after the confirmation", *active.confirmedAt, stamp(active.notHeldUntil), until)
	}
	if active.heldSince != nil || active.userID == nil || *active.userID != "directory-ada" {
		t.Fatalf("after the first Active outcome the row reads %+v; want held_since cleared and directory_user_id directory-ada", active)
	}
	membership, _, err = people.Membership(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	if membership.DirectoryHeld || !sameInstant(membership.DirectoryConfirmedAt, *active.confirmedAt) ||
		!sameInstant(membership.NotHeldUntil, *active.notHeldUntil) || membership.DirectoryUserID != "directory-ada" {
		t.Fatalf("Membership after an Active outcome reads %s; want not DirectoryHeld, with the stored confirmation %s, deadline %s and id", directoryMembership(membership), *active.confirmedAt, *active.notHeldUntil)
	}

	// A Held outcome after it records a hold later than the confirmation, and leaves the
	// confirmation and the deadline as they are.
	if err := people.RecordDirectoryHeld(ctx, ada); err != nil {
		t.Fatal(err)
	}
	heldAfter := readDirectoryRow(t, database, ada)
	if heldAfter.heldSince == nil || !heldAfter.heldSince.After(*active.confirmedAt) ||
		!sameInstant(heldAfter.confirmedAt, *active.confirmedAt) || !sameInstant(heldAfter.notHeldUntil, *active.notHeldUntil) {
		t.Fatalf("after a Held outcome that follows the Active one the row reads %+v; want held_since after %s and the confirmation and deadline unchanged", heldAfter, *active.confirmedAt)
	}
	membership, _, err = people.Membership(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	if !membership.DirectoryHeld {
		t.Fatalf("Membership reads %s after a hold later than the confirmation; want DirectoryHeld", directoryMembership(membership))
	}

	// A Failed outcome moves directory_checked_at and nothing else, on a held row and on a row
	// never checked.
	for _, email := range []string{ada, bob} {
		rest := rowLeavingOut(t, database, email, "directory_checked_at")
		checked := readDirectoryRow(t, database, email).checkedAt
		if err := people.RecordDirectoryFailed(ctx, email); err != nil {
			t.Fatal(err)
		}
		failed := readDirectoryRow(t, database, email)
		if failed.checkedAt == nil || (checked != nil && !failed.checkedAt.After(*checked)) {
			t.Fatalf("a Failed outcome left directory_checked_at of %s at %s (was %s); want it moved to the check", email, stamp(failed.checkedAt), stamp(checked))
		}
		if got := rowLeavingOut(t, database, email, "directory_checked_at"); got != rest {
			t.Fatalf("a Failed outcome changed more of %s's row than directory_checked_at:\nbefore %s\nafter  %s", email, rest, got)
		}
	}
	if membership, _, err = people.Membership(ctx, ada); err != nil || !membership.DirectoryHeld {
		t.Fatalf("Membership reads %s, %v after a Failed outcome on a held row; want it still DirectoryHeld", directoryMembership(membership), err)
	}

	// A later Active outcome keeps the stored id, moves the confirmation and the deadline, and
	// releases the hold.
	until, err = people.RecordDirectoryActive(ctx, ada, "directory-other", databaseNow(t, database))
	if err != nil {
		t.Fatal(err)
	}
	later := readDirectoryRow(t, database, ada)
	if later.userID == nil || *later.userID != "directory-ada" {
		t.Fatalf("a later Active outcome answered by directory-other left the row at %v; want the stored directory-ada kept", later)
	}
	if later.confirmedAt == nil || !later.confirmedAt.After(*active.confirmedAt) || !sameInstant(later.notHeldUntil, later.confirmedAt.Add(24*time.Hour)) ||
		!until.Equal(*later.notHeldUntil) || later.heldSince != nil {
		t.Fatalf("after a later Active outcome the row reads %+v and the write answered %s; want a later confirmation, its deadline 24 hours on, and no hold", later, until)
	}
	if membership, _, err = people.Membership(ctx, ada); err != nil || membership.DirectoryHeld {
		t.Fatalf("Membership reads %s, %v after a later Active outcome; want not DirectoryHeld", directoryMembership(membership), err)
	}

	// A Held outcome, as a recreated address that still holds the earlier person's access answers,
	// then a rebind once it holds none: the new id replaces the stored one, the confirmation and the
	// deadline move, the hold is released, and nothing else of the row moves.
	if err := people.RecordDirectoryHeld(ctx, ada); err != nil {
		t.Fatal(err)
	}
	rest := rowLeavingOut(t, database, ada, "directory_user_id", "directory_confirmed_at", "directory_checked_at", "not_held_until", "held_since")
	before = databaseNow(t, database)
	until, err = people.RecordDirectoryRebound(ctx, ada, "directory-new", before)
	if err != nil {
		t.Fatal(err)
	}
	after = databaseNow(t, database)
	rebound := readDirectoryRow(t, database, ada)
	if rebound.userID == nil || *rebound.userID != "directory-new" {
		t.Fatalf("a rebind answered by directory-new left the row at %v; want directory-new in place of directory-ada", rebound)
	}
	if rebound.confirmedAt == nil || rebound.confirmedAt.Before(before) || rebound.confirmedAt.After(after) || !sameInstant(rebound.checkedAt, *rebound.confirmedAt) ||
		!sameInstant(rebound.notHeldUntil, rebound.confirmedAt.Add(24*time.Hour)) || !until.Equal(*rebound.notHeldUntil) || rebound.heldSince != nil {
		t.Fatalf("after a rebind the row reads %+v and the write answered %s; want a confirmation and check of one time between %s and %s, its deadline 24 hours on, and no hold", rebound, until, before, after)
	}
	if got := rowLeavingOut(t, database, ada, "directory_user_id", "directory_confirmed_at", "directory_checked_at", "not_held_until", "held_since"); got != rest {
		t.Fatalf("a rebind changed more than the Directory columns and the hold:\nbefore %s\nafter  %s", rest, got)
	}
	if membership, _, err = people.Membership(ctx, ada); err != nil || membership.DirectoryHeld || membership.DirectoryUserID != "directory-new" {
		t.Fatalf("Membership reads %s, %v after a rebind; want not DirectoryHeld, with directory-new", directoryMembership(membership), err)
	}
}

// TestPgPeopleStoreKeepsAHoldRecordedSinceTheActiveRead: an Active outcome and a rebind rest on
// the check's read of the row (readAt), made before its Directory read. On a row whose hold was
// recorded after that read, or at its very time, which is the Held of a check whose span overlaps
// this one's, each writes nothing and answers the zero time: the whole row, the hold included,
// stays as it was. That holds too for a row that was already held when the check read it and is
// held again after: the later Held outcome stamps held_since with its own time, so the guard
// compares the read with the latest hold, not the first. The same write resting on a read made
// after the hold releases it.
//
// Controls: drop the held_since guard from either write; its first case fails, naming the row.
// Keep a held row's first held_since on a later Held outcome (coalesce); the re-held cases fail,
// naming the hold the Held outcome after the read did not record.
func TestPgPeopleStoreKeepsAHoldRecordedSinceTheActiveRead(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	active := func(email string, readAt time.Time) (time.Time, error) {
		return people.RecordDirectoryActive(ctx, email, "directory-"+email, readAt)
	}
	rebind := func(email string, readAt time.Time) (time.Time, error) {
		return people.RecordDirectoryRebound(ctx, email, "directory-new-"+email, readAt)
	}
	for _, write := range []struct {
		name, email string
		// heldBefore is whether the row was already held when the check read it.
		heldBefore bool
		record     func(email string, readAt time.Time) (time.Time, error)
	}{
		{"an Active outcome", "active@example.com", false, active},
		{"a rebind", "rebind@example.com", false, rebind},
		{"an Active outcome on a row held before the read and held again", "active-reheld@example.com", true, active},
		{"a rebind on a row held before the read and held again", "rebind-reheld@example.com", true, rebind},
	} {
		t.Run(write.name, func(t *testing.T) {
			email := write.email
			if err := people.Record(ctx, email); err != nil {
				t.Fatal(err)
			}
			if _, err := people.RecordDirectoryActive(ctx, email, "directory-"+email, databaseNow(t, database)); err != nil {
				t.Fatal(err)
			}
			if write.heldBefore {
				if err := people.RecordDirectoryHeld(ctx, email); err != nil {
					t.Fatal(err)
				}
			}
			read := databaseNow(t, database)
			if err := people.RecordDirectoryHeld(ctx, email); err != nil {
				t.Fatal(err)
			}
			held := readDirectoryRow(t, database, email)
			if held.heldSince == nil || !held.heldSince.After(read) {
				t.Errorf("the Held outcome after the read at %s left the row at %v; want a hold recorded after the read", read, held)
			}
			if held.heldSince == nil {
				t.FailNow()
			}
			whole := rowLeavingOut(t, database, email, []string{}...)
			for _, readAt := range []time.Time{read, *held.heldSince} {
				until, err := write.record(email, readAt)
				if err != nil || !until.IsZero() {
					t.Fatalf("%s resting on a read at %s, with a hold recorded since %s, answered %s, %v; want the zero time and no error",
						write.name, readAt, *held.heldSince, until, err)
				}
				if got := rowLeavingOut(t, database, email, []string{}...); got != whole {
					t.Fatalf("%s resting on a read at %s wrote to a row held since %s:\nbefore %s\nafter  %s", write.name, readAt, *held.heldSince, whole, got)
				}
			}
			if membership, _, err := people.Membership(ctx, email); err != nil || !membership.DirectoryHeld {
				t.Fatalf("Membership reads %s, %v after %s that rested on an earlier read; want DirectoryHeld", directoryMembership(membership), err, write.name)
			}
			until, err := write.record(email, databaseNow(t, database))
			if released := readDirectoryRow(t, database, email); err != nil || until.IsZero() || released.heldSince != nil {
				t.Fatalf("%s resting on a read after the hold answered %s, %v and left the row at %v; want its deadline, and the hold released", write.name, until, err, released)
			}
		})
	}
}

// TestPgPeopleStoreRecordsAHeldOutcomeOverAnOverlappingActiveOne: two checks of one person whose
// spans overlap, each reading the row before the other writes, cannot be ordered by their
// Directory reads, and the Held outcome stands whichever writes last. Here the Held check reads the
// row, the Active check reads it after and writes its Active outcome, and the Held write lands
// last: it records a hold later than that confirmation, which Membership reads as DirectoryHeld,
// and keeps the confirmation and the deadline the Active write set. The other order, the Held
// write first, is TestPgPeopleStoreKeepsAHoldRecordedSinceTheActiveRead's. An overlap so fails
// closed: a person the Directory restored during it stays held until a later check answers Active,
// and a leaver it suspended during it is never served on an Active answer read before the
// suspension.
//
// Control: a Held write that yields to an Active outcome confirmed since its check's read, as the
// Active write yields to a hold, fails this row, naming the row the Held write left unheld.
func TestPgPeopleStoreRecordsAHeldOutcomeOverAnOverlappingActiveOne(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	const ada = "ada@example.com"
	if err := people.Record(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := people.RecordDirectoryActive(ctx, ada, "directory-ada", databaseNow(t, database)); err != nil {
		t.Fatal(err)
	}
	heldRead := databaseNow(t, database)
	until, err := people.RecordDirectoryActive(ctx, ada, "directory-ada", databaseNow(t, database))
	if err != nil || until.IsZero() {
		t.Fatalf("the Active outcome of the check that read the row after the Held check answered %s, %v; want its deadline", until, err)
	}
	active := readDirectoryRow(t, database, ada)
	if active.confirmedAt == nil || !active.confirmedAt.After(heldRead) || active.notHeldUntil == nil {
		t.Fatalf("the Active outcome left the row at %v; want a confirmation and deadline later than the Held check's read at %s", active, heldRead)
	}
	if err := people.RecordDirectoryHeld(ctx, ada); err != nil {
		t.Fatal(err)
	}
	held := readDirectoryRow(t, database, ada)
	if held.heldSince == nil || !held.heldSince.After(*active.confirmedAt) ||
		!sameInstant(held.confirmedAt, *active.confirmedAt) || !sameInstant(held.notHeldUntil, *active.notHeldUntil) {
		t.Fatalf("the Held write of a check that read the row at %s, landing after an Active outcome confirmed at %s, left the row at %v; want a hold later than that confirmation, and the confirmation and its deadline kept",
			heldRead, *active.confirmedAt, held)
	}
	if membership, _, err := people.Membership(ctx, ada); err != nil || !membership.DirectoryHeld {
		t.Fatalf("Membership reads %s, %v after the overlapping Held write landed last; want DirectoryHeld", directoryMembership(membership), err)
	}
}

// TestPgPeopleStoreClaimsDueDirectoryRows (c-iap-directory, K10): ClaimDirectoryDue claims each full
// member's row whose later of directory_confirmed_at and directory_checked_at is older than age, or
// null, once, the row due longest first: a held row, an idle row, a row with no refresh token and
// a row never checked. It skips a row checked within age, though its confirmation is older, a row
// confirmed within age, and a restricted member's row never checked. Each claim sets
// directory_checked_at and nothing else, so a held row stays held. Two concurrent claims of one due
// row give it to one.
//
// Control (verify step): a claim that skips held rows fails the claim order.
func TestPgPeopleStoreClaimsDueDirectoryRows(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := NewPgPeopleStore(database.Pool, testSigningKey, nil)
	age := func(email, by string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			update people set directory_confirmed_at = directory_confirmed_at - $2::interval,
				directory_checked_at = directory_checked_at - $2::interval,
				not_held_until = not_held_until - $2::interval,
				held_since = held_since - $2::interval
			where email = $1
		`, email, by); err != nil {
			t.Fatalf("age the Directory columns of %s by %s: %v", email, by, err)
		}
	}
	record := func(email string) {
		t.Helper()
		if err := people.Record(ctx, email); err != nil {
			t.Fatal(err)
		}
	}
	active := func(email string) {
		t.Helper()
		if _, err := people.RecordDirectoryActive(ctx, email, "directory-"+email, databaseNow(t, database)); err != nil {
			t.Fatal(err)
		}
	}
	const (
		held       = "held@example.com"
		idle       = "idle@example.com"
		tokenless  = "tokenless@example.com"
		never      = "never@example.com"
		fresh      = "fresh@example.com"
		checked    = "checked@example.com"
		restricted = "restricted@example.com"
	)
	record(held)
	if err := people.RecordDirectoryHeld(ctx, held); err != nil {
		t.Fatal(err)
	}
	age(held, "150 minutes")
	if err := people.SignInSubject(ctx, idle, "g-idle", "refresh-idle", "", model.MemberFull); err != nil {
		t.Fatal(err)
	}
	active(idle)
	age(idle, "2 hours")
	record(tokenless)
	active(tokenless)
	age(tokenless, "3 hours")
	record(never)
	record(fresh)
	active(fresh)
	record(checked)
	active(checked)
	age(checked, "2 hours")
	if err := people.RecordDirectoryFailed(ctx, checked); err != nil {
		t.Fatal(err)
	}
	record(restricted)
	if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, restricted); err != nil {
		t.Fatal(err)
	}

	rest := map[string]string{}
	for _, email := range []string{held, idle, tokenless, never} {
		rest[email] = rowLeavingOut(t, database, email, "directory_checked_at")
	}
	var order []string
	for {
		before := databaseNow(t, database)
		email, ok, err := people.ClaimDirectoryDue(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if len(order) > 4 {
			t.Fatalf("claims went on past the four due rows: %v, then %s", order, email)
		}
		if row := readDirectoryRow(t, database, email); row.checkedAt == nil || row.checkedAt.Before(before) {
			t.Fatalf("the claim of %s left directory_checked_at at %s; want it set at the claim, after %s", email, stamp(row.checkedAt), before)
		}
		if want, ok := rest[email]; ok {
			if got := rowLeavingOut(t, database, email, "directory_checked_at"); got != want {
				t.Fatalf("the claim of %s changed more than directory_checked_at:\nbefore %s\nafter  %s", email, want, got)
			}
		}
		order = append(order, email)
	}
	if want := []string{never, tokenless, held, idle}; !slices.Equal(order, want) {
		t.Fatalf("claims = %v, want %v: the row never checked, then each due row by its last check, the oldest first; never %s, %s or %s",
			order, want, fresh, checked, restricted)
	}
	if membership, _, err := people.Membership(ctx, held); err != nil || !membership.DirectoryHeld {
		t.Fatalf("Membership of the claimed held row reads %s, %v; want it still DirectoryHeld", directoryMembership(membership), err)
	}
	if membership, _, err := people.Membership(ctx, idle); err != nil || membership.RefreshToken == "" || membership.DirectoryConfirmedAt == nil || membership.NotHeldUntil == nil {
		t.Fatalf("Membership of the idle row, which holds a refresh token, reads %s with a token %t, %v; want its token and its Directory confirmation and deadline",
			directoryMembership(membership), membership.RefreshToken != "", err)
	}

	// A row is claimed again once its last check is older than age, and not before.
	if _, err := database.Pool.Exec(ctx, `update people set directory_checked_at = now() - interval '59 minutes' where email = $1`, tokenless); err != nil {
		t.Fatal(err)
	}
	if email, ok, err := people.ClaimDirectoryDue(ctx, time.Hour); err != nil || ok {
		t.Fatalf("a claim while the last check is 59 minutes old: %q, %t, %v; want none", email, ok, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set directory_checked_at = now() - interval '61 minutes' where email = $1`, tokenless); err != nil {
		t.Fatal(err)
	}
	if email, ok, err := people.ClaimDirectoryDue(ctx, time.Hour); err != nil || !ok || email != tokenless {
		t.Fatalf("a claim once the last check is 61 minutes old: %q, %t, %v; want %s again", email, ok, err, tokenless)
	}

	// Two concurrent claims of one due row give it to one, in every round.
	type claim struct {
		email string
		ok    bool
		err   error
	}
	for round := range 25 {
		if _, err := database.Pool.Exec(ctx, `update people set directory_checked_at = now() - interval '2 hours' where email = $1`, never); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		claims := make([]claim, 2)
		var wg sync.WaitGroup
		for i := range claims {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				claims[i].email, claims[i].ok, claims[i].err = people.ClaimDirectoryDue(ctx, time.Hour)
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for _, c := range claims {
			if c.err != nil {
				t.Fatalf("round %d: a concurrent claim: %v", round, c.err)
			}
			if c.ok {
				if c.email != never {
					t.Fatalf("round %d: a concurrent claim took %s; want only %s due", round, c.email, never)
				}
				won++
			}
		}
		if won != 1 {
			t.Fatalf("round %d: two concurrent claims of one due row answered %+v; want it given to exactly one", round, claims)
		}
	}
}
