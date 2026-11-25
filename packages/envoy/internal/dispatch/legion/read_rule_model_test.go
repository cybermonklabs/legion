package legion

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// read_rule_model_test.go is a seeded random test of the read rule of the Legion state intake
// (dispatch://INFRA-3931, the spec's R3; the cases of the rule are the issue's document
// read-rule-cases.md). Each seed draws one sequence of steps over the kinds of input the cases
// name, runs it on the package's intake rig with real NATS and real Postgres, keeps a model of
// each fake daemon's true state beside the rig, and checks four invariants of R3 as it goes and
// once the inputs stop.

// readRuleRow is one sequence the test runs: the seed its steps are drawn from, how many steps it
// draws, and whether it draws the compound steps of a held starting read too (heldStarts,
// heldStartStep), from a second stream of the seed, so a row that leaves heldStarts off draws the
// steps it drew before those compound steps were added.
type readRuleRow struct {
	seed       uint64
	steps      int
	heldStarts bool
}

// readRuleRows is what the test runs with no variable set. Six rows are seeds that failed at
// 7b1ad653, each cut to the fewest steps that still failed there and that pass with the rule as it
// stands (the skipped sequences, cursor.skips and skippedAbove, and a held read made by its holder
// of a dropped store, cursor.heldBy), and each fails invariant 2 there:
//   - seeds 7 (5 steps), 18 (7), 36 (24) and 29 (25): a read in flight that the cursor owed for gap
//     clears gap although events past the cap above its snapshot were skipped. Each fails at its
//     last step, a burst whose read a take-over brings back: "the intake is idle, and INFRA's cursor
//     is {... readOwed=false ...}: it owes no read, though the stream delivered s0's messages
//     through N, past the cursor".
//   - seeds 12 (27 steps) and 5 (40): a held read orphaned when its holder is a message of a store
//     the cursor drops - seed 12's holder a boot of a marked store replayed after a take-over, seed
//     5's a message past the cap of the store a write left - so at the end "it names the message at
//     stream sequence N as the owed read's holder, and no message of INFRA is pending: that holder
//     never makes the read".
//
// Seed 1030 (120 steps), the tester's round 19, failed invariant 2 at bc057cc2 and 60cd52d6: the
// setting's daemon is redeployed into the recorded old store and boots there (row 3.5), its next
// reads all fail until the boot that holds the read is released, and every later message of that
// daemon is of a store the cursor forgets, which then made no read while the cursor held a store,
// so the owed read was never made: "it owes a read that no message holds, and its last read failed
// or a holder was released, but 3 next messages of the daemon did not bring the cursor up". It
// passes with row 4.3 of read-rule-cases v9: such a message makes an owed, due and unheld read
// whatever store the cursor holds (callRead).
//
// Seeds 8 and 37 meet the rest: the read that stops answering until its holder is let go, six
// take-overs, a redeploy to the old store, a stall and four redeploys. Each end waits up to
// ReadRetryAfter of real time more where its steps left the owed read's holder handed back with a
// delay, which the steps alone do not decide.
//
// Seed 1 (30 steps, heldStarts on) draws the held starting read's steps (heldStartStep): with the
// rule that a write of the same URL keeps the hold reverted (resetCursor kept the cursor only where
// it followed the write's URL, as at 0a0ea48a), it fails invariant 3 at its last step, a take-over
// after two held starting reads that end with a third write of the same URL (steps 23 and 28): "the
// message at stream sequence 9 went back to the stream as hand-back 1 after hand-back 1, and nothing
// ended its hold between", the failure the model found on 8 seeds at 0dc86e28. It passes with the
// rule (read-rule-cases row 6.1b).
//
// Every other row leaves heldStarts off: each is cut to a recorded failure from before those steps
// were added, so each draws exactly the steps it drew when it was cut. Each seed that
// LEGION_READ_RULE_SEED or LEGION_READ_RULE_SEEDS names turns them on.
var readRuleRows = []readRuleRow{
	{seed: 7, steps: 5}, {seed: 18, steps: 7}, {seed: 36, steps: 24}, {seed: 29, steps: 25},
	{seed: 12, steps: 27}, {seed: 5, steps: 40}, {seed: 8, steps: 40}, {seed: 37, steps: 40},
	{seed: 1030, steps: 120}, {seed: 1, steps: 30, heldStarts: true},
}

// readRuleDefaultSteps is how many steps a seed named by LEGION_READ_RULE_SEED or
// LEGION_READ_RULE_SEEDS draws, unless LEGION_READ_RULE_STEPS says otherwise.
const readRuleDefaultSteps = 40

// readRuleWindow is the rig's client window (windowed): a read the daemon stalls fails after it,
// and a burst lets its held answer go well within it. readRuleCatchUp is how long a sequence's end
// waits in real time for the cursor to reach the setting's daemon once the intake's clock has moved
// past ReadRetryAfter: a message that holds the owed read goes back for ReadRetryAfter of real time
// at the most. readRuleFloor is how long it then waits for the durable's ack floor to reach the
// last message, after a take-over where the floor is not reached within readRuleSettle: a message
// handed back with a delay before the inputs stopped comes back only when the delay ends, up to
// ReadRetryAfter of real time, and a take-over brings it back at once, to be judged on the cursor
// that caught up. It may hold a read of its own (a store read of its store, say) until that read is
// due, ReadRetryAfter after the read before it began; it then makes the read, which the daemon
// answers, and is acknowledged. readRuleIdle is how long the check after each step waits for the
// intake to be idle (checkIdleCursor), a burst's excepted.
const (
	readRuleWindow  = 3 * time.Second
	readRuleCatchUp = 75 * time.Second
	readRuleSettle  = 2 * time.Second
	readRuleFloor   = 2*ReadRetryAfter + 15*time.Second
	readRuleIdle    = 300 * time.Millisecond
)

// readRuleInvariants is each invariant the test checks, by the number a failure names it with.
var readRuleInvariants = map[int]string{
	1: "no event of a store other than the cursor's is applied",
	2: "once the inputs stop and the setting's daemon answers, the cursor reaches the daemon's sequence and store within about a minute",
	3: "no message is held past about 11 minutes",
	4: "a project starts at most one whole read a minute",
}

