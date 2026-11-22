package consumernames

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// RecordedSessionConsumers is `envoy-infra streams census`'s reader of the selection bucket
// (writerhold.RecordedSessionConsumers), which the census calls after its barrier on the bucket:
// every key of the bucket (statusSelectionKeys), and for each record, every physical name it holds
// (its current, its open operation's candidate, and its retired names, which the census keeps
// until cleanup deletes them; censusNamesOf), each by its exact name in its family's shape.
//
//   - A session's record (a key selection.CheckKey accepts) names session consumers: pull
//     consumers acknowledged one at a time with no deliver group (bus.SessionConsumers'
//     applySettings), whose filters match each form of the session's agent subject. Their whole
//     filter list is the session's interest row's, a bucket the operator's grant does not read,
//     so the shape names only what every one of them holds (cmd/listener sessionConsumerFilter
//     puts contracts.LegacyAgentSubject first, and bus.SessionConsumerFilters gives its forms).
//   - A listener durable's record (a key selection.CheckDurableKey accepts, of a durable named
//     listener-<machine id>) names that listener's push durable, which the census compares as it
//     compares the fixed one (streams.DeclaredConsumers): every notification subject the build
//     carries, delivered to a reply inbox, acknowledged one at a time. These shapes stay beside the
//     recorded fresh names the census also takes (RecordedFreshNames, whose listener set holds the
//     same names): streams.shapeMismatch keeps a listener-<m>~<nuid> of that set only as the fresh
//     form of a declared listener-<m> shape, which the census has for each --listener-machine-id
//     alone, so a recorded durable of a machine the census does not declare is kept by its shape
//     here and by nothing else.
//
// Every other key holding a '.' is a system key (selection.SentinelKey, hold.barrier.<hold id>)
// and names nothing. A key whose last write is a delete marker is lost state, which this reader
// cannot tell from a session that never had a record, so it answers an error, and the census is
// UNKNOWN and removes nothing; so is a malformed value (readSelectionValue), a durable key of no
// listener, and a walk of the bucket that is not whole.
func RecordedSessionConsumers(ctx context.Context, conn *nats.Conn) ([]streams.ConsumerShape, error) {
	sessions, listeners, err := censusSelectionRecords(ctx, conn)
	if err != nil {
		return nil, err
	}
	var shapes []streams.ConsumerShape
	add := func(shape streams.ConsumerShape, record selection.Record) {
		for _, name := range censusNamesOf(record) {
			named := shape
			named.Name = name
			shapes = append(shapes, named)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(sessions)) {
		add(streams.ConsumerShape{
			Stream: notifications, AckPolicy: nats.AckExplicitPolicy,
			FilterCovers: bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject(id)}),
		}, sessions[id])
	}
	for _, durable := range slices.Sorted(maps.Keys(listeners)) {
		add(streams.ConsumerShape{
			Stream: notifications, FilterSubjects: bus.StreamSubjects(), Push: true, AckPolicy: nats.AckExplicitPolicy,
		}, listeners[durable])
	}
	return shapes, nil
}

