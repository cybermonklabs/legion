package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The hold's convert record (holdrecord.Client.PutRecord): kind convert, key set, the saved set
// and its complete mark.
const (
	convertSetKind = "convert"
	convertSetKey  = "set"
)

// convertSettleAge is Step 0's age (selection.Config.SettleAge's default): an open operation
// whose candidate the server does not hold is settled only once it is this old, so a create still
// in flight from the process that reserved it is never answered by a second create.
const convertSettleAge = 2 * time.Second

// convertListings bounds how many listings convert makes after its conversions, each converting
// what it lists, before a listing that still holds a fixed name is UNKNOWN.
const convertListings = 8

// convertConflicts bounds how often one name's record is read again after a refused write.
const convertConflicts = 16

// convertSet is the hold's convert record: every fixed name convert converts, saved before its
// first mutation, with the configuration it read from each one's info, so a rerun after a crash
// between F's delete and N's create still creates N from F's configuration, and restore --prepare
// copies it into its own saved set, from which restore --finish creates F again; and the complete
// mark, set once a listing holds no fixed name and every family's broker cutover row and store
// marker read done.
type convertSet struct {
	Names    []convertSetName `json:"names"`
	Complete bool             `json:"complete"`
}

// convertSetName is one fixed name of the saved set.
type convertSetName struct {
	Family  string `json:"family"`
	Logical string `json:"logical"`
	Fixed   string `json:"fixed"`
	// Config is F's configuration as its info answered it, nil for a cohort member whose F was
	// gone when the set was saved.
	Config *jetstream.ConsumerConfig `json:"config,omitempty"`
}

// has reports whether the set holds fixed.
func (s convertSet) has(fixed string) bool {
	return slices.ContainsFunc(s.Names, func(n convertSetName) bool { return n.Fixed == fixed })
}

// readConvertSet reads hold holdID's convert record and its gen, and false when it has none.
func readConvertSet(ctx context.Context, records *holdrecord.Client, holdID string) (convertSet, uint64, bool, error) {
	return readHoldRecord[convertSet](ctx, records, holdID, convertSetKind, convertSetKey,
		fmt.Sprintf("hold %s's convert record does not decode", holdID))
}

// convertRule is convert's rule, with the seam its meta-leader row's control flips.
type convertRule struct {
	// noTermCheck takes each listing after the meta barrier alone, outside the barrier's two
	// readings of the meta group, so a listing a meta leader other than the barrier's answered is
	// not discarded (TestForcedOrderAMetaLeaderChangeAfterTheBarrierDiscardsTheListing's control).
	noTermCheck bool
	// listed, when set, runs after each listing, with its names, before the meta group is read
	// again: the row moves the meta leader there.
	listed func(ctx context.Context, names []string)
}

// convert is `consumer-names convert --hold <id>` (§3 step 4), in the cutover's held row: it
// collects every fixed name behind the meta barrier and saves the set before any mutation,
// converts each logical name to a fresh name at its safe start (convertRule.convertName), lists
// again until a listing holds no fixed name, then writes each family's broker cutover row done
// and its store's marker cutover = done, marks the set complete, and writes its done entry. It is
// resumable: a rerun settles an open operation (Step 0) and goes on from the saved set. Every exit
// after its intent ends in its stop (admission.end): its own refusals name what they found
// (convertLeft), and any other refusal or UNKNOWN records what convert's runs left by
// convertProgress, an UNKNOWN under leftUnknown, so the hold always has its way out.
func (r *run) convert(ctx context.Context) error {
	err := convertRule{}.run(ctx, r)
	return connRefusal(r.nc, err)
}

// convertProgress is what a run of convert knows of its saved set since its intent, which the
// stop of an exit that wrote none records (account).
type convertProgress struct {
	set convertSet
	// saved: a run of convert in this hold saved set, so every change it made can follow.
	saved bool
}

// account is this run's account of what convert's runs in this hold left, for its stop
// (admission.end): unchanged while no run saved its set, since every change a run makes comes
// after that save (convertLeft's rule), else convertLeft's record of the saved set.
func (p convertProgress) account(holdID string) (bool, string) {
	if !p.saved {
		return true, "no run of convert in this hold saved its set, and every change a run makes comes after that save"
	}
	return false, convertLeft(holdID, p.set, true)
}

// convertNoStart is a logical name convert has no safe start for, and why.
type convertNoStart struct {
	logical, why string
}

func (c convertRule) run(ctx context.Context, r *run) error {
	a, err := r.admit(ctx, actionConvert, nil)
	if err != nil {
		return err
	}
	var p convertProgress
	err = c.held(ctx, r, a, &p)
	unchanged, account := p.account(r.o.holdID)
	return a.end(ctx, err, unchanged, account)
}