// TestAnySequenceOfInputsAppliesNoOtherStoreCatchesUpHoldsNoMessageLongAndReadsOnceAMinute: R3's
// read rule, over random sequences of the inputs its cases name. Each seed is a subtest on a rig of
// its own (its own database and stream). Its steps, drawn from the seed (math/rand/v2, PCG):
//   - the daemon behind the setting's URL commits one to three events, each moving INFRA-1's phase
//     to a value that names the store and the sequence, and publishes them as one message; or drops
//     the message, a gap the stream never holds; or publishes a message whose seq its events do not
//     reach; or boots under a new run at the next sequence, sometimes moving its schema; or
//     publishes a transaction split in two parts;
//   - a daemon the setting named before (the recorded old store, mostly) commits and publishes, or
//     boots; a foreign store that no URL ever serves commits and publishes, or boots;
//   - a burst: the answers of the daemon the setting names are held while a read is made (for a gap;
//     by the message holding the owed gap read, which a take-over brings back; or at a write of the
//     same URL or of another daemon), stateMaxWaiting messages fill the project's runner and one to
//     four more arrive past the cap near the end of the read's minute, the first sometimes of a
//     dropped store; then the answer is let go;
//   - an outcome for the next reads of the setting's daemon: good, refused (refusesFor, or refuses
//     until it next commits), stalled (each RequestTimeout on the intake's clock), slow, or refused
//     a minute late, several reads in a row; or the daemon stops answering for 8 to 14 reads, each
//     refused a minute late, while a message past a gap holds the read and its redeliveries make
//     them, so a hold reaches stateMaxHolds hand-backs;
//   - a write of the setting: the same URL, another daemon in a store of its own, another daemon that
//     serves the same store (the daemon moved), or back to an earlier URL;
//   - a redeploy: the daemon behind the setting's URL comes to serve the old store, an earlier
//     daemon's store or a store of its own, with no write, and boots there;
//   - a take-over of the durable; the intake's clock moving on 5 to 70 seconds;
//   - where the row turns heldStarts on, which every seed a variable names does, two compound steps
//     drawn from a second stream of the seed (heldStartStep), each one step in 16: two writes of
//     the same URL, the second once the intake is idle after the first's wake, and a message of the
//     setting's daemon, which then holds the starting read where the cursor follows no daemon and
//     that read is not due (read-rule-cases state S0h); then a third write of the same URL (row
//     6.1b), or the intake's clock moving past the minute and a take-over (row 7.5).
//
// The model keeps each store's sequence, run, schema and INFRA-1's latest phase, and each fake
// daemon serves the store it is behind (daemonStateIn): a daemon commits before it publishes, so no
// message carries an event the daemon's served state does not hold. After each step it checks
// invariants 1, 3 and 4, and invariant 2's cheap form; once the steps end, the setting's daemon
// answers its reads (and publishes its next message where its last events were never published,
// since no message shows them), the intake's clock moves past ReadRetryAfter, and it checks all
// four:
//  1. No observed value of INFRA-1's phase names a store that no URL ever served behind the setting,
//     or a value no daemon committed; at the end the current value is the setting's daemon's latest.
//  2. The cursor reaches the daemon's sequence and store within readRuleCatchUp. The one exception R3
//     allows is a failed read that no message holds (the cursor owes a read, names no holder, and
//     the last read failed or a holder went back stateMaxHolds times), which waits for the project's
//     next message: there the daemon publishes its next message and the wait starts again. A cursor
//     that is behind with nothing pending and owes no read, or names a holder no delivery brings
//     back, fails the sequence. Its cheap form, after each step: where the intake is idle, a cursor
//     that follows the setting's URL and owes no read holds a store, and is not behind a delivered
//     message of it (checkIdleCursor).
//  3. No hold goes back to the stream more than stateMaxHolds times: each handBack heldLine logs is
//     stateMaxHolds at the most, and a message's count starts again only once its hold ended
//     (checkHandBacks); and the durable's ack floor reaches the last message (finishedThrough):
//     within readRuleSettle, or within readRuleFloor of a take-over that brings back at once each
//     message handed back with a delay. The take-over comes after invariant 2 is checked, so it
//     reads nothing that check needs.
//  4. Consecutive whole reads of the project start at least ReadRetryAfter apart on the intake's
//     clock, whatever their reasons, with no exemption: a starting read, the read right after a
//     good starting read and the starting reads of two writes of the setting within a minute
//     included (read-rule-cases v11, cursor.readFor). legion_reads.read_at is the read's time once
//     the daemon answered (fetchWhole), not its start, so the start is last_read_started_at as
//     the read's row finds it on the cursor when it is inserted (readStartsTrigger).
//
// With no variable set it runs readRuleRows, each with the heldStarts its row sets.
// LEGION_READ_RULE_SEED=<n> runs that seed alone, LEGION_READ_RULE_SEEDS=<from>-<to> each seed of
// the range, both with heldStarts on, and LEGION_READ_RULE_STEPS=<n> sets the steps of every
// sequence. A failure names the seed, the step, the invariant, every step drawn so far and how to
// run the sequence alone.
func TestAnySequenceOfInputsAppliesNoOtherStoreCatchesUpHoldsNoMessageLongAndReadsOnceAMinute(t *testing.T) {
	for _, row := range readRuleRowsOf(t) {
		t.Run(fmt.Sprintf("seed=%d", row.seed), func(t *testing.T) {
			newReadRuleModel(t, row).run()
		})
	}
}

// readRuleRowsOf is the sequences the variables name (LEGION_READ_RULE_SEED,
// LEGION_READ_RULE_SEEDS, LEGION_READ_RULE_STEPS), each with heldStarts on, or readRuleRows with
// none set. A value that does not parse fails the test, naming it.
func readRuleRowsOf(t *testing.T) []readRuleRow {
	t.Helper()
	steps := 0
	if value := os.Getenv("LEGION_READ_RULE_STEPS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			t.Fatalf("LEGION_READ_RULE_STEPS=%q is not a count of steps of at least 1", value)
		}
		steps = parsed
	}
	parseSeed := func(name, value string) uint64 {
		seed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: %q is not a seed (an unsigned integer)", name, os.Getenv(name), value)
		}
		return seed
	}
	one, seeds := os.Getenv("LEGION_READ_RULE_SEED"), os.Getenv("LEGION_READ_RULE_SEEDS")
	var rows []readRuleRow
	switch {
	case one != "" && seeds != "":
		t.Fatalf("LEGION_READ_RULE_SEED=%q and LEGION_READ_RULE_SEEDS=%q are both set: set one of them", one, seeds)
	case one != "":
		rows = []readRuleRow{{seed: parseSeed("LEGION_READ_RULE_SEED", one), heldStarts: true}}
	case seeds != "":
		from, to, found := strings.Cut(seeds, "-")
		if !found {
			t.Fatalf("LEGION_READ_RULE_SEEDS=%q is not a range <from>-<to>", seeds)
		}
		first, last := parseSeed("LEGION_READ_RULE_SEEDS", from), parseSeed("LEGION_READ_RULE_SEEDS", to)
		if last < first {
			t.Fatalf("LEGION_READ_RULE_SEEDS=%q ends before it begins", seeds)
		}
		for seed := first; seed <= last; seed++ {
			rows = append(rows, readRuleRow{seed: seed, heldStarts: true})
		}
	default:
		rows = append(rows, readRuleRows...)
	}
	for i := range rows {
		switch {
		case steps > 0:
			rows[i].steps = steps
		case rows[i].steps == 0:
			rows[i].steps = readRuleDefaultSteps
		}
	}
	return rows
}

// modelStore is one daemon store as the model knows it: its name in the phase values (s<n> for a
// store a daemon serves, f<n> for a foreign store that no URL ever serves), its stateId, its run,
// the sequence it has committed and INFRA-1's phase there, its schema version, and the highest
// sequence a message of it has carried (published), which a whole read of its daemon also covers.
type modelStore struct {
	name, id  string
	run       int
	seq       int
	published int
	phase     string
	schema    int64
}

// state is the state document the store's daemon serves: daemonStateIn at the store's sequence,
// store and run, INFRA-1 in its latest phase, at the store's schema version.
func (s *modelStore) state() string {
	body := daemonStateIn("INFRA", s.seq, inPhase(s.phase), s.id, runID(s.run))
	if s.schema != stateSchema {
		body = strings.Replace(body, fmt.Sprintf(`"schemaVersion":%d`, stateSchema), fmt.Sprintf(`"schemaVersion":%d`, s.schema), 1)
	}
	return body
}

// runID is the run id of the model's run n.
func runID(n int) string { return fmt.Sprintf("00000000-0000-4000-9000-%012x", n) }

// modelDaemon is one fake daemon, the store it serves now, whether the setting ever named its URL,
// and the generation of the outcome its reads have (reset), which a stall or a lag that re-arms
// itself checks, so a later outcome is not undone by an earlier one.
type modelDaemon struct {
	name       string
	fake       *fakeDaemon
	store      *modelStore
	named      bool
	generation atomic.Uint64
}