// censusSelectionRecords reads every record of the selection bucket (statusSelectionKeys): each
// session's by its id (a key selection.CheckKey accepts), and each listener durable's by its
// durable name (a key selection.DurableKey made of a listener-<machine id>). Every other key
// holding a '.' is a system key (selection.SentinelKey, hold.barrier.<hold id>) and names nothing.
// Every value is read by its key's kind (readSelectionValue), the system keys' too. A record whose
// last write is a delete marker is lost state, which cannot be told from a session that never had
// a record; a durable's record of no listener is one no census shape takes; a malformed value, a
// document a reply subject stored there, cannot be told from a record or a sentinel; and a record
// its family's creator refuses (checkRecordFence: a name of another key or family, a retired fixed
// name) is no account of which consumers are the key's: each is an error, all of them named in one,
// which names the session family's store; so is a walk of the bucket that is not whole.
func censusSelectionRecords(ctx context.Context, conn *nats.Conn) (map[string]selection.Record, map[string]selection.Record, error) {
	keys, err := statusSelectionKeys(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	sessions, listeners := map[string]selection.Record{}, map[string]selection.Record{}
	var deleted, foreign, malformed []string
	for _, key := range keys {
		session := selection.CheckKey(key.Key) == nil
		durable, isDurable := censusDurableOf(key.Key)
		switch {
		case key.Deleted && (session || isDurable):
			deleted = append(deleted, key.Key)
			continue
		case key.Deleted:
			continue
		case isDurable && !strings.HasPrefix(durable, listenerFixedPrefix):
			foreign = append(foreign, key.Key)
			continue
		}
		value, err := readSelectionValue(key)
		if err == nil && value.record != nil {
			err = checkRecordFence(familySession, key.Key, *value.record)
		}
		switch {
		case err != nil:
			malformed = append(malformed, fmt.Sprintf("%s (%v)", key.Key, err))
		case session:
			sessions[key.Key] = *value.record
		case isDurable:
			listeners[durable] = *value.record
		}
	}
	var problems []string
	if len(deleted) > 0 {
		problems = append(problems, fmt.Sprintf("the records %s were deleted: their consumers cannot be told from planted ones", strings.Join(deleted, ", ")))
	}
	if len(foreign) > 0 {
		problems = append(problems, fmt.Sprintf("the records %s are durables' and no listener's: the census knows no shape for them", strings.Join(foreign, ", ")))
	}
	if len(malformed) > 0 {
		problems = append(problems, fmt.Sprintf("the values of %s are malformed: a document stored there cannot be told from a record", strings.Join(malformed, ", ")))
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("%s's store %s: %s", familySession, selection.SessionBucket, strings.Join(problems, "; "))
	}
	return sessions, listeners, nil
}

// The keys of RecordedNames' sets that are not cutover families: the session consumers and the
// listener durables, whose records share the selection bucket (and the cutover family session)
// but whose consumers the census compares by two different shapes.
const (
	recordedSessions  = "session"
	recordedListeners = "listener"
)

// RecordedNames are the physical names each family's selection records hold, a set per family:
// "session" (the session consumers), "listener" (every listener's durable), "dispatch"
// (Dispatch's durables) and daemon/<project key> for each daemon. A name in a set is one a record
// selects or may still select: its current, its open operation's candidate, or a retired name
// cleanup has not yet deleted (censusNamesOf). Every declared family has a set, empty when its
// store holds no record, but a daemon whose store is gone, which has none.
type RecordedNames map[string]map[string]bool

// ReadRecordedNames is the census's reader of every family's recorded names (RecordedNames), which
// the census calls after its barrier on the selection bucket. It reads the bucket for the
// session and listener sets (censusSelectionRecords) and each Postgres store for its family's set
// (selection.PGStore.List, its sentinel row aside): Dispatch's when families names dispatch or
// dispatchDatabaseFile names its store, from the database that file's URL names (openDatabase);
// and each daemon's that daemonDatabases names, every daemon family of families among them, each
// read by the census's rule for the daemon stores, at the address its one source names
// (censusReadDaemonStores, with deployments). A daemon store the rule finds gone has no set, and
// is answered instead, beside the names. A family families names whose store no flag names, a
// store it cannot open or whose table is missing, a daemon store the rule leaves unreconciled, a
// listing of the sources it cannot read, and a bucket censusSelectionRecords refuses each fail
// the read, named, and the census is then UNKNOWN and removes nothing.
func ReadRecordedNames(ctx context.Context, conn *nats.Conn, deployments writerhold.DaemonDeployments, families []string, dispatchDatabaseFile string, daemonDatabases []string) (RecordedNames, []writerhold.GoneStore, error) {
	if slices.Contains(families, familyDispatch) && dispatchDatabaseFile == "" {
		return nil, nil, fmt.Errorf("the recorded names: %s's store: no --dispatch-database-file names it", familyDispatch)
	}
	daemons, closeDaemons, err := censusReadDaemonStores(ctx, deployments, families, daemonDatabases)
	defer closeDaemons()
	if err != nil {
		return nil, nil, fmt.Errorf("the recorded names: %w", err)
	}
	sessions, listeners, err := censusSelectionRecords(ctx, conn)
	if err != nil {
		return nil, nil, fmt.Errorf("the recorded names: %w", err)
	}
	names := RecordedNames{recordedSessions: {}, recordedListeners: {}}
	for _, record := range sessions {
		censusAddNames(names[recordedSessions], record)
	}
	for _, record := range listeners {
		censusAddNames(names[recordedListeners], record)
	}
	var unread []string
	if dispatchDatabaseFile != "" {
		set, err := censusStoreNames(ctx, options{dispatchDatabaseFile: dispatchDatabaseFile}, familyDispatch)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s's store: %v", familyDispatch, err))
		} else {
			names[familyDispatch] = set
		}
	}
	var gone []writerhold.GoneStore
	for _, s := range daemons {
		family := daemonFamily(s.project)
		switch {
		case s.unreconciled != "":
			unread = append(unread, fmt.Sprintf("%s's store: %s", family, s.unreconciled))
			continue
		case s.gone != nil:
			gone = append(gone, *s.gone)
			continue
		}
		set, err := censusListNames(ctx, s.db, family)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s's store: %v", family, err))
			continue
		}
		names[family] = set
	}
	if len(unread) > 0 {
		return nil, nil, fmt.Errorf("the recorded names: %s", strings.Join(unread, "; "))
	}
	return names, gone, nil
}

