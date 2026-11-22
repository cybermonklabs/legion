package consumernames

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The consumer-cleanup workflow's commands as the hold's journal names them
// (internal/holdrecord/table.go, consumerCleanupCommands): the word and the flag that picks its
// mode, and each with finalSuffix for its run with --final, which completes the hold's record.
const (
	cleanupCommand     = "consumer-names cleanup"
	cleanupGoneCommand = "consumer-names cleanup --gone"
	replaceCommand     = "consumer-names replace"
	finalSuffix        = " --final"
)

// consumerCleanupAdmit admits this run of command, one of the consumer-cleanup workflow's, as its
// run with --final (command and finalSuffix) when the run names --final: the row read and the
// held re-read (admit, requirePhase), then its intent with inputs. A hold of another workflow
// refuses it at its intent, which it does not write (holdrecord.ErrNotAdmitted: that workflow's
// table names no such command), and the refusal names the workflow the command runs in.
func (r *run) consumerCleanupAdmit(ctx context.Context, command string, inputs map[string]string) (*admission, error) {
	if r.o.final {
		command += finalSuffix
	}
	a, err := r.admit(ctx, command, inputs)
	if errors.Is(err, holdrecord.ErrNotAdmitted) {
		return nil, fmt.Errorf("%s runs only in a %s hold: %w", command, holdrecord.WorkflowConsumerCleanup, err)
	}
	return a, err
}

// namesOrNone is names joined by ", " in their order, or "none".
func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// cleanupMaxConflicts is how many refused writes of one record cleanup reads again past before it
// leaves the record for its next run.
const cleanupMaxConflicts = 10

// cleanupTarget is one record cleanup works on: its family's store and its logical name.
type cleanupTarget struct {
	family  string
	logical string
	store   selection.Store
}

