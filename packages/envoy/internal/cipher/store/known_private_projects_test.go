package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// knownRow is a row of known_private_projects as the test reads it straight from the table.
type knownRow struct {
	channels  []string
	storedAt  *time.Time
	listed    *int64
	record    *int64
	forgotten *int64
}

// readKnownRow reads key's row, and whether there is one.
func readKnownRow(t *testing.T, st *store.Store, key string) (knownRow, bool) {
	t.Helper()
	var row knownRow
	err := st.Pool.QueryRow(context.Background(), `select slack_channels, channels_stored_at, listed, record, forgotten
		from known_private_projects where project = $1`, key).
		Scan(&row.channels, &row.storedAt, &row.listed, &row.record, &row.forgotten)
	if err != nil {
		return knownRow{}, false
	}
	return row, true
}

// heldKeys is every held row's key, as KnownPrivateProjects answers them.
func heldKeys(t *testing.T, st *store.Store) []string {
	t.Helper()
	known, err := st.KnownPrivateProjects(context.Background())
	if err != nil {
		t.Fatalf("KnownPrivateProjects: %v", err)
	}
	keys := []string{}
	for _, project := range known {
		keys = append(keys, project.Key)
	}
	return keys
}

// heldRow is the held row of key, as KnownPrivateProjects answers it, failing t when key is not held.
func heldRow(t *testing.T, st *store.Store, key string) store.KnownPrivateProject {
	t.Helper()
	known, err := st.KnownPrivateProjects(context.Background())
	if err != nil {
		t.Fatalf("KnownPrivateProjects: %v", err)
	}
	for _, project := range known {
		if project.Key == key {
			return project
		}
	}
	t.Fatalf("%s is not held; held: %v", key, heldKeys(t, st))
	return store.KnownPrivateProject{}
}

// stamp takes a read's stamp, failing t on an error.
func stamp(t *testing.T, st *store.Store) store.PrivateListStamp {
	t.Helper()
	s, err := st.StampPrivateList(context.Background())
	if err != nil {
		t.Fatalf("StampPrivateList: %v", err)
	}
	return s
}

// keep keeps listed under s, failing t on an error, and answers what it added.
func keep(t *testing.T, st *store.Store, s store.PrivateListStamp, listed ...store.KnownPrivateProject) []string {
	t.Helper()
	added, err := st.KeepPrivateProjects(context.Background(), s, listed)
	if err != nil {
		t.Fatalf("KeepPrivateProjects: %v", err)
	}
	return added
}

// recordSwitch records key with channels, failing t on an error, and answers the record's number.
func recordSwitch(t *testing.T, st *store.Store, key string, channels ...string) int64 {
	t.Helper()
	record, err := st.RecordPrivateProject(context.Background(), key, channels)
	if err != nil {
		t.Fatalf("RecordPrivateProject(%s): %v", key, err)
	}
	return record
}

// forget forgets key's record (nil for none), failing t on an error.
func forget(t *testing.T, st *store.Store, key string, record *int64) store.ForgetAnswer {
	t.Helper()
	answer, err := st.ForgetPrivateProject(context.Background(), key, record)
	if err != nil {
		t.Fatalf("ForgetPrivateProject(%s): %v", key, err)
	}
	return answer
}

// advance moves the database's time forward by d.
func advance(st *store.Store, d time.Duration) {
	st.DatabaseClockOffset.Set(st.DatabaseClockOffset.Get() + d)
}

// listedAs is a project a read names, with its channels (nil for an entry with no slack_channels).
func listedAs(key string, channels []string) store.KnownPrivateProject {
	return store.KnownPrivateProject{Key: key, SlackChannels: channels}
}