// RecordedFreshNames is ReadRecordedNames in the form the census's input takes
// (writerhold.FreshNamesRead): family to the fresh names the records hold, each list sorted, for
// the listener durables, Dispatch's durables and each daemon's; and for each daemon whose store is
// gone, each of its consumers' names (enrollLogicalNames) to why, so the census leaves those
// consumers as they are and names each: a database that does not exist is evidence to skip its
// store, not to delete what its records would have kept (streams.CensusInput.RecordedFreshNames,
// which the census calls after its checks and its barrier on the selection bucket, so every record
// read is one the bucket's leader has applied). The session consumers' names are left out: the
// census declares them by their records' shapes (RecordedSessionConsumers). A read
// ReadRecordedNames refuses is refused, and the census is then UNKNOWN and removes nothing.
func RecordedFreshNames(ctx context.Context, conn *nats.Conn, deployments writerhold.DaemonDeployments, families []string, dispatchDatabaseFile string, daemonDatabases []string) (writerhold.FreshNamesRead, error) {
	names, gone, err := ReadRecordedNames(ctx, conn, deployments, families, dispatchDatabaseFile, daemonDatabases)
	if err != nil {
		return writerhold.FreshNamesRead{}, err
	}
	read := writerhold.FreshNamesRead{Names: make(map[string][]string, len(names))}
	for family, set := range names {
		if family == recordedSessions {
			continue
		}
		list := make([]string, 0, len(set))
		for name := range set {
			if strings.Contains(name, "~") {
				list = append(list, name)
			}
		}
		slices.Sort(list)
		read.Names[family] = list
	}
	for _, g := range gone {
		if read.Gone == nil {
			read.Gone = map[string]string{}
		}
		why := fmt.Sprintf("left, store gone: %s's database %s on instance %s does not exist", daemonFamily(g.Project), g.Database, g.Instance)
		for _, name := range enrollLogicalNames(g.Project) {
			read.Gone[name] = why
		}
	}
	return read, nil
}

// censusStoreNames is every name family's intake_consumers records hold, read from the store its
// file names (openDatabase, censusListNames), on a pool closed when it returns.
func censusStoreNames(ctx context.Context, o options, family string) (map[string]bool, error) {
	pool, err := openDatabase(ctx, o, family)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	return censusListNames(ctx, pool, family)
}