// held is convert's work after its intent, what it knows of its saved set kept in p.
func (c convertRule) held(ctx context.Context, r *run, a *admission, p *convertProgress) error {
	// stop is convert's stop for reason, its left read from p at the stop (convertLeft): nothing
	// while no run of convert saved the set, else the saved set.
	stop := func(reason string) error {
		return a.stop(ctx, reason, convertLeft(r.o.holdID, p.set, p.saved))
	}
	// checkStores stops convert when a family's store refuses p's set (convertRule.checkStores).
	checkStores := func() error {
		refusal, err := c.checkStores(ctx, r, p.set)
		if err != nil || refusal == "" {
			return err
		}
		return stop(refusal)
	}
	cohort, _, found, err := readPrepareCohort(ctx, r.records, r.o.holdID)
	if err != nil {
		return err
	}
	if !found {
		return stop(fmt.Sprintf("hold %s has no cohort record: consumer-names prepare --hold %s did not run before hold set, so no fixed name's floor before the hold is known", r.o.holdID, r.o.holdID))
	}
	r.read(fmt.Sprintf("hold %s's cohort record", r.o.holdID))
	nc, _, err := r.conn(ctx)
	if err != nil {
		return unknownf(err, "the operator's connection")
	}
	op, err := r.operatorHub(ctx)
	if err != nil {
		return err
	}
	declared, err := writerhold.Discover(ctx, op.Hub)
	if err != nil {
		return hubError(err, "the declared servers")
	}
	r.read("the declared servers")
	barrier := writerhold.MetaBarrier{Hub: op.Hub, Declared: declared, HoldID: r.o.holdID}

	set, gen, found, err := readConvertSet(ctx, r.records, r.o.holdID)
	if err != nil {
		return err
	}
	if found {
		p.set, p.saved = set, true
		r.read(fmt.Sprintf("convert's saved set of %d fixed names", len(set.Names)))
	} else {
		listed, err := c.list(ctx, nc, barrier)
		if err != nil {
			return err
		}
		r.read("a listing of " + notifications + " behind the meta barrier")
		if p.set, err = c.collect(ctx, nc, cohort, listed); err != nil {
			return err
		}
		r.read("the info of each fixed name it listed")
		p.set = c.withDispatch(r, p.set)
	}
	if err := checkStores(); err != nil {
		return err
	}
	r.read("the store of each of its families: " + strings.Join(c.families(r, p.set), ", "))
	if !found {
		if gen, err = c.saveSet(ctx, r, p.set, 0); err != nil {
			return err
		}
		p.saved = true
	}
	// Before its first mutation of the hub, the held re-read again (plan §3a, "Every held
	// command").
	if err := a.reread(ctx); err != nil {
		return err
	}

	var noStart []convertNoStart
	converted := 0
	// convertOne converts one saved name (convertName), counting it in converted when it answers
	// done, and in noStart when it has no safe start.
	convertOne := func(name convertSetName) error {
		done, missing, err := c.convertName(ctx, r, nc, name, cohort.member(name.Fixed))
		if err != nil {
			return err
		}
		if missing != nil {
			noStart = append(noStart, *missing)
		}
		if done {
			converted++
		}
		return nil
	}
	for _, name := range p.set.Names {
		if err := convertOne(name); err != nil {
			return err
		}
	}
	if len(noStart) > 0 {
		return stop(convertNoStartReason(r.o.holdID, noStart))
	}
	r.read(fmt.Sprintf("the record and consumers of each of the %d saved names", len(p.set.Names)))

	// List again: a fixed name still present is converted, whatever its family's marker says, and
	// convert goes on only from a listing that holds none.
	clean := false
	for listing := range convertListings {
		listed, err := c.list(ctx, nc, barrier)
		if err != nil {
			return err
		}
		r.read(fmt.Sprintf("listing %d of %s after the conversions", listing+1, notifications))
		var fixed []convertSetName
		for _, name := range listed {
			if m, ok := classify(name); ok {
				fixed = append(fixed, convertSetName{Family: m.Family, Logical: m.Logical, Fixed: name})
			}
		}
		if len(fixed) == 0 {
			clean = true
			break
		}
		added := false
		for _, name := range fixed {
			if p.set.has(name.Fixed) {
				continue
			}
			entry, err := convertEntry(ctx, nc, name)
			if err != nil {
				return err
			}
			p.set.Names = append(p.set.Names, entry)
			added = true
		}
		if added {
			if err := checkStores(); err != nil {
				return err
			}
			if gen, err = c.saveSet(ctx, r, p.set, gen); err != nil {
				return err
			}
		}
		for _, name := range fixed {
			for _, saved := range p.set.Names {
				if saved.Fixed != name.Fixed {
					continue
				}
				if err := convertOne(saved); err != nil {
					return err
				}
			}
		}
		if len(noStart) > 0 {
			return stop(convertNoStartReason(r.o.holdID, noStart))
		}
	}
	if !clean {
		return unknownf(nil, "%s still listed a fixed name after %d listings that each converted what they listed", notifications, convertListings)
	}

	// A daemon's first create of a new project starts at the stream's end as the cutover leaves it,
	// which its store's sentinel keeps, so the daemon reads no stream state for it.
	stream, err := streamInfo(ctx, nc, notifications)
	if err != nil {
		return unknownf(err, "%s's state for the daemons' boundary", notifications)
	}
	r.read(notifications + "'s state for the daemons' boundary")
	daemon := convertDaemonMark{boundary: stream.State.LastSeq + 1, streamID: selection.StreamIdentity(stream)}
	session := convertSessionMark{boundary: cohort.Boundary, hold: r.o.holdID}
	families := c.families(r, p.set)
	for _, family := range families {
		if _, err := r.records.PutCutover(ctx, family, selection.CutoverDone, r.o.holdID); err != nil {
			return fmt.Errorf("write the %s family's broker cutover row: %w", family, err)
		}
		if err := c.markDone(ctx, r, family, daemon, session); err != nil {
			return err
		}
		r.read(family + "'s store marker")
	}
	p.set.Complete = true
	if _, err = c.saveSet(ctx, r, p.set, gen); err != nil {
		return err
	}
	if err := a.done(ctx, map[string]string{"converted": strconv.Itoa(converted), "names": strconv.Itoa(len(p.set.Names)),
		"families": strings.Join(families, ",")}); err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "hold %s: converted %d of %d fixed names; the final listing holds no fixed name; cutover done for %s\n",
		r.o.holdID, converted, len(p.set.Names), strings.Join(families, ", "))
	return nil
}