// cleanup is `consumer-names cleanup --hold <id> [--final]`, a command of the hold's
// consumer-cleanup workflow (consumerCleanupAdmit), and `consumer-names cleanup --gone <project>`
// (cleanupGone). It deletes, with the row held under --hold, the retired names of every family's
// records that the server still holds, and drops from each record every retired name the server
// reports absent: the session family's and every listener durable's records in the selection
// bucket, walked behind the hold's bucket barrier (behindBarrier, writerhold.ReadBucket), and
// Dispatch's and each daemon's intake_consumers rows. It is the one command that deletes a
// daemon's names, whose grant has no consumer delete (§2, "Cleanup"). It deletes only names a
// record retired, never a name the record still selects (its current, or its open operation's
// candidate or predecessor), and never by a name pattern. Each record's write is conditional on
// the revision it read; a refused write (selection.ErrConflict) reads the record again, which
// finds the names it deleted absent.
//
// It reads every store before it deletes anything: a store a family needs that no flag names
// refuses the run (Dispatch's, and each daemon's that the broker's cutover rows or
// --daemon-database name, a retired project's among them), and a reading that is not whole is
// UNKNOWN; either way nothing is deleted. It reads each daemon's store by the census's rule for
// the daemon stores (cleanupDaemonStores), at exactly the address the project's one source names:
// a store there whose database does not exist (SQLSTATE 3D000) is gone and holds no record to
// clean, so cleanup names it and goes on, and a broker cutover row naming a dropped project, which
// no command removes, does not stop every later cleanup; a store the rule leaves unreconciled is
// UNKNOWN. It does not use selection.Names.Cleanup, which runs Step 0 on a record's open
// operation (a creator's protocol, whose owner may be the process the hold stopped) and refuses a
// store whose marker is not done, so it could not clean the names a rollback retired.
//
// Its intent is its admission, after the held re-read; it runs the re-read again before its first
// delete. Its done entry names each consumer it deleted (deleted), how many records held a retired
// name (records), how many names it dropped from them (dropped) and each daemon store it found
// gone (gone.<family>, daemonGoneText). Every exit after its intent,
// a refusal, an UNKNOWN, the re-read's and a done entry whose write failed, writes its stop, which
// records what every run of the command in this hold left (admission.stopped): nothing while no
// run deleted a name, wrote a record or put this hold's barrier key (cleanupSessionTargets puts
// it, and nothing cleanup does deletes it), else this run's deletions, the key when this run put
// it, and how each earlier run ended; an UNKNOWN's records what the run read and could not read
// (leftUnknown), never unchanged.
func (r *run) cleanup(ctx context.Context) error {
	if r.o.gone != "" {
		return r.cleanupGone(ctx)
	}
	a, err := r.consumerCleanupAdmit(ctx, cleanupCommand, nil)
	if err != nil {
		return err
	}
	const nothing = "this run deleted no consumer and wrote no record"
	cutovers, err := r.records.Cutovers(ctx)
	if err != nil {
		return a.stopped(ctx, unknownf(err, "the broker's cutover rows"), true, nothing)
	}
	r.read("the broker's cutover rows")
	families := pgFamilies(r.o, cutovers)
	var unnamed []string
	for _, family := range families {
		if file, _ := r.o.storeFile(family); file == "" {
			unnamed = append(unnamed, family)
		}
	}
	if len(unnamed) > 0 {
		return a.stopped(ctx, fmt.Errorf("cleanup reads every family's store before it deletes a name, and no flag names the store of %s (--dispatch-database-file, --daemon-database <project>=<file>; a retired or dropped project's cutover row needs its file too, where the census's rule finds its store gone)", strings.Join(unnamed, ", ")), true, nothing)
	}
	gone, err := r.cleanupDaemonStores(ctx, families)
	if err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return a.stopped(ctx, unknownf(err, "the hub"), true, nothing)
	}
	targets, err := r.cleanupSessionTargets(ctx, a, nc)
	if err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	r.read("the records of " + selectionStreamName + " behind the bucket barrier")
	for _, family := range families {
		if _, ok := gone[family]; ok {
			continue
		}
		more, err := r.cleanupPGTargets(ctx, family)
		if err != nil {
			return a.stopped(ctx, err, true, nothing)
		}
		r.read(family + "'s records")
		targets = append(targets, more...)
	}
	// Every held command reads the hold again before its mutation steps (heldReread), not only
	// when it starts: its refusal deletes nothing.
	if err := a.reread(ctx); err != nil {
		return a.stopped(ctx, err, true, nothing)
	}

	var deletedAll []string
	var droppedAll int
	var failures []string
	for _, target := range targets {
		deleted, dropped, err := cleanupRecord(ctx, nc, target)
		deletedAll = append(deletedAll, deleted...)
		droppedAll += len(dropped)
		if len(deleted) > 0 || len(dropped) > 0 {
			fmt.Fprintf(r.stdout, "cleanup %s %s: deleted %v, dropped from retired %v\n", target.family, target.logical, deleted, dropped)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", target.family, target.logical, err))
		}
	}
	fmt.Fprintf(r.stdout, "cleanup: %d records with retired names, %d names deleted, %d dropped from retired\n", len(targets), len(deletedAll), droppedAll)
	if len(failures) > 0 {
		// A delete or a record's write whose answer failed may have landed, so the stop never
		// records this run unchanged.
		account := fmt.Sprintf("this run deleted %s and dropped %d retired names from their records, and a failed delete or write may have landed",
			namesOrNone(deletedAll), droppedAll)
		return a.stopped(ctx, fmt.Errorf("cleanup left retired names for its next run: %s", strings.Join(failures, "; ")), false, account)
	}
	r.read("each record that holds a retired name, and the server's answer for each of those names")
	outputs := map[string]string{"records": strconv.Itoa(len(targets)), "deleted": namesOrNone(deletedAll), "dropped": strconv.Itoa(droppedAll)}
	for family, why := range gone {
		outputs["gone."+family] = why
	}
	if err := a.done(ctx, outputs); err != nil {
		return a.stopped(ctx, err, len(deletedAll) == 0 && droppedAll == 0,
			fmt.Sprintf("this run deleted %s and dropped %d retired names from their records", namesOrNone(deletedAll), droppedAll))
	}
	return nil
}

