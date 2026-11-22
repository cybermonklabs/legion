package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// statusKey is one key of the selection bucket, as its last message holds it.
type statusKey struct {
	// Key is the KV key: a session id, a listener durable's selection.DurableKey, or a system key
	// (selection.SentinelKey, hold.barrier.<hold id>).
	Key string
	// Seq is the stream sequence of the key's last message, which is the record's revision.
	Seq uint64
	// Deleted is a last message that is a delete or a purge marker; Value is then nil.
	Deleted bool
	// Value is the last message's data.
	Value []byte
}

// statusSelectionKeys reads every key of the selection bucket on nc, by sequence: STREAM.INFO of
// its stream, then STREAM.MSG.GET from the first sequence with next_by_subj on the bucket's
// subjects, each answer's sequence + 1 the next request's, until the server answers that no
// message is left (jetstream.JSErrCodeMessageNotFound). These are the two calls
// natsgrant.Operator(released) names on the bucket (natsgrant.go operatorReleased), so status,
// which runs in phase released, reads with no key listing. The last message of each key is kept: a
// key written again during the walk is met again past the end it had, so the walk ends only once
// it has passed every key's last write. A missing bucket wraps selection.ErrNoStore; a walk that
// cannot end, a refused request or an answer that does not decode is *Unknown. The keys are
// sorted.
func statusSelectionKeys(ctx context.Context, nc *nats.Conn) ([]statusKey, error) {
	stream := selectionStreamName
	info, err := streamInfo(ctx, nc, stream)
	if apiErrorIs(err, jetstream.JSErrCodeStreamNotFound) {
		return nil, fmt.Errorf("%w: %s", selection.ErrNoStore, stream)
	}
	if err != nil {
		return nil, unknownf(err, "the info of %s", stream)
	}
	keys := map[string]statusKey{}
	next := max(info.State.FirstSeq, 1)
	// The walk reads each message once, and a key written during it once more: twice the
	// messages the stream held, and a margin, bound it.
	for range 2*info.State.Msgs + 10_000 {
		raw, err := apiRequest(ctx, nc, "$JS.API.STREAM.MSG.GET."+stream, map[string]any{"seq": next, "next_by_subj": selectionKeyPrefix + ">"})
		if apiErrorIs(err, jetstream.JSErrCodeMessageNotFound) {
			return slices.SortedFunc(maps.Values(keys), func(a, b statusKey) int { return strings.Compare(a.Key, b.Key) }), nil
		}
		if err != nil {
			return nil, unknownf(err, "the message of %s at or after sequence %d", stream, next)
		}
		// An answer that holds no message decodes as sequence 0, which is before next.
		var answer msgGetAnswer
		if err := json.Unmarshal(raw, &answer); err != nil || answer.Message.Seq < next {
			return nil, unknownf(err, "the answer for %s at or after sequence %d holds no message past it", stream, next)
		}
		m := answer.Message
		op, err := kvOperation(m.Header)
		if err != nil {
			return nil, unknownf(err, "the header of %s's message %d", stream, m.Seq)
		}
		key := statusKey{Key: strings.TrimPrefix(m.Subject, selectionKeyPrefix), Seq: m.Seq, Value: m.Data}
		if op == "DEL" || op == "PURGE" {
			key.Deleted, key.Value = true, nil
		}
		keys[key.Key] = key
		next = m.Seq + 1
	}
	return nil, unknownf(nil, "the walk of %s did not end", stream)
}

// statusFamily is one family's store as status read it.
type statusFamily struct {
	family string
	// where names the store: the bucket's stream or the table.
	where string
	// present is a store that exists: the bucket, or the table.
	present bool
	// state is the store's sentinel, nil where none was read.
	state *selection.StoreState
	// records are the family's records by logical name.
	records map[string]selection.Record
	// deleted are keys whose last write is a delete or a purge marker.
	deleted []string
	// malformed are keys whose value is not exactly a value of their kind (readSelectionValue),
	// each with why: status reads none of them, and reports each UNKNOWN.
	malformed map[string]string
	// gone names a daemon store the census's rule found gone (daemonGoneText), "" for any other.
	gone string
}

