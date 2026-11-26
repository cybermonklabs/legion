package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/listenerwire"
)

// ErrEntryUnbound is a stored session record that is not the registration of the key it is stored
// under (DecodeEntry): one naming another key or the empty key, or naming its key with no machine,
// registration time or projects. Every reader reads it as no record.
var ErrEntryUnbound = errors.New("the session record is not the registration of its key")

type SessionEntry struct {
	// SessionID is the key the record is written under, which every writer sets (PutWithin). A
	// record naming another key is read as absent (DecodeEntry): it is not that session's
	// registration but a copy of another session's, which the server itself stores when a request
	// names the key's subject as its reply subject, so a direct get of one session's record can
	// lay that session's machine and projects over another's key. It is empty only on a record
	// an earlier build wrote, which names no key and is read as before by every reader here; a
	// reader that grants a session's scope across a project reads such an entry as no project's
	// (cmd/listener's edgeScope.shows). An entry naming no key encodes none, so it reads as
	// such a record and never as one naming the empty key, which every reader refuses.
	SessionID string `json:"session_id,omitempty"`
	// Port is always 0 for an entry this build writes: a registration naming a serving port is
	// refused (cmd/listener's subscribeHandler), and no delivery opens a connection to a
	// session. The field is kept because every released client parses it in the listener's
	// answers, and because an entry an older listener stored can still carry a port until it
	// lapses (the session bucket's TTL, 5 minutes by default).
	Port           int      `json:"port"`
	MachineID      string   `json:"machine_id"`
	Dir            string   `json:"dir"`
	Title          string   `json:"title"`
	Capabilities   []string `json:"capabilities"`
	UpdatedAt      int64    `json:"updated_at"`
	SelfSubscribed bool     `json:"self_subscribed,omitempty"`
	// Host is the machine the client-reported `machineID()` (envoy-client's
	// os.hostname()/ENVOY_MACHINE_ID) names as where the session process actually runs, as
	// opposed to MachineID, which is the registering listener's own ENVOY_MACHINE_ID, whether
	// every session registers through one shared listener or each machine runs its own; a
	// re-registration through another listener moves both, this entry's and the machine the
	// stored subscription names (store's mergeForUpsert). No delivery reads Host: a message
	// reaches the session from its own consumer, relayed by the listener the entry names,
	// wherever its process runs. Host is empty for a session whose plugin predates this field
	// (its registration had no "host"), and HostOrMachine falls back to MachineID so every display
	// caller sees a populated value.
	Host string `json:"host,omitempty"`
	// TmuxPlace is where the session's process sits in tmux, as its own client read it and sent
	// it with this registration. Nil for a session that is in no pane — one started outside
	// tmux, or one whose tmux server has gone — and for a session whose plugin predates the
	// field. A registration that carries no place clears one a previous registration stored,
	// since the entry is built fresh from each body (cmd/listener's sessionEntryFromSubscribe).
	TmuxPlace *TmuxPlace `json:"tmux_place,omitempty"`
	// Delivery is DeliveryConsumer when the session registered for consumer delivery and the
	// listener it registered through holds its session consumer (bus.SessionConsumers): the
	// session's stored messages reach it from that consumer, relayed by the listener that owns it,
	// so the durable path acknowledges them with nothing to deliver and no exception
	// (Resolve). Empty for a session on the core path: a plugin that did not ask, an older
	// listener, or a hub that refused the consumer.
	Delivery string `json:"delivery,omitempty"`
	// Epoch is the session's delivery epoch while Delivery is DeliveryConsumer: a random string per
	// plugin NATS connection, so a new one tells the relay the session reconnected and its consumer
	// restarts at its ack floor.
	Epoch string `json:"epoch,omitempty"`
	// ConsumerStart is the stream sequence the session's consumer starts at, as the registration
	// that wrote this entry read it (bus.SessionConsumers.Ensure). Every stored message at or after
	// it is the consumer's to deliver. One below it is in no consumer, and since a session receives
	// only what its own consumer relays, the durable path does not deliver it; which message it
	// NAKs, defers or skips, and what it logs, is decided by agentSubjectDelivery and
	// fanoutDelivery in cmd/listener/delivery.go (Resolve, ConsumerHoldsStored). A restart of the
	// consumer normally moves its start forward, to the sequence after its ack floor, past messages
	// it has acked, so a start recorded before that restart still names a sequence the consumer
	// covers from. Only a registration writes this field (cmd/listener sessionEntryFromSubscribe),
	// though, so a repair or a relay restart that creates the consumer below it leaves it above the
	// consumer's real start until the session's next registration rewrites it. A create that clamps
	// a start past the stream's end to the stream's first sequence does that where an earlier build
	// or a restored stream left a start past the end here, and so does a restart at an ack floor
	// the server lowered after the stream lost data, or at the stream's first sequence after its
	// sequences went back (bus.SessionConsumers.create, bus.SessionConsumers.RestartAtFloor). Until
	// then a message between the two starts reads as in no consumer, so the durable path does not
	// deliver it; which message it NAKs, defers or skips, and what it logs, is decided by
	// agentSubjectDelivery and fanoutDelivery in cmd/listener/delivery.go. The consumer holds that
	// message all the same and its relay delivers it, and once the next registration has rewritten
	// the start, ConsumerHoldsStored answers that the consumer holds it. Zero for a session on the
	// core path, and for one whose entry an earlier release wrote, which the durable path reads as
	// today: the consumer holds every stored message of it.
	ConsumerStart uint64 `json:"consumer_start,omitempty"`
	// Plugin is the npm package the session registered through and PluginVersion that plugin's
	// build; both empty for a session whose plugin predates the fields.
	Plugin        string `json:"plugin,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	// ClientInstanceID identifies the client instance that registered this session: one Oh My
	// Pi Envoy extension instance, one Claude Code channel-server process, each minting the id
	// at its own start and sending it on every registration. A soft claim that names this
	// session as its predecessor takes the role only when the claimant carries the same id, or
	// when either side carries none: the same conversation continues inside the instance that
	// held it, while another instance's live session keeps its role however it is named. Empty
	// for a client that predates the field.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// Projects is the projects the token that wrote this registration lists: the broker
	// token's own list, sorted, each once, and [] for a registration that presented none or
	// whose token lists none. Every listener that verifies broker tokens writes it at each
	// registration (cmd/listener's sessionEntryFromSubscribe), and an entry an earlier build
	// wrote decodes it as nil, which every reader takes as none. The edge listener reads it to
	// hold a send and a read to the target's projects (cmd/listener's project_scope.go). It is
	// the registry's own record: no answer of the listener's API carries it.
	Projects []string `json:"projects"`
	// RepositoryGap is the gap mark the session is owed, "" for none: an edge listener withheld a
	// GitHub event of the session while no fresh repository answer stood, and no frame carrying
	// the mark has been sent since (cmd/listener's sessionRelays). It lives on the entry so the
	// next listener to relay the session, after a move or a restart, sends the mark. Its value is
	// the id the relay that set it gave it. SetRepositoryGap and ClearRepositoryGap raise and clear
	// it, and PutClearingGap writes it empty for a claim refresh over a record no writer's stands
	// for, while keeping a standing writer's record's mark; every other write of the entry carries
	// over the mark of the entry it replaces (PutWithin). It is the registry's own record: no answer
	// of the listener's API carries it.
	RepositoryGap string `json:"repository_gap,omitempty"`
}

// storedEntry is a session record as DecodeEntry reads it: the entry, with the key it names and
// its projects kept raw, so a record naming the empty key or null is told apart from one naming no
// key at all, and one naming null projects from one naming none. The outer fields are the
// shallower of the two tagged session_id and projects, so they are the ones decoded.
type storedEntry struct {
	SessionEntry
	SessionID json.RawMessage `json:"session_id"`
	Projects  json.RawMessage `json:"projects"`
}

// DecodeEntry reads value, stored under key in the session bucket, as key's registration. A value
// naming a key (session_id) is that registration only when the key it names is key, and it names
// a machine (machine_id), a registration time (updated_at above 0) and its projects (the projects
// key present, null or a list), which every registration this build writes names; any other is
// refused with an error wrapping ErrEntryUnbound. The server itself stores such a value when a
// request names key's subject as its reply subject: a direct get's copy of another session's
// record, or of the session's own interest row, which names its key, machine and time but no
// projects. A value naming no key is what every build before SessionEntry.SessionID wrote: it is
// read exactly as before, and its entry's SessionID is empty, which a reader of a session's scope
// reads as no project's. A value that is not JSON is the decode error itself, as before. A field
// this build does not know is no refusal: a newer build's record must not read as gone here for
// the length of a rolling deploy.
func DecodeEntry(key string, value []byte) (SessionEntry, error) {
	var stored storedEntry
	if err := json.Unmarshal(value, &stored); err != nil {
		return SessionEntry{}, err
	}
	entry := stored.SessionEntry
	if stored.Projects != nil {
		if err := json.Unmarshal(stored.Projects, &entry.Projects); err != nil {
			return SessionEntry{}, err
		}
	}
	if stored.SessionID == nil {
		return entry, nil
	}
	switch {
	case json.Unmarshal(stored.SessionID, &entry.SessionID) != nil || entry.SessionID != key:
		return SessionEntry{}, fmt.Errorf("%w: stored under %s, it names session %s", ErrEntryUnbound, key, stored.SessionID)
	case entry.MachineID == "":
		return SessionEntry{}, fmt.Errorf("%w: stored under %s, it names no machine", ErrEntryUnbound, key)
	case entry.UpdatedAt <= 0:
		return SessionEntry{}, fmt.Errorf("%w: stored under %s, it names no registration time", ErrEntryUnbound, key)
	case stored.Projects == nil:
		return SessionEntry{}, fmt.Errorf("%w: stored under %s, it names no projects", ErrEntryUnbound, key)
	}
	return entry, nil
}

// TmuxPlace is a session's pane and the window and tmux session holding it, as the listener's
// wire package declares it (listenerwire.TmuxPlace): a session outside tmux is a nil TmuxPlace on
// the entry, never a place with empty members.
type TmuxPlace = listenerwire.TmuxPlace

// DeliveryConsumer is SessionEntry.Delivery for a session whose stored messages its session consumer
// delivers.
const DeliveryConsumer = "consumer"

// ConsumerHoldsStored reports whether the session's own consumer holds the stored message of stream
// sequence storedSeq, so no delivery path owes it anything: a session on consumer delivery is held
// every message at or after the sequence its consumer starts at (ConsumerStart). A durable message
// below that start is in no consumer — one NAKed, or deferred, before the consumer was created, and
// redelivered after it — and the durable path does not deliver it; which message it NAKs, defers or
// skips, and what it logs, is decided by agentSubjectDelivery and fanoutDelivery in
// cmd/listener/delivery.go. storedSeq 0 names a message that is not the durable's copy of a stored
// one (a core frame), whose stored copy the consumer holds whatever its sequence.
func (e SessionEntry) ConsumerHoldsStored(storedSeq uint64) bool {
	return e.Delivery == DeliveryConsumer && (storedSeq == 0 || storedSeq >= e.ConsumerStart)
}

// HostOrMachine is the session's real machine for display: Host when the registering plugin
// reported one, MachineID (the routing listener's own id) otherwise. MachineID keeps its
// routing meaning either way; this method only decides what a human sees.
func (e SessionEntry) HostOrMachine() string {
	return cmp.Or(e.Host, e.MachineID)
}

type SessionRegistryOption func(*sessionRegistryOpts)

type sessionRegistryOpts struct {
	replicas          int
	ttl               time.Duration
	maxAge            time.Duration
	liveWindow        time.Duration
	goneHorizon       time.Duration
	lastSeenRetention time.Duration
	log               *slog.Logger
	now               func() time.Time
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

// BucketMaxAge is the maximum age OpenSessionRegistry creates the envoy_sessions bucket with, and
// so the age a registration record lives for in a deployment that sets no ENVOY_SESSION_MAX_AGE
// (WithSessionMaxAge). A record the bucket expires reads gone, whatever its own age says, so a
// reader that acts on a gone session - the role claim reaper's period, say
// (cmd/listener's defaultRoleClaimReapInterval) - reads this rather than a copy of it.
const BucketMaxAge = 5 * time.Minute

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
}

// WithSessionMaxAge sets the maximum age the registry writes on the session bucket, at open and on
// every pass of its reaper (bus.SetKeyValueMaxAge, ParseSessionMaxAge). The default, and any
// non-positive value, writes none and leaves the bucket's own age exactly as it is. It is not the
// TTL the registry expires a cached row by: that is always the age the bucket really carries,
// read back after this one is written (SetTTL).
func WithSessionMaxAge(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.maxAge = d }
}

// WithLastSeenRetention sets how long after a session leaves the cache LastSeen still answers its
// final heartbeat. The default is LastSeenMemory (2 hours), longer than the gone horizon so that
// a caller told a session is gone can still say when it was last seen; any value shorter than the
// bucket's TTL is one TTL.
func WithLastSeenRetention(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.lastSeenRetention = d }
}

// WithSessionLiveWindow sets how long a session's last registration keeps it live (LiveWindow by
// default). A test makes it short to reach the unresponsive state without waiting out five
// minutes.
func WithSessionLiveWindow(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.liveWindow = d }
}

// WithSessionGoneHorizon sets how long past the live window an unresponsive session's record
// still stands before the session is gone (GoneHorizon by default).
func WithSessionGoneHorizon(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.goneHorizon = d }
}

// WithSessionLogger sets the logger the registry and its cache watcher write through. The listener
// passes its own, so their lines are JSON records with machine_id, as internal/store's are
// (store.WithLogger says why the listener never sets the default instead); without it they log
// where slog.Default() points.
func WithSessionLogger(log *slog.Logger) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.log = log }
}

// WithSessionClock sets the registry's clock, time.Now by default: the time a heartbeat records,
// and the time a row's expiry and a last-seen record's retention are measured against. Tests move
// it past a session's TTL without waiting it out.
func WithSessionClock(now func() time.Time) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.now = now }
}

// cachedSession is a SessionEntry plus its local expiry deadline. JetStream
// MaxAge TTL expiry is NOT delivered as a watcher delete event, so the registry
// enforces the TTL locally: a read reports an entry past expiresAt as absent, and
// the expiry timer removes it from the cache at that deadline (expireDue).
type cachedSession struct {
	entry     SessionEntry
	expiresAt time.Time // zero means never expires (ttl <= 0)
}

func (c cachedSession) expired(now time.Time) bool {
	return !c.expiresAt.IsZero() && now.After(c.expiresAt)
}

type lastSeenSession struct {
	lastSeen  int64
	expiresAt time.Time
}

func (s lastSeenSession) expired(now time.Time) bool {
	return !s.expiresAt.IsZero() && now.After(s.expiresAt)
}

// SessionRegistry is a cache-backed view of the envoy_sessions KV bucket. A
// long-lived WatchAll() goroutine keeps an in-memory cache in sync so List()
// and Get() answer from memory with no per-key JetStream round-trips. The old
// implementation did kv.Keys() then a sequential kv.Get() per key, which took
// 12s+ with ~83 sessions over a ~150ms Tailscale RTT and timed out
// GET /v1/sessions. This mirrors store.Registry, which already solved the same
// problem for the interest registry.
//
// Every watcher the registry's watcher installs reads the whole bucket again (kvwatch.Cache): the
// cache keeps every row through the read and removes the rows the read found no evidence of only
// once it completes (sessionCache), so a list served during the read is the copy it had.
type SessionRegistry struct {
	now func() time.Time
	// liveWindow and goneHorizon are the windows this registry judges a record's age against
	// (StateOf); zero, as in a registry built from a struct literal, means the package's own
	// (windows). Both are fixed when the registry is opened, so no lock covers them.
	liveWindow  time.Duration
	goneHorizon time.Duration
	// lastSeenRetention is WithLastSeenRetention's value; zero, as in a registry built from a
	// struct literal, means one TTL (retention).
	lastSeenRetention time.Duration
	// conn and js are the connection and the JetStream context the bucket was opened through,
	// which the maximum age is written through (applyMaxAge), and maxAge is WithSessionMaxAge's value, zero when the deployment
	// sets none and the bucket's own age stands. Both are fixed when the registry is opened, and
	// a registry built from a struct literal, which only a test does, has neither and writes no
	// age.
	conn   *nats.Conn
	js     nats.JetStreamContext
	maxAge time.Duration

	mu sync.RWMutex
	// ttl is the bucket's MaxAge, the lifetime of a row's local expiry and of a last-seen record,
	// and nothing to do with a session's state, which its own registration time decides (StateOf).
	// A read of a bucket with another MaxAge moves it (sessionCache.BeginRead), and so does the
	// listener when a pass of its reaper reads one (SetTTL).
	ttl time.Duration
	// cache is the copy of the bucket: its rows, revision fence, marks and read epoch
	// (kvwatch.Copy), which every write site reaches through. A row it removes keeps its last
	// heartbeat in lastSeen (removedLocked), and one it stores drops it (storedLocked). Its rows
	// are pointers so that a changed TTL can move every deadline in place with no row copied and
	// no record decoded again (SetTTL).
	cache    kvwatch.Copy[*cachedSession]
	lastSeen map[string]lastSeenSession
	// followingSince is when the read that last restored following swept the copy
	// (sessionCache.CompleteRead), and the zero time until the first one does, as in a registry
	// built from a struct literal, which has no watcher and so misses no record. No row left the
	// live view before it: a deadline that passed while the copy did not follow its bucket is no
	// evidence that the session ended, since the copy could not see the heartbeats that renewed
	// it either, so the retention of such a row's last heartbeat runs from here (departedLocked).
	followingSince time.Time

	// expiries holds the deadline of each cached row and of each lastSeen record, earliest first;
	// expiryIndex finds an id's item so a write moves it in place. expiryTimer is armed for
	// expiryArmedFor, the earliest deadline when it was last armed, and runs expireDue. A registry
	// built from a struct literal starts with none of them (scheduleLocked).
	expiries       expiryHeap
	expiryIndex    map[expiryKey]*expiryItem
	expiryTimer    *time.Timer
	expiryArmedFor time.Time

	// watcher feeds the cache from the session bucket and holds the handle the registry writes
	// through. Once a read has completed the cache is consistent with the bucket.
	watcher *kvwatch.Watcher

	log *slog.Logger

	// changeMu guards onChange, the functions OnChange registered, by registration number.
	changeMu   sync.Mutex
	onChange   map[uint64]func(sessionID string)
	nextChange uint64

	// changed is every session the work under r.mu changed, which unlockAndNotify reports once
	// that lock is released. Guarded by r.mu.
	changed []string

	// leaderSeam is a test-only, per-session override of the leader read StoredByWriter makes
	// (bus.KeyValue.NewestFromLeader). Its zero value, the production value, overrides nothing, so
	// every StoredByWriter reads the bucket's stream leader; a cmd/listener handler test installs
	// overrides through SetStoredByWriterSeamForTest (testhooks.go).
	leaderSeam storedByWriterSeam
}

// OnChange registers changed to be called with a session's id after each change to the session's
// cached entry: a write (Put, Refresh, or the watcher delivering another listener's write), a delete
// or purge, an eviction of a malformed value, the cache's reset for a recreated bucket (each id it
// held), and a lapse. A lapse has no watch event (JetStream MaxAge expiry is not delivered, see
// cachedSession), so the registry finds it from the entry's own deadline: the expiry timer removes
// it then (expireDue), and PruneLapsed runs that sweep at once for a caller that must not wait for
// the timer.
//
// changed runs outside the registry's lock, on the goroutine that made the change: the watcher's,
// the expiry timer's, or a caller's of Put, Delete, Refresh or PruneLapsed. Calls can therefore run
// concurrently and out of revision order, so changed reads the session's current state (Get) rather
// than trusting the order of its calls, and it must not block. stop unregisters it.
func (r *SessionRegistry) OnChange(changed func(sessionID string)) (stop func()) {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	if r.onChange == nil {
		r.onChange = map[uint64]func(string){}
	}
	r.nextChange++
	id := r.nextChange
	r.onChange[id] = changed
	return func() {
		r.changeMu.Lock()
		delete(r.onChange, id)
		r.changeMu.Unlock()
	}
}

// PruneLapsed drops every cached entry whose deadline has passed, calling OnChange's functions for
// each, instead of leaving each one to the expiry timer that holds its deadline (expireDue): a
// caller that must see a lapse promptly calls it on its own ticker. It removes an entry exactly as
// that timer would, through the copy's own removal (kvwatch.Copy.Remove), so the timer's item for
// it finds the entry gone and does nothing, and a deadline the heap never held (an entry a test
// placed in the cache itself) is dropped here too. While the copy does not follow its bucket it
// drops nothing, as that timer pops nothing: the copy could not see the heartbeats that renewed
// those rows either, so a deadline that passed there is no lapse. List answers the entries that
// remain, the sessions that are live.
func (r *SessionRegistry) PruneLapsed() {
	r.mu.Lock()
	defer r.unlockAndNotify()
	if !r.following() {
		return
	}
	now := r.now()
	for sessionID, cs := range r.cache.All() {
		if !cs.expired(now) {
			continue
		}
		r.scheduleLocked(sessionID, expireLastSeen, r.retireLocked(sessionID, cs, now))
		r.cache.Remove(sessionID)
		r.changedLocked(sessionID)
	}
}

// notifyChanged calls OnChange's functions for each of sessionIDs. Callers must not hold r.mu.
func (r *SessionRegistry) notifyChanged(sessionIDs ...string) {
	if len(sessionIDs) == 0 {
		return
	}
	r.changeMu.Lock()
	if len(r.onChange) == 0 {
		r.changeMu.Unlock()
		return
	}
	callbacks := make([]func(string), 0, len(r.onChange))
	for _, changed := range r.onChange {
		callbacks = append(callbacks, changed)
	}
	r.changeMu.Unlock()
	for _, sessionID := range sessionIDs {
		for _, changed := range callbacks {
			changed(sessionID)
		}
	}
}

// changedLocked records sessionID as changed by the work under way, for unlockAndNotify to report
// once the lock is released. Callers hold r.mu for writing.
func (r *SessionRegistry) changedLocked(sessionID string) {
	r.changed = append(r.changed, sessionID)
}

// unlockAndNotify releases r.mu and calls OnChange's functions for every session the work under
// that lock changed: each row the copy stored or removed for the bucket's sake (storedLocked,
// removedLocked) and each the cache removed itself (expireDue, PruneLapsed). Every write site
// defers it in place of r.mu.Unlock, so no path changes the copy without reporting it. Callers
// hold r.mu for writing.
func (r *SessionRegistry) unlockAndNotify() {
	changed := r.changed
	r.changed = nil
	r.mu.Unlock()
	r.notifyChanged(changed...)
}

// OpenSessionRegistry opens the session bucket on conn, creating or raising it to the registry's
// copies (bus.EnsureKeyValue) for as long as ctx lasts, and starts its cache.
func OpenSessionRegistry(ctx context.Context, conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
	opts := sessionRegistryOpts{replicas: 1, ttl: BucketMaxAge, liveWindow: LiveWindow, goneHorizon: GoneHorizon, lastSeenRetention: LastSeenMemory, now: time.Now}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(ctx, conn, js, &nats.KeyValueConfig{
		Bucket:   bus.SessionBucket,
		TTL:      opts.ttl,
		Replicas: opts.replicas,
		Storage:  nats.FileStorage,
	})
	if err != nil {
		return nil, err
	}
	// The deployment's maximum age is written before the bucket's own is read, so the TTL below is
	// the age the bucket carries from here on. A bucket created by an older build at the 5-minute
	// default is raised here, and a record then stands past the gone horizon instead of being
	// dropped while its session is only unresponsive.
	if _, err := bus.SetKeyValueMaxAge(ctx, conn, js, bus.SessionBucket, opts.maxAge); err != nil {
		return nil, err
	}
	// Expire against the bucket's real MaxAge, not the hardcoded default. If the
	// envoy_sessions bucket was created with a different TTL (an older build, a
	// config change), expiring against opts.ttl would either hide live sessions
	// or surface expired ones. Status() is one round-trip; fall back to the
	// configured ttl if it fails or reports no TTL. Every read of the bucket adopts its MaxAge
	// again (sessionCache.BeginRead), so a bucket recreated with another TTL expires by its own.
	ttl := opts.ttl
	if status, statusErr := kv.Status(); statusErr == nil {
		if bucketTTL := status.TTL(); bucketTTL > 0 {
			ttl = bucketTTL
		}
	}
	r := &SessionRegistry{
		now:               opts.now,
		liveWindow:        opts.liveWindow,
		goneHorizon:       opts.goneHorizon,
		lastSeenRetention: opts.lastSeenRetention,
		conn:              conn,
		js:                js,
		maxAge:            opts.maxAge,
		ttl:               ttl,
		lastSeen:          map[string]lastSeenSession{},
		log:               opts.log,
	}
	r.cache = newSessionCopy(r)
	r.watcher = kvwatch.New("session registry", kv, sessionCache{r}, kvwatch.WithLogger(opts.log))
	// The watcher populates the cache asynchronously. A synchronous Keys()+per-key Get() loop
	// blocks for seconds when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// newSessionCopy is the copy of the session bucket r keeps: the one place its rows are decoded and
// the one place what it records beside them is kept in step (storedLocked, removedLocked).
func newSessionCopy(r *SessionRegistry) kvwatch.Copy[*cachedSession] {
	return kvwatch.NewCopy(kvwatch.Rows[*cachedSession]{Decode: r.decodeLocked, Stored: r.storedLocked, Removed: r.removedLocked})
}

// Ping verifies the session KV bucket is reachable AND the cache watcher is
// still alive. It uses kv.Status() (one round-trip) so /healthz and the
// self-health monitor stay fast — they must never iterate keys. A dead watcher
// is reported so /healthz exposes the stale cache while NATS reconnects.
func (r *SessionRegistry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	return r.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the registry writes through to conn
// (kvwatch.Watcher.Rewatch), waiting on nothing past ctx, the reconnect hook's run.
func (r *SessionRegistry) Rewatch(ctx context.Context, conn *nats.Conn) error {
	return r.watcher.Rewatch(ctx, conn)
}

// Servable reports whether the copy List answers from can be served (kvwatch.Watcher.Servable):
// nil once a read of the bucket has completed and the copy follows it, or stopped following it no
// longer ago than the stale bound, and kvwatch.ErrRefilling otherwise. It reads the watcher's
// state only, never the cache.
func (r *SessionRegistry) Servable() error {
	return r.watcher.Servable()
}

// NeedsRead reports whether the copy does not follow its bucket with no read under way to make it,
// which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (r *SessionRegistry) NeedsRead() bool {
	return r.watcher.NeedsRead()
}

// Disconnected says the connection the cache's watcher reads through was lost
// (kvwatch.Watcher.Disconnected).
func (r *SessionRegistry) Disconnected() {
	r.watcher.Disconnected()
}

// following reports whether the copy follows its bucket (kvwatch.Watcher.Following). A registry
// built from a struct literal, which only a test does, has no watcher and so misses no record: its
// rows expire by their deadlines alone.
func (r *SessionRegistry) following() bool {
	return r.watcher == nil || r.watcher.Following()
}

// logger is the logger WithSessionLogger set, or the default one.
func (r *SessionRegistry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// sessionCache is the registry as the cache its watcher feeds (kvwatch.Cache). The hooks are its
// own type's, not the registry's methods, so nothing but the watcher calls them.
type sessionCache struct{ r *SessionRegistry }

// BeginRead starts a read of the session bucket (kvwatch.Copy.BeginRead) with the bucket's MaxAge
// as the ttl when it has one, so a bucket recreated with another TTL expires its rows by its own.
// Every row stays.
func (c sessionCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.unlockAndNotify()
	r.cache.BeginRead(read)
	if read.MaxAge > 0 {
		r.ttl = read.MaxAge
	}
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could not
// decode, which evicted the session.
func (c sessionCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.unlockAndNotify()
	if err := r.cache.Delivered(entry); err != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every row and fence entry the read found no evidence of: the bucket no
// longer holds that session. A removed row's last heartbeat is kept for LastSeen, and its record's
// own deadline is scheduled, as an expired row's are (removedLocked). The copy follows its bucket
// from here (kvwatch.Watcher.completeRead calls this and only then records that it does), so this
// is where a row the copy retained past its deadline while it did not follow leaves the live view:
// the sweep takes away the rows the bucket no longer holds, and the rows it keeps are judged
// against their age again from now (departedLocked). A recreated bucket's first read is where the
// rows of the bucket that went are reported gone, since the copy keeps them until the sweep.
func (c sessionCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.unlockAndNotify()
	r.followingSince = r.now()
	r.cache.CompleteRead()
}

// decodeLocked is the row a put record of the bucket, delivered or read, leaves in the cache, with
// its local expiry measured from the record's Created (expiryFor). A record that is not the
// registration of its key (DecodeEntry) is an error, as
// one that does not decode is: the copy keeps no row for it and evicts the row it held
// (kvwatch.Rows.Decode), its caller logs it, and every read answers the session as having no
// record — Lookup gone, Get and Refresh nats.ErrKeyNotFound, List without it — so no reader takes
// another session's machine or projects from a record copied under this key.
//
// A record older than the row the copy removed at its deadline needs no test of its own here: the
// copy keeps that key's revision in its fence when it removes a row for the cache's own reasons
// (kvwatch.Copy.Remove), and drops the fence only at a completed read that found the bucket no
// longer holds the key, so such a replay is fenced out before it is decoded. A record the copy
// does keep is answered absent once it is past its deadline (Get) and removed at it (expireDue).
// Callers hold r.mu.
func (r *SessionRegistry) decodeLocked(entry kvwatch.Entry) (*cachedSession, bool, error) {
	item, err := DecodeEntry(entry.Key, entry.Value)
	if err != nil {
		return nil, false, err
	}
	return &cachedSession{entry: item, expiresAt: r.expiryFor(entry.Created, item.UpdatedAt)}, true, nil
}

// expiryFor computes the local TTL deadline for a cached entry. It prefers the
// KV revision's Created() timestamp (the JetStream MaxAge clock), falling back
// to the SessionEntry.UpdatedAt set by Put when no KV metadata is available
// (the write-through path). A non-positive ttl disables local expiry. Callers hold r.mu.
func (r *SessionRegistry) expiryFor(created time.Time, updatedAtMillis int64) time.Time {
	if r.ttl <= 0 {
		return time.Time{}
	}
	if !created.IsZero() {
		return created.Add(r.ttl)
	}
	if updatedAtMillis > 0 {
		return time.UnixMilli(updatedAtMillis).Add(r.ttl)
	}
	return r.now().Add(r.ttl)
}

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (r *SessionRegistry) StopWatch() {
	r.watcher.Stop()
}

// EndWatch ends the cache's watchers at shutdown, within ctx, while the connection still carries
// requests (kvwatch.Watcher.End).
func (r *SessionRegistry) EndWatch(ctx context.Context) {
	r.watcher.End(ctx)
}

// WaitForCacheReady blocks until the watcher has delivered every existing session, or until ctx
// is done. Callers bound ctx; on error the caller logs and proceeds (fail open): Put write-through
// keeps local sessions visible and /healthz exposes an unavailable registry.
func (r *SessionRegistry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}

// TTL returns the maximum age of the backing session bucket, the backstop the registry expires a
// cached row by. It is not the window a session's state is measured in (LiveWindow).
func (r *SessionRegistry) TTL() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ttl
}

// SetTTL moves the bucket's maximum age the registry expires its rows by, for a caller that has
// just read one from the bucket itself. Every cached row's deadline moves with it, by the same
// amount, so each still stands one TTL from the moment its record was written: the registry keeps
// no other record of that moment, and decoding every row again to find it would cost a read of
// the whole bucket. A row whose new deadline has already passed is not removed here; it is
// scheduled like any other, and the expiry timer takes it at once through the one path that
// removes an expired row (expireDue), which keeps its last heartbeat for the retention and its
// revision in the fence. While the copy does not follow its bucket that timer expires nothing, so
// such a row waits for the read that restores following, as every other expiry does.
//
// A non-positive TTL turns local expiry off: the rows lose their deadlines and stay until a
// record or a completed read removes them.
func (r *SessionRegistry) SetTTL(ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.ttl
	if ttl == was {
		return
	}
	r.ttl = ttl
	for id, cs := range r.cache.All() {
		switch {
		case ttl <= 0:
			cs.expiresAt = time.Time{}
		case cs.expiresAt.IsZero():
			cs.expiresAt = r.expiryFor(time.Time{}, cs.entry.UpdatedAt)
		default:
			cs.expiresAt = cs.expiresAt.Add(ttl - was)
		}
		if !cs.expiresAt.IsZero() {
			r.setExpiryLocked(id, expireLive, cs.expiresAt)
		}
	}
	r.armExpiryLocked()
}

// windows is the live window and the gone horizon this registry judges a record's age against:
// the options' values, and the package's own for a registry built from a struct literal, which
// only a test does. Both are fixed when the registry is opened, so no lock covers them.
func (r *SessionRegistry) windows() (live, horizon time.Duration) {
	return cmp.Or(r.liveWindow, LiveWindow), cmp.Or(r.goneHorizon, GoneHorizon)
}

// stateOf is entry's state on this registry's windows (StateOf).
func (r *SessionRegistry) stateOf(entry SessionEntry, now time.Time) State {
	live, horizon := r.windows()
	return StateOf(entry, now, live, horizon)
}

// Windows is the live window and the gone horizon this registry judges a record's age against,
// for a caller that must measure a session's age by the same windows: the listener's role-claim
// reaper gives a restored claim's holder one live window to register again, since that is how
// long a registration keeps a session live here.
func (r *SessionRegistry) Windows() (live, horizon time.Duration) {
	return r.windows()
}

// EntryState is entry's state on this registry's windows, as of now, for a caller holding a
// record this registry's cache did not answer for: a read of the bucket the copy did not take
// (Refresh). A caller naming a session asks Lookup, which reads the record and judges it
// together.
func (r *SessionRegistry) EntryState(entry SessionEntry) State {
	return r.stateOf(entry, r.now())
}

// applyMaxAge writes the deployment's maximum age on the bucket when it has one (WithSessionMaxAge)
// and then adopts the age the bucket really carries, so a raise or a rollback another process made
// reaches this registry within one call and with no restart (SetTTL). The write is skipped on a
// registry built from a struct literal, which has no JetStream context, and on one whose
// deployment sets no age, which leaves the bucket's own standing.
//
// A write answers with the age the bucket carries once it has run (bus.SetKeyValueMaxAge), so
// only the registry that writes none reads the bucket's status for it: one stream read a pass,
// not two.
//
// It adopts only a positive age, as the open-time read does: a status read that reports none is
// as likely a bucket this build did not create as one that truly keeps its records for good, and
// turning local expiry off on that word would keep every row of a recreated bucket for the life
// of the process.
func (r *SessionRegistry) applyMaxAge(ctx context.Context) error {
	if r.js != nil && r.maxAge > 0 {
		written, err := bus.SetKeyValueMaxAge(ctx, r.conn, r.js, bus.SessionBucket, r.maxAge)
		if err != nil {
			return err
		}
		r.adoptMaxAge(written)
		return nil
	}
	kv, _ := r.watcher.Handle()
	status, err := kv.Status()
	if err != nil {
		return fmt.Errorf("read the %s bucket's maximum age: %w", bus.SessionBucket, err)
	}
	r.adoptMaxAge(status.TTL())
	return nil
}

// adoptMaxAge expires cached rows by age, the bucket's own backstop, when that age is positive
// and is not the one this registry already runs with.
func (r *SessionRegistry) adoptMaxAge(age time.Duration) {
	if age > 0 && age != r.TTL() {
		r.SetTTL(age)
	}
}

// retention is how long after a session leaves the cache its lastSeen record is kept:
// WithLastSeenRetention's value, and never less than one TTL, so a registry built from a struct
// literal keeps it one TTL. It means nothing when the TTL is not positive: then nothing expires
// and no record is kept. Callers hold r.mu.
func (r *SessionRegistry) retention() time.Duration {
	return max(r.lastSeenRetention, r.ttl)
}

// CacheReady reports whether the cache is ready (kvwatch.Watcher.Ready). Observability for
// /healthz; it does not iterate keys, so it never hangs.
func (r *SessionRegistry) CacheReady() bool {
	return r.watcher.Ready()
}

// CacheSize returns the number of cached sessions. Observability only; reports
// the raw count, including expired entries the expiry timer has not removed yet.
func (r *SessionRegistry) CacheSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache.Len()
}

// LiveCount returns the number of live sessions List would answer, counted under the shared lock
// with no allocation: the rows whose last registration is within the live window. It counts no
// unresponsive row, whatever the bucket's maximum age is — with a maximum age far longer than the
// live window, the rows that have not expired are every row the bucket holds, and counting those
// would report a box full of stalled sessions as a box full of working ones.
func (r *SessionRegistry) LiveCount() int {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	live := 0
	for _, cs := range r.cache.All() {
		if r.stateOf(cs.entry, now) == Live {
			live++
		}
	}
	return live
}

// WatchErr is the cache watcher's terminal error, or nil while it runs, so the listener can tell a
// rebuildable dead cache from a transient KV timeout.
func (r *SessionRegistry) WatchErr() error {
	return r.watcher.Err()
}

// Put is PutWithin with no deadline of its own, for a caller that answers no request.
func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	return r.PutWithin(context.Background(), sessionID, entry)
}

// PutWithin writes sessionID's entry, each bucket call waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. A write ctx abandoned may still land after this
// answer. That is safe: it writes the entry the caller's retry writes again, it rides the
// connection ahead of any write this listener sends on it later, so it cannot undo one, and it is
// conditional on the revision it replaces.
//
// The entry written carries the gap mark of the entry it replaces (RepositoryGap), whatever
// entry's own field says: the mark is SetRepositoryGap's and ClearRepositoryGap's alone to write,
// and a registration or a renewal written over an entry never clears a mark no frame has carried,
// nor raises one no withheld event owes. So the write is conditional on the revision of the entry
// it replaces (writeEntryAt): the copy's own entry and revision first, with no bucket call, and
// when the copy holds none, or another write landed since (another listener's mark among them),
// the bucket's entry read again (readEntry), from the bucket stream's leader after a lost write
// (readEntryFromLeader), and entry written over it with that entry's mark, until the write lands,
// ctx ends or entryWriteLosses writes are lost (writeEntryAt's stop rule). Every other field is
// entry's, so against another listener's write it is last-writer-wins, as it always was.
//
// The entry is written naming sessionID (SessionEntry.SessionID) and stamped with this moment, so
// every reader reads it as sessionID's registration (DecodeEntry). A caller that writes back an
// entry it read keeps that entry's Projects only where the entry names sessionID already: one
// naming no key may be another session's record the server copied under this one, and this write
// would bind its projects to sessionID (cmd/listener's roleSetHandler). An entry naming no machine
// is refused with ErrEntryUnbound and nothing is written: every reader would read it as no record,
// while this registry's own copy held it until its watcher delivered it.
func (r *SessionRegistry) PutWithin(ctx context.Context, sessionID string, entry SessionEntry) error {
	if entry.MachineID == "" {
		return fmt.Errorf("write session %s to the bucket: %w: the entry names no machine", sessionID, ErrEntryUnbound)
	}
	entry.SessionID = sessionID
	// Several live processes can hold the same session (shared on-disk state) - a harness can
	// keep a session's state on shared disk - and each heartbeats a claim of its own. The route
	// follows whichever wrote last, and this write stamps the entry with the moment it was made,
	// so a reader weighing the route's freshness (ClaimStaleAfter) reads the latest heartbeat.
	entry.UpdatedAt = r.now().UnixMilli()
	// The gap mark the session is owed (SessionEntry.RepositoryGap) is carried from the entry this
	// write replaces, never from this registry's copy where that copy trails the bucket. carried
	// holds that mark through the write loop: the copy's entry where the copy holds one at the fence,
	// else the bucket's read - a direct get on the first pass, decoded with DecodeEntry so a record
	// every reader reads as absent (one naming another key) owes no mark, and the stream leader on a
	// retry (readEntryFromLeader), which reports the sequence a record the server stored carries
	// behind another stream's header so the write replaces it there. The copy's entry is at the
	// fence's revision whenever the copy holds one, since the copy stores an entry only at or past its
	// fence and moves the fence with it (kvwatch.Copy.FenceFor). A fence of another epoch reads 0, and
	// a key the copy holds no entry of is no entry to carry a mark from, so either is read from the
	// bucket first, and the write is fenced on the revision it read. Every other field is entry's, so
	// against another listener's write it is last-writer-wins, as it always was. (PutClearingGap has
	// its own loop, since the cache cannot tell a RePublish from a registration.)
	carried := entry.RepositoryGap
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	replaced, cached := r.cache.Get(sessionID)
	revision := r.cache.FenceFor(epoch, sessionID)
	known := cached && revision > 0
	if known {
		carried = replaced.entry.RepositoryGap
	}
	r.mu.RUnlock()
	for pass := range entryWriteLosses {
		if !known {
			if pass > 0 {
				// A record the server stored carries another stream's first Nats-Sequence header,
				// so a direct get's revision fences the write out for good; the leader read reports
				// the sequence the record is stored at, so the write replaces it there, and carries
				// only a writer's record's mark (readEntryFromLeader). The leader holds every write
				// the bucket took, so a copy that answers a direct get a write behind cannot spend
				// the write's bound of lost writes (entryWriteLosses).
				stored, at, found, err := readEntryFromLeader(ctx, kv, sessionID)
				if err != nil {
					return fmt.Errorf("read session %s from the bucket's stream leader before writing it: %w", sessionID, err)
				}
				revision, carried = at, ""
				if found {
					carried = stored.RepositoryGap
				}
			} else {
				// The direct get is decoded with DecodeEntry, so a record every reader reads as
				// absent - one naming another key the server copied under this one - owes no mark,
				// and is written over at the revision it was read at.
				stored, at, err := readEntry(ctx, kv, sessionID)
				switch {
				case errors.Is(err, errUndecodableEntry):
					revision, carried = at, ""
				case err != nil:
					return fmt.Errorf("read session %s from the bucket before writing it: %w", sessionID, err)
				default:
					revision, carried = at, stored.RepositoryGap
				}
			}
		}
		entry.RepositoryGap = carried
		lost, err := r.writeEntryAt(ctx, kv, epoch, sessionID, entry, revision)
		if err != nil {
			return fmt.Errorf("write session %s to the bucket: %w", sessionID, err)
		}
		if !lost {
			return nil
		}
		known = false
	}
	return fmt.Errorf("write session %s to the bucket: %w", sessionID, errEntryWritesLost)
}

// PutClearingGap writes machineID, selfSubscribed and projects over a session's record as a role
// claim's refresh, clearing a forged record (cmd/listener's roleSetHandler). It reads the stream
// leader on every pass (readForClear), never the cache, since the cache cannot tell a RePublish from a
// registration, and decides from the record each pass read:
//   - the session's own writer's record (DecodeEntry binds it and its SessionID is the key): the forged
//     premise the clear rested on is gone, so this writes that record back with only machineID,
//     selfSubscribed and projects changed, UpdatedAt stamped and Port written out, and every other
//     field kept - the gap mark, the delivery set (Delivery, Epoch, ConsumerStart, read together so the
//     record never reads as an earlier release's), the client instance, capabilities and display
//     fields - so a live session's own registration that landed since the claim read it as forged is
//     not cleared by this write.
//   - any other record - a RePublish, a marker, a copy of another key's record, or one naming no key:
//     the three values and nothing else, no gap mark, so the forged record is cleared at its true
//     sequence.
//
// The entry is rebuilt on every pass, so a pass that reads a forged record after one that read the
// writer's record clears, and never writes the earlier pass's record's fields. It ends, as every
// write of an entry does, once it has lost entryWriteLosses writes (writeEntryAt's stop rule). An
// empty machineID is refused with ErrEntryUnbound.
func (r *SessionRegistry) PutClearingGap(ctx context.Context, sessionID, machineID string, selfSubscribed bool, projects []string) error {
	if machineID == "" {
		return fmt.Errorf("write session %s to the bucket: %w: the entry names no machine", sessionID, ErrEntryUnbound)
	}
	kv, epoch := r.watcher.Handle()
	for range entryWriteLosses {
		stored, revision, found, err := r.readForClear(ctx, kv, sessionID)
		if err != nil {
			return fmt.Errorf("read session %s from the bucket's stream leader before writing it: %w", sessionID, err)
		}
		entry := SessionEntry{SessionID: sessionID, MachineID: machineID, SelfSubscribed: selfSubscribed, Projects: projects}
		if found && stored.SessionID == sessionID {
			entry = stored
			entry.MachineID, entry.SelfSubscribed, entry.Projects, entry.Port = machineID, selfSubscribed, projects, 0
		}
		entry.UpdatedAt = r.now().UnixMilli()
		lost, err := r.writeEntryAt(ctx, kv, epoch, sessionID, entry, revision)
		if err != nil {
			return fmt.Errorf("write session %s to the bucket: %w", sessionID, err)
		}
		if !lost {
			return nil
		}
	}
	return fmt.Errorf("write session %s to the bucket: %w", sessionID, errEntryWritesLost)
}

// entryWriteLosses bounds the writes one write of an entry may lose (writeEntryAt's stop rule):
// far more than the writers of one session's entry at once - its processes registering and
// heartbeating, a relay marking the entry, an outage renewal - each of which lands in turn, so a
// write that reaches it is one a run of other writers kept from landing, and it ends rather than
// holding a registration, or a relay and the session's frames behind it, at the bucket's round
// trip for as long as the caller's context lasts. A copy of the bucket that answers a write behind
// spends one of them at most: a write reads the leader after it lost one (readEntryFromLeader).
const entryWriteLosses = 32

// errEntryWritesLost is the error of a write of an entry that lost entryWriteLosses writes.
var errEntryWritesLost = fmt.Errorf("%d conditional writes lost to other writers", entryWriteLosses)

// errUndecodableEntry is readEntry's answer for a stored value no read decodes as an entry.
var errUndecodableEntry = errors.New("the stored value is no session entry")

// readEntry reads sessionID's entry from the bucket by a direct get through kv, waiting no longer
// than ctx (bus.Bounded), with the revision it was read at: revision 0 and no error for a key the
// bucket holds no entry under, and the revision with errUndecodableEntry for a value DecodeEntry
// refuses for the key. A direct get can answer a record the server stored as its copy of another
// key, which names that other key: DecodeEntry refuses a session_id that is null, empty or another
// key's, so PutWithin carries no mark from such a copy and writes over it. A record with no
// session_id field is read as before (DecodeEntry accepts it, with an empty SessionID). A write
// fenced on a direct get's revision over a server-stored record is
// refused for good, since the record carries another stream's first Nats-Sequence header; PutWithin
// reads the stream leader on the retry (readEntryFromLeader), which reports the true sequence.
func readEntry(ctx context.Context, kv bus.KeyValue, sessionID string) (SessionEntry, uint64, error) {
	got, _, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if errors.Is(err, nats.ErrKeyNotFound) {
		return SessionEntry{}, 0, nil
	}
	if err != nil {
		return SessionEntry{}, 0, err
	}
	entry, decodeErr := DecodeEntry(sessionID, got.Value())
	if decodeErr != nil {
		return SessionEntry{}, got.Revision(), fmt.Errorf("%w: %w", errUndecodableEntry, decodeErr)
	}
	return entry, got.Revision(), nil
}

// writeEntryAt is the one write of sessionID's entry every write of it sends (PutWithin,
// PutClearingGap, rewriteStoredEntry): entry, created where revision is 0 and updated at revision otherwise,
// waiting no longer than ctx (bus.Bounded), and put in the copy through epoch once it lands
// (kvwatch.Copy.Put), so a write made through a handle on a bucket since recreated is dropped
// rather than fencing out the new bucket's rows. lost reports a write another writer landed first,
// as bus.IsCASConflict reads it (it reads a replicated bucket's answer too): the entry is left as
// that writer wrote it, and the error is nil.
//
// The stop rule of every caller: a conditional write loses to a write that landed, which is
// another writer's progress, or to a read that a copy of a replicated bucket answered a write
// behind (a direct get is answered by any copy near enough to current, Refresh), which only the
// first read can be, since every read after a lost write is the leader's (readEntryFromLeader). The caller
// reads the entry again and writes again until its write lands, ctx ends, or it has lost
// entryWriteLosses writes, when it ends with errEntryWritesLost. Several processes of one session
// claiming and heartbeating at once, and a relay marking the session's entry beside them, each
// land in turn well inside that bound.
func (r *SessionRegistry) writeEntryAt(ctx context.Context, kv bus.KeyValue, epoch uint64, sessionID string, entry SessionEntry, revision uint64) (lost bool, err error) {
	buf, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	written, _, err := bus.Bounded(ctx, func() (uint64, error) {
		if revision == 0 {
			return kv.Create(sessionID, buf)
		}
		return kv.Update(sessionID, buf, revision)
	})
	if err != nil {
		if bus.IsCASConflict(err) {
			return true, nil
		}
		return false, err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, &cachedSession{entry: entry, expiresAt: r.expiryFor(time.Time{}, entry.UpdatedAt)}, written)
	r.unlockAndNotify()
	return false, nil
}

// readEntryFromLeader reads sessionID's newest message from the bucket's stream leader through kv
// and classifies it as sessionID's registration, with the revision a write-back is fenced on (the
// classification StoredByWriter answers by, which calls this function, with the revision added): the
// decoded entry and the revision the
// message is stored at, found true, when the leader's newest message is a writer's put DecodeEntry
// binds to sessionID; and the zero entry, found false, for a key the stream holds no message for,
// a delete or purge marker, a put the server wrote (bus.LeaderServerWritten), and a put DecodeEntry
// refuses for sessionID - a record the server stored at the key's subject, a direct get's copy or
// a RePublish among them, which no write of the entry may take for a writer's. A read the leader
// refused or did not answer is the error, since no write may proceed over a key it could not read
// from the leader.
func readEntryFromLeader(ctx context.Context, kv bus.KeyValue, sessionID string) (entry SessionEntry, revision uint64, found bool, err error) {
	leader, err := kv.ReadFromLeader(ctx, sessionID)
	if err != nil {
		return SessionEntry{}, 0, false, err
	}
	if leader.Kind != bus.LeaderPut {
		return SessionEntry{}, leader.Revision, false, nil
	}
	entry, decodeErr := DecodeEntry(sessionID, leader.Value)
	if decodeErr != nil {
		return SessionEntry{}, leader.Revision, false, nil
	}
	return entry, leader.Revision, true, nil
}

// readForClear reads sessionID's leader record for a clearing write (PutClearingGap), honoring the
// StoredByWriter seam so a cmd/listener handler test's forged-record cases reach this write as they
// reach the claim's own confirmation (SetStoredByWriterSeamForTest): a seamed session answers the
// seam's (entry, is-a-writer's, error) at the revision the stream leader holds now, and every other
// session answers the leader read (readEntryFromLeader). The real leader read is made either way, so
// the write still fences on the bucket and a lost compare-and-swap still reads a fresh revision.
func (r *SessionRegistry) readForClear(ctx context.Context, kv bus.KeyValue, sessionID string) (SessionEntry, uint64, bool, error) {
	stored, revision, found, err := readEntryFromLeader(ctx, kv, sessionID)
	if res, ok := r.leaderSeam.lookup(sessionID); ok {
		return res.entry, revision, res.written, res.err
	}
	return stored, revision, found, err
}

// rewriteStoredEntry is the change of sessionID's entry as the bucket holds it that
// SetRepositoryGap, ClearRepositoryGap and RenewWithin share: the entry is read from the stream
// leader (readEntryFromLeader), handed to change, and written back at the revision read when change
// reports it changed (writeEntryAt), an update and never a create, so a registration or another
// write that lands between the read and the write is never overwritten: the entry is read again and
// change asked again, by writeEntryAt's stop rule. The leader read, never a direct get, is what a
// bucket read by a stream message get (an edge listener's) needs so a record the server wrote is
// never written back as a writer's: a direct get reports such a record at the key's true sequence
// there, so a write fenced on it would land. A key the stream holds no writer's record for - absent,
// a marker, a record the server wrote, or one that does not bind to the key - is missing, the
// caller's error or nil, so no such record is rewritten. A read that did not answer is the error;
// what names the change in each.
func (r *SessionRegistry) rewriteStoredEntry(ctx context.Context, sessionID, what string, missing error, change func(*SessionEntry) bool) error {
	kv, epoch := r.watcher.Handle()
	for range entryWriteLosses {
		entry, revision, found, err := readEntryFromLeader(ctx, kv, sessionID)
		if err != nil {
			return fmt.Errorf("read session %s to %s: %w", sessionID, what, err)
		}
		if !found {
			return missing
		}
		if !change(&entry) {
			return nil
		}
		lost, err := r.writeEntryAt(ctx, kv, epoch, sessionID, entry, revision)
		if err != nil {
			return fmt.Errorf("write session %s to %s: %w", sessionID, what, err)
		}
		if !lost {
			return nil
		}
	}
	return fmt.Errorf("write session %s to %s: %w", sessionID, what, errEntryWritesLost)
}

// SetRepositoryGap writes mark as sessionID's RepositoryGap and nothing else, over any mark the
// entry holds (rewriteStoredEntry). UpdatedAt is kept as read: the mark says nothing of whether
// the session answers. An entry that already carries mark is left as it is. An entry the bucket
// does not hold is the error, matching nats.ErrKeyNotFound: no mark is then stored where a move
// or a restart finds it, so its caller does not take the mark as kept (cmd/listener's
// noteRepositoryGap gives its event back).
func (r *SessionRegistry) SetRepositoryGap(ctx context.Context, sessionID, mark string) error {
	missing := fmt.Errorf("mark session %s's repository gap: the bucket holds no entry of it: %w", sessionID, nats.ErrKeyNotFound)
	return r.rewriteStoredEntry(ctx, sessionID, "mark its repository gap", missing, func(entry *SessionEntry) bool {
		if entry.RepositoryGap == mark {
			return false
		}
		entry.RepositoryGap = mark
		return true
	})
}

// ClearRepositoryGap clears sessionID's RepositoryGap while it is mark, the mark a frame carried,
// as SetRepositoryGap writes it. Any other mark is left as it is: a relay of the session set it
// since, for an event withheld later (each withheld event's mark is a new id), and a later frame
// clears it. An entry the bucket does not hold carries no mark to clear, and is no error.
func (r *SessionRegistry) ClearRepositoryGap(ctx context.Context, sessionID, mark string) error {
	return r.rewriteStoredEntry(ctx, sessionID, "clear its repository gap", nil, func(entry *SessionEntry) bool {
		if entry.RepositoryGap != mark {
			return false
		}
		entry.RepositoryGap = ""
		return true
	})
}

// RenewWithin writes sessionID's entry back as the bucket holds it, stamped now, while owns says
// the stored entry is the caller's to renew (rewriteStoredEntry): a registration another listener
// wrote between the read and the write is never overwritten with what was read, and owns is asked
// again of the entry read again. Every other field is kept as read, the gap mark among them. An
// entry the bucket does not hold, and one owns refuses, are left as they are, and neither is an
// error. cmd/listener's edge listener renews a session so through a repository outage that
// refuses its heartbeats.
func (r *SessionRegistry) RenewWithin(ctx context.Context, sessionID string, owns func(SessionEntry) bool) error {
	return r.rewriteStoredEntry(ctx, sessionID, "renew it", nil, func(entry *SessionEntry) bool {
		if !owns(*entry) {
			return false
		}
		entry.UpdatedAt = r.now().UnixMilli()
		return true
	})
}

// Get returns the entry sessionID's cached record carries, or nats.ErrKeyNotFound when the cache
// holds none or holds one past its deadline, even while the copy retains that row for List
// because it does not follow its bucket (expireDue). Its one measure is the bucket's maximum age
// — the age at which the bucket itself drops a record, which the registry expires its rows by
// (SetTTL) — and never the live window or the gone horizon: it answers the record of a session in
// any state while the bucket would still hold it, and nothing once the bucket would not. So it
// says nothing about whether a session is answering, and no caller may read its not-found as a
// session's death: with the deployed maximum age past the gone horizon, the shape this registry's
// windows are written for (WithSessionMaxAge), a record it does not find is one no state could be
// read from either.
//
// It is the read for a caller that enriches what it already holds (a title, a last registration)
// and asks nothing of the session's state. A caller that must tell an unresponsive session from
// a gone one, or that acts on a session being missing, asks Lookup or Refresh, which judge the
// record itself (StateOf), since a record this answers can be two hours old. It takes only the
// shared lock and changes nothing: the expiry timer, not a read, removes an expired entry.
func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	now := r.now()
	r.mu.RLock()
	cs, ok := r.cache.Get(sessionID)
	r.mu.RUnlock()
	if !ok || cs.expired(now) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// Lookup answers sessionID's record and the state that record puts its session in (StateOf):
// live within the registry's live window of the last registration, unresponsive past it and
// within the gone horizon, and gone past that or with no record here at all. An unresponsive
// session is not a dead one — its claims stand, its messages are stored and no replacement is
// started for it — so every caller that would take something away from a session, or tell a
// sender its destination has ended, asks here rather than reading a not-found from Get as death.
//
// The state is the record's age alone, never its membership of this cache: a row the copy kept
// through a re-read is judged again, and a row past the bucket's backstop deadline, which Get
// reports absent, still reads unresponsive here while its registration is within the horizon. The
// entry is the record the cache holds, so a caller can report the last registration (UpdatedAt) of
// a gone session whose row is still here; a session with no row at all answers the zero entry, and
// LastSeen is what still knows when it was last seen. It takes only the shared lock and changes
// nothing.
func (r *SessionRegistry) Lookup(sessionID string) (SessionEntry, State) {
	now := r.now()
	r.mu.RLock()
	cs, ok := r.cache.Get(sessionID)
	r.mu.RUnlock()
	if !ok {
		return SessionEntry{}, Gone
	}
	return cs.entry, r.stateOf(cs.entry, now)
}

// Refresh reads sessionID's entry from the session bucket itself, with the direct get nats.go
// answers a KV read with, applies it to the cache as a delivered record is applied, and then
// answers as Get does. The cache follows the bucket through a watcher and so trails it: a
// session another listener registered a moment ago, or one this listener has not re-read since
// its connection to the bus came back, can be in the bucket and not yet here. A caller that
// acts on a session being missing asks here — one about to take something away from a session
// because it looks gone, and one about to tell a sender that its destination is gone, which
// makes that sender route around the pane, report it, or stop. Get is the read for a caller that
// only enriches what it already holds, or that asks again anyway, since a cache miss cannot tell
// a session that ended from one this cache has not caught up with. A key no read may name is
// nats.ErrKeyNotFound, since no session can register under it: one bus.KeyValue refuses
// (bus.ErrRefused), `ses:bad` among them, which is outside nats.go's key alphabet and which an
// earlier build or a direct bucket write could leave as a role holder. Any other error is a read
// that did not answer, ctx's end among them: nats.go's KV read takes no context and waits out
// the JetStream MaxWait (10 s), so Refresh stops waiting when ctx ends and lets that read
// finish on its own (bus.Bounded).
//
// A direct get of a clustered bucket is answered by any copy near enough to current, so this
// read can report a record the bucket holds as missing while the copy answering catches up. A
// caller that cannot afford that miss asks RefreshFromLeader.
//
// A read the cache does not take is answered as read, uncached, as store.Registry.Get answers its
// own: one made before a watch moved the epoch (a probe or a Rewatch installed a recreated stream),
// or one the revision fence drops because the cache holds a higher revision of the key, which may
// belong to a bucket incarnation the watcher has not yet found replaced. The bucket answered for
// the session a moment ago, and answering from a cache that did not take that answer would tell
// the caller a live session is gone. It answers as Get would for the same row: a delete or purge
// marker, a value that does not decode and a row past its TTL are not found.
func (r *SessionRegistry) Refresh(ctx context.Context, sessionID string) (SessionEntry, error) {
	kv, epoch := r.watcher.Handle()
	got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return SessionEntry{}, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
	}
	return r.applyRead(sessionID, epoch, got, err)
}

// RefreshFromLeader is Refresh with the leader of the session bucket's stream answering the read
// (bus.KeyValue.GetFromLeader) instead of whichever copy of the bucket a direct get reaches: it
// applies what it read to the cache as Refresh does, answers as Get would for that row, and maps
// every error the same way, ctx's end included.
//
// It is the read for the callers that cannot afford a stale answer, all of them in cmd/listener.
// Most act on a miss, taking something away from the session, or passing it over, on the strength
// of the session having no record at all:
//
//   - the listener's delete of a session's JetStream consumer, which re-reads this bucket under
//     the session's lock and deletes only where the session is gone from it;
//   - the interest and role claim reapers' test of a session the cache does not hold
//     (sessionMayBeLive);
//   - a soft role claim's reads of the holder it would take the role from, its liveness and the
//     registration of a declared predecessor (roleSupersessionVerdict);
//   - the Legion hand-off's read of the stopped agent it would hand a message to its daemon for
//     (legionHandoff.handOffToLegion);
//   - the three resolutions of a role's holder that read through leaderHolderSessions: the
//     publish route's confirmation of a miss before it hands the message off
//     (roleSend.missConfirmed), the marked send's resolution before it hands the send off or
//     keeps it (handOffOrKeepRoleSend), and the kept lane handover's (handOverKeptRole).
//
// A copy that has not applied a relaunch's registration, or the registration of a session that
// took a role a moment ago, would answer that read "not found": the delete would then remove the
// consumer of a live relaunch, whose relay stops until it registers again, the reaper a live
// session's interest or role, the soft claim a live holder's role, the hand-off would give a live
// session's message to its daemon, and a resolution would read a live holder as lapsed, handing
// its message to the daemon of the role's stopped agent or leaving its kept copy on the lane.
//
// The other two read a registration for its client instance id, which an entry a copy has not
// caught up with can lack or carry stale (bucketSessionEntry): the claimant's own registration in
// POST /v1/roles/set (roleSetHandler), whose id a claim weighed against a predecessor falls back
// to and which the claim writes back, and the registration view of GET /v1/registry/<session_id>
// (writeSessionRegistration), whose caller names that id in the delete that follows.
//
// Every other caller asks Refresh, which costs the server less: a direct get is answered by any
// copy that is near enough to current, where this read is one more thing for the leader to do.
func (r *SessionRegistry) RefreshFromLeader(ctx context.Context, sessionID string) (SessionEntry, error) {
	kv, epoch := r.watcher.Handle()
	got, err := kv.GetFromLeader(ctx, sessionID)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return SessionEntry{}, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
	}
	return r.applyRead(sessionID, epoch, got, err)
}

// StoredByWriter reads sessionID's newest message on the session bucket's stream from the stream's
// leader with the headers the stream stored with it (bus.KeyValue.NewestFromLeader) and returns the
// record a writer stored there: the message decoded as sessionID's registration (DecodeEntry) when
// it is a put carrying none of the headers the server writes (bus.ReadFromLeader, LeaderPut), with true. A
// message the server wrote - a direct get's copy of another key's record, or a RePublish of another
// stream naming the key's subject, whatever body that stream was given - can read as a record of
// sessionID (DecodeEntry) naming any projects, and neither the watcher's entries nor a direct get
// carry the headers that tell it apart, so it answers the zero entry and false. A key the stream
// holds no message for, one whose newest message is a delete or purge marker, and one whose newest
// message is a put the server wrote or that does not decode as sessionID's registration are each no
// writer's record: the zero entry and false, no error. The entry it returns is the one the leader
// holds, so a caller judges its scope on the record the key is bound to now, not on a cached entry
// an older message left. It is the edge listener's confirmation of a session it is about to show,
// send to or name as a role's holder across what a cached entry says; every other reader has no use
// for its cost, a read the leader alone answers.
func (r *SessionRegistry) StoredByWriter(ctx context.Context, sessionID string) (SessionEntry, bool, error) {
	if res, ok := r.leaderSeam.lookup(sessionID); ok {
		return res.entry, res.written, res.err
	}
	kv, _ := r.watcher.Handle()
	entry, _, found, err := readEntryFromLeader(ctx, kv, sessionID)
	switch {
	case errors.Is(err, bus.ErrRefused):
		return SessionEntry{}, false, nil
	case err != nil:
		return SessionEntry{}, false, fmt.Errorf("read session %s from the bucket's stream leader: %w", sessionID, err)
	}
	return entry, found, nil
}

// applyRead is what Refresh and RefreshFromLeader both do with the read they made: got and err
// are that read's answer, and epoch the watcher epoch it was taken in. A key the bucket refuses
// is not found, since no session can register under it; any other error is the read's own. The
// entry is applied to the cache as a delivered record is, and the answer is Get's for the row
// that leaves, or uncachedLocked's where the cache did not take it.
func (r *SessionRegistry) applyRead(sessionID string, epoch uint64, got nats.KeyValueEntry, err error) (SessionEntry, error) {
	if errors.Is(err, bus.ErrRefused) {
		return SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, err)
	}
	if err != nil {
		return SessionEntry{}, err
	}
	entry := kvwatch.LegacyEntry(got)
	now := r.now()
	r.mu.Lock()
	outcome, malformed := r.cache.Apply(r.watcher, epoch, entry)
	if outcome != kvwatch.Applied {
		answer, err := r.uncachedLocked(entry, now)
		r.unlockAndNotify()
		return answer, err
	}
	if malformed != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", malformed.Error()))
	}
	r.unlockAndNotify()
	return r.Get(sessionID)
}

// uncachedLocked answers entry, a read of the bucket the cache did not take (applyRead, for
// Refresh and RefreshFromLeader alike), as Get would answer the row applying it leaves: a delete
// or purge marker, and a value that does not decode, which applying evicts, are not found, and
// so is a row past its TTL, measured as the cache measures one (decodeLocked). Callers hold
// r.mu.
func (r *SessionRegistry) uncachedLocked(entry kvwatch.Entry, now time.Time) (SessionEntry, error) {
	if entry.Op != kvwatch.OpPut {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	cs, _, err := r.decodeLocked(entry)
	if err != nil || cs.expired(now) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for the registry's retention (one TTL unless
// WithLastSeenRetention set longer) after the session left the cache, so callers
// can report why an otherwise durable reference stopped resolving. A live
// session answers 0. It takes only the shared lock and changes nothing: an
// expired entry the expiry timer has not removed yet, which is also a row the copy retains while
// it does not follow its bucket, answers what its removal will record (its UpdatedAt until the
// retention past the moment it left the live view, departedLocked), so the answer is the same
// before and after the removal.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if cs, ok := r.cache.Get(sessionID); ok {
		if !cs.expired(now) {
			return 0
		}
		// A row past its deadline that the copy retains because it does not follow its bucket
		// answers its last heartbeat for as long as it is retained: no removal has recorded one,
		// and the read that restores following is what decides whether the session is gone.
		if !r.following() {
			return cs.entry.UpdatedAt
		}
		if seen, ok := r.lastSeenRecord(cs.entry.UpdatedAt, r.departedLocked(cs, now)); ok && !seen.expired(now) {
			return seen.lastSeen
		}
	}
	seen, ok := r.lastSeen[sessionID]
	if !ok || seen.expired(now) {
		return 0
	}
	return seen.lastSeen
}

// ListEntry is a single entry from List(), keyed by session ID, with the state its record puts
// its session in. The row's last registration is its SessionEntry.UpdatedAt, which the listener's
// routes answer as last_seen.
type ListEntry struct {
	SessionID string `json:"session_id"`
	SessionEntry
	State State `json:"state"`
}

// ListOption selects which states List answers for. It is an option rather than an argument so
// that List() stays the live-only listing every caller already writes, and only the caller that
// wants more names what it wants.
type ListOption func(*listOpts)

type listOpts struct{ unresponsive bool }

// IncludeUnresponsive adds the unresponsive rows to the live ones List answers with, for a caller
// that must show, or route to, a session that has stopped answering but is not gone.
func IncludeUnresponsive() ListOption {
	return func(o *listOpts) { o.unresponsive = true }
}

// List returns the sessions the in-memory cache holds that are live, each with its state and its
// last registration, and with IncludeUnresponsive the unresponsive ones as well. A row is judged
// by its own last registration (StateOf), never by its presence here: a row the copy kept through
// a re-read is judged again, so a listener refilling its cache no longer leaves a working session
// out, and a stale copy no longer holds a session live. A gone row is never listed; the expiry
// timer and the session reaper are what remove it.
//
// It takes only the shared lock and changes nothing. Cache-only: no JetStream round-trips, so it
// can't hang on a slow KV leader the way the old Keys()+per-key Get() loop did. It never refuses:
// whether the copy can be served at all is Servable's to say.
func (r *SessionRegistry) List(include ...ListOption) ([]ListEntry, error) {
	var opts listOpts
	for _, o := range include {
		o(&opts)
	}
	now := r.now()
	r.mu.RLock()
	entries := make([]ListEntry, 0, r.cache.Len())
	for key, cs := range r.cache.All() {
		state := r.stateOf(cs.entry, now)
		if state == Gone || (state == Unresponsive && !opts.unresponsive) {
			continue
		}
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry, State: state})
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// storedLocked and removedLocked are what the cache records beside its copy when the copy stores
// or removes a row (kvwatch.Rows), and where each row's expiry is scheduled. A stored row's
// session is live, so its last-seen record goes and its own deadline is scheduled. A removed row's
// last heartbeat is kept for LastSeen, for the retention from the moment it left the live view
// (departedLocked), which is what its expiry would have recorded, so LastSeen answers the same
// before and after the removal. Neither walks the whole lastSeen map: a read applies every record
// of the bucket through them, and each record's own deadline is in the expiry heap, which removes
// the record for good (expireDue). Each records its session as changed, for OnChange's functions
// once the caller has released r.mu (unlockAndNotify). Callers hold r.mu for writing.
func (r *SessionRegistry) storedLocked(sessionID string, stored *cachedSession) {
	delete(r.lastSeen, sessionID)
	r.scheduleLocked(sessionID, expireLive, stored.expiresAt)
	r.changedLocked(sessionID)
}

func (r *SessionRegistry) removedLocked(sessionID string, removed *cachedSession) {
	r.scheduleLocked(sessionID, expireLastSeen, r.retireLocked(sessionID, removed, r.now()))
	r.changedLocked(sessionID)
}

// retireLocked keeps row's final heartbeat for the retention after the moment that row left the
// live view (departedLocked), and returns the deadline that record expires at, which its caller
// arms the way its own path arms deadlines: a removal schedules it (scheduleLocked), while the
// expiry sweep sets it and re-arms once at the end (setExpiryLocked). Callers hold r.mu for
// writing.
func (r *SessionRegistry) retireLocked(sessionID string, row *cachedSession, now time.Time) time.Time {
	left := r.departedLocked(row, now)
	r.retainLastSeenLocked(sessionID, row.entry.UpdatedAt, left)
	return left.Add(r.retention())
}

// departedLocked is the moment row left, or leaves, the live view, from which its last heartbeat
// is kept for the retention: its own deadline when it is past it, and now when it is not, for a
// row the bucket no longer holds. A deadline that passed while the copy did not follow its bucket
// is no such moment: the copy could not see the heartbeats that renewed the row either, so it
// expired nothing (expireDue), and the row leaves the live view no earlier than the read that
// restored following (followingSince). Callers hold r.mu.
func (r *SessionRegistry) departedLocked(row *cachedSession, now time.Time) time.Time {
	if !row.expired(now) {
		return now
	}
	if row.expiresAt.Before(r.followingSince) {
		return r.followingSince
	}
	return row.expiresAt
}

// retainLastSeenLocked records lastSeen as sessionID's final heartbeat until the retention after
// left, the time the session left the cache. Callers must hold r.mu for writing.
func (r *SessionRegistry) retainLastSeenLocked(sessionID string, lastSeen int64, left time.Time) {
	seen, ok := r.lastSeenRecord(lastSeen, left)
	if !ok {
		return
	}
	if r.lastSeen == nil {
		r.lastSeen = map[string]lastSeenSession{}
	}
	r.lastSeen[sessionID] = seen
}

// lastSeenRecord is the record a session whose final heartbeat was lastSeen keeps for the
// retention after it left the cache at left, or false when none is kept (no TTL, or no heartbeat
// time). LastSeen answers an expired entry still in the cache from the same record, so it answers
// the same before and after the entry's removal.
func (r *SessionRegistry) lastSeenRecord(lastSeen int64, left time.Time) (lastSeenSession, bool) {
	if r.ttl <= 0 || lastSeen <= 0 {
		return lastSeenSession{}, false
	}
	return lastSeenSession{lastSeen: lastSeen, expiresAt: left.Add(r.retention())}, true
}
