package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/consumernames"
)

// A sentinel's family round-trips through the store: what the caller gives is what State reads
// back. (The KV store's and CreateSessionBucket's half is consumernames' own row of this name.)
func TestASentinelsFamilyRoundTrips(t *testing.T) {
	ctx := context.Background()
	for _, family := range []string{consumernames.DispatchFamily, daemonFamily(t, "legion-go")} {
		store := newStore(t, family, newMemPG())
		if _, err := store.WriteSentinel(ctx, consumernames.StoreState{Family: family, Cutover: consumernames.CutoverDone}, 0); err != nil {
			t.Fatal(err)
		}
		if state, err := store.State(ctx); err != nil || state.Family != family {
			t.Fatalf("the store's sentinel written with the family %s read back as %+v, %v", family, state, err)
		}
	}
}

// The store's WriteSentinel writes its own family itself: a state that names no family is written
// naming the store's, and one that names another family is refused, naming both, with nothing
// written, so no caller writes another family's sentinel, or one its store cannot read, by
// mistake. The control is the family-less sentinel put under the key as bytes, which the store
// refuses to read. (The KV store's half is consumernames' own row of this name.)
func TestWriteSentinelWritesItsStoresOwnFamily(t *testing.T) {
	ctx := context.Background()
	db := newMemPG()
	family := daemonFamily(t, "legion-go")
	store := newStore(t, family, db)
	revision, err := store.WriteSentinel(ctx, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}, 0)
	if err != nil {
		t.Fatalf("WriteSentinel of a state naming no family = %v", err)
	}
	if state, err := store.State(ctx); err != nil || state.Family != family || state.Cutover != consumernames.CutoverDone {
		t.Errorf("the sentinel written with no family read back as %+v, %v; want the store's family %s, done", state, err, family)
	}
	_, err = store.WriteSentinel(ctx, consumernames.StoreState{Family: consumernames.DispatchFamily, Cutover: consumernames.CutoverRolledBack}, revision)
	if !errors.Is(err, consumernames.ErrSentinelFamily) || !strings.Contains(err.Error(), `"`+consumernames.DispatchFamily+`"`) || !strings.Contains(err.Error(), `"`+family+`"`) {
		t.Errorf("WriteSentinel of another family's state = %v; want ErrSentinelFamily naming both families", err)
	}
	if state, err := store.State(ctx); err != nil || state.Cutover == consumernames.CutoverRolledBack {
		t.Errorf("after the refused write the sentinel reads %+v, %v; want the one written before", state, err)
	}
	data, err := json.Marshal(consumernames.StoreState{Key: consumernames.PGSentinelKey(family), Cutover: consumernames.CutoverDone})
	if err != nil {
		t.Fatal(err)
	}
	db.rows[consumernames.PGSentinelKey(family)] = pgRow{data: data, revision: 9}
	if state, err := store.State(ctx); !errors.Is(err, consumernames.ErrMalformedSentinel) {
		t.Errorf("control: a sentinel under the key that names no family read as %+v, %v; want ErrMalformedSentinel", state, err)
	}
}

// The store reads only a sentinel that names its own family, the one New was given: a key alone
// cannot tell two stores apart, so a well-formed sentinel naming another family, or one naming
// none, under the store's sentinel key is a malformed sentinel, never a marker read. The control is
// a sentinel of its own family, read. Without the family, the store read another store's sentinel
// as its own: Dispatch's store took a daemon's done marker, which a creator in the fixed mode
// would obey. (The KV store's half is consumernames' own row of this name.)
func TestEachStoreReadsOnlyTheSentinelOfItsOwnFamily(t *testing.T) {
	ctx := context.Background()
	others := map[string][]string{
		consumernames.DispatchFamily: {consumernames.SessionFamily, daemonFamily(t, "legion-go")},
		daemonFamily(t, "legion-go"): {daemonFamily(t, "legion-other"), consumernames.DispatchFamily, consumernames.SessionFamily},
	}
	for _, family := range []string{consumernames.DispatchFamily, daemonFamily(t, "legion-go")} {
		t.Run("the store of "+family, func(t *testing.T) {
			db := newMemPG()
			store := newStore(t, family, db)
			sentinel := func(of string) {
				data, err := json.Marshal(consumernames.StoreState{Key: consumernames.PGSentinelKey(family), Family: of, Cutover: consumernames.CutoverDone})
				if err != nil {
					t.Fatal(err)
				}
				db.rows[consumernames.PGSentinelKey(family)] = pgRow{data: data, revision: 1}
			}
			for _, other := range append(others[family], "") {
				sentinel(other)
				state, err := store.State(ctx)
				if !errors.Is(err, consumernames.ErrMalformedSentinel) || errors.Is(err, consumernames.ErrNoSentinel) {
					t.Fatalf("the store of %s reading a sentinel of the family %q = %+v, %v; want ErrMalformedSentinel, never a missing sentinel", family, other, state, err)
				}
				if other != "" && (!errors.Is(err, consumernames.ErrSentinelFamily) || !strings.Contains(err.Error(), other)) {
					t.Fatalf("the store of %s reading a sentinel of the family %q = %v; want ErrSentinelFamily naming it", family, other, err)
				}
			}
			sentinel(family)
			if state, err := store.State(ctx); err != nil || state.Family != family || state.Cutover != consumernames.CutoverDone {
				t.Fatalf("control: the store of %s reading its own family's sentinel = %+v, %v", family, state, err)
			}
		})
	}
}