// TestTheStoredPrivateListKeepsWhatAReadNamed holds the stored private list's rules against
// Postgres, moving the database's time with DatabaseClockOffset: a read adds what it names with its
// channels and its stamp, and moves the channels' time only when the set gains a channel; a slower
// read stamped earlier undoes nothing a later one wrote; a project whose channels a read did not
// answer is kept with unknown channels; a row no read names leaves only past KnownPrivateHold; a
// switch's record is held, numbered from the sequence, and survives every read stamped before it or
// within the hold after it; a forget changes only the record it names, or none younger than the
// hold; and a record made again after its row was deleted is numbered above the old one, so a
// delayed forget naming the old record changes nothing.
func TestTheStoredPrivateListKeepsWhatAReadNamed(t *testing.T) {
	c1, c2, c9 := "C0PRIV00001", "C0PRIV00002", "C0STALE0009"

	t.Run("a read adds what it names, and moves the channels' time only when the set gains one", func(t *testing.T) {
		st := storetest.Open(t)
		first := stamp(t, st)
		if added := keep(t, st, first, listedAs("PRIV", []string{c1})); !slices.Equal(added, []string{"PRIV"}) {
			t.Fatalf("the first read naming PRIV added %v; want [PRIV]", added)
		}
		row, _ := readKnownRow(t, st, "PRIV")
		if row.listed == nil || *row.listed != first.Number || !slices.Equal(row.channels, []string{c1}) || row.storedAt == nil {
			t.Fatalf("PRIV after the first read = %+v; want listed %d, channels [%s] and a stored time", row, first.Number, c1)
		}
		storedAt := *row.storedAt

		advance(st, time.Minute)
		if added := keep(t, st, stamp(t, st), listedAs("PRIV", []string{c1, c2})); len(added) != 0 {
			t.Fatalf("a second read of a held PRIV added %v; want nothing", added)
		}
		row, _ = readKnownRow(t, st, "PRIV")
		if !slices.Equal(row.channels, []string{c1, c2}) || row.storedAt == nil || !row.storedAt.After(storedAt) {
			t.Fatalf("PRIV after a read naming %s too = %+v; want both channels and a later stored time than %v", c2, row, storedAt)
		}
		storedAt = *row.storedAt

		advance(st, time.Minute)
		keep(t, st, stamp(t, st), listedAs("PRIV", []string{c1}))
		row, _ = readKnownRow(t, st, "PRIV")
		if !slices.Equal(row.channels, []string{c1}) || !row.storedAt.Equal(storedAt) {
			t.Fatalf("PRIV after a read that drops %s = %+v; want [%s] and the stored time unchanged at %v", c2, row, c1, storedAt)
		}
	})

	t.Run("a slower read stamped earlier changes nothing a later read wrote", func(t *testing.T) {
		st := storetest.Open(t)
		earlier := stamp(t, st)
		later := stamp(t, st)
		keep(t, st, later, listedAs("PRIV", []string{c1}))
		keep(t, st, earlier, listedAs("PRIV", []string{c9}))
		row, _ := readKnownRow(t, st, "PRIV")
		if !slices.Equal(row.channels, []string{c1}) || *row.listed != later.Number {
			t.Fatalf("PRIV after a slower read stamped %d = %+v; want the later read's [%s] and listed %d", earlier.Number, row, c1, later.Number)
		}
	})

	t.Run("an entry with no slack_channels key is kept with unknown channels", func(t *testing.T) {
		st := storetest.Open(t)
		keep(t, st, stamp(t, st), listedAs("SYNTH", nil))
		if got := heldRow(t, st, "SYNTH"); got.SlackChannels != nil || !got.ChannelsStoredAt.IsZero() {
			t.Fatalf("SYNTH kept from an entry with no slack_channels = %+v; want unknown channels", got)
		}
	})

	t.Run("a row no read names leaves only past the hold", func(t *testing.T) {
		st := storetest.Open(t)
		keep(t, st, stamp(t, st), listedAs("PRIV", []string{c1}))
		advance(st, store.KnownPrivateHold-time.Second)
		keep(t, st, stamp(t, st))
		if _, there := readKnownRow(t, st, "PRIV"); !there {
			t.Fatal("PRIV left at a read stamped within the hold of its listing; want it kept")
		}
		advance(st, 2*time.Second)
		keep(t, st, stamp(t, st))
		if _, there := readKnownRow(t, st, "PRIV"); there {
			t.Fatal("PRIV stayed at a read stamped past the hold of its listing; want it gone")
		}
	})

	t.Run("a switch's record is held, and survives every read stamped before it or within the hold after it", func(t *testing.T) {
		st := storetest.Open(t)
		before := stamp(t, st)
		record := recordSwitch(t, st, "SWITCH", c2)
		if record <= before.Number {
			t.Fatalf("SWITCH's record is %d, not above the earlier stamp %d", record, before.Number)
		}
		if got := heldRow(t, st, "SWITCH"); !slices.Equal(got.SlackChannels, []string{c2}) {
			t.Fatalf("SWITCH after its record = %+v; want held with [%s]", got, c2)
		}
		keep(t, st, before)
		if _, there := readKnownRow(t, st, "SWITCH"); !there {
			t.Fatal("SWITCH was deleted by a read stamped before its record; want it kept")
		}
		advance(st, store.KnownPrivateHold-time.Second)
		keep(t, st, stamp(t, st))
		if _, there := readKnownRow(t, st, "SWITCH"); !there {
			t.Fatal("SWITCH was deleted by a read stamped within the hold after its record (the switch's race); want it kept")
		}
		if again := recordSwitch(t, st, "SWITCH", c2); again <= record {
			t.Fatalf("SWITCH's second record is %d, not above its first %d", again, record)
		}
		advance(st, store.KnownPrivateHold+time.Second)
		keep(t, st, stamp(t, st))
		if _, there := readKnownRow(t, st, "SWITCH"); there {
			t.Fatal("SWITCH stayed at a read stamped past the hold after its record; want it gone")
		}
	})

	t.Run("a forget changes only the record it names, and none younger than the hold", func(t *testing.T) {
		st := storetest.Open(t)
		first := recordSwitch(t, st, "SWITCH", c2)
		second := recordSwitch(t, st, "SWITCH", c2)
		if got := forget(t, st, "SWITCH", &first); got.Outcome != store.ForgetSuperseded {
			t.Fatalf("a forget of SWITCH naming its earlier record %d, while its current is %d, = %+v; want superseded", first, second, got)
		}
		if !slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatal("SWITCH is not held after a superseded forget; want its later record kept")
		}
		in := forget(t, st, "SWITCH", nil)
		row, _ := readKnownRow(t, st, "SWITCH")
		if in.Outcome != store.ForgetInFlight || in.RetryAt.IsZero() || !slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatalf("a forget of SWITCH naming no record within the hold of its record = %+v (row %+v); want in flight with a time, and SWITCH held", in, row)
		}
		advance(st, store.KnownPrivateHold+time.Second)
		if got := forget(t, st, "SWITCH", nil); got.Outcome != store.ForgetForgotten || slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatalf("a forget of SWITCH naming no record past the hold = %+v, held %v; want forgotten and SWITCH unheld", got, heldKeys(t, st))
		}

		other := storetest.Open(t)
		record := recordSwitch(t, other, "SWITCH", c2)
		if got := forget(t, other, "SWITCH", &record); got.Outcome != store.ForgetForgotten || slices.Contains(heldKeys(t, other), "SWITCH") {
			t.Fatalf("a forget of SWITCH naming its current record = %+v, held %v; want forgotten and SWITCH unheld", got, heldKeys(t, other))
		}
	})

	t.Run("a read stamped before a numberless forget leaves the row unheld, and one after holds it again", func(t *testing.T) {
		st := storetest.Open(t)
		keep(t, st, stamp(t, st), listedAs("SWITCH", []string{c2}))
		before := stamp(t, st)
		if got := forget(t, st, "SWITCH", nil); got.Outcome != store.ForgetForgotten {
			t.Fatalf("a numberless forget of a listed SWITCH no switch recorded = %+v; want forgotten", got)
		}
		keep(t, st, before, listedAs("SWITCH", []string{c2}))
		if slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatal("SWITCH is held after a read stamped before its forget; want it unheld")
		}
		if added := keep(t, st, stamp(t, st), listedAs("SWITCH", []string{c2})); !slices.Equal(added, []string{"SWITCH"}) || !slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatalf("a read stamped after the forget added %v, held %v; want SWITCH held again", added, heldKeys(t, st))
		}
	})

	t.Run("reuse: a delayed forget naming a deleted row's record changes nothing of a new record", func(t *testing.T) {
		st := storetest.Open(t)
		old := recordSwitch(t, st, "SWITCH", c2)
		advance(st, store.KnownPrivateHold+time.Second)
		keep(t, st, stamp(t, st))
		if _, there := readKnownRow(t, st, "SWITCH"); there {
			t.Fatal("SWITCH stayed at a read stamped past the hold after its record; want it gone before the new record")
		}
		renewed := recordSwitch(t, st, "SWITCH", c2)
		if renewed <= old {
			t.Fatalf("SWITCH's new record %d is not above the deleted row's %d", renewed, old)
		}
		if got := forget(t, st, "SWITCH", &old); got.Outcome != store.ForgetSuperseded {
			t.Fatalf("the delayed forget of SWITCH naming %d = %+v; want superseded", old, got)
		}
		if !slices.Contains(heldKeys(t, st), "SWITCH") {
			t.Fatalf("SWITCH is not held after the delayed forget naming %d; want its new record %d kept", old, renewed)
		}
	})

	t.Run("a record refuses what Dispatch would not send", func(t *testing.T) {
		st := storetest.Open(t)
		for _, c := range []struct {
			key      string
			channels []string
		}{{"priv", nil}, {"PRIV", []string{"#general"}}, {"PRIV", make([]string, 17)}} {
			if _, err := st.RecordPrivateProject(context.Background(), c.key, c.channels); !errors.Is(err, store.ErrPrivateProjectInput) {
				t.Errorf("RecordPrivateProject(%q, %v) = %v; want ErrPrivateProjectInput", c.key, c.channels, err)
			}
		}
	})
}