// readRuleModel is one seed's rig and model: the steps drawn so far, each store and daemon, the
// daemon behind the setting's URL, the daemon the setting named before the last write that named
// another (old), every store a URL served while the setting named it (behind), and the store each
// phase value names (values). quiet is how long the check after the step drawn last waits for the
// intake to be idle (checkIdleCursor). random is the seed's stream every step kind is drawn from,
// and heldRandom the second stream the held starting read's steps are drawn from (heldStartStep),
// nil where the row leaves them off.
type readRuleModel struct {
	t          *testing.T
	rig        *intakeRig
	random     *rand.Rand
	heldRandom *rand.Rand
	seed       uint64
	total      int
	steps      []string
	took       []time.Duration
	failed     bool
	stores     []*modelStore
	foreign    []*modelStore
	daemons    []*modelDaemon
	setting    *modelDaemon
	old        *modelDaemon
	behind     map[*modelStore]bool
	values     map[string]*modelStore
	commits    int
	runs       int
	quiet      time.Duration
}

// newReadRuleModel opens row's rig with INFRA's setting naming daemon A, in store s0 at 10, starts
// the intake and waits for its good starting read.
func newReadRuleModel(t *testing.T, row readRuleRow) *readRuleModel {
	t.Helper()
	seed := row.seed
	m := &readRuleModel{
		t: t, seed: seed, total: row.steps, random: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		behind: map[*modelStore]bool{}, values: map[string]*modelStore{},
	}
	if row.heldStarts {
		m.heldRandom = rand.New(rand.NewPCG(seed^0xd1b54a32d192ed03, seed))
	}
	t.Cleanup(func() {
		if t.Failed() && !m.failed {
			t.Logf("seed %d failed outside its invariants; reproduce it alone with %s\n%s", seed, m.alone(), m.stepList())
		}
	})
	windowed(t, readRuleWindow)
	m.rig = newIntakeRig(t)
	m.readStartsTrigger()
	first := m.newStore(false, 10)
	a := m.newDaemon(first)
	seedLegionProject(t, m.rig.database, "INFRA", a.fake.URL)
	a.named, m.setting, m.behind[first] = true, a, true
	m.rig.start()
	m.rig.awaitStartingRead("INFRA")
	return m
}

// readStartsTrigger records in the rig's database the start of each whole read beside its
// legion_reads row: as a row of kind read is inserted, the trigger copies the project's cursor's
// last_read_started_at for it. That is the read's own start (wholeRead.started): readWhole wrote
// it on the cursor before it called the daemon (read-rule-cases row 7.6), in a statement of its
// own that committed before recordWhole's transaction inserts the row, and no other read of the
// project begins in between. recordWhole's write of the cursor after the row changes nothing
// there, since it keeps the later of the two starts, which are one. A read whose record is
// dropped, since the setting moved meanwhile, inserts no row, so the trigger never gives its start
// to another read. The trigger never fails a write of the intake.
func (m *readRuleModel) readStartsTrigger() {
	for _, statement := range []string{
		`create table read_rule_read_starts (id bigserial primary key, read_id bigint, started timestamptz)`,
		`create function read_rule_read_start() returns trigger language plpgsql as $$
		begin
			insert into read_rule_read_starts (read_id, started)
			select new.id, last_read_started_at from legion_event_cursors
			where project = new.project and last_read_started_at is not null;
			return null;
		end
		$$`,
		`create trigger read_rule_read_start after insert on legion_reads
			for each row when (new.kind = 'read') execute function read_rule_read_start()`,
	} {
		if _, err := m.rig.database.Pool.Exec(context.Background(), statement); err != nil {
			m.t.Fatalf("record the start of each whole read: %v", err)
		}
	}
}

// newStore is a store the model counts from seq, with a phase value of its own there.
func (m *readRuleModel) newStore(foreign bool, seq int) *modelStore {
	prefix, count := "s", len(m.stores)-len(m.foreign)
	if foreign {
		prefix, count = "f", len(m.foreign)
	}
	m.runs++
	s := &modelStore{
		name: fmt.Sprintf("%s%d", prefix, count), id: fmt.Sprintf("00000000-0000-4000-8000-%012x", len(m.stores)+1),
		run: m.runs, seq: seq, published: seq, schema: stateSchema,
	}
	s.phase = m.value(s)
	m.stores = append(m.stores, s)
	if foreign {
		m.foreign = append(m.foreign, s)
	}
	return s
}

// newDaemon is a fake daemon serving store, named by the next letter (D<n> past Z).
func (m *readRuleModel) newDaemon(store *modelStore) *modelDaemon {
	name := fmt.Sprintf("D%d", len(m.daemons))
	if len(m.daemons) < 26 {
		name = string(rune('A' + len(m.daemons)))
	}
	d := &modelDaemon{name: name, fake: newFakeDaemon(m.t, store.state()), store: store}
	m.daemons = append(m.daemons, d)
	return d
}

// value is INFRA-1's phase at store s's sequence, naming the store, the sequence and the commit.
func (m *readRuleModel) value(s *modelStore) string {
	m.commits++
	value := fmt.Sprintf("%s.%d#%d", s.name, s.seq, m.commits)
	m.values[value] = s
	return value
}

// refresh has every daemon behind store s serve its state as it stands.
func (m *readRuleModel) refresh(s *modelStore) {
	for _, d := range m.daemons {
		if d.store == s {
			d.fake.serves(s.state())
		}
	}
}

// commit commits n events to store s, each moving INFRA-1's phase, and answers them.
func (m *readRuleModel) commit(s *modelStore, n int) []map[string]any {
	var events []map[string]any
	for range n {
		s.seq++
		s.phase = m.value(s)
		events = append(events, change(s.seq, "INFRA-1", moved("phase", s.phase)))
	}
	m.refresh(s)
	return events
}

// boot commits a boot of store s under a new run at its next sequence, moving its schema where
// schemaMoved is set, and answers the event.
func (m *readRuleModel) boot(s *modelStore, schemaMoved bool) map[string]any {
	m.runs++
	s.run = m.runs
	s.seq++
	if schemaMoved {
		s.schema++
	}
	m.refresh(s)
	return booted(s.seq, s.schema, 2)
}

// publish publishes events of store s as one message, more of its transaction to come where more
// is set.
func (m *readRuleModel) publish(s *modelStore, more bool, events ...map[string]any) {
	m.rig.publishIn("INFRA", runID(s.run), s.id, time.Now().UTC().Truncate(time.Microsecond), more, events...)
	if !more {
		s.published = max(s.published, events[len(events)-1]["seq"].(int))
	}
}

// publishMalformed publishes a message of store s whose seq is the last of events, carrying every
// event but the last: a message whose seq its events do not reach, which does not decode.
func (m *readRuleModel) publishMalformed(s *modelStore, events []map[string]any) {
	m.rig.publishMessage("INFRA", map[string]any{
		"project": "INFRA", "run": runID(s.run), "stateId": s.id, "firstSeq": events[0]["seq"], "seq": events[len(events)-1]["seq"],
		"at": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"), "events": events[:len(events)-1],
	})
}

// write writes INFRA's setting naming d's URL and signals the write's wake, unless a wake is
// signalled already, which reads the settings as they stand once it is heard.
func (m *readRuleModel) write(d *modelDaemon) {
	if _, err := PutSettings(m.t.Context(), m.rig.database.Pool, "INFRA", d.fake.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		m.t.Fatalf("write INFRA's setting naming %s: %v", d.name, err)
	}
	select {
	case m.rig.wake <- struct{}{}:
	default:
	}
	if d != m.setting {
		m.old = m.setting
	}
	d.named, m.setting, m.behind[d.store] = true, d, true
}