// A sentinel row that is there but is not the store's own exact sentinel is never a missing
// sentinel: a creator in the fixed mode reads ErrNoSentinel as no marker (Dispatch and the daemons
// do), so such a row must be a failed read, which never runs fixed names over a store a newer
// build may have converted, and it still refuses every call in the fresh mode. The rows are
// another family's sentinel, a sentinel under another key, one carrying a field this build does
// not know (as a newer operator build's could), and the server's STREAM.INFO answer. The control is
// the absent row, the one missing sentinel. (The KV store's half is consumernames' own row of this
// name.)
func TestAMalformedSentinelIsNeverNoMarker(t *testing.T) {
	ctx := context.Background()
	family := consumernames.DispatchFamily
	key := consumernames.PGSentinelKey(family)
	malformed := map[string]string{
		"another family's sentinel":        fmt.Sprintf(`{"key":%q,"family":"daemon/legion-other","cutover":"done"}`, key),
		"a sentinel under another key":     fmt.Sprintf(`{"key":"envoy.sentinel/daemon/legion-other","family":%q,"cutover":"done"}`, family),
		"a field this build does not know": fmt.Sprintf(`{"key":%q,"family":%q,"cutover":"done","epoch":2}`, key, family),
		"a STREAM.INFO answer":             `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0}`,
	}
	rows := func(t *testing.T, store *Store, put func(data string)) {
		if _, err := store.State(ctx); !errors.Is(err, consumernames.ErrNoSentinel) {
			t.Fatalf("control: State with no sentinel row = %v, want ErrNoSentinel", err)
		}
		for row, data := range malformed {
			put(data)
			_, err := store.State(ctx)
			if errors.Is(err, consumernames.ErrNoSentinel) || !errors.Is(err, consumernames.ErrMalformedSentinel) || !consumernames.IsSelectionStateLost(err) {
				t.Fatalf("State over %s = %v; want ErrMalformedSentinel, which is no missing sentinel and refuses every call", row, err)
			}
		}
	}
	t.Run("in memory", func(t *testing.T) {
		db := newMemPG()
		rows(t, newStore(t, family, db), func(data string) {
			db.rows[key] = pgRow{data: []byte(data), revision: 1}
		})
	})
	t.Run("on a real Postgres", func(t *testing.T) {
		pool := pgTestDatabase(t)
		rows(t, newStore(t, family, pool), func(data string) {
			if _, err := pool.Exec(ctx, `insert into `+table+` (logical, record, revision) values ($1, $2, 1)
				on conflict (logical) do update set record = excluded.record`, key, data); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// The store reads and writes the same self-keyed records: a write stamps the record and the
// sentinel with their own keys, the family's (PGKey, PGSentinelKey), and a read refuses a row whose
// record is another key's or no record at all, as the KV store does.
func TestThePGStoreWritesAndReadsOnlySelfKeyedRecords(t *testing.T) {
	ctx := context.Background()
	db := newMemPG()
	family := daemonFamily(t, "legion-go")
	store := newStore(t, family, db)
	if _, err := store.Write(ctx, "legion-go-CAPTURE-wake", consumernames.Record{Current: "legion-go-CAPTURE-wake~A"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteSentinel(ctx, consumernames.StoreState{Family: family, Cutover: consumernames.CutoverDone}, 0); err != nil {
		t.Fatal(err)
	}
	own := "daemon/legion-go/legion-go-CAPTURE-wake"
	if got := string(db.rows[own].data); !strings.Contains(got, `"key":"`+own+`"`) {
		t.Fatalf("the written record %s carries not its own key %s", got, own)
	}
	if got := string(db.rows["envoy.sentinel/daemon/legion-go"].data); !strings.Contains(got, `"key":"envoy.sentinel/daemon/legion-go"`) {
		t.Fatalf("the written sentinel %s carries not its own key", got)
	}
	db.rows[consumernames.PGKey(family, "legion-go-other")] = pgRow{data: db.rows[own].data, revision: 4}
	if record, _, err := store.Read(ctx, "legion-go-CAPTURE-wake"); err != nil || record.Current != "legion-go-CAPTURE-wake~A" {
		t.Fatalf("control: Read of the record's own key = %+v, %v", record, err)
	}
	if state, err := store.State(ctx); err != nil || state.Cutover != consumernames.CutoverDone {
		t.Fatalf("control: State = %+v, %v", state, err)
	}
	if _, _, err := store.Read(ctx, "legion-go-other"); !errors.Is(err, consumernames.ErrMalformedRecord) || !strings.Contains(err.Error(), "legion-go-other") {
		t.Fatalf("Read of a row holding another key's record = %v, want ErrMalformedRecord naming legion-go-other", err)
	}
	db.rows[consumernames.PGSentinelKey(family)] = pgRow{data: []byte(`{"cutover":"done"}`), revision: 2}
	if _, err := store.State(ctx); !errors.Is(err, consumernames.ErrMalformedSentinel) {
		t.Fatalf("State of a sentinel row carrying no key = %v, want ErrMalformedSentinel", err)
	}
}

// Several families' stores can share one database: several projects' daemons do. Each reads and
// writes only its own family's sentinel, under its own key, and lists only its own family's records
// and never a sentinel, so one project's cutover (its convert writing done to its sentinel) leaves
// another project's fixed-mode daemon reading "not done", and each reads only its own records. A
// record copied into one family's place from another's carries the other family's key, and is
// malformed there. The control is the single sentinel row every store shared before: the second
// project's sentinel could not be written beside the first's, and once the first's was written, the
// second's daemon read it as another family's, malformed, and could never boot.
func TestStoresOfSeveralFamiliesShareOneDatabase(t *testing.T) {
	ctx := context.Background()
	db := newMemPG()
	p, q, d := newStore(t, daemonFamily(t, "p"), db), newStore(t, daemonFamily(t, "q"), db), newStore(t, consumernames.DispatchFamily, db)
	for name, store := range map[string]*Store{"p": p, "q": q, "dispatch": d} {
		if _, err := store.WriteSentinel(ctx, consumernames.StoreState{Family: store.family}, 0); err != nil {
			t.Fatalf("the %s store's first sentinel beside the others' = %v", name, err)
		}
	}
	writes := map[*Store]string{p: "legion-go-p-github", q: "legion-go-q-github", d: "delivery-events"}
	for store, logical := range writes {
		if _, err := store.Write(ctx, logical, consumernames.Record{Start: 7}, 0); err != nil {
			t.Fatalf("write %s in the %s store = %v", logical, store.family, err)
		}
	}
	// p's convert marks p's store converted.
	if _, err := p.WriteSentinel(ctx, consumernames.StoreState{Family: daemonFamily(t, "p"), Cutover: consumernames.CutoverDone}, 1); err != nil {
		t.Fatalf("p's convert writing done to p's sentinel = %v", err)
	}
	if state, err := p.State(ctx); err != nil || state.Family != daemonFamily(t, "p") || state.Cutover != consumernames.CutoverDone {
		t.Fatalf("p's sentinel after p's convert = %+v, %v; want p's, done", state, err)
	}
	for _, store := range []*Store{q, d} {
		if state, err := store.State(ctx); err != nil || state.Family != store.family || state.Cutover != "" {
			t.Fatalf("the %s store's sentinel after p's convert = %+v, %v; want its own, not done", store.family, state, err)
		}
	}
	for store, logical := range writes {
		records, err := store.List(ctx)
		if err != nil || len(records) != 1 || records[logical].Start != 7 {
			t.Fatalf("List of the %s store = %v, %v; want %s alone, and no sentinel", store.family, records, err, logical)
		}
		for other, otherLogical := range writes {
			if other != store {
				if _, _, err := store.Read(ctx, otherLogical); !errors.Is(err, consumernames.ErrNoRecord) {
					t.Fatalf("the %s store reading %s, the %s store's = %v; want ErrNoRecord", store.family, otherLogical, other.family, err)
				}
			}
		}
	}
	// q's record copied whole into p's place for the same name.
	db.rows[daemonFamily(t, "p")+"/legion-go-q-github"] = db.rows[daemonFamily(t, "q")+"/legion-go-q-github"]
	if _, _, err := p.Read(ctx, "legion-go-q-github"); !errors.Is(err, consumernames.ErrMalformedRecord) {
		t.Fatalf("p reading q's record copied into p's place = %v; want ErrMalformedRecord", err)
	}
}

// A store is made only for a family it can keep, so no store reads, writes or lists the rows of a
// family it does not keep: New refuses any other, as ErrInvalidFamily naming it. Before, a store
// of "daemon", with no project, listed every project's records, each well formed under its own
// key, and a store of daemon/p/b wrote rows that daemon/p's store then listed as its own. The
// control is New's acceptance of every family a store keeps. (DaemonFamily's half is
// consumernames' own row of this name.)
func TestAStoreIsMadeOnlyForAFamilyItCanKeep(t *testing.T) {
	db := newMemPG()
	for _, family := range []string{"daemon", "daemon/", "daemon/p/b", "daemon/p q", "", "sessions", "dispatch/x"} {
		if store, err := New(family, db); !errors.Is(err, consumernames.ErrInvalidFamily) || !strings.Contains(err.Error(), fmt.Sprintf("%q", family)) || store != nil {
			t.Fatalf("New(%q) = %v, %v; want ErrInvalidFamily naming it", family, store, err)
		}
	}
	for _, family := range []string{consumernames.SessionFamily, consumernames.DispatchFamily, "daemon/p", "daemon/legion-go_a.b"} {
		if store, err := New(family, db); err != nil || store.family != family {
			t.Fatalf("control: New(%q) = %v, %v; want the store of that family", family, store, err)
		}
	}
}

// The store refuses to write a record every read of the key would refuse (EncodeRecord), as
// ErrMalformedRecord with nothing written, so a writer outside Names that builds one fails at its
// own write and the key stays readable for every other reader. The control is the same record
// with the abandoned name retired, written and read back. (The KV store's half is consumernames'
// own row of this name.)
func TestAStoreRefusesToWriteARecordNoReaderWouldRead(t *testing.T) {
	ctx := context.Background()
	until := time.Unix(1_700_000_000, 0)
	store := newStore(t, consumernames.DispatchFamily, newMemPG())
	prefix := consumernames.DispatchDeliveryDurable
	a, x := prefix+"~AAAAAAAAAAAAAAAAAAAAAA", prefix+"~XXXXXXXXXXXXXXXXXXXXXX"
	for i, c := range []struct {
		name   string
		record consumernames.Record
	}{
		{name: "an abandoned name that is not retired", record: consumernames.Record{Start: 1, Retired: []string{a}, Abandoned: []consumernames.Abandoned{{Name: x, Until: until}}}},
		{name: "an abandoned name with no time", record: consumernames.Record{Start: 1, Retired: []string{a, x}, Abandoned: []consumernames.Abandoned{{Name: x}}}},
		{name: "an operation reserved at no time", record: consumernames.Record{Start: 1, Op: &consumernames.Op{Kind: consumernames.OpCreate, Candidate: x}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logical := fmt.Sprintf("%s-%d", prefix, i)
			_, err := store.Write(ctx, logical, c.record, 0)
			_, _, read := store.Read(ctx, logical)
			if !errors.Is(err, consumernames.ErrMalformedRecord) || !errors.Is(read, consumernames.ErrNoRecord) {
				t.Fatalf("Write of %+v = %v, and the key then reads %v; want ErrMalformedRecord, nothing written (ErrNoRecord)", c.record, err, read)
			}
		})
	}
	t.Run("control: the abandoned name retired too", func(t *testing.T) {
		sound := consumernames.Record{Start: 1, Retired: []string{a, x}, Abandoned: []consumernames.Abandoned{{Name: x, Until: until}}}
		if _, err := store.Write(ctx, prefix, sound, 0); err != nil {
			t.Fatalf("Write of %+v = %v", sound, err)
		}
		if record, _, err := store.Read(ctx, prefix); err != nil || len(record.Abandoned) != 1 || len(record.Retired) != 2 {
			t.Fatalf("Read of the written record = %+v, %v; want it as written", record, err)
		}
	})
}