// convertNoStartReason is the stop entry's reason for the logical names convert has no safe start
// for: each one's way out is a replay of it under the hold, then convert again.
func convertNoStartReason(holdID string, names []convertNoStart) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "no safe start for " + n.logical + " (" + n.why + ")"
	}
	return strings.Join(parts, "; ") + fmt.Sprintf(": give each one with consumer-names replay --key <K> --from <seq> --hold %s, then run convert --hold %s again", holdID, holdID)
}

// convertLeft is what a stop of convert left outside the journal: holdrecord.LeftUnchanged while no
// run of convert in this hold has saved its set, since every change a run makes (the bucket
// barrier's put, a record, a consumer's delete or create, a marker) comes after that save; else the
// saved set and whatever every run of convert made of it.
func convertLeft(holdID string, set convertSet, saved bool) string {
	if !saved {
		return holdrecord.LeftUnchanged
	}
	return fmt.Sprintf("convert's saved set of %d fixed names under hold %s (its convert record), and every selection record, fresh name and fixed name's deletion a run of convert made of it", len(set.Names), holdID)
}

// list is one listing of the notification stream's consumers behind the meta barrier: the barrier
// stream made, listed and deleted, then CONSUMER.NAMES, counted only when every declared server
// answered one meta term and leader before the barrier and after the listing; otherwise the
// barrier and the listing are made again (writerhold.MetaBarrier.Run).
func (c convertRule) list(ctx context.Context, nc *nats.Conn, barrier writerhold.MetaBarrier) ([]string, error) {
	var names []string
	listing := func(ctx context.Context) error {
		var err error
		if names, err = consumerNames(ctx, nc, notifications); err == nil && c.listed != nil {
			c.listed(ctx, names)
		}
		return err
	}
	if c.noTermCheck {
		if err := barrier.Run(ctx, nc, nil); err != nil {
			return nil, hubError(err, "the meta barrier")
		}
		if err := listing(ctx); err != nil {
			return nil, err
		}
		return names, nil
	}
	if err := barrier.Run(ctx, nc, listing); err != nil {
		var unknown *Unknown
		if errors.As(err, &unknown) {
			return nil, err
		}
		return nil, hubError(err, "the meta barrier and its listing")
	}
	return names, nil
}

// withDispatch is set with Dispatch's two durables added when Dispatch is a family of the
// conversion: the fresh mode refuses a missing row for either (§2, "Missing selection state means
// stop"), so convert writes both, and a durable gone with no saved start stops it for a replay.
func (c convertRule) withDispatch(r *run, set convertSet) convertSet {
	if !slices.Contains(c.families(r, set), familyDispatch) {
		return set
	}
	for _, durable := range dispatchDurables() {
		if !set.has(durable) {
			set.Names = append(set.Names, convertSetName{Family: familyDispatch, Logical: durable, Fixed: durable})
		}
	}
	slices.SortFunc(set.Names, func(a, b convertSetName) int { return strings.Compare(a.Fixed, b.Fixed) })
	return set
}

// collect is the set convert converts: every fixed name listed, with its configuration as its
// info answers it, and every cohort member the listing no longer holds.
func (c convertRule) collect(ctx context.Context, nc *nats.Conn, cohort prepareCohort, listed []string) (convertSet, error) {
	var set convertSet
	for _, name := range listed {
		m, ok := classify(name)
		if !ok {
			continue
		}
		entry, err := convertEntry(ctx, nc, convertSetName{Family: m.Family, Logical: m.Logical, Fixed: name})
		if err != nil {
			return convertSet{}, err
		}
		set.Names = append(set.Names, entry)
	}
	for _, m := range cohort.Members {
		if !set.has(m.Fixed) {
			set.Names = append(set.Names, convertSetName{Family: m.Family, Logical: m.Logical, Fixed: m.Fixed})
		}
	}
	slices.SortFunc(set.Names, func(a, b convertSetName) int { return strings.Compare(a.Fixed, b.Fixed) })
	return set, nil
}

// convertEntry is entry, a fixed name of the set, with its configuration as its info answers it,
// and none for a name the server does not hold. collect and held's listings both build a set
// entry so.
func convertEntry(ctx context.Context, nc *nats.Conn, entry convertSetName) (convertSetName, error) {
	info, err := consumerInfo(ctx, nc, notifications, entry.Fixed)
	if err != nil {
		return convertSetName{}, unknownf(err, "the info of %s", entry.Fixed)
	}
	if info != nil {
		config := info.Config
		entry.Config = &config
	}
	return entry, nil
}

// saveSet writes set as the hold's convert record at gen and answers its new gen.
func (c convertRule) saveSet(ctx context.Context, r *run, set convertSet, gen uint64) (uint64, error) {
	value, err := json.Marshal(set)
	if err != nil {
		return 0, err
	}
	record, err := r.records.PutRecord(ctx, r.o.holdID, convertSetKind, convertSetKey, gen, value)
	if err != nil {
		return 0, fmt.Errorf("write hold %s's convert record at gen %d: %w", r.o.holdID, gen, err)
	}
	return record.Gen, nil
}

// families are the cutover families convert marks done: the session family always (every
// listener's durable and the session consumers), Dispatch's when its store is given or the set
// holds one of its names, and each daemon whose store is given or whose names the set holds.
func (c convertRule) families(r *run, set convertSet) []string {
	families := []string{familySession}
	add := func(family string) {
		if !slices.Contains(families, family) {
			families = append(families, family)
		}
	}
	if r.o.dispatchDatabaseFile != "" {
		add(familyDispatch)
	}
	for _, project := range r.o.daemonProjects() {
		add(daemonFamily(project))
	}
	for _, name := range set.Names {
		add(name.Family)
	}
	return families
}