// censusListNames is every name family's intake_consumers records on db hold, read through the
// family's store's list (selection.PGStore.List, which reads only the family's own rows and leaves
// its sentinel row out). A row that is not its key's record of the family, one copied there from
// another key or family among them, fails the list (selection.ErrMalformedRecord), and so does a
// record its family's creator refuses (checkRecordFence).
func censusListNames(ctx context.Context, db Database, family string) (map[string]bool, error) {
	store, err := pgStore(family, db)
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if errors.Is(err, selection.ErrNoStore) {
		return nil, fmt.Errorf("it has no %s table", selection.IntakeConsumersTable)
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, logical := range slices.Sorted(maps.Keys(records)) {
		if err := checkRecordFence(family, logical, records[logical]); err != nil {
			return nil, err
		}
		censusAddNames(set, records[logical])
	}
	return set, nil
}

// censusAddNames adds every name record holds (censusNamesOf) to set.
func censusAddNames(set map[string]bool, record selection.Record) {
	for _, name := range censusNamesOf(record) {
		set[name] = true
	}
}

// censusNamedProjects are the project keys daemonDatabases names, sorted: one for each entry,
// <project>=<file> as consumer-names' --daemon-database takes it, each project once
// (parseDaemonDatabases), and the first entry's problem when one has one.
func censusNamedProjects(daemonDatabases []string) ([]string, error) {
	projects, problems := parseDaemonDatabases(daemonDatabases)
	if len(problems) > 0 {
		return nil, errors.New(problems[0])
	}
	slices.Sort(projects)
	return projects, nil
}

// censusUndefinedDatabase is Postgres's SQLSTATE for a database that does not exist: the answer
// that makes a daemon store gone (censusReadDaemonStore).
const censusUndefinedDatabase = "3D000"

// censusDaemonRead is one daemon store as the census's rule for the daemon stores found it
// (censusReadDaemonStore): read, with its store open and its sentinel row read; gone; or
// unreconciled, and why.
type censusDaemonRead struct {
	project string
	// db is the store's pool, open at the address the rule checked, nil for one the rule did not
	// open; the close censusReadDaemonStores answers closes it.
	db       *pgxpool.Pool
	state    selection.StoreState
	revision uint64
	// found is false for a store whose table holds no sentinel row.
	found bool
	// gone is the store confirmed gone, nil for any other.
	gone *writerhold.GoneStore
	// unreconciled is why the rule leaves the store as it is, "" for a store it read or found gone.
	unreconciled string
	// retired is true for a store whose one source is a retired project's entry of the retired
	// projects' ConfigMap (writerhold.RetiredStoresConfigMap), false for one its Deployment names.
	retired bool
}

// addressSource names the source of the address the rule read s at, as cleanup --gone and status
// print it: its Deployment, or, for a retired project, its entry of the retired projects' ConfigMap.
func (s censusDaemonRead) addressSource() string {
	if s.retired {
		return "its entry of " + retiredStoresSource
	}
	return "its Deployment"
}

// retiredStoresSource names the retired projects' ConfigMap as every reader of a daemon store names
// it (writerhold.RetiredStoresNamespace, writerhold.RetiredStoresConfigMap).
const retiredStoresSource = "ConfigMap " + writerhold.RetiredStoresNamespace + "/" + writerhold.RetiredStoresConfigMap

// daemonSource names d, one source of a daemon store's address (writerhold.KubeDaemonDeployments),
// as the readers of the daemon stores name it: "Deployment legion/<name>", or, for a retired
// project's entry, the retired projects' ConfigMap (retiredStoresSource).
func daemonSource(d writerhold.DaemonDeployment) string {
	if d.Retired {
		return retiredStoresSource
	}
	return "Deployment " + writerhold.DaemonNamespace + "/" + d.Name
}

// daemonSourceField names where d states annotation (writerhold.DaemonStoreDatabaseAnnotation or
// writerhold.DaemonStoreInstanceAnnotation): that annotation of a Deployment, or the data key
// <project>.database or <project>.instance of a retired project's entry.
func daemonSourceField(d writerhold.DaemonDeployment, annotation string) string {
	if !d.Retired {
		return "annotation " + annotation
	}
	if annotation == writerhold.DaemonStoreInstanceAnnotation {
		return "key " + d.Name + writerhold.RetiredStoreInstanceSuffix
	}
	return "key " + d.Name + writerhold.RetiredStoreDatabaseSuffix
}

// censusReadDaemonStores reads every daemon store a census names by the census's rule for the
// daemon stores, which both its readers of them keep (ReadRecordedNames,
// ReconcileDaemonStreamIDs). The stores are one for each daemonDatabases entry, <project>=<file>
// as consumer-names' --daemon-database takes it (censusNamedProjects); each daemon family of
// families (daemon/<project key>) must be one of them, and one that is not is unreconciled, since
// a declared daemon whose store it could not read would read as one with nothing recorded. It
// lists the sources of the stores' addresses once (deployments: the daemons' Deployments and the
// retired projects' entries, writerhold.KubeDaemonDeployments), and reads each store as
// censusReadDaemonStore does. It answers the stores in the order of their project keys, and a
// close of every store it opened, which the caller runs, error or not. A malformed entry, and a
// listing of the sources it cannot read, are its error.
func censusReadDaemonStores(ctx context.Context, deployments writerhold.DaemonDeployments, families, daemonDatabases []string) ([]censusDaemonRead, func(), error) {
	var stores []censusDaemonRead
	closeStores := func() {
		for _, s := range stores {
			if s.db != nil {
				s.db.Close()
			}
		}
	}
	projects, err := censusNamedProjects(daemonDatabases)
	if err != nil {
		return nil, closeStores, err
	}
	var listed []writerhold.DaemonDeployment
	if len(projects) > 0 {
		if deployments == nil {
			return nil, closeStores, errors.New("no reader of the daemon stores' addresses (the daemons' Deployments and the retired projects' entries), against which each daemon store's address is checked")
		}
		if listed, err = deployments(ctx); err != nil {
			return nil, closeStores, err
		}
	}
	o := options{daemonDatabases: daemonDatabases}
	for _, project := range projects {
		stores = append(stores, censusReadDaemonStore(ctx, o, project, listed))
	}
	for _, family := range families {
		project, ok := strings.CutPrefix(family, familyDaemonPrefix)
		if !ok || slices.ContainsFunc(stores, func(s censusDaemonRead) bool { return s.project == project }) {
			continue
		}
		stores = append(stores, censusDaemonRead{project: project, unreconciled: fmt.Sprintf("no --daemon-database %s=<file> names it", project)})
	}
	slices.SortFunc(stores, func(a, b censusDaemonRead) int { return strings.Compare(a.project, b.project) })
	return stores, closeStores, nil
}

// censusReadDaemonStore reads project's daemon store, its file named by o's --daemon-database
// entry, only at the address its one source names:
//
//   - exactly one source of deployments names the project (writerhold.DaemonProjectAnnotation): a
//     Deployment, or, for a project retired after the cutover, its entry of the retired projects'
//     ConfigMap (writerhold.KubeDaemonDeployments), never both, and it names the store's database
//     and instance (writerhold.DaemonStoreDatabaseAnnotation, writerhold.DaemonStoreInstanceAnnotation,
//     an entry's keys <project>.database and <project>.instance);
//   - no other source names that database on that instance (daemonAddressSharers): a store two
//     daemons' sources name is no one daemon's, so its records, its gone answer and its stream
//     identity would be read as each daemon's;
//   - the URL on the file's first line names that database, and the instance its second line
//     states, the one the operator's own proxy reaches with that URL, is that instance.
//
// A store that fails any is unreconciled, naming why and the source (a shared address names each
// source and its project), and is not opened. At that address it reads the store's sentinel row
// (censusReadSentinel): a server that answers SQLSTATE 3D000, the database does not exist, makes
// the store gone, with that answer. Every other answer leaves it unreconciled, naming why: a server
// that does not answer, any other SQLSTATE among them (57P01 and 57P03, which a dying server
// answers for a database it still holds, as a proxy's cached connection can reach it in the
// seconds after its instance is deleted), and any other failed read; and so does a row that is not
// exactly the family's own sentinel, one naming another family or none among them, which the
// family's store refuses (selection.ErrMalformedSentinel, its selection.ErrSentinelFamily). A store
// whose table holds no sentinel row is read, with found false.
func censusReadDaemonStore(ctx context.Context, o options, project string, deployments []writerhold.DaemonDeployment) censusDaemonRead {
	s := censusDaemonRead{project: project}
	family := daemonFamily(project)
	file, err := familyDatabaseFile(o, family)
	if err != nil {
		s.unreconciled = err.Error()
		return s
	}
	config, err := pgxpool.ParseConfig(file.url)
	if err != nil {
		s.unreconciled = fmt.Sprintf("%s: its first line is no database URL (the URL is not echoed): %v", file.flag, err)
		return s
	}
	var named []writerhold.DaemonDeployment
	deployed := 0
	for _, d := range deployments {
		if d.Annotations[writerhold.DaemonProjectAnnotation] == project {
			named = append(named, d)
			if !d.Retired {
				deployed++
			}
		}
	}
	if len(named) != 1 {
		sources := make([]string, len(named))
		for i, d := range named {
			sources[i] = daemonSource(d)
		}
		entry := "does not name it"
		if deployed < len(named) {
			entry = "names it"
		}
		s.unreconciled = fmt.Sprintf("%d Deployments of namespace %s name project %s (annotation %s), and %s %s (keys %s%s, %s%s): not one source, so no address is the store's: %s",
			deployed, writerhold.DaemonNamespace, project, writerhold.DaemonProjectAnnotation, retiredStoresSource, entry,
			project, writerhold.RetiredStoreDatabaseSuffix, project, writerhold.RetiredStoreInstanceSuffix, cmp.Or(strings.Join(sources, ", "), "none"))
		return s
	}
	source := daemonSource(named[0])
	database := named[0].Annotations[writerhold.DaemonStoreDatabaseAnnotation]
	instance := named[0].Annotations[writerhold.DaemonStoreInstanceAnnotation]
	sharers := daemonAddressSharers(deployments, named[0], database, instance)
	switch {
	case database == "":
		s.unreconciled = fmt.Sprintf("%s has no %s, so the store's address is unknown", source, daemonSourceField(named[0], writerhold.DaemonStoreDatabaseAnnotation))
	case instance == "":
		s.unreconciled = fmt.Sprintf("%s has no %s, so the store's address is unknown", source, daemonSourceField(named[0], writerhold.DaemonStoreInstanceAnnotation))
	case len(sharers) > 0:
		s.unreconciled = daemonSharedAddress(project, named[0], database, instance, sharers)
	case config.ConnConfig.Database != database:
		s.unreconciled = fmt.Sprintf("%s reaches database %q, and %s names database %q", file.flag, config.ConnConfig.Database, source, database)
	case file.instance == "":
		s.unreconciled = fmt.Sprintf("%s states no instance on its second line, so it does not show it reaches instance %q, which %s names", file.flag, instance, source)
	case file.instance != instance:
		s.unreconciled = fmt.Sprintf("%s states instance %q, and %s names instance %q", file.flag, file.instance, source, instance)
	}
	if s.unreconciled != "" {
		return s
	}
	s.retired = named[0].Retired
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		s.unreconciled = fmt.Sprintf("open its database (%s; the URL is not echoed): %v", file.flag, err)
		return s
	}
	s.db = pool
	s.state, s.revision, s.found, err = censusReadSentinel(ctx, pool, family)
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == censusUndefinedDatabase:
		s.gone = &writerhold.GoneStore{Project: project, Database: database, Instance: instance, StatedInstance: file.instance, Answer: pg.Error()}
	case errors.Is(err, selection.ErrSentinelFamily):
		s.unreconciled = fmt.Sprintf("its %s row is another family's sentinel, so the address %s names holds another store: %v", selection.PGSentinelKey(family), source, err)
	case err != nil:
		s.unreconciled = err.Error()
	}
	return s
}