// cleanupGone is `consumer-names cleanup --gone <project> --hold <id> [--final]`, a command of the
// hold's consumer-cleanup workflow (consumerCleanupAdmit, input daemon): it removes the consumers
// of a Legion daemon whose store is gone, which the census leaves (RecordedFreshNames's gone names),
// and only then. Under its intent it reads the sources of the daemon stores' addresses again
// (writerhold.KubeDaemonDeployments, with the hold's own Kubernetes client: the daemons'
// Deployments and the retired projects' entries) and reads the project's store by the census's
// rule for the daemon stores (censusReadDaemonStores), at exactly the address the project's one
// source names, its Deployment or, for a project retired after the cutover, its entry, from its
// --daemon-database file: only a server there answering SQLSTATE 3D000, the database does not
// exist, makes the store gone. Any other reading refuses the run, naming it, before anything is
// removed: a store that answers, one the rule leaves unreconciled (an address no one source names,
// that is not that source's, or that another source names too, a server that does not answer, or
// answers another SQLSTATE, such as 57P01 or 57P03), and a listing it cannot read. Then, after the
// held re-read again, it deletes every consumer of the notification stream whose logical name is
// one of the daemon's (enrollLogicalNames, selection.LogicalOf), in any form, fixed or fresh,
// naming each, and its done entry names the store and each removal. A
// delete that fails stops the run, which records each consumer it removed before; every other exit
// after its intent writes its stop too (admission.stopped), an UNKNOWN's under leftUnknown.
func (r *run) cleanupGone(ctx context.Context) error {
	project := r.o.gone
	family := daemonFamily(project)
	a, err := r.consumerCleanupAdmit(ctx, cleanupGoneCommand, map[string]string{"daemon": project})
	if err != nil {
		return err
	}
	const nothing = "this run removed no consumer"
	store, err := r.daemonStoreByRule(ctx, project)
	switch {
	case err != nil:
		return a.stopped(ctx, err, true, nothing)
	case store.unreconciled != "":
		return a.stopped(ctx, fmt.Errorf("--gone %s: %s's store is unreconciled, so nothing shows it is gone, and cleanup removes none of its consumers: %s", project, family, store.unreconciled), true, nothing)
	case store.gone == nil:
		return a.stopped(ctx, fmt.Errorf("--gone %s: %s's store answers at the address %s names, so it is not gone, and cleanup removes none of its consumers", project, family, store.addressSource()), true, nothing)
	}
	r.read(family + "'s store, at the address " + store.addressSource() + " names")
	why := daemonGoneText(*store.gone)
	nc, _, err := r.conn(ctx)
	if err != nil {
		return a.stopped(ctx, unknownf(err, "the hub"), true, nothing)
	}
	listed, err := consumerNames(ctx, nc, notifications)
	if err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	r.read("the consumers of " + notifications)
	logicals := enrollLogicalNames(project)
	var targets []string
	for _, name := range listed {
		if slices.Contains(logicals, selection.LogicalOf(name)) {
			targets = append(targets, name)
		}
	}
	slices.Sort(targets)
	if err := a.reread(ctx); err != nil {
		return a.stopped(ctx, err, true, nothing)
	}
	var removed []string
	for _, name := range targets {
		// Each name's logical name is one of the daemon's (selection.LogicalOf), and its delete
		// needs the consumer to be that logical name's own (deleteOwned): the fixed name by its
		// name, a fresh one by the metadata the daemon's creator writes. One that is not, a
		// consumer of the pattern another principal made, is refused and stops the run.
		logical := selection.LogicalOf(name)
		m := member{Family: family, Logical: logical, Fixed: logical, Prefix: logical, Project: project}
		if err := deleteOwned(ctx, nc, m, name); err != nil {
			// A delete whose answer failed may have landed, so the stop never records this run unchanged.
			return a.stopped(ctx, fmt.Errorf("cleanup --gone %s: the delete of %s: %w", family, name, err),
				false, "this run removed "+namesOrNone(removed)+" from "+notifications+", and the delete of "+name+" may have landed")
		}
		removed = append(removed, name)
		fmt.Fprintf(r.stdout, "cleanup --gone %s: removed %s consumer %s: store gone: %s\n", family, notifications, name, why)
	}
	fmt.Fprintf(r.stdout, "cleanup --gone %s: %d consumers removed; store gone: %s\n", family, len(removed), why)
	if err := a.done(ctx, map[string]string{"gone." + family: why, "removed": namesOrNone(removed)}); err != nil {
		return a.stopped(ctx, err, len(removed) == 0, "this run removed "+namesOrNone(removed)+" from "+notifications)
	}
	return nil
}

// cleanupDaemonStores reads the store of each daemon family of families by the census's rule for
// the daemon stores (daemonStoresByRule), before cleanup deletes anything: a store the rule reads
// is the run's database of its family, which cleanupPGTargets lists; a store the rule finds gone
// holds no record to clean, and is answered, family to why (daemonGoneText), and printed; a store
// the rule leaves unreconciled, and a listing of the sources it cannot read, are UNKNOWN, naming
// each, so cleanup deletes nothing.
func (r *run) cleanupDaemonStores(ctx context.Context, families []string) (map[string]string, error) {
	stores, err := r.daemonStoresByRule(ctx, families)
	if err != nil {
		return nil, err
	}
	gone := map[string]string{}
	var unreconciled []string
	for _, s := range stores {
		family := daemonFamily(s.project)
		switch {
		case s.unreconciled != "":
			unreconciled = append(unreconciled, fmt.Sprintf("%s's store: %s", family, s.unreconciled))
		case s.gone != nil:
			gone[family] = daemonGoneText(*s.gone)
			fmt.Fprintf(r.stdout, "cleanup %s: store gone: %s; it holds no record to clean\n", family, gone[family])
		}
	}
	if len(unreconciled) > 0 {
		return nil, unknownf(nil, "the daemon stores, by the census's rule for them, so cleanup deletes nothing: %s", strings.Join(unreconciled, "; "))
	}
	r.read("the daemon stores, each at the address its one source names")
	return gone, nil
}