// checkStores reads every family's store the set and the flags name, before convert changes
// anything, and answers the refusal of the first that cannot hold the conversion: a selection
// bucket that create-bucket has not made, and a Dispatch or daemon store that no flag names or
// that has no intake_consumers table (a project whose daemon never booted the migration). A store
// with no sentinel row yet is the state convert writes it from. Each daemon store a
// --daemon-database file names it reads first by the census's rule for the daemon stores
// (daemonStoresByRule), at exactly the address the project's one source names, and converts
// through the pool the rule opened there: a store the rule leaves unreconciled or finds gone
// refuses the run, naming each.
func (c convertRule) checkStores(ctx context.Context, r *run, set convertSet) (string, error) {
	families := c.families(r, set)
	if refusal, err := c.checkDaemonAddresses(ctx, r, families); err != nil || refusal != "" {
		return refusal, err
	}
	for _, family := range families {
		st, err := r.store(ctx, family)
		if err != nil {
			return convertStoreRefusal(r.o, family, err), nil
		}
		// Whether the store is there at all: convert acts on nothing this read answers, and each
		// read it acts on is behind the bucket barrier (readStore).
		_, err = st.State(ctx)
		switch {
		case err == nil:
		case family == familySession && (errors.Is(err, selection.ErrNoStore) || errors.Is(err, selection.ErrNoSentinel)):
			return fmt.Sprintf("the selection bucket %s cannot hold the session family's records (%v): consumer-names create-bucket --hold %s comes before convert",
				selection.SessionBucket, err, r.o.holdID), nil
		case family != familySession && errors.Is(err, selection.ErrNoStore):
			return convertStoreRefusal(r.o, family, err), nil
		case family != familySession && errors.Is(err, selection.ErrNoSentinel):
		default:
			return "", unknownf(err, "the %s family's store", family)
		}
	}
	return "", nil
}

// checkDaemonAddresses reads, by the census's rule for the daemon stores (daemonStoresByRule), the
// store of each daemon family of families that a --daemon-database file names, which becomes the
// run's database of its family, and answers the refusal naming each the rule leaves unreconciled
// or finds gone: convert writes no store but the one at the address its project's one source
// names. A daemon family no file names is left to checkStores' refusal.
func (c convertRule) checkDaemonAddresses(ctx context.Context, r *run, families []string) (string, error) {
	var named []string
	for _, family := range families {
		if project, ok := strings.CutPrefix(family, familyDaemonPrefix); ok {
			if _, ok := r.o.daemonDatabase(project); ok {
				named = append(named, family)
			}
		}
	}
	if len(named) == 0 {
		return "", nil
	}
	stores, err := r.daemonStoresByRule(ctx, named)
	if err != nil {
		return "", err
	}
	var refusals []string
	for _, s := range stores {
		family := daemonFamily(s.project)
		switch {
		case s.unreconciled != "":
			refusals = append(refusals, fmt.Sprintf("%s's store is unreconciled, so convert writes none of its records: %s", family, s.unreconciled))
		case s.gone != nil:
			refusals = append(refusals, fmt.Sprintf("%s's store is gone at the address %s names (%s): remove its consumers with cleanup --gone %s in a consumer-cleanup hold, and give convert no --daemon-database for it",
				family, s.addressSource(), daemonGoneText(*s.gone), s.project))
		}
	}
	return strings.Join(refusals, "; "), nil
}

// convertStoreRefusal names the store a family's conversion needs and cannot reach, and its remedy.
// For Dispatch that is --dispatch-database-file while o names no store for it or the file cannot be
// read, and Dispatch's migration that makes intake_consumers (dispatchIntakeConsumersMigration) when
// the flag names a store without that table (selection.ErrNoStore): a flag the operator gave is no
// remedy.
func convertStoreRefusal(o options, family string, err error) string {
	if project, ok := strings.CutPrefix(family, familyDaemonPrefix); ok {
		return fmt.Sprintf("the Legion daemon of project %s has fixed names to convert and its store cannot hold them (%v): give --daemon-database %s=<file>, and boot the project once so its migration makes intake_consumers", project, err, project)
	}
	if o.dispatchDatabaseFile != "" && errors.Is(err, selection.ErrNoStore) {
		return fmt.Sprintf("Dispatch has fixed names to convert and its store, which --dispatch-database-file names, has no %s table (%v): Dispatch's boot makes the table with its migration %s, so deploy the Dispatch release that carries it, then run convert again",
			selection.IntakeConsumersTable, err, dispatchIntakeConsumersMigration)
	}
	return fmt.Sprintf("Dispatch has fixed names to convert and its store cannot hold them (%v): give --dispatch-database-file <file>", err)
}

// dispatchIntakeConsumersMigration is the migration of Dispatch's database that makes its
// intake_consumers table (internal/dispatch/store/migrations), which Dispatch's boot applies.
const dispatchIntakeConsumersMigration = "20261121002206_intake_consumers"

// convertDaemonMark is what every daemon store's sentinel gains beside its marker: the boundary,
// ENVOY_NOTIFICATIONS' last sequence + 1 at the cutover, where the daemon's first create of a new
// project starts (its DeliverNew, by sequence), and the stream's identity
// (selection.StreamIdentity), so the daemon reads no stream state for either.
type convertDaemonMark struct {
	boundary uint64
	streamID string
}

// convertSessionMark is what the selection bucket's sentinel holds beside its marker: this
// cutover's boundary B, its cohort's, and its hold id. create-bucket wrote both in the cutover that
// made the bucket; a second cutover after a rollback makes no bucket, so without this mark its
// sentinel would keep the first cutover's B and hold while the session family's broker cutover row
// names the second, and repair-sentinel, which reads the row, could not give it back.
type convertSessionMark struct {
	boundary uint64
	hold     string
}