// daemonAddressSharers are the sources of deployments, but own, that name the store address
// database on instance (writerhold.DaemonStoreDatabaseAnnotation,
// writerhold.DaemonStoreInstanceAnnotation): the Deployments of other daemons, and the entries of
// retired projects, whose store is the same database. Every daemon's store is its own database, so
// a shared address is refused by every reader of a daemon store (censusReadDaemonStore, enroll),
// never read as one daemon's.
func daemonAddressSharers(deployments []writerhold.DaemonDeployment, own writerhold.DaemonDeployment, database, instance string) []writerhold.DaemonDeployment {
	var sharers []writerhold.DaemonDeployment
	for _, d := range deployments {
		if (d.Name != own.Name || d.Retired != own.Retired) && d.Annotations[writerhold.DaemonStoreDatabaseAnnotation] == database &&
			d.Annotations[writerhold.DaemonStoreInstanceAnnotation] == instance {
			sharers = append(sharers, d)
		}
	}
	return sharers
}

// daemonSharedAddress is why a store whose address, database on instance, project's source own and
// each of sharers name is no one daemon's, naming each source (daemonSource) and its project.
func daemonSharedAddress(project string, own writerhold.DaemonDeployment, database, instance string, sharers []writerhold.DaemonDeployment) string {
	named := []string{fmt.Sprintf("%s (project %s)", daemonSource(own), project)}
	for _, d := range sharers {
		named = append(named, fmt.Sprintf("%s (project %s)", daemonSource(d), cmp.Or(d.Annotations[writerhold.DaemonProjectAnnotation], "none")))
	}
	return fmt.Sprintf("%s each name database %q on instance %q: a store two daemons share is no one daemon's, so none of them reads it",
		strings.Join(named, " and "), database, instance)
}