// statusRuleSet is the rules status reads a store by. Every field set is an earlier rule, which
// only a row's control sets, to show that the row fails under it.
type statusRuleSet struct {
	// goneUnknown: a daemon store whose database answers SQLSTATE 3D000 is a failed read, UNKNOWN,
	// as every other (status before it read such a store by the census's rule).
	goneUnknown bool
}

// statusRules are the rules status runs by; a row's control alone changes them.
var statusRules statusRuleSet

// status reads, with the row released, every family's store and the notification stream's
// consumers, and prints: the row; with --hold, that hold's journal; the broker's cutover rows;
// each store's sentinel; each record, with its current and whether the stream holds it, its open
// operation, start, last floor, retired names and restore; and each blocker: a store or sentinel
// that is missing, a record deleted, a record its family's creator refuses (checkRecordFence,
// naming the field and the name), a current the stream does not hold, an open operation, and a
// fixed name still on the stream. It changes nothing. A complete reading exits 0, blockers or
// not, and ends with their count; a store it could not read (no flag names it, or a read failed)
// or a listing it could not complete is UNKNOWN, exit 2, after it has printed what it read.
func (r *run) status(ctx context.Context) error {
	row, err := r.requirePhase(ctx, natsgrant.OperatorPhaseReleased)
	if err != nil {
		return err
	}
	out := r.stdout
	fmt.Fprintf(out, "row: gen %d, phase %s, hold %q\n", row.Gen, row.Phase, row.HoldID)
	if r.o.holdID != "" {
		d, gen, err := r.records.Journal(ctx, r.o.holdID)
		if err != nil {
			return unknownf(err, "hold %s's journal", r.o.holdID)
		}
		fmt.Fprintf(out, "hold %s: workflow %s, record gen %d, complete %t, hold_gen %d\n", r.o.holdID, d.Workflow, gen, d.Complete, d.HoldGen)
		for i, op := range d.Ops {
			fmt.Fprintf(out, "  %d. %s %s\n", i+1, op.Command, op.State)
		}
	}
	cutovers, err := r.records.Cutovers(ctx)
	if err != nil {
		return unknownf(err, "the broker's cutover rows")
	}
	slices.SortFunc(cutovers, func(a, b holdrecord.CutoverRow) int { return strings.Compare(a.Family, b.Family) })
	for _, c := range cutovers {
		fmt.Fprintf(out, "cutover %s: %s (hold %s, hold_gen %d)\n", c.Family, c.State, c.HoldID, c.HoldGen)
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return unknownf(err, "the hub")
	}
	names, err := consumerNames(ctx, nc, notifications)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, name := range names {
		held[name] = true
	}

	var unknown, blockers []string
	families := []statusFamily{}
	session, err := statusSessionFamily(ctx, nc)
	if err != nil {
		unknown = append(unknown, err.Error())
	} else {
		families = append(families, session)
	}
	for _, family := range pgFamilies(r.o, cutovers, names...) {
		got, err := r.statusPGFamily(ctx, family)
		if err != nil {
			unknown = append(unknown, err.Error())
			continue
		}
		families = append(families, got)
	}
	for _, f := range families {
		blockers = append(blockers, statusPrintFamily(out, f, held)...)
		for _, key := range slices.Sorted(maps.Keys(f.malformed)) {
			unknown = append(unknown, fmt.Sprintf("%s %s: its value in %s is malformed, so status does not read it: %s", f.family, key, f.where, f.malformed[key]))
		}
	}
	for _, name := range names {
		if m, ok := classify(name); ok {
			blockers = append(blockers, fmt.Sprintf("%s: the fixed name %s is on %s", m.Family, name, notifications))
		}
	}
	for _, b := range blockers {
		fmt.Fprintf(out, "BLOCKER %s\n", b)
	}
	if len(unknown) > 0 {
		return &Unknown{What: strings.Join(unknown, "; ")}
	}
	fmt.Fprintf(out, "blockers: %d\n", len(blockers))
	return nil
}