// daemonStoresByRule reads, by the census's rule for the daemon stores (censusReadDaemonStores),
// the store of each daemon family of families (daemon/<project key>) from the file its
// --daemon-database entry names, at exactly the address the project's one source names, its
// Deployment or its entry of the retired projects' ConfigMap, which it lists once with the hold's
// own Kubernetes client (writerhold.KubeDaemonDeployments): each read, gone, or unreconciled and
// why, for its caller to tell apart; a family no entry names is unreconciled. A store the rule read
// becomes the run's database of its family (database), so the command reads and writes it at the
// address the rule checked, never through a second pool on the file; what the rule opened closes
// when the run ends (close). A listing it cannot read is UNKNOWN.
func (r *run) daemonStoresByRule(ctx context.Context, families []string) ([]censusDaemonRead, error) {
	op, err := r.operatorHub(ctx)
	if err != nil {
		return nil, unknownf(err, "the daemon stores' addresses")
	}
	var entries []string
	for _, family := range families {
		if project, ok := strings.CutPrefix(family, familyDaemonPrefix); ok {
			if file, ok := r.o.daemonDatabase(project); ok {
				entries = append(entries, project+"="+file)
			}
		}
	}
	stores, closeStores, err := censusReadDaemonStores(ctx, writerhold.KubeDaemonDeployments(op.Hub.Client), families, entries)
	r.closeDatabases = append(r.closeDatabases, closeStores)
	if err != nil {
		return nil, unknownf(err, "the daemon stores, each at the address its Deployment or its entry of %s names", retiredStoresSource)
	}
	for _, s := range stores {
		if s.db == nil || s.gone != nil || s.unreconciled != "" {
			continue
		}
		if r.databases == nil {
			r.databases = map[string]Database{}
		}
		r.databases[daemonFamily(s.project)] = s.db
	}
	return stores, nil
}

// daemonStoreByRule is project's daemon store as daemonStoresByRule reads it: read, gone, or
// unreconciled and why. A rule that reads other than one store is UNKNOWN.
func (r *run) daemonStoreByRule(ctx context.Context, project string) (censusDaemonRead, error) {
	family := daemonFamily(project)
	stores, err := r.daemonStoresByRule(ctx, []string{family})
	switch {
	case err != nil:
		return censusDaemonRead{}, err
	case len(stores) != 1:
		return censusDaemonRead{}, unknownf(nil, "%s's store: the census's rule read %d stores, not one", family, len(stores))
	}
	return stores[0], nil
}

// daemonGoneText names a daemon store the census's rule found gone, as cleanup --gone and status
// print it: its database and instance, the instance the operator's file states, and the server's
// answer.
func daemonGoneText(gone writerhold.GoneStore) string {
	return fmt.Sprintf("database %s on instance %s (the operator's file states %s): %s", gone.Database, gone.Instance, gone.StatedInstance, gone.Answer)
}