// censusNamesOf is every physical name record holds that the census keeps, once each: its
// current, its open operation's candidate, and its retired names. An open operation's predecessor
// (Op.Old) is not among them: a replace or delete the protocol reserves names the current there
// (selection.Names.Replace, Delete), which a publish retires in the same write (names.go publish),
// and convert's names the fixed name, which the declared set names by its own rule.
func censusNamesOf(record selection.Record) []string {
	names := []string{record.Current}
	if record.Op != nil {
		names = append(names, record.Op.Candidate)
	}
	names = append(names, record.Retired...)
	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	slices.Sort(names)
	return slices.Compact(names)
}

// censusDurableOf is the durable name a selection.DurableKey key was made from (durableOf), and
// false for a key DurableKey did not make: one CheckDurableKey refuses, one whose escape does not
// read back, and one DurableKey would not make again (lowercase digits, an escaped byte it writes
// plain), since it writes each escape in one form.
func censusDurableOf(key string) (string, bool) {
	if selection.CheckDurableKey(key) != nil {
		return "", false
	}
	name, err := durableOf(key)
	if err != nil || selection.DurableKey(name) != key {
		return "", false
	}
	return name, true
}

// censusMaxConflicts is how many refused writes of one sentinel ReconcileDaemonStreamIDs reads
// again past before it stops.
const censusMaxConflicts = 10