// statusSessionFamily is the session family's store read whole: the selection bucket's sentinel
// and every record of a session key (selection.CheckKey) or a durable key
// (selection.CheckDurableKey), each value read by its key's kind (readSelectionValue), the barrier
// keys' too. A malformed value is kept by its key and why, never decoded leniently. A missing
// bucket is a store status reports as missing, not an error; a walk that is not whole is.
func statusSessionFamily(ctx context.Context, nc *nats.Conn) (statusFamily, error) {
	f := statusFamily{family: familySession, where: selectionStreamName, records: map[string]selection.Record{}, malformed: map[string]string{}}
	keys, err := statusSelectionKeys(ctx, nc)
	if errors.Is(err, selection.ErrNoStore) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	for _, key := range keys {
		recordKey := selection.CheckKey(key.Key) == nil || selection.CheckDurableKey(key.Key) == nil
		switch {
		case key.Deleted && recordKey:
			f.deleted = append(f.deleted, key.Key)
			continue
		case key.Deleted:
			continue
		}
		value, err := readSelectionValue(key)
		switch {
		case err != nil:
			f.malformed[key.Key] = err.Error()
		case value.state != nil:
			f.state = value.state
		case value.record != nil:
			f.records[key.Key] = *value.record
		}
	}
	f.present = true
	return f, nil
}

// statusPGFamily reads family's intake_consumers rows through the family's store: its sentinel row
// and every record (selection.PGStore.List). A family whose store no flag names is an error, which
// status reports as UNKNOWN; a missing table is a missing store; a sentinel row that is not
// exactly the family's own (selection.ErrMalformedSentinel) is a failed read, UNKNOWN, never no
// marker; and a daemon store whose database answers SQLSTATE 3D000 is read again by the census's
// rule (statusGone).
func (r *run) statusPGFamily(ctx context.Context, family string) (statusFamily, error) {
	f := statusFamily{family: family, where: selection.IntakeConsumersTable, records: map[string]selection.Record{}}
	if file, flagName := r.o.storeFile(family); file == "" {
		return f, fmt.Errorf("%s's store: no %s names it", family, flagName)
	}
	store, err := r.pgStoreOf(ctx, family)
	if err != nil {
		return f, err
	}
	state, err := store.State(ctx)
	switch {
	case errors.Is(err, selection.ErrNoStore):
		// The table is missing: a store status reports as missing.
		return f, nil
	case errors.Is(err, selection.ErrNoSentinel):
	case err != nil:
		f.gone, err = r.statusGone(ctx, family, err)
		return f, err
	default:
		f.state = &state
	}
	records, err := store.List(ctx)
	if err != nil {
		return f, fmt.Errorf("%s's records: %w", family, err)
	}
	f.records = records
	f.present = true
	return f, nil
}

// statusGone answers err, the failed first read of family's store, as status reports it: a daemon
// store whose database answers SQLSTATE 3D000 (censusUndefinedDatabase) is read again by the
// census's rule for the daemon stores, at exactly the address the daemon's one source names, its
// Deployment or its entry of the retired projects' ConfigMap (daemonStoreByRule), and is gone,
// named by daemonGoneText, when the server there answers 3D000; status then goes on, so a broker
// cutover row naming a deleted project, which no command removes,
// does not keep every reading UNKNOWN. Anything else the rule reads is an error naming why, and so
// is any other failure: status stays UNKNOWN on the store.
func (r *run) statusGone(ctx context.Context, family string, err error) (string, error) {
	project, daemon := strings.CutPrefix(family, familyDaemonPrefix)
	var pg *pgconn.PgError
	if !daemon || statusRules.goneUnknown || !errors.As(err, &pg) || pg.Code != censusUndefinedDatabase {
		return "", fmt.Errorf("%s's sentinel: %w", family, err)
	}
	store, rerr := r.daemonStoreByRule(ctx, project)
	switch {
	case rerr != nil:
		return "", fmt.Errorf("%s's store answers SQLSTATE %s, and the census's rule could not read it again (its answer: %v): %w", family, pg.Code, pg, rerr)
	case store.gone != nil:
		return daemonGoneText(*store.gone), nil
	case store.unreconciled != "":
		return "", fmt.Errorf("%s's store answers SQLSTATE %s, and the census's rule does not confirm it gone: %s (its answer: %v)", family, pg.Code, store.unreconciled, pg)
	}
	return "", fmt.Errorf("%s's store answers SQLSTATE %s, and the census's rule does not confirm it gone: it answers at the address %s names (its answer: %v)", family, pg.Code, store.addressSource(), pg)
}