// earlier is a daemon the setting named before that is not behind its URL now, nil for none.
func (m *readRuleModel) earlier() *modelDaemon {
	var candidates []*modelDaemon
	for _, d := range m.daemons {
		if d.named && d != m.setting {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[m.random.IntN(len(candidates))]
}

// foreignStore is one of up to three foreign stores, counting near the setting's store's sequence,
// so a message of it can follow the cursor.
func (m *readRuleModel) foreignStore() *modelStore {
	if len(m.foreign) == 0 || (len(m.foreign) < 3 && m.random.IntN(3) == 0) {
		return m.newStore(true, max(1, m.setting.store.seq-1+m.random.IntN(3)))
	}
	return m.foreign[m.random.IntN(len(m.foreign))]
}

// droppedStore is a store the cursor drops for a burst's message past the cap: the old store where
// it is not the setting's, and a foreign store otherwise.
func (m *readRuleModel) droppedStore() *modelStore {
	if m.old != nil && m.old.store != m.setting.store {
		return m.old.store
	}
	return m.foreignStore()
}

// reset gives d's reads no outcome of their own: every read answers d's state. It answers the new
// outcome generation.
func (d *modelDaemon) reset() uint64 {
	generation := d.generation.Add(1)
	d.fake.stalls(nil)
	d.fake.refusesFor(0, 0)
	d.fake.lagsNext(nil)
	d.fake.serves(d.store.state())
	return generation
}

// record appends one step's line to the steps drawn so far.
func (m *readRuleModel) record(format string, args ...any) {
	m.steps = append(m.steps, fmt.Sprintf(format, args...))
}

// span names events of store s: s<n> 12, or s<n> 12-14.
func span(s *modelStore, events []map[string]any) string {
	first, last := events[0]["seq"].(int), events[len(events)-1]["seq"].(int)
	if first == last {
		return fmt.Sprintf("%s %d", s.name, first)
	}
	return fmt.Sprintf("%s %d-%d", s.name, first, last)
}

// readRuleSteps is each kind of step and its weight: a sequence of 30 to 60 steps meets most kinds.
var readRuleSteps = []struct {
	weight int
	step   func(*readRuleModel)
}{
	{28, (*readRuleModel).commitStep},
	{8, (*readRuleModel).oldStoreStep},
	{6, (*readRuleModel).foreignStep},
	{5, (*readRuleModel).burstStep},
	{12, (*readRuleModel).outcomeStep},
	{3, (*readRuleModel).stopsAnsweringStep},
	{11, (*readRuleModel).writeStep},
	{5, (*readRuleModel).redeployStep},
	{5, (*readRuleModel).takeOverStep},
	{20, (*readRuleModel).clockStep},
}

// run draws and runs the sequence's steps, each followed by the cheap invariants once its messages
// reach the intake, and then ends the inputs and checks all four (finish). Where the row turns
// heldStarts on, each step first draws from the second stream whether it is one of the held
// starting read's (heldStartStep), and draws nothing from the seed's stream where it is.
func (m *readRuleModel) run() {
	total := 0
	for _, kind := range readRuleSteps {
		total += kind.weight
	}
	for index := range m.total {
		began := time.Now()
		if !m.heldStartStep() {
			draw := m.random.IntN(total)
			for _, kind := range readRuleSteps {
				if draw < kind.weight {
					kind.step(m)
					break
				}
				draw -= kind.weight
			}
		}
		m.settle()
		m.checkValues(index)
		m.checkHandBacks(index)
		m.checkReadsApart(index)
		m.checkIdleCursor(index, max(m.quiet, readRuleIdle))
		m.quiet = 0
		m.took = append(m.took, time.Since(began))
	}
	m.t.Logf("seed %d:\n%s", m.seed, m.stepList())
	m.finish()
}

// settle waits a while for every message published so far to have reached the intake.
func (m *readRuleModel) settle() {
	testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool { return m.rig.consumerInfo().NumPending == 0 })
	time.Sleep(50 * time.Millisecond)
}

// commitStep: the daemon behind the setting's URL commits and publishes, drops a message, publishes
// one that does not decode, boots, or publishes a split transaction.
func (m *readRuleModel) commitStep() {
	d, s := m.setting, m.setting.store
	switch kind := m.random.IntN(10); {
	case kind < 6:
		events := m.commit(s, 1+m.random.IntN(3))
		m.record("%s commits %s and publishes it", d.name, span(s, events))
		m.publish(s, false, events...)
	case kind < 8 && m.random.IntN(3) == 0:
		events := m.commit(s, 2)
		m.record("%s commits %s and publishes a message of it whose seq its events do not reach", d.name, span(s, events))
		m.publishMalformed(s, events)
	case kind < 8:
		events := m.commit(s, 1+m.random.IntN(2))
		m.record("%s commits %s and drops its message: a gap the stream never holds", d.name, span(s, events))
	case kind < 9:
		schemaMoved := m.random.IntN(4) == 0
		event := m.boot(s, schemaMoved)
		m.record("%s boots %s under run r%d at %d, schema %d", d.name, s.name, s.run, s.seq, s.schema)
		m.publish(s, false, event)
	default:
		events := m.commit(s, 2+m.random.IntN(2))
		split := 1 + m.random.IntN(len(events)-1)
		m.record("%s commits %s and publishes it in two parts, %s and %s", d.name, span(s, events), span(s, events[:split]), span(s, events[split:]))
		m.publish(s, true, events[:split]...)
		m.publish(s, false, events[split:]...)
	}
}

// oldStoreStep: a daemon the setting named before, mostly the one the last write that named
// another replaced, commits and publishes, or boots.
func (m *readRuleModel) oldStoreStep() {
	d := m.old
	if d == nil || m.random.IntN(10) < 3 {
		if earlier := m.earlier(); earlier != nil {
			d = earlier
		}
	}
	if d == nil {
		m.foreignStep()
		return
	}
	s := d.store
	if m.random.IntN(5) == 0 {
		event := m.boot(s, false)
		m.record("%s, named before, boots %s under run r%d at %d", d.name, s.name, s.run, s.seq)
		m.publish(s, false, event)
		return
	}
	events := m.commit(s, 1+m.random.IntN(2))
	m.record("%s, named before, commits %s and publishes it", d.name, span(s, events))
	m.publish(s, false, events...)
}

// foreignStep: a foreign store, which no URL ever serves, commits and publishes, or boots.
func (m *readRuleModel) foreignStep() {
	f := m.foreignStore()
	if m.random.IntN(5) == 0 {
		event := m.boot(f, false)
		m.record("foreign store %s boots under run r%d at %d", f.name, f.run, f.seq)
		m.publish(f, false, event)
		return
	}
	events := m.commit(f, 1+m.random.IntN(2))
	m.record("foreign store %s commits %s and publishes it", f.name, span(f, events))
	m.publish(f, false, events...)
}

// burstStep: the answers of the daemon a read reads are held while the read is made - for a gap
// with the read due; by the message that holds the owed gap read, which a take-over brings back
// once the read is due; or at a write of the same URL or of another daemon - stateMaxWaiting
// messages of the setting's store fill the project's runner, the intake's clock moves to a few
// seconds before the read's minute ends, one to four messages arrive past the cap (the first
// sometimes of a store the cursor drops: after a write naming another daemon, the store that write
// replaces), and the answer is let go.
func (m *readRuleModel) burstStep() {
	trigger := m.random.IntN(4)
	past := 1 + m.random.IntN(4)
	left := time.Duration(2+m.random.IntN(8)) * time.Second
	withDropped := m.random.IntN(3) == 0
	held, how, fill := m.setting, "a message past a gap, the read due", stateMaxWaiting-1
	switch trigger {
	case 1:
		how = "the redelivery of a message past a gap that holds the owed read, which a take-over brings back once the read is due"
	case 2:
		how, fill = "a write of the same URL", stateMaxWaiting
	case 3:
		held = m.newDaemon(m.newStore(false, 3+m.random.IntN(18)))
		how, fill = fmt.Sprintf("a write naming another daemon %s in store %s at %d", held.name, held.store.name, held.store.seq), stateMaxWaiting
		withDropped = withDropped || m.random.IntN(2) == 0
	}
	var dropped *modelStore
	switch {
	case withDropped && trigger == 3:
		dropped = m.setting.store
	case withDropped:
		dropped = m.droppedStore()
	}
	line := fmt.Sprintf("burst: %s's answers are held, the read made at %s; %d messages fill the runner, then %d past the cap %s before the read's minute ends",
		held.name, how, fill, past, left)
	if dropped != nil {
		line += fmt.Sprintf(", the first of dropped store %s", dropped.name)
	}
	m.record("%s", line)

	release := held.fake.holds(m.t)
	before := held.fake.served()
	switch trigger {
	case 0:
		s := m.setting.store
		m.rig.clock.advance(ReadRetryAfter + time.Second)
		m.commit(s, 1)
		m.publish(s, false, m.commit(s, 1)...)
	case 1:
		// The message past the gap holds the read where it is not due, and makes it, held, where it
		// is; the take-over cuts such a read short and brings the message back either way.
		s := m.setting.store
		m.commit(s, 1)
		m.publish(s, false, m.commit(s, 1)...)
		holder := m.rig.lastStreamSeq()
		testwait.Holds(3*time.Second, 20*time.Millisecond, func() bool { return m.rig.handBacksOf()[holder] > 0 || held.fake.served() > before })
		m.rig.clock.advance(ReadRetryAfter + time.Second)
		before = held.fake.served()
		m.rig.stop()
		m.rig.start()
	default:
		m.write(held)
	}
	began := testwait.Holds(3*time.Second, 20*time.Millisecond, func() bool { return held.fake.served() > before })
	s := m.setting.store
	for range fill {
		m.publish(s, false, m.commit(s, 1)...)
	}
	testwait.Holds(5*time.Second, 20*time.Millisecond, func() bool {
		info := m.rig.consumerInfo()
		return info.NumPending == 0 && (!began || info.NumAckPending >= stateMaxWaiting)
	})
	m.rig.clock.advance(ReadRetryAfter - left)
	for i := range past {
		store := s
		if i == 0 && dropped != nil {
			store = dropped
		}
		m.publish(store, false, m.commit(store, 1)...)
	}
	testwait.Holds(5*time.Second, 20*time.Millisecond, func() bool { return m.rig.consumerInfo().NumPending == 0 })
	time.Sleep(300 * time.Millisecond)
	release()
	m.quiet = 3 * time.Second
}

// outcomeStep: what the next reads of the daemon behind the setting's URL come to.
func (m *readRuleModel) outcomeStep() {
	d := m.setting
	switch kind := m.random.IntN(6); kind {
	case 0:
		m.record("%s answers its reads", d.name)
		d.reset()
	case 1:
		reads := 1 + m.random.IntN(3)
		m.record("%s refuses its next %d reads with 503", d.name, reads)
		d.reset()
		d.fake.refusesFor(reads, 503)
	case 2:
		m.record("%s refuses every read with 503 until it next commits", d.name)
		d.reset()
		d.fake.refuses(503)
	case 3:
		reads := 1 + m.random.IntN(2)
		m.record("%s stalls its next %d reads, each lasting RequestTimeout on the intake's clock", d.name, reads)
		generation := d.reset()
		var left atomic.Int64
		left.Store(int64(reads))
		d.fake.stalls(func() {
			m.rig.clock.advance(RequestTimeout)
			if left.Add(-1) <= 0 && d.generation.Load() == generation {
				d.fake.stalls(nil)
			}
		})
	case 4:
		late := time.Duration(10+m.random.IntN(41)) * time.Second
		m.record("%s answers its next read %s late on the intake's clock", d.name, late)
		generation := d.reset()
		d.fake.lagsNext(func() {
			if d.generation.Load() == generation {
				m.rig.clock.advance(late)
			}
		})
	default:
		reads := 1 + m.random.IntN(12)
		m.record("%s refuses its next %d reads with 503, each answered RequestTimeout late on the intake's clock", d.name, reads)
		m.refusesLate(d, reads)
	}
}

// refusesLate has d refuse its next reads with 503, each answered RequestTimeout late on the
// intake's clock, so a message that holds the read comes back holdAtLeast after each one.
func (m *readRuleModel) refusesLate(d *modelDaemon, reads int) {
	generation := d.reset()
	d.fake.refusesFor(reads, 503)
	var left atomic.Int64
	left.Store(int64(reads))
	var lag func()
	lag = func() {
		if d.generation.Load() != generation {
			return
		}
		m.rig.clock.advance(RequestTimeout)
		if left.Add(-1) > 0 {
			d.fake.lagsNext(lag)
		}
	}
	d.fake.lagsNext(lag)
}

// stopsAnsweringStep: the daemon behind the setting's URL stops answering for a while, 8 to 14
// reads, each refused RequestTimeout late on the intake's clock. The clock moves past the minute and
// the daemon publishes past a gap, so a message calls for the read, and the step waits while the
// redeliveries of the message that holds it make those reads: stateMaxHolds hand-backs at the
// most, after which the message is let go and the read waits for the project's next message.
func (m *readRuleModel) stopsAnsweringStep() {
	d, s := m.setting, m.setting.store
	reads := 8 + m.random.IntN(7)
	m.record("%s stops answering: it refuses its next %d reads, each RequestTimeout late; the intake's clock moves on %s and %s commits %s, dropped, and %s, published",
		d.name, reads, ReadRetryAfter+time.Second, d.name, span(s, []map[string]any{{"seq": s.seq + 1}}), span(s, []map[string]any{{"seq": s.seq + 2}}))
	m.refusesLate(d, reads)
	before := d.fake.served()
	m.rig.clock.advance(ReadRetryAfter + time.Second)
	m.commit(s, 1)
	m.publish(s, false, m.commit(s, 1)...)
	testwait.Holds(time.Duration(reads+2)*1500*time.Millisecond, 50*time.Millisecond, func() bool { return d.fake.served()-before >= reads })
}

// writeStep: a write of INFRA's setting naming the same URL, another daemon in a store of its own,
// another daemon serving the setting's store (the daemon moved), or an earlier URL.
func (m *readRuleModel) writeStep() {
	var target *modelDaemon
	switch kind := m.random.IntN(4); kind {
	case 0:
		target = m.setting
		m.record("write of the same URL, %s's", target.name)
	case 2:
		target = m.newDaemon(m.setting.store)
		m.record("write naming another daemon %s that serves %s's store %s (the daemon moved)", target.name, m.setting.name, target.store.name)
	case 3:
		if target = m.earlier(); target != nil {
			m.record("write back to an earlier URL, %s's, serving %s at %d", target.name, target.store.name, target.store.seq)
			break
		}
		fallthrough
	default:
		target = m.newDaemon(m.newStore(false, 3+m.random.IntN(18)))
		m.record("write naming another daemon %s in store %s at %d", target.name, target.store.name, target.store.seq)
	}
	m.write(target)
}

// redeployStep: the daemon behind the setting's URL comes to serve another store with no write of
// the setting - the old store, an earlier daemon's store, or a store of its own - and boots there.
func (m *readRuleModel) redeployStep() {
	d := m.setting
	var target *modelStore
	what := ""
	switch m.random.IntN(3) {
	case 0:
		if m.old != nil && m.old.store != d.store {
			target, what = m.old.store, fmt.Sprintf("the old store, %s's", m.old.name)
		}
	case 1:
		var candidates []*modelDaemon
		for _, earlier := range m.daemons {
			if earlier.named && earlier != d && earlier.store != d.store {
				candidates = append(candidates, earlier)
			}
		}
		if len(candidates) > 0 {
			earlier := candidates[m.random.IntN(len(candidates))]
			target, what = earlier.store, fmt.Sprintf("the store of earlier daemon %s", earlier.name)
		}
	}
	if target == nil {
		target, what = m.newStore(false, 3+m.random.IntN(18)), "a store of its own"
	}
	schemaMoved := m.random.IntN(4) == 0
	d.store = target
	event := m.boot(target, schemaMoved)
	m.behind[target] = true
	m.record("redeploy: %s's URL comes to serve %s, %s, with no write of the setting, and boots under run r%d at %d, schema %d",
		d.name, target.name, what, target.run, target.seq, target.schema)
	m.publish(target, false, event)
}

// takeOverStep: the intake stops, and another takes the durable over.
func (m *readRuleModel) takeOverStep() {
	m.record("take-over")
	m.rig.stop()
	m.rig.start()
}

// clockStep: the intake's clock moves on 5 to 70 seconds.
func (m *readRuleModel) clockStep() {
	by := time.Duration(5+m.random.IntN(66)) * time.Second
	m.record("the intake's clock moves on %s", by)
	m.rig.clock.advance(by)
}

// heldStartStep: where the row turns heldStarts on, two steps in 16, drawn from the second stream
// (heldRandom), one in 16 each, are a compound step that makes a held starting read on purpose
// (heldStart): one ends with a third write of the same URL (read-rule-cases row 6.1b), the other
// with the intake's clock moving past the minute and a take-over (row 7.5). It reports whether it
// ran one; a row that leaves heldStarts off runs none and draws nothing.
func (m *readRuleModel) heldStartStep() bool {
	if m.heldRandom == nil {
		return false
	}
	switch m.heldRandom.IntN(16) {
	case 0:
		m.heldStart("a third write of the same URL")
		m.write(m.setting)
	case 1:
		m.heldStart(fmt.Sprintf("the intake's clock moves on %s and a take-over", ReadRetryAfter+time.Second))
		m.rig.clock.advance(ReadRetryAfter + time.Second)
		m.rig.stop()
		m.rig.start()
	default:
		return false
	}
	return true
}

// heldStart records a held starting read's step, whose end is then, and makes a cursor that
// follows no daemon and whose starting read a message holds, where the state it starts from allows
// it. A write of the same URL resets a cursor whose read no message holds, and its wake reads the
// daemon where that read is due. Once the intake is idle, a second write of the same URL resets the
// cursor again, within the minute of that read unless the read stalled or came late, so the
// starting read is not due. The setting's daemon then commits and publishes one event, whose
// message holds the starting read and goes back to the stream for the rest of the minute. Where a
// message held a read already, each write leaves the cursor as it was, and the step ends during
// that hold.
func (m *readRuleModel) heldStart(then string) {
	d, s := m.setting, m.setting.store
	m.record("held starting read: a write of the same URL, %s's, and a second once the intake is idle; %s commits %s and publishes it, which holds the starting read where the cursor follows no daemon and the read is not due; then %s",
		d.name, d.name, span(s, []map[string]any{{"seq": s.seq + 1}}), then)
	m.write(d)
	m.idleAfterWake()
	m.write(d)
	m.idleAfterWake()
	m.publish(s, false, m.commit(s, 1)...)
	m.settle()
	testwait.Holds(readRuleWindow+2*time.Second, 20*time.Millisecond, m.intakeIdle)
}

// idleAfterWake waits a while for the intake to hear the wake a write signalled and to be idle
// after it: the read of the wake, where it made one, has ended.
func (m *readRuleModel) idleAfterWake() {
	testwait.Holds(5*time.Second, 20*time.Millisecond, func() bool { return len(m.rig.wake) == 0 })
	time.Sleep(100 * time.Millisecond)
	testwait.Holds(readRuleWindow+2*time.Second, 20*time.Millisecond, m.intakeIdle)
}

// finish ends the inputs: the setting's daemon answers its reads, publishes its next message where
// no message carried its last events, and the intake's clock moves past ReadRetryAfter. Then the
// cursor must catch up (invariant 2, and the current value of invariant 1), the durable's ack floor
// reach the last message (invariant 3), and invariants 1, 3 and 4 hold over the whole sequence.
func (m *readRuleModel) finish() {
	d, s := m.setting, m.setting.store
	index := m.total
	m.record("end: %s answers its reads", d.name)
	d.reset()
	if s.published < s.seq {
		events := m.commit(s, 1)
		m.record("end: %s publishes %s, since no message carried its events after %d", d.name, span(s, events), s.published)
		m.publish(s, false, events...)
	}
	m.record("end: the intake's clock moves on %s", ReadRetryAfter+time.Second)
	m.rig.clock.advance(ReadRetryAfter + time.Second)
	ended := time.Now()
	m.awaitCaughtUp(index)
	caughtUp := time.Since(ended)
	last := m.rig.lastStreamSeq()
	if !testwait.Holds(readRuleSettle, 50*time.Millisecond, m.rig.finishedThrough(last)) {
		m.record("end: take-over, so each message handed back with a delay comes back at once")
		m.rig.stop()
		m.rig.start()
		if !testwait.Holds(readRuleFloor, 100*time.Millisecond, m.rig.finishedThrough(last)) {
			m.fail(index, 3, "%s after a take-over once INFRA caught up, the durable has not acknowledged every message through stream sequence %d: %+v; hand-backs by stream sequence %v",
				readRuleFloor, last, m.rig.consumerInfo(), m.rig.handBacksOf())
		}
	}
	mostHandBacks := 0
	for _, handBacks := range m.rig.handBacksOf() {
		mostHandBacks = max(mostHandBacks, handBacks)
	}
	m.t.Logf("seed %d: INFRA caught up %s after the inputs stopped, and the durable acknowledged its last message %s after that; %d whole reads, %d hand-backs, %d at the most of one message, %d released after %d",
		m.seed, caughtUp.Round(time.Millisecond), (time.Since(ended) - caughtUp).Round(time.Millisecond), len(m.reads()),
		len(m.rig.logRecords(heldLine)), mostHandBacks, len(m.rig.logRecords(releasedLine)), stateMaxHolds)
	m.checkValues(index)
	m.checkHandBacks(index)
	m.checkReadsApart(index)
}

// awaitCaughtUp waits for INFRA's cursor to reach the setting's daemon's sequence and store, and
// fails invariant 2 unless it does within readRuleCatchUp, or fails it at once where nothing is
// pending and the cursor is behind for good. A failed read that no message holds waits for the
// project's next message: there the daemon publishes it, and the wait starts again.
func (m *readRuleModel) awaitCaughtUp(index int) {
	d, s := m.setting, m.setting.store
	deadline := time.Now().Add(readRuleCatchUp)
	var quietSince time.Time
	nexts := 0
	for {
		c := cursorOf(m.t, m.rig.database, "INFRA")
		if c.stateID != nil && *c.stateID == s.id && c.lastSeq != nil && *c.lastSeq == int64(s.seq) {
			want, _ := json.Marshal(s.phase)
			if got := currentValues(m.t, m.rig.database, "INFRA")["INFRA-1/phase"]; got != string(want) {
				m.fail(index, 1, "INFRA's cursor reached %s's store %s at %d, and INFRA-1's phase is %s, want %s, the daemon's latest", d.name, s.name, s.seq, got, want)
			}
			return
		}
		info := m.rig.consumerInfo()
		if info.NumPending == 0 && info.NumAckPending == 0 && m.intakeIdle() {
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
		} else {
			quietSince = time.Time{}
		}
		behind := fmt.Sprintf("INFRA's cursor is %s, behind %s's store %s (%s) at %d", c, d.name, s.name, s.id, s.seq)
		if !quietSince.IsZero() && time.Since(quietSince) >= time.Second {
			switch {
			case c.readOwed && c.readHolder == nil && nexts < 3 && m.failedReadUnheld():
				nexts++
				events := m.commit(s, 1)
				m.record("end: %s publishes %s, the project's next message, which the failed read that no message holds waits for", d.name, span(s, events))
				m.publish(s, false, events...)
				deadline, quietSince = time.Now().Add(readRuleCatchUp), time.Time{}
				continue
			case !c.readOwed:
				m.fail(index, 2, "%s; it owes no read, and no message or read of INFRA is pending: nothing brings it up. INFRA's whole reads: %s", behind, m.readList())
			case c.readHolder != nil:
				m.fail(index, 2, "%s; it names the message at stream sequence %d as the owed read's holder, and no message of INFRA is pending: that holder never makes the read. INFRA's whole reads: %s",
					behind, *c.readHolder, m.readList())
			case c.readOwed && c.readHolder == nil && m.failedReadUnheld():
				m.fail(index, 2, "%s; it owes a read that no message holds, and its last read failed or a holder was released, but %d next messages of the daemon did not bring the cursor up. INFRA's whole reads: %s",
					behind, nexts, m.readList())
			default:
				m.fail(index, 2, "%s; it owes a read that no message holds, though its last read was good and no holder went back %d times. INFRA's whole reads: %s",
					behind, stateMaxHolds, m.readList())
			}
		}
		if time.Now().After(deadline) {
			m.fail(index, 2, "%s %s after the intake's clock moved past ReadRetryAfter; the durable is %+v. INFRA's whole reads: %s",
				behind, readRuleCatchUp, info, m.readList())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// intakeIdle reports whether the running intake has no whole read of INFRA in flight, no call for
// INFRA's owed read queued and no message of INFRA waiting on its runner.
func (m *readRuleModel) intakeIdle() bool {
	in := m.rig.intake
	if in.readsNow("INFRA") {
		return false
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return !in.calling["INFRA"] && in.waiting.projects["INFRA"] == 0
}

// checkIdleCursor is invariant 2's cheap form, checked after each step: where the intake is idle
// within quiet - no message of INFRA pending or in hand, no whole read in flight and no call for one
// queued - a cursor that owes no read must hold a store and be at the newest message of it that the
// stream delivered (published). A cursor that follows the setting's URL and holds no store takes no
// message (outcomeNoStore) and calls for no read, and one behind a delivered message of its store
// calls for none either, so the project, quiet from here, would stay behind with nothing to bring
// it up. A cursor that follows no URL (a write's reset) is read at the next message, settings
// change or take-over. A burst waits a while for the intake to drain (quiet), since the read it held
// lands and the messages it held back are judged after it; the wait ends early once the only
// messages pending are ones handed back with a delay, which keep the intake from being idle for up
// to ReadRetryAfter.
func (m *readRuleModel) checkIdleCursor(index int, quiet time.Duration) {
	var floor uint64
	var parked bool
	idle := func() bool {
		info := m.rig.consumerInfo()
		floor = info.AckFloor.Stream
		inHand := !m.intakeIdle()
		parked = info.NumPending == 0 && info.NumAckPending > 0 && !inHand
		return info.NumPending == 0 && info.NumAckPending == 0 && !inHand
	}
	parkedPolls := 0
	if !testwait.Holds(quiet, 20*time.Millisecond, func() bool {
		if idle() {
			return true
		}
		if parked {
			parkedPolls++
		} else {
			parkedPolls = 0
		}
		return parkedPolls >= 5
	}) || !idle() {
		return
	}
	seen := floor
	c := cursorOf(m.t, m.rig.database, "INFRA")
	if c.readOwed {
		return
	}
	if c.stateID == nil {
		var stateURL string
		if err := m.rig.database.Pool.QueryRow(context.Background(),
			`select state_url from legion_event_cursors where project = 'INFRA'`).Scan(&stateURL); err != nil {
			m.t.Fatalf("read the URL INFRA's cursor follows: %v", err)
		}
		// The intake did nothing while the cursor was read.
		if stateURL != m.setting.fake.URL || !idle() || floor != seen {
			return
		}
		m.fail(index, 2, "the intake is idle, and INFRA's cursor is %s: it follows %s's URL, holds no store and owes no read, so no message of the daemon is applied and none calls for a read. INFRA's whole reads: %s",
			c, m.setting.name, m.readList())
	}
	if c.lastSeq == nil {
		return
	}
	for _, s := range m.stores {
		if s.id != *c.stateID || s.published <= int(*c.lastSeq) {
			continue
		}
		if !idle() || floor != seen {
			return
		}
		m.fail(index, 2, "the intake is idle, and INFRA's cursor is %s: it owes no read, though the stream delivered %s's messages through %d, past the cursor. A project quiet from here stays behind. INFRA's whole reads: %s",
			c, s.name, s.published, m.readList())
	}
}

// failedReadUnheld reports whether R3's exception can hold of a cursor that owes a read no message
// holds, for the read owed now only: INFRA's last whole read failed, or a holder went back
// stateMaxHolds times (releasedLine) after the last whole read was logged (wholeReadLine).
func (m *readRuleModel) failedReadUnheld() bool {
	released := false
	for line := range strings.Lines(m.rig.logs.String()) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		switch record["msg"] {
		case releasedLine:
			released = true
		case wholeReadLine:
			released = false
		}
	}
	reads := m.reads()
	return (len(reads) > 0 && !reads[len(reads)-1].ok) || released
}

// checkValues fails invariant 1 when INFRA-1's phase took a value no daemon committed, or one of a
// store that no URL ever served behind INFRA's setting.
func (m *readRuleModel) checkValues(index int) {
	for _, raw := range history(m.t, m.rig.database, "INFRA", "INFRA-1", FieldPhase) {
		value := JSONString([]byte(raw))
		s, known := m.values[value]
		switch {
		case !known:
			m.fail(index, 1, "INFRA-1's phase took %s, a value no daemon committed; its history is %v", raw, history(m.t, m.rig.database, "INFRA", "INFRA-1", FieldPhase))
		case !m.behind[s]:
			m.fail(index, 1, "INFRA-1's phase took %s, an event of store %s, which no URL ever served behind INFRA's setting; its history is %v",
				raw, s.name, history(m.t, m.rig.database, "INFRA", "INFRA-1", FieldPhase))
		}
	}
}

// checkHandBacks fails invariant 3 when a hold goes back to the stream more than stateMaxHolds
// times. The cursor counts the hand-backs of each hold (read_holds), and heldLine logs that count
// as handBack, so the bound is per hold, not per stream sequence: each handBack is stateMaxHolds at
// the most, and one message's count may start again (a handBack at or below the one it logged
// before) only once its hold has ended - its release after stateMaxHolds hand-backs (releasedLine),
// a write that ended it (writeReleasedLine), a heldLine of another message, since a cursor names
// one holder at a time, or a good whole read while it held the read - for example when a take-over
// replays it from the durable's ack floor. The holder is the message that logged heldLine last. A
// good read ends, for this check, the hold of that message: such a read clears the holder unless a
// sequence skipped past the cap above its stateSeq keeps the gap read owed, and the log cannot show
// which, so the check lets that holder's count start again. A good read also ends the hold of a
// message whose two hand-backs its record lands between, by its legion_reads row's recorded_at
// against each heldLine's time, whatever the log's order: the read's own line comes only after
// its record commits, and a message judged beside the project's runner in that gap (judgeCrowded,
// row 8.1) can go back first, on the cursor the read left.
func (m *readRuleModel) checkHandBacks(index int) {
	recorded := m.goodReadsRecorded()
	last, ended := map[uint64]float64{}, map[uint64]bool{}
	lastAt := map[uint64]time.Time{}
	var holder uint64
	for line := range strings.Lines(m.rig.logs.String()) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		streamSeq, _ := record["streamSeq"].(float64)
		seq := uint64(streamSeq)
		switch record["msg"] {
		case heldLine:
			handBack, _ := record["handBack"].(float64)
			if handBack > stateMaxHolds {
				m.fail(index, 3, "the message at stream sequence %d went back to the stream as hand-back %v of its hold, want %d at the most: %s",
					seq, handBack, stateMaxHolds, strings.TrimSpace(line))
			}
			at := logTime(record)
			if before, held := last[seq]; held && handBack <= before && !ended[seq] && !recordedBetween(recorded, lastAt[seq], at) {
				m.fail(index, 3, "the message at stream sequence %d went back to the stream as hand-back %v after hand-back %v, and nothing ended its hold between: %s",
					seq, handBack, before, strings.TrimSpace(line))
			}
			if holder != 0 && holder != seq {
				ended[holder] = true
			}
			last[seq], ended[seq], lastAt[seq], holder = handBack, false, at, seq
		case releasedLine:
			if handBacks, _ := record["handBacks"].(float64); handBacks > stateMaxHolds {
				m.fail(index, 3, "the message at stream sequence %d was released after %v hand-backs, want %d at the most: %s",
					seq, handBacks, stateMaxHolds, strings.TrimSpace(line))
			}
			ended[seq] = true
		case writeReleasedLine:
			ended[seq] = true
		case wholeReadLine:
			if record["ok"] == true && holder != 0 {
				ended[holder] = true
			}
		}
	}
}

// goodReadsRecorded is when Dispatch recorded each good whole read of INFRA (legion_reads'
// recorded_at, Postgres's clock at the start of the record's transaction), oldest first.
func (m *readRuleModel) goodReadsRecorded() []time.Time {
	rows, err := m.rig.database.Pool.Query(context.Background(), `
		select recorded_at from legion_reads where project = 'INFRA' and kind = $1 and ok order by recorded_at
	`, readKindRead)
	if err != nil {
		m.t.Fatalf("read when INFRA's good whole reads were recorded: %v", err)
	}
	times, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		m.t.Fatalf("read when INFRA's good whole reads were recorded: %v", err)
	}
	return times
}

// recordedBetween reports whether a time of recorded falls after from and at or before to. A zero
// from or to, a log line with no time, is no span.
func recordedBetween(recorded []time.Time, from, to time.Time) bool {
	if from.IsZero() || to.IsZero() {
		return false
	}
	return slices.ContainsFunc(recorded, func(at time.Time) bool { return at.After(from) && !at.After(to) })
}

// logTime is the time a JSON log record carries (slog's "time"), and the zero time for a record
// with none.
func logTime(record map[string]any) time.Time {
	text, _ := record["time"].(string)
	at, _ := time.Parse(time.RFC3339Nano, text)
	return at
}

// readRuleRead is one whole read of INFRA: its legion_reads row and its start on the intake's
// clock (readStartsTrigger), nil where none was recorded beside the row.
type readRuleRead struct {
	id      int64
	reason  string
	ok      bool
	at      time.Time
	started *time.Time
}

func (r readRuleRead) String() string {
	started := "none"
	if r.started != nil {
		started = r.started.Format("15:04:05.000")
	}
	return fmt.Sprintf("{%d %s ok=%v started=%s at=%s}", r.id, r.reason, r.ok, started, r.at.Format("15:04:05.000"))
}

// reads is INFRA's whole reads, oldest first, each with its start (readStartsTrigger).
func (m *readRuleModel) reads() []readRuleRead {
	rows, err := m.rig.database.Pool.Query(context.Background(), `
		select r.id, r.reason, r.ok, r.read_at, s.started
		from legion_reads r
		left join lateral (select started from read_rule_read_starts where read_id = r.id order by id desc limit 1) s on true
		where r.project = 'INFRA' and r.kind = $1
		order by r.id
	`, readKindRead)
	if err != nil {
		m.t.Fatalf("read INFRA's whole reads: %v", err)
	}
	defer rows.Close()
	var reads []readRuleRead
	for rows.Next() {
		var read readRuleRead
		var reason *string
		if err := rows.Scan(&read.id, &reason, &read.ok, &read.at, &read.started); err != nil {
			m.t.Fatalf("scan INFRA's whole reads: %v", err)
		}
		if reason != nil {
			read.reason = *reason
		}
		reads = append(reads, read)
	}
	if err := rows.Err(); err != nil {
		m.t.Fatalf("read INFRA's whole reads: %v", err)
	}
	return reads
}

// readList is INFRA's whole reads as a failure names them.
func (m *readRuleModel) readList() string {
	return fmt.Sprint(m.reads())
}

// checkReadsApart fails invariant 4 when two consecutive whole reads of INFRA started less than
// ReadRetryAfter apart, whatever their reasons: a starting read and the read right after a good
// starting read included (read-rule-cases v11, rule 4).
func (m *readRuleModel) checkReadsApart(index int) {
	reads := m.reads()
	for i := 1; i < len(reads); i++ {
		earlier, later := reads[i-1], reads[i]
		if earlier.started == nil || later.started == nil {
			m.fail(index, 4, "whole read %v or %v records no start; INFRA's whole reads: %v", earlier, later, reads)
		}
		if apart := later.started.Sub(*earlier.started); apart < ReadRetryAfter {
			m.fail(index, 4, "whole read %v started %s after %v, want at least %s; INFRA's whole reads: %v", later, apart, earlier, ReadRetryAfter, reads)
		}
	}
}

// fail fails the sequence at step index on invariant, naming the seed, the invariant, every step
// drawn so far and how to run the sequence alone (alone), and printing the intake's log lines.
func (m *readRuleModel) fail(index, invariant int, format string, args ...any) {
	m.t.Helper()
	m.failed = true
	m.t.Fatalf("seed %d, step %d of %d: invariant %d (%s) fails: %s\nreproduce it alone with %s\n%s\nthe intake's log:\n%s",
		m.seed, index, m.total, invariant, readRuleInvariants[invariant], fmt.Sprintf(format, args...), m.alone(), m.stepList(), m.logDigest())
}

// alone is how to run the sequence alone. A seed that a variable names draws the held starting
// read's steps (heldStartStep), as a fixed row with heldStarts on does, and a fixed row with
// heldStarts off does not, so such a row runs alone by its subtest's name, with its steps and no
// seed variable set.
func (m *readRuleModel) alone() string {
	if m.heldRandom != nil {
		return fmt.Sprintf("LEGION_READ_RULE_SEED=%d LEGION_READ_RULE_STEPS=%d", m.seed, m.total)
	}
	return fmt.Sprintf("-run '^%s$' with LEGION_READ_RULE_STEPS=%d and neither LEGION_READ_RULE_SEED nor LEGION_READ_RULE_SEEDS set",
		m.t.Name(), m.total)
}

// stepList is the steps drawn so far, one line each.
func (m *readRuleModel) stepList() string {
	var list strings.Builder
	list.WriteString("steps:")
	for index, step := range m.steps {
		fmt.Fprintf(&list, "\n  %d: %s", index, step)
		if index < len(m.took) {
			fmt.Fprintf(&list, " [%s]", m.took[index].Round(10*time.Millisecond))
		}
	}
	return list.String()
}

// logDigest is every line the rig's intakes logged, without its time and level.
func (m *readRuleModel) logDigest() string {
	var digest strings.Builder
	for line := range strings.Lines(m.rig.logs.String()) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		delete(record, "time")
		delete(record, "level")
		message, _ := record["msg"].(string)
		delete(record, "msg")
		encoded, _ := json.Marshal(record)
		fmt.Fprintf(&digest, "  %s %s\n", strings.TrimPrefix(message, "legion intake: "), encoded)
	}
	return digest.String()
}