// ReconcileDaemonStreamIDs is the census's writer of the daemon stores' stream identity
// (selection.StoreState.StreamID; writerhold.StreamIDWriter). A Legion daemon reads no stream info
// and tags each last floor with its store's recorded identity, so after a hold that can recreate
// ENVOY_NOTIFICATIONS a floor read on the old stream must stop counting on the new one. It reads
// the stream's identity on conn (selection.StreamIdentity of STREAM.INFO, a read every phase of
// the operator's grant names), then every daemon store by the census's rule for them
// (censusReadDaemonStores, with deployments): one for each daemonDatabases entry, <project>=<file>
// as consumer-names' --daemon-database takes it, each daemon family of families (daemon/<project
// key>) among them, each read only at the address its one source names. It reads every store's
// sentinel row before it writes any, then rewrites each sentinel whose stream_id differs from the
// stream's identity, every other field kept, with selection.PGStore.WriteSentinel at the revision
// it read; a refused write (selection.ErrConflict) reads the row again. Its result places each
// store, and so each daemon family of families, in exactly one of its lists
// (writerhold.StreamIDResult.Covers), each list in the order of the stores' project keys: Changed
// for each store it rewrote, Gone for each it found gone, Reconciled for each it read and found
// already at the stream's identity, which it leaves unchanged, and Unreconciled for each it left
// as it is, with why.
//
// A store whose table holds no sentinel row yet (one enroll has not written) records no identity:
// it is left as it is, reconciled with none. A store the rule finds gone, its database answering
// that it does not exist, is no daemon's, and does not hold back the others. A store the rule
// leaves unreconciled is named with why: a declared family no entry names, an address that is not
// its source's, that another daemon's source names too, or that no one source names, a file or
// database it cannot open, a server that does not answer or answers any SQLSTATE but 3D000, a
// missing table, and a sentinel that does not decode or holds a field this build does not know,
// which a rewrite would drop. While any is, it writes nothing, and each stale store is
// unreconciled too, not written. A failed write after others leaves the stores changed so far in
// the result, and the failed one and each stale store still to write unreconciled. A malformed
// daemonDatabases entry, a listing of the sources it cannot read, and a stream that carries no
// identity fail the whole call, with nothing written: an empty identity is never written over a
// recorded one.
// The result it answers with an error is the empty one, and carries no promise: it places no
// store.
func ReconcileDaemonStreamIDs(ctx context.Context, conn *nats.Conn, deployments writerhold.DaemonDeployments, families, daemonDatabases []string) (writerhold.StreamIDResult, error) {
	if _, err := censusNamedProjects(daemonDatabases); err != nil {
		return writerhold.StreamIDResult{}, fmt.Errorf("the daemon stores' stream identity: %w", err)
	}
	info, err := streamInfo(ctx, conn, notifications)
	if err != nil {
		return writerhold.StreamIDResult{}, fmt.Errorf("the daemon stores' stream identity: the info of %s: %w", notifications, err)
	}
	current := selection.StreamIdentity(info)
	if current == "" {
		return writerhold.StreamIDResult{}, fmt.Errorf("the daemon stores' stream identity: %s carries no %s, so there is no identity to record", notifications, selection.StreamIdentityMetadataKey)
	}
	stores, closeStores, err := censusReadDaemonStores(ctx, deployments, families, daemonDatabases)
	defer closeStores()
	if err != nil {
		return writerhold.StreamIDResult{}, fmt.Errorf("the daemon stores' stream identity: %w", err)
	}
	// held is why a stale store is not written, "" while it may be: no store is written while any
	// is unreconciled, and none after a write that failed.
	var held string
	var unreconciled []string
	for _, s := range stores {
		if s.unreconciled != "" {
			unreconciled = append(unreconciled, daemonFamily(s.project))
		}
	}
	if len(unreconciled) > 0 {
		held = fmt.Sprintf("not written, since no store is written while any is unreconciled (%s)", strings.Join(unreconciled, ", "))
	}
	var result writerhold.StreamIDResult
	for _, s := range stores {
		switch {
		case s.unreconciled != "":
			result.Unreconciled = append(result.Unreconciled, writerhold.UnreconciledStore{Project: s.project, Reason: s.unreconciled})
		case s.gone != nil:
			result.Gone = append(result.Gone, *s.gone)
		case !s.found || s.state.StreamID == current:
			result.Reconciled = append(result.Reconciled, writerhold.ReconciledStore{Project: s.project, StreamID: s.state.StreamID})
		case held != "":
			result.Unreconciled = append(result.Unreconciled, writerhold.UnreconciledStore{Project: s.project, Reason: held})
		default:
			change, kept, err := censusRewriteStreamID(ctx, s, current)
			switch {
			case err != nil:
				result.Unreconciled = append(result.Unreconciled, writerhold.UnreconciledStore{Project: s.project, Reason: fmt.Sprintf("its %s row's write failed: %v", selection.PGSentinelKey(daemonFamily(s.project)), err)})
				held = fmt.Sprintf("not written, since %s's write failed before it", daemonFamily(s.project))
			case kept != nil:
				result.Reconciled = append(result.Reconciled, *kept)
			default:
				result.Changed = append(result.Changed, change)
			}
		}
	}
	return result, nil
}