// markDone writes family's store marker cutover = done, keeping everything else its sentinel
// holds, and in a daemon's store daemon's boundary and stream identity, and in the selection bucket
// session's boundary and hold, with a write conditional on the sentinel's revision, read again
// after a refused one. A marker already done, with a daemon's boundary already written or the
// selection bucket's boundary and hold already session's, is left as it is.
func (c convertRule) markDone(ctx context.Context, r *run, family string, daemon convertDaemonMark, session convertSessionMark) error {
	isDaemon := strings.HasPrefix(family, familyDaemonPrefix)
	isSession := family == familySession
	st, err := r.store(ctx, family)
	if err != nil {
		return err
	}
	for range convertConflicts {
		var state selection.StoreState
		var revision uint64
		err := c.readStore(ctx, r, family, func(ctx context.Context) error {
			var err error
			state, revision, err = convertSentinel(ctx, st, family)
			return err
		})
		if err != nil {
			return unknownf(err, "the %s family's store marker", family)
		}
		if state.Cutover == selection.CutoverDone && (!isDaemon || state.Boundary > 0) &&
			(!isSession || (state.Boundary == session.boundary && state.Hold == session.hold)) {
			return nil
		}
		state.Cutover = selection.CutoverDone
		if isDaemon {
			state.Boundary = daemon.boundary
			state.StreamID = daemon.streamID
		}
		if isSession {
			state.Boundary, state.Hold = session.boundary, session.hold
		}
		_, err = st.WriteSentinel(ctx, state, revision)
		if errors.Is(err, selection.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("write the %s family's store marker: %w", family, err)
		}
		return nil
	}
	return unknownf(nil, "the %s family's store marker moved under %d writes", family, convertConflicts)
}

// convertSentinel is the store's sentinel and the revision a write of it is conditional on
// (familyStore's sentinel: the revision read first, the state after it through the store's own
// check, so a write between the two is refused and read again), 0 for a Postgres store with no
// sentinel row yet. A selection bucket holding no sentinel is the store's selection.ErrNoSentinel:
// create-bucket writes it before any convert, and a sentinel that is not the store's own is the
// store's selection.ErrMalformedSentinel, never none.
func convertSentinel(ctx context.Context, st familyStore, family string) (selection.StoreState, uint64, error) {
	state, revision, found, err := st.sentinel(ctx)
	switch {
	case err != nil:
		return selection.StoreState{}, 0, err
	case !found && family == familySession:
		return selection.StoreState{}, 0, fmt.Errorf("%w: the bucket %s holds no %s", selection.ErrNoSentinel, selection.SessionBucket, selection.SentinelKey)
	}
	return state, revision, nil
}

// convertOwner names convert in each operation it reserves.
func convertOwner() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("envoy-infra consumer-names convert %s, pid %d", host, os.Getpid())
}