// statusPrintFamily prints f's store and records, each malformed key as UNKNOWN, and answers its
// blockers.
func statusPrintFamily(out io.Writer, f statusFamily, held map[string]bool) []string {
	var blockers []string
	_, sentinelMalformed := f.malformed[selection.SentinelKey]
	switch {
	case f.gone != "":
		fmt.Fprintf(out, "store %s: gone: %s\n", f.family, f.gone)
		return nil
	case !f.present:
		fmt.Fprintf(out, "store %s: missing\n", f.family)
		return append(blockers, fmt.Sprintf("%s: its store is missing", f.family))
	case sentinelMalformed:
		fmt.Fprintf(out, "store %s (%s): sentinel UNKNOWN, its value is malformed\n", f.family, f.where)
	case f.state == nil:
		fmt.Fprintf(out, "store %s (%s): no sentinel\n", f.family, f.where)
		blockers = append(blockers, fmt.Sprintf("%s: its store has no sentinel", f.family))
	default:
		s := f.state
		fmt.Fprintf(out, "store %s (%s): cutover=%s boundary=%d replay_from=%d orphans=%v hold=%s\n", f.family,
			f.where, statusOrNone(s.Cutover), s.Boundary, s.ReplayFrom, s.Orphans, statusOrNone(s.Hold))
	}
	for _, key := range slices.Sorted(maps.Keys(f.malformed)) {
		if key != selection.SentinelKey {
			fmt.Fprintf(out, "record %s %s: UNKNOWN, its value is malformed\n", f.family, key)
		}
	}
	for _, key := range f.deleted {
		fmt.Fprintf(out, "record %s %s: deleted\n", f.family, key)
		blockers = append(blockers, fmt.Sprintf("%s %s: its record's last write is a delete marker", f.family, key))
	}
	for _, logical := range slices.Sorted(maps.Keys(f.records)) {
		record := f.records[logical]
		fmt.Fprintf(out, "record %s %s: %s\n", f.family, logical, statusRecordLine(record, held))
		if err := checkRecordFence(f.family, logical, record); err != nil {
			blockers = append(blockers, fmt.Sprintf("%s %s: its family's creator refuses the record: %v", f.family, logical, err))
		}
		if record.Current != "" && !held[record.Current] {
			blockers = append(blockers, fmt.Sprintf("%s %s: its current %s is not on %s", f.family, logical, record.Current, notifications))
		}
		if op := record.Op; op != nil {
			blockers = append(blockers, fmt.Sprintf("%s %s: an open %s (candidate %s, old %s, owner %s, reserved %s)", f.family, logical,
				op.Kind, statusOrNone(op.Candidate), statusOrNone(op.Old), statusOrNone(op.Owner), op.ReservedAt.UTC().Format(time.RFC3339)))
		}
	}
	return blockers
}

// statusRecordLine is one record as status prints it.
func statusRecordLine(record selection.Record, held map[string]bool) string {
	on := func(name string) string {
		if held[name] {
			return name + " (on the stream)"
		}
		return name + " (absent)"
	}
	parts := []string{"current -"}
	if record.Current != "" {
		parts[0] = "current " + on(record.Current)
	}
	if op := record.Op; op != nil {
		parts = append(parts, fmt.Sprintf("op %s candidate %s old %s", op.Kind, statusOrNone(op.Candidate), statusOrNone(op.Old)))
	}
	parts = append(parts, fmt.Sprintf("start %d", record.Start))
	if lf := record.LastFloor; lf != nil {
		parts = append(parts, fmt.Sprintf("last_floor %d of %s on stream %s", lf.Floor, lf.Name, statusOrNone(lf.StreamID)))
	}
	if len(record.Retired) > 0 {
		retired := make([]string, len(record.Retired))
		for i, name := range record.Retired {
			retired[i] = on(name)
		}
		parts = append(parts, "retired ["+strings.Join(retired, ", ")+"]")
	}
	if rs := record.Restore; rs != nil {
		parts = append(parts, fmt.Sprintf("restore target %s start %d phase %s", rs.Target, rs.Start, rs.Phase))
	}
	return strings.Join(parts, ", ")
}

// statusOrNone is s, or "-" for an empty one.
func statusOrNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