// censusReadSentinel reads family's sentinel row of the intake_consumers table on db with its
// revision, through the family's store's check (readPGSentinel), and reports found false for a
// table that holds none. A row that is not exactly the family's own sentinel, one under another
// key, naming another family or none, or holding a field this build does not know, which a rewrite
// would drop, is refused (selection.ErrMalformedSentinel).
func censusReadSentinel(ctx context.Context, db Database, family string) (selection.StoreState, uint64, bool, error) {
	key := selection.PGSentinelKey(family)
	state, revision, found, err := readPGSentinel(ctx, db, family)
	switch {
	case pgNoTable(err):
		return selection.StoreState{}, 0, false, fmt.Errorf("it has no %s table", selection.IntakeConsumersTable)
	case errors.Is(err, selection.ErrMalformedSentinel):
		return selection.StoreState{}, 0, false, fmt.Errorf("its %s row is no sentinel of %s this build reads whole, and a rewrite would drop what it does not know: %w", key, family, err)
	case err != nil:
		return selection.StoreState{}, 0, false, fmt.Errorf("read its %s row: %w", key, err)
	}
	return state, revision, found, nil
}

// censusRewriteStreamID writes s's sentinel with stream_id current over the revision s read, and
// answers the change. A refused write reads the row again and writes over the revision read then,
// unless the row now records current, or no row is left, which it answers as the store
// reconciled, left unchanged by it, and no change.
func censusRewriteStreamID(ctx context.Context, s censusDaemonRead, current string) (writerhold.StreamIDChange, *writerhold.ReconciledStore, error) {
	family := daemonFamily(s.project)
	store, err := pgStore(family, s.db)
	if err != nil {
		return writerhold.StreamIDChange{}, nil, err
	}
	state, revision := s.state, s.revision
	for range censusMaxConflicts {
		next := state
		next.StreamID = current
		_, err := store.WriteSentinel(ctx, next, revision)
		if err == nil {
			return writerhold.StreamIDChange{Project: s.project, From: state.StreamID, To: current}, nil, nil
		}
		if !errors.Is(err, selection.ErrConflict) {
			return writerhold.StreamIDChange{}, nil, err
		}
		var found bool
		if state, revision, found, err = censusReadSentinel(ctx, s.db, family); err != nil {
			return writerhold.StreamIDChange{}, nil, err
		}
		if !found || state.StreamID == current {
			return writerhold.StreamIDChange{}, &writerhold.ReconciledStore{Project: s.project, StreamID: state.StreamID}, nil
		}
	}
	return writerhold.StreamIDChange{}, nil, fmt.Errorf("it changed under %d writes: %w", censusMaxConflicts, selection.ErrConflict)
}