// convertName converts one logical name of the set to a fresh name, from whatever state its record
// is in, and reports whether it converted it (done), or the logical name with no safe start
// (missing), which stops convert once every other name is converted:
//
//   - An open operation is settled first (Step 0, convertSettle), and the record read again.
//   - A record whose current is a fresh name is converted already; a fixed name still present is
//     a stray, which convert deletes itself: no record retires the logical's fixed name (retire).
//   - With F present: the start S is the lowest of F's ack floor + 1, Dispatch's hold-token start
//     and any start the record holds, with today's clamp (clampStart). The record is written
//     op = {replace, old F, candidate N} and start = S, with no current: F is named only as the
//     replace's old name, since the selection package refuses a record whose current is a fixed
//     name (consumernames Names.Check), and a current F that a rollback left (restore) is cleared.
//     Then F is deleted, then N is created at S with F's configuration and the member's metadata,
//     then published: F goes first, so no logical name holds two slots and no cap update is needed.
//   - With F gone: the record is written with no current and start = the lowest of the cohort's
//     floor + 1, the hold token's start and the record's start, with today's clamp; with none of
//     them, there is no safe start.
func (c convertRule) convertName(ctx context.Context, r *run, nc *nats.Conn, name convertSetName, bound *prepareCohortMember) (bool, *convertNoStart, error) {
	m, ok := classify(name.Fixed)
	if !ok {
		return false, nil, fmt.Errorf("the saved set holds %s, which is no family's fixed name", name.Fixed)
	}
	st, err := r.store(ctx, m.Family)
	if err != nil {
		return false, nil, err
	}
	for conflicts := 0; conflicts <= convertConflicts; conflicts++ {
		var record selection.Record
		var revision uint64
		err := c.readStore(ctx, r, m.Family, func(ctx context.Context) error {
			var err error
			record, revision, err = st.Read(ctx, m.Logical)
			return err
		})
		switch {
		case errors.Is(err, selection.ErrNoRecord):
			record, revision = selection.Record{}, 0
		case errors.Is(err, selection.ErrRecordDeleted):
			return false, &convertNoStart{logical: m.Logical, why: "its selection record's last write is a delete"}, nil
		case err != nil:
			return false, nil, unknownf(err, "%s's selection record", m.Logical)
		}
		if record.Op != nil {
			err := c.settle(ctx, r, nc, st, m, name, bound, record, revision)
			if errors.Is(err, selection.ErrConflict) {
				continue
			}
			if err != nil {
				return false, nil, err
			}
			continue
		}
		info, err := consumerInfo(ctx, nc, notifications, name.Fixed)
		if err != nil {
			return false, nil, unknownf(err, "the info of %s", name.Fixed)
		}
		// A current that is F is a rollback's (restore writes its target as the current), which
		// convert converts again; convert's own open conversion names F only as its replace's old
		// name, and is settled above.
		if record.Current != "" && record.Current != name.Fixed {
			// Converted already. F is no name the record selects or may retire, as the selection
			// package's own replace never retires the fixed name and deletes it itself; convert
			// holds F's delete. A crash before it leaves F listed, which this command's rerun and
			// final listing delete again.
			if err := deleteIfOwned(ctx, nc, m, info); err != nil {
				return false, nil, fmt.Errorf("delete %s, which %s's record no longer selects: %w", name.Fixed, m.Logical, err)
			}
			return false, nil, nil
		}
		stream, err := streamInfo(ctx, nc, notifications)
		if err != nil {
			return false, nil, unknownf(err, "%s's state", notifications)
		}
		token, err := c.tokenStart(ctx, r, m)
		if err != nil {
			return false, nil, err
		}
		var starts []uint64
		if record.Start > 0 {
			starts = append(starts, record.Start)
		}
		if token > 0 {
			starts = append(starts, token)
		}
		if info == nil {
			if bound != nil {
				starts = append(starts, bound.Floor+1)
			}
			if len(starts) == 0 {
				return false, &convertNoStart{logical: m.Logical, why: name.Fixed + " is gone, and no floor of it, no hold token start and no saved start is known"}, nil
			}
			next := convertClone(record)
			if next.Current == name.Fixed {
				next.Current = ""
			}
			next.Start = clampStart(slices.Min(starts), stream.State)
			next.LastFloor, next.Restore = nil, nil
			if revision != 0 && next.Start == record.Start && next.Current == record.Current {
				return false, nil, nil
			}
			if _, err := st.Write(ctx, m.Logical, next, revision); err != nil {
				if errors.Is(err, selection.ErrConflict) {
					continue
				}
				return false, nil, fmt.Errorf("write %s's start: %w", m.Logical, err)
			}
			return true, nil, nil
		}
		starts = append(starts, clampStart(convertFloor(info)+1, stream.State))
		config := name.Config
		if config == nil {
			config = &info.Config
		}
		next := convertClone(record)
		next.Current = ""
		next.Start = clampStart(slices.Min(starts), stream.State)
		next.Op = &selection.Op{Kind: selection.OpReplace, Owner: convertOwner(), Old: name.Fixed,
			Candidate: selection.FreshName(m.Prefix), ReservedAt: r.now()}
		next.LastFloor, next.Restore = nil, nil
		revision, err = st.Write(ctx, m.Logical, next, revision)
		if errors.Is(err, selection.ErrConflict) {
			continue
		}
		if err != nil {
			return false, nil, fmt.Errorf("reserve %s's conversion: %w", m.Logical, err)
		}
		if err := deleteIfOwned(ctx, nc, m, info); err != nil {
			return false, nil, fmt.Errorf("delete %s, whose start %d %s's record saved: %w", name.Fixed, next.Start, m.Logical, err)
		}
		if err := c.launch(ctx, r, nc, st, m, convertCreatorThreshold(*config, bound), next, revision); err != nil {
			if errors.Is(err, selection.ErrConflict) {
				continue
			}
			return false, nil, err
		}
		return true, nil, nil
	}
	return false, nil, unknownf(nil, "%s's selection record moved under %d writes", m.Logical, convertConflicts)
}

// settle is Step 0 for convert: it settles record's open operation, writing the record, and
// answers selection.ErrConflict when the write was refused, so the caller reads again.
//
//   - replace (old O, candidate N, start S): N present is published, O retired (unless it is the
//     fixed name, which no record retires) and deleted; N absent, once the operation is
//     convertSettleAge old: O, when it is the logical's fixed name F, is deleted first (plan §3
//     step 2: F goes first, so no logical name holds two slots and no cap update is needed), then
//     N is retired and a fresh N′ created at S from the saved configuration, then published. A
//     replace with no start destroyed nothing: its candidate is retired and the operation cleared.
//   - create (candidate N): N present is published; N absent is retired and the operation cleared,
//     its start kept.
//   - delete (old O): O's floor is recorded as the start unless it was, O deleted, the current
//     cleared.
func (c convertRule) settle(ctx context.Context, r *run, nc *nats.Conn, st familyStore, m member, name convertSetName, bound *prepareCohortMember, record selection.Record, revision uint64) error {
	op := record.Op
	// dropCandidate retires op's candidate and clears the operation, its start kept.
	dropCandidate := func() error {
		next := retire(convertClone(record), m.Fixed, op.Candidate)
		next.Op = nil
		_, err := st.Write(ctx, m.Logical, next, revision)
		return err
	}
	if op.Kind == selection.OpDelete {
		next := convertClone(record)
		if !op.FloorRecorded && op.Old != "" {
			info, err := consumerInfo(ctx, nc, notifications, op.Old)
			if err != nil {
				return unknownf(err, "the info of %s", op.Old)
			}
			if info != nil {
				start := convertFloor(info) + 1
				if next.Start == 0 || start < next.Start {
					next.Start = start
				}
			}
		}
		if op.Old != "" {
			if err := deleteOwned(ctx, nc, m, op.Old); err != nil {
				return fmt.Errorf("delete %s, %s's open delete: %w", op.Old, m.Logical, err)
			}
		}
		if next.Current == op.Old {
			next.Current = ""
		}
		next.Op, next.LastFloor = nil, nil
		_, err := st.Write(ctx, m.Logical, next, revision)
		return err
	}
	if op.Kind == selection.OpReplace && record.Start == 0 {
		return dropCandidate()
	}
	info, err := c.awaitCandidate(ctx, r, nc, *op)
	if err != nil {
		return err
	}
	if info != nil {
		return c.publish(ctx, nc, st, m, record, revision, info)
	}
	if op.Kind != selection.OpReplace {
		return dropCandidate()
	}
	config := name.Config
	if config == nil {
		old, err := consumerInfo(ctx, nc, notifications, op.Old)
		if err != nil {
			return unknownf(err, "the info of %s", op.Old)
		}
		if old == nil {
			return unknownf(nil, "%s's open replace of %s at %d: the candidate %s is absent, %s is gone, and the saved set holds no configuration of it",
				m.Logical, op.Old, record.Start, op.Candidate, op.Old)
		}
		config = &old.Config
	}
	// F goes first: the saved start S is no later than F's floor + 1 and F's configuration is read
	// above, so its delete loses nothing, and the fresh candidate's create then needs no slot of
	// F's. A failed delete stops convert with the operation open and F retired nowhere.
	if op.Old == m.Fixed {
		if err := deleteOwned(ctx, nc, m, op.Old); err != nil {
			return fmt.Errorf("delete %s, the old name of %s's open replace at its saved start %d: %w", op.Old, m.Logical, record.Start, err)
		}
	}
	next, revision, err := c.rereserve(ctx, r, st, m, record, revision)
	if err != nil {
		return err
	}
	return c.launch(ctx, r, nc, st, m, convertCreatorThreshold(*config, bound), next, revision)
}