// cleanupSessionTargets are the selection bucket's records that hold a retired name, a session's
// or a listener durable's, walked behind the hold's bucket barrier (behindBarrier), as every held
// reader of the bucket reads it, each value by its key's kind (readSelectionValue). A missing
// bucket holds none, and gets no barrier. The barrier's key is a change a's stop counts
// (admission.barrier), from right before its put, whose answer may be lost after it landed. A
// malformed value anywhere in the bucket is UNKNOWN, naming each such key, so cleanup deletes
// nothing.
func (r *run) cleanupSessionTargets(ctx context.Context, a *admission, nc *nats.Conn) ([]cleanupTarget, error) {
	if _, err := streamInfo(ctx, nc, selectionStreamName); err != nil {
		if apiErrorIs(err, jetstream.JSErrCodeStreamNotFound) {
			return nil, nil
		}
		return nil, unknownf(err, "the info of %s", selectionStreamName)
	}
	var keys []statusKey
	a.barrier = true
	if err := behindBarrier(ctx, nc, r.o.holdID, func(ctx context.Context) error {
		var err error
		keys, err = statusSelectionKeys(ctx, nc)
		return err
	}); err != nil {
		return nil, err
	}
	store, err := r.store(ctx, familySession)
	if err != nil {
		return nil, err
	}
	var targets []cleanupTarget
	var malformed []string
	for _, key := range keys {
		if key.Deleted {
			continue
		}
		value, err := readSelectionValue(key)
		switch {
		case err != nil:
			malformed = append(malformed, fmt.Sprintf("%s (%v)", key.Key, err))
		case value.record != nil && len(value.record.Retired) > 0:
			targets = append(targets, cleanupTarget{family: familySession, logical: key.Key, store: store})
		}
	}
	if len(malformed) > 0 {
		return nil, unknownf(nil, "the values of %s in %s are malformed, so cleanup reads no record there", strings.Join(malformed, ", "), selection.SessionBucket)
	}
	return targets, nil
}

// cleanupPGTargets are family's intake_consumers records that hold a retired name. A missing table
// holds none; a row that is not its key's record of the family is UNKNOWN (selection.PGStore.List,
// selection.ErrMalformedRecord).
func (r *run) cleanupPGTargets(ctx context.Context, family string) ([]cleanupTarget, error) {
	store, err := r.pgStoreOf(ctx, family)
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if errors.Is(err, selection.ErrNoStore) {
		return nil, nil
	}
	if err != nil {
		return nil, unknownf(err, "%s's records", family)
	}
	var targets []cleanupTarget
	for _, logical := range slices.Sorted(maps.Keys(records)) {
		if len(records[logical].Retired) > 0 {
			targets = append(targets, cleanupTarget{family: family, logical: logical, store: store})
		}
	}
	return targets, nil
}

// cleanupRecord deletes target's retired names the server holds and drops from its record each one
// the server reports absent, the deleted ones included, in a write conditional on the revision it
// read. It answers the names it deleted and dropped. A record its family's creator refuses
// (checkRecordFence) is an error, and nothing of it is deleted; each name it deletes is the key's
// own (deleteIfOwned), and one that is not stays retired and is answered in the error. A retired
// name the record still selects is left as it is. A name whose info or delete failed stays
// retired, and is answered in the error.
func cleanupRecord(ctx context.Context, nc *nats.Conn, target cleanupTarget) ([]string, []string, error) {
	m, err := familyMember(target.family, target.logical)
	if err != nil {
		return nil, nil, err
	}
	var deleted []string
	var failed []string
	for range cleanupMaxConflicts {
		record, revision, err := target.store.Read(ctx, target.logical)
		if err != nil {
			return deleted, nil, err
		}
		if err := checkRecordFence(target.family, target.logical, record); err != nil {
			return deleted, nil, err
		}
		selected := []string{record.Current}
		if record.Op != nil {
			selected = append(selected, record.Op.Candidate, record.Op.Old)
		}
		failed = failed[:0]
		var gone []string
		for _, name := range record.Retired {
			if slices.Contains(selected, name) {
				continue
			}
			info, err := consumerInfo(ctx, nc, notifications, name)
			if err != nil {
				failed = append(failed, fmt.Sprintf("the info of %s: %v", name, err))
				continue
			}
			if info != nil {
				if err := deleteIfOwned(ctx, nc, m, info); err != nil {
					failed = append(failed, fmt.Sprintf("the delete of %s: %v", name, err))
					continue
				}
				if !slices.Contains(deleted, name) {
					deleted = append(deleted, name)
				}
			}
			gone = append(gone, name)
		}
		var failure error
		if len(failed) > 0 {
			failure = errors.New(strings.Join(failed, "; "))
		}
		if len(gone) == 0 {
			return deleted, nil, failure
		}
		next := record
		next.Retired = slices.DeleteFunc(slices.Clone(record.Retired), func(name string) bool { return slices.Contains(gone, name) })
		if len(next.Retired) == 0 {
			next.Retired = nil
		}
		_, err = target.store.Write(ctx, target.logical, next, revision)
		if errors.Is(err, selection.ErrConflict) {
			continue
		}
		if err != nil {
			return deleted, nil, err
		}
		return deleted, gone, failure
	}
	return deleted, nil, fmt.Errorf("its record changed under %d writes: %w", cleanupMaxConflicts, selection.ErrConflict)
}