// readStore runs read, a read of family's store that convert acts on: for the session family
// behind the hold's bucket barrier, taken again before each read (behindBarrier,
// writerhold.ReadBucket: the put hold.barrier.<hold id>, acknowledged by the stream's leader once
// it has applied every entry before it, and the leader read before the barrier and after the read,
// the barrier made again when it changed), so neither a record committed before the barrier nor a
// new leader that answers before it has applied its log can give convert a stale record (plan §3a,
// "The bucket barrier, before the first read of each bucket" and "A later read follows the same
// rule"). The other families' stores are Postgres, which needs none.
func (c convertRule) readStore(ctx context.Context, r *run, family string, read func(context.Context) error) error {
	if family != familySession {
		return read(ctx)
	}
	return behindBarrier(ctx, r.nc, r.o.holdID, read)
}

// convertCreatorThreshold is config, F's configuration, with the inactive threshold of F's
// creator: a cohort member whose threshold prepare raised to prepareThreshold keeps that raise
// only for the window, so its fresh name gets the threshold prepare read before the raise
// (prepareCohortMember.Threshold). A threshold prepare did not raise is F's creator's already.
func convertCreatorThreshold(config jetstream.ConsumerConfig, bound *prepareCohortMember) jetstream.ConsumerConfig {
	if bound != nil && config.InactiveThreshold == prepareThreshold {
		config.InactiveThreshold = bound.Threshold
	}
	return config
}

// awaitCandidate reads op's candidate, and while the server does not hold it and op is younger
// than convertSettleAge by this process's clock, reads it again once it is that old: nil is a
// candidate absent at that age.
func (c convertRule) awaitCandidate(ctx context.Context, r *run, nc *nats.Conn, op selection.Op) (*jetstream.ConsumerInfo, error) {
	info, err := consumerInfo(ctx, nc, notifications, op.Candidate)
	if err != nil || info != nil {
		return info, err
	}
	if wait := convertSettleAge - r.now().Sub(op.ReservedAt); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		return consumerInfo(ctx, nc, notifications, op.Candidate)
	}
	return nil, nil
}

// rereserve retires record's open operation's candidate and reserves a fresh one, with the same
// start, by a conditional write, and answers the record written and its revision.
func (c convertRule) rereserve(ctx context.Context, r *run, st familyStore, m member, record selection.Record, revision uint64) (selection.Record, uint64, error) {
	next := retire(convertClone(record), m.Fixed, record.Op.Candidate)
	retry := *record.Op
	retry.Candidate, retry.ReservedAt, retry.Owner = selection.FreshName(m.Prefix), r.now(), convertOwner()
	next.Op = &retry
	written, err := st.Write(ctx, m.Logical, next, revision)
	return next, written, err
}

// launch creates record's open operation's candidate at the record's start from config, F's
// configuration, and publishes it. A create whose outcome is unknown is settled with an info: a
// candidate the server holds is published, and one it does not is retired, with one fresh
// candidate tried in its place. At the stream's max_consumers the retired names the server holds
// are deleted and a fresh candidate tried, then the operation's old name, whose start is saved:
// the cap is never updated. A refusal the server answered leaves the operation open for a rerun.
func (c convertRule) launch(ctx context.Context, r *run, nc *nats.Conn, st familyStore, m member, config jetstream.ConsumerConfig, record selection.Record, revision uint64) error {
	retried, cleared, oldDeleted := false, false, false
	for {
		op := record.Op
		info, err := writeConsumer(ctx, nc, notifications, convertConsumerConfig(config, op.Candidate, record.Start, m), actionCreateConsumer)
		if err == nil {
			return c.publish(ctx, nc, st, m, record, revision, info)
		}
		var api *jetstream.APIError
		if errors.As(err, &api) {
			if api.ErrorCode != jetstream.JSErrCodeMaximumConsumersLimit {
				return fmt.Errorf("create %s, %s's fresh name, at %d: %w", op.Candidate, m.Logical, record.Start, err)
			}
			switch {
			case !cleared:
				cleared = true
				for _, retired := range record.Retired {
					_ = deleteOwned(ctx, nc, m, retired)
				}
			case op.Old != "" && !oldDeleted:
				oldDeleted = true
				if err := deleteOwned(ctx, nc, m, op.Old); err != nil {
					return fmt.Errorf("delete %s under %s's saved start %d at the stream's max_consumers: %w", op.Old, m.Logical, record.Start, err)
				}
			default:
				return fmt.Errorf("create %s, %s's fresh name, at %d: %w", op.Candidate, m.Logical, record.Start, err)
			}
		} else {
			found, infoErr := consumerInfo(ctx, nc, notifications, op.Candidate)
			if infoErr == nil && found != nil {
				return c.publish(ctx, nc, st, m, record, revision, found)
			}
			if infoErr != nil || retried {
				return fmt.Errorf("create %s, %s's fresh name, at %d, whose outcome is unknown: %w", op.Candidate, m.Logical, record.Start, err)
			}
			retried = true
		}
		if record, revision, err = c.rereserve(ctx, r, st, m, record, revision); err != nil {
			return err
		}
	}
}

// publish makes the candidate info created the record's current, retires the operation's old
// name unless it is the logical's fixed name (retire) and clears the operation, then deletes the
// old name (retired, so cleanup deletes it if this delete fails; the fixed name, which convert's
// rerun and final listing delete again).
func (c convertRule) publish(ctx context.Context, nc *nats.Conn, st familyStore, m member, record selection.Record, revision uint64, info *jetstream.ConsumerInfo) error {
	op := record.Op
	next := convertClone(record)
	next.Current = op.Candidate
	if info.Config.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && info.Config.OptStartSeq > 0 {
		next.Start = info.Config.OptStartSeq
	}
	if op.Kind == selection.OpReplace {
		next = retire(next, m.Fixed, op.Old)
	}
	next.Op, next.LastFloor, next.Restore = nil, nil, nil
	if _, err := st.Write(ctx, m.Logical, next, revision); err != nil {
		return err
	}
	if op.Kind == selection.OpReplace && op.Old != "" {
		_ = deleteOwned(ctx, nc, m, op.Old)
	}
	return nil
}

// convertConsumerConfig is the fresh consumer name's configuration: F's, renamed, started by start
// sequence at start, and carrying the member's metadata alone.
func convertConsumerConfig(config jetstream.ConsumerConfig, name string, start uint64, m member) jetstream.ConsumerConfig {
	if config.Durable != "" {
		config.Durable = name
	}
	config.Name = name
	config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	config.OptStartTime = nil
	config.Metadata = m.Metadata()
	return config
}

// tokenStart is, for Dispatch's durables, the start its intake's hold token saved: the duty_holds
// row delivery-intake-<durable>, whose token is `<deliver subject> start=<seq>` when a take-over
// deleted the durable and recorded where it was to start (internal/dispatch/delivery/
// held_durable.go intakeHoldToken). 0 for every other family, and for a token with no start.
func (c convertRule) tokenStart(ctx context.Context, r *run, m member) (uint64, error) {
	if m.Family != familyDispatch {
		return 0, nil
	}
	db, err := r.database(ctx, familyDispatch)
	if err != nil {
		return 0, err
	}
	var token string
	err = db.QueryRow(ctx, `select token from duty_holds where name = $1`, "delivery-intake-"+m.Logical).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, unknownf(err, "Dispatch's hold token of %s", m.Logical)
	}
	_, start, found := strings.Cut(token, " start=")
	if !found {
		return 0, nil
	}
	seq, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		return 0, nil
	}
	return seq, nil
}

// convertFloor is a fixed name's ack floor by its info: the server's, or, for a consumer created
// by start sequence that has acked nothing and was delivered up to its start, the sequence before
// its start, which nats-server v2.10.29 reports as 0 (internal/bus/session_consumers.go ackFloor's
// first clause). It never reads a message awaiting an ack as acked, so floor + 1 skips nothing.
func convertFloor(info *jetstream.ConsumerInfo) uint64 {
	floor := info.AckFloor.Stream
	if floor == 0 && info.AckFloor.Consumer == 0 && info.Config.OptStartSeq > 0 && info.Delivered.Stream+1 >= info.Config.OptStartSeq {
		floor = info.Config.OptStartSeq - 1
	}
	return floor
}

// clampStart is today's clamp (internal/bus/session_consumers.go create, firstStart) of a create's
// start on a stream in state: a start past the sequence after the stream's last becomes the
// stream's first sequence, at least 1, the sequence an empty stream stores next. convertName and
// replace start each fresh name through it; convert's settle reuses the start an open operation
// saved.
func clampStart(start uint64, state jetstream.StreamState) uint64 {
	if start > state.LastSeq+1 {
		return max(1, state.FirstSeq)
	}
	return max(1, start)
}

// convertClone is a copy of record that shares nothing with it.
func convertClone(record selection.Record) selection.Record {
	c := record
	if record.Op != nil {
		op := *record.Op
		c.Op = &op
	}
	if record.LastFloor != nil {
		floor := *record.LastFloor
		c.LastFloor = &floor
	}
	if record.Restore != nil {
		restore := *record.Restore
		c.Restore = &restore
	}
	c.Retired = slices.Clone(record.Retired)
	return c
}

// retire is record with name added to its retired names, once, in a copy of its list: convert,
// replay and restore each retire a name a record no longer selects through it. It never retires
// fixed, the logical's fixed name: the selection package refuses a record that names it anywhere
// but as an open operation's old name (consumernames Names.Check), and its own replace deletes the
// fixed name rather than retiring it, so whoever replaces the fixed name deletes it.
func retire(record selection.Record, fixed, name string) selection.Record {
	if name != "" && name != fixed && !slices.Contains(record.Retired, name) {
		record.Retired = append(slices.Clone(record.Retired), name)
	}
	return record
}
