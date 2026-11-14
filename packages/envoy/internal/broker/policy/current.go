package policy

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// Current is the live policy. The first load must succeed; a later load that fails keeps the
// previous set, so a Secrets Manager outage never turns into deny-everything. A secret refused for
// its own tags leaves only that secret out. Refresh re-describes each name it holds an anchor for
// (recent) for as long as the store's metadata may lag a write (Store.ListLag), so a lagging
// listing neither undoes what a single-name read settled nor undoes a change that reload's own
// re-describe caught. Each set it makes live is compared with the live one before it (publish),
// and only a change of a secret's state writes a line: a load that finds what the one before
// found writes nothing, however often it runs.
//
// It also reads each kind secret's stored value, once for each version, kind and shape a load or
// reread lists that this pod has not decoded together, and decodes it with secrets.ParseValue
// under that kind and shape (readValues), so a value no mint could use is refused
// (ReasonValueMalformed) from the load that first lists it, at pod start too, rather than at the
// first mint an agent needed. A secret of no kind is never read, and a load that lists what the
// one before listed reads no value. The value is decoded and dropped: it is never logged, counted
// by value or kept.
type Current struct {
	loader Loader
	set    atomic.Pointer[Set]
	// mu is the writer lock, held by every writer, Refresh and RefreshOne, from its read of the
	// store's metadata to its Store, so neither stores a set built on one the other has since
	// replaced; the value reads a load or reread makes once it has stored its set are made without
	// it, and their outcomes recorded and published under it again (readValues), so no reread
	// waits on a value read. RefuseValue takes it too. It guards recent, base and values, and
	// orders the state-change lines as the sets they describe are ordered. It is a one-slot
	// channel so a writer waiting for it gives up when its context ends (lock).
	mu chan struct{}
	// recent is, for each name a reread or a reload's own re-describe last changed the live answer
	// for, when it did: RefreshOne's reread and Refresh's own re-describe each record one only when
	// the answer differed from what the live set held, so a reread that confirms the live answer
	// leaves any earlier anchor in place and adds none. Each anchor lives until the store's ListLag
	// has passed.
	recent map[string]time.Time
	// base is the policy as the loads and rereads read it from the store's metadata, before any
	// value is refused: every set Current makes live is base with the secrets whose values are
	// refused taken out (withhold), and a reread's answer is compared with base, not with that.
	base *Set
	// values is, for each kind secret base serves, by Name, what this pod knows of its stored
	// value: the version, kind and shape a read now decodes under, the ones it last decoded under,
	// whether that value is refused, and whether its reads are failing (valueState). A secret base
	// stops serving as a kind secret leaves it, with whatever refusal it held.
	values map[string]*valueState
	now    func() time.Time
}

// valueKey is what a kind secret's value is decoded under: the version of the value the listing
// names (secrets.Listing.Version), and the kind and shape tags that say how it is read, which a
// retag changes and leaves the version as it was.
type valueKey struct{ version, kind, shape string }

// valueState is what Current knows of one kind secret's stored value.
type valueState struct {
	// listed is the key the latest load or reread listed: what a read of the value decodes under
	// now.
	listed valueKey
	// decoded is the key the value was last read and decoded under, the zero key before any read
	// succeeded. A secret whose listed key is not its decoded one is read at the next load or
	// reread of it; one whose keys agree is read by none.
	decoded valueKey
	// refused is whether the value is refused, ReasonValueMalformed, at the version refusedAt:
	// by the decoder of a read (readValues) or by a mint's own (RefuseValue). Each read that
	// decodes sets it afresh; a read that fails keeps it when the read was of that version, and
	// ends it when the read was of another version listed since.
	refused   bool
	refusedAt string
	// failing is whether the last read of the value failed: ValueReadFailedMessage was written for
	// it, and ValueReadRecoveredMessage is owed at the next read that succeeds.
	failing bool
}

// valueRead is one value read a load or reread hands readValues: the secret's Name, its store
// source (Secret.Source without the kind KindSource froze into it) and the key its value is
// decoded under.
type valueRead struct {
	name, source string
	key          valueKey
}

// valueOutcome is what one valueRead came to: the read's error, or whether the decoder refused the
// value it read.
type valueOutcome struct {
	valueRead
	err     error
	refused bool
}

// NewCurrent loads the policy once, failing when that load fails, reads and decodes the value of
// each kind secret it serves (readValues), on ctx like the load, so a value the decoder refuses
// is refused before NewCurrent answers and so before the first request, and then reloads it
// every interval until ctx ends, each reload given that interval before it fails, so one stuck
// on a Secrets Manager that does not answer holds the writer lock no longer. The first load
// writes a RefusedMessage line for each secret it refuses, a value refused by its reads included,
// marked first_load. A run of reloads that fail while ctx is live writes one LoadFailedMessage
// line, at its first failure, and the reload that ends it one LoadRecoveredMessage line, as
// enroll.ServingClock.Run does for its beats, so an outage of any length is two lines and not one
// for each interval. A reload that fails once ctx has ended, the broker shutting down, is no
// failed load and writes nothing.
func NewCurrent(ctx context.Context, loader Loader, every time.Duration) (*Current, error) {
	c := &Current{loader: loader, mu: make(chan struct{}, 1), recent: map[string]time.Time{}, values: map[string]*valueState{}, now: time.Now}
	if err := c.Refresh(ctx); err != nil {
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reload, cancel := context.WithTimeout(ctx, every)
				err := c.Refresh(reload)
				cancel()
				switch {
				case ctx.Err() != nil:
				case err != nil && !failing:
					slog.Error(LoadFailedMessage, "error", err)
					failing = true
				case err == nil && failing:
					slog.Info(LoadRecoveredMessage)
					failing = false
				}
			}
		}
	}()
	return c, nil
}

// lock takes the writer lock, or answers ctx's error, holding nothing, once ctx has ended first.
func (c *Current) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock releases the writer lock lock took.
func (c *Current) unlock() { <-c.mu }

// Refresh loads the policy now and, when the load succeeds, makes it the live one (publish, which
// writes the lines its changes call for); a load that fails leaves the live set as it was and
// writes nothing. A name recent anchors within the store's ListLag of the moment this reload
// began - before its listing was fetched - is read again alone and kept as that read finds it,
// since the full listing may not show the change yet. recent anchors a name a reread settled, and
// one an earlier reload's own re-describe caught a change to. Any failed metadata read fails the
// refresh. Then it reads the value of each kind secret the load lists at a version, kind or shape
// this pod has not decoded (readValues), on ctx, and makes live what those decodes leave; a value
// read that fails fails no refresh, nor do value reads ctx ended before they were recorded, which
// the next load reads again.
func (c *Current) Refresh(ctx context.Context) error {
	reads, first, err := c.reload(ctx)
	if err != nil {
		return err
	}
	_ = c.readValues(ctx, reads, first)
	return nil
}

// reload is Refresh up to its value reads, under the writer lock: the load, the rereads of what
// recent anchors, and the publish of the set they leave. It answers the value reads that set calls
// for (list), and whether it was the pod's first load.
func (c *Current) reload(ctx context.Context) (reads []valueRead, first bool, err error) {
	if err := c.lock(ctx); err != nil {
		return nil, false, err
	}
	defer c.unlock()
	// The window is measured from before the listing is fetched: a listing read inside it may lag
	// a reread however long Load takes to return.
	now := c.now()
	lag := c.loader.Store.ListLag()
	set, err := c.loader.Load(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(c.recent) > 0 {
		for name, at := range c.recent {
			if now.Sub(at) > lag {
				delete(c.recent, name)
				continue
			}
			lk, err := c.loader.LoadOne(ctx, name)
			if err != nil {
				return nil, false, err
			}
			id := c.storeID(name)
			merge(set, name, id, lk)
			// A change this read catches that no reread did - a console delete, a tag that now
			// refuses the secret - is as new to the listing as a reread's answer, so it is kept for
			// the store's ListLag from now; the policy before, not this listing, is what it changed.
			if !answers(c.base, name, id, lk) {
				c.recent[name] = c.now()
			}
		}
		set = setOf(set.Secrets, set.Refused, set.versions)
	}
	first = c.base == nil
	c.base = set
	reads = c.list()
	c.publish(c.withhold(), first)
	return reads, first, nil
}

// RefreshOne rereads name alone (Loader.LoadOne) and merges the answer into the live policy: a
// served secret replaces or adds name, a refused one takes it out of what is served and records
// its refusal, and an absent one takes it out of both. Every other name stays as the live policy
// had it. A failed read leaves the live policy as it was and writes nothing, and a name no secret
// can carry is ErrNameInvalid before the writer lock (CheckName). A reread that finds what the
// live policy already held - the same secret served at the same value version, the same refusal,
// or neither - changes nothing and writes nothing; one that changes it is published (publish) and
// writes the lines that change calls for. Refresh keeps a reread for the store's ListLag only when
// it changed the live answer, as Refresh does for its own re-describes: a reread that confirms
// what the live policy already held settled nothing a lagging listing could undo, and anyone may
// ask for a reread, so anchoring it would let each one cost every reload in the next ListLag a
// describe under the writer lock (on Secret Manager, two metadata reads from a quota the whole
// project shares). A served name found absent is a change and is kept, so a lagging listing
// cannot bring back a deleted secret; a secret created and deleted before any read served it can
// still show from a lagging listing until ListLag has passed. A refusal the reread changed is kept
// the same way, so a lagging listing does not undo a reason the reread settled and write the line
// back.
//
// A kind secret whose value this pod has not decoded at the version, kind and shape the reread
// lists is then read and decoded (readValues), on ctx, and the answer is the policy's after that:
// a served secret whose value is refused answers ReasonValueMalformed. When ctx ends before that
// read is recorded - its Store.Read cut short, or its wait for the writer lock - the reread did
// not finish, and RefreshOne answers ctx's error rather than a Lookup the value read never
// checked: the reread route answers it 503 REQUEST_ENDED, and the miss path takes it as the
// request's own failure. What the reread merged stays live, and the value is read at the next
// load or reread.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	if err := c.CheckName(name); err != nil {
		return Lookup{}, err
	}
	lk, reads, err := c.reread(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	if err := c.readValues(ctx, reads, false); err != nil {
		return Lookup{}, err
	}
	if lk.Served {
		if refusal := c.Get().Refused[c.storeID(name)]; refusal.Reason == ReasonValueMalformed {
			return Lookup{Reason: refusal.Reason, Owner: refusal.Owner}, nil
		}
	}
	return lk, nil
}

// reread is RefreshOne up to its value read, under the writer lock: the reread, its merge into
// base and the publish of a change, answering the reread's Lookup and the value read it calls for
// (listOne), if any.
func (c *Current) reread(ctx context.Context, name string) (Lookup, []valueRead, error) {
	if err := c.lock(ctx); err != nil {
		return Lookup{}, nil, err
	}
	defer c.unlock()
	lk, err := c.loader.LoadOne(ctx, name)
	if err != nil {
		return Lookup{}, nil, err
	}
	id := c.storeID(name)
	changed := !answers(c.base, name, id, lk)
	if changed {
		merged := &Set{Secrets: maps.Clone(c.base.Secrets), Refused: maps.Clone(c.base.Refused), versions: maps.Clone(c.base.versions)}
		merge(merged, name, id, lk)
		c.base = setOf(merged.Secrets, merged.Refused, merged.versions)
		c.recent[name] = c.now()
	}
	read, unread := c.listOne(name)
	if changed {
		c.publish(c.withhold(), false)
	}
	if !unread {
		return lk, nil, nil
	}
	return lk, []valueRead{read}, nil
}

// CheckName answers ErrNameInvalid for a name no secret under the prefix can carry (free text, or
// one whose Secrets Manager name would pass its length limit) and nil for any other, by the rule
// LoadOne applies, so a caller refuses such a name before it waits on mu or spends a reread.
func (c *Current) CheckName(name string) error {
	_, _, err := c.loader.secretName(name)
	return err
}

// storeID is the whole name in the store of the secret a session asks for as name, a name
// CheckName passed: the key its refusal has in Set.Refused.
func (c *Current) storeID(name string) string { return c.loader.Prefix + slugOf(name) }

// merge sets name, whose whole name in the store is id, in set's maps as lk found it: the served
// secret or no entry in Secrets, its value's version or no entry in versions, and its refusal or
// no entry in Refused. set's Version is not recomputed.
func merge(set *Set, name, id string, lk Lookup) {
	if lk.Served {
		set.Secrets[name] = lk.Secret
	} else {
		delete(set.Secrets, name)
	}
	if lk.version != "" {
		set.versions[name] = lk.version
	} else {
		delete(set.versions, name)
	}
	if refusal := lk.refusal(); refusal.Reason != "" {
		set.Refused[id] = refusal
	} else {
		delete(set.Refused, id)
	}
}

// answers reports whether set already answers name, whose whole name in the store is id, as lk
// does: the same secret served at the same value version, or the same refusal, or neither.
func answers(set *Set, name, id string, lk Lookup) bool {
	secret, served := set.Secrets[name]
	return served == lk.Served && secret == lk.Secret && set.versions[name] == lk.version && set.Refused[id] == lk.refusal()
}

// list records, for each kind secret base serves, the key a read of its value decodes under now
// (listOne), forgets every name base does not serve, with any refusal of its value, and answers
// a read for each secret whose value this pod has not decoded under that key, in name order. The
// caller holds mu.
func (c *Current) list() []valueRead {
	for name := range c.values {
		if _, served := c.base.Secrets[name]; !served {
			delete(c.values, name)
		}
	}
	var reads []valueRead
	for name := range c.base.Secrets {
		if read, unread := c.listOne(name); unread {
			reads = append(reads, read)
		}
	}
	slices.SortFunc(reads, func(a, b valueRead) int { return strings.Compare(a.name, b.name) })
	return reads
}

// listOne is list for name alone: it records the version, kind and shape base lists for it as
// the key a read of its value decodes under, or forgets it when base serves it as no kind secret
// or not at all, and answers the read of its value, with true, when this pod has not decoded the
// value under that key. The caller holds mu.
func (c *Current) listOne(name string) (valueRead, bool) {
	secret, served := c.base.Secrets[name]
	kind, source, minted, err := ParseKindSource(secret.Source)
	if !served || !minted || err != nil {
		delete(c.values, name)
		return valueRead{}, false
	}
	key := valueKey{version: c.base.versions[name], kind: kind, shape: secret.Shape}
	state, known := c.values[name]
	if !known {
		state = &valueState{}
		c.values[name] = state
	}
	state.listed = key
	if state.decoded == key {
		return valueRead{}, false
	}
	return valueRead{name: name, source: source, key: key}, true
}

// readValues reads each value reads names through the loader's store (Store.Read, counted as
// read=value) and decodes it under its kind and shape (DecodeValue), dropping the value at once;
// the reads are made without the writer lock. It then takes the writer lock for as long as ctx
// lives (lock), as the load or reread before it did and as RefuseValue does, so a reread whose
// caller has gone does not wait out a reload stuck on a store that does not answer. When ctx ends
// before the reads are recorded - a read that fails once ctx has ended, or ctx ending while it
// waits for the lock - it stops at once and answers ctx's error, recording nothing: each secret
// keeps the state it had, and each version it read, still not decoded, is read again at the next
// load or reread. Otherwise it answers nil once, holding the lock, it has recorded each outcome
// whose key is still the one listed (a load or reread since that lists another reads that
// itself):
//
//   - the decoder refuses the value, or the store answers a value that is not text
//     (secrets.ErrValueNotText), which no mint could use either: the secret's value is refused at
//     that version, and the set made live leaves the secret out under ReasonValueMalformed
//     (withhold), which publish writes as one RefusedMessage line, marked first_load when first;
//   - the value decodes: any refusal of the value ends, which publish writes as one
//     ServableMessage line;
//   - the read fails (a permission, a timeout, the store's own failure): nothing is decoded, and
//     the secret keeps the state it had, but for a refusal of another version than the one read,
//     which ends, since a new version was listed. The first failure of a run writes
//     ValueReadFailedMessage, and the read that succeeds after it ValueReadRecoveredMessage. A
//     failure recorded after a read of the same key succeeded - a reload and a reread that read
//     it at once - is stale and records nothing: that key is decoded, so no later read would come
//     to end the failure it would start.
func (c *Current) readValues(ctx context.Context, reads []valueRead, first bool) error {
	if len(reads) == 0 {
		return nil
	}
	outcomes := make([]valueOutcome, 0, len(reads))
	for _, read := range reads {
		c.loader.StoreReads.Inc(readValue)
		value, err := c.loader.Store.Read(ctx, read.source)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		outcome := valueOutcome{valueRead: read}
		switch {
		case errors.Is(err, secrets.ErrValueNotText):
			outcome.refused = true
		case err != nil:
			outcome.err = err
		default:
			outcome.refused = DecodeValue(read.key.kind, read.key.shape, value) != nil
		}
		outcomes = append(outcomes, outcome)
	}
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	changed := false
	for _, o := range outcomes {
		state, known := c.values[o.name]
		if !known || state.listed != o.key || (o.err != nil && state.decoded == o.key) {
			continue
		}
		id := c.storeID(o.name)
		if o.err != nil {
			if !state.failing {
				slog.Warn(ValueReadFailedMessage, "name", id, "version", o.key.version, "error", o.err)
				state.failing = true
			}
			if state.refused && state.refusedAt != o.key.version {
				state.refused, changed = false, true
			}
			continue
		}
		if state.failing {
			slog.Info(ValueReadRecoveredMessage, "name", id, "version", o.key.version)
			state.failing = false
		}
		state.decoded = o.key
		if state.refused != o.refused {
			changed = true
		}
		state.refused, state.refusedAt = o.refused, o.key.version
	}
	if changed {
		c.publish(c.withhold(), first)
	}
	return nil
}

// RefuseValue refuses the value of the kind secret name at version, read as kind under shape, as
// a load's decode refuses one (ReasonValueMalformed): the mint path calls it when
// secrets.ParseValue refused the value a release read, with the version the live policy listed
// for name when that release read it (Set.ValueVersion) and the kind and shape that release
// decoded it under, so a pod whose own load could not read the value refuses the secret from its
// first mint. A key that is no longer the one listed - a load or reread has listed a new value
// since, or a retag a new kind or shape for the same value - is not refused, since that key is
// decoded afresh; nor is a name Current holds no kind secret for. A secret it newly refuses is
// published, writing one RefusedMessage line. It waits on the writer lock for as long as ctx
// lives, the release's own request: one whose caller has gone marks nothing, and the next mint of
// the secret, or the next load that reads a new version, marks it then.
func (c *Current) RefuseValue(ctx context.Context, name, version, kind, shape string) {
	if c.lock(ctx) != nil {
		return
	}
	defer c.unlock()
	state, known := c.values[name]
	if !known || state.listed != (valueKey{version: version, kind: kind, shape: shape}) {
		return
	}
	wasRefused := state.refused
	state.refused, state.refusedAt = true, version
	if !wasRefused {
		c.publish(c.withhold(), false)
	}
}

// withhold is base as Current serves it: each kind secret whose value is refused
// (valueState.refused) taken out of Secrets and put in Refused under ReasonValueMalformed, with
// its owner tag. With none refused it is base itself. The caller holds mu.
func (c *Current) withhold() *Set {
	var served map[string]Secret
	var refused map[string]Refusal
	for name, state := range c.values {
		secret, held := c.base.Secrets[name]
		if !state.refused || !held {
			continue
		}
		if served == nil {
			served = maps.Clone(c.base.Secrets)
			refused = make(map[string]Refusal, len(c.base.Refused)+1)
			maps.Copy(refused, c.base.Refused)
		}
		delete(served, name)
		refused[c.storeID(name)] = Refusal{Reason: ReasonValueMalformed, Owner: secret.Owner}
	}
	if served == nil {
		return c.base
	}
	return setOf(served, refused, c.base.versions)
}

// publish makes next the live policy, first writing a line for each secret whose state next
// changes from the live one's, in the order of the secrets' whole names in the store, so whoever
// reads next has had its lines written already; the writer lock is held, so the lines of two sets
// come in the order the sets do. first is the pod's first load, every line of which says so
// (first_load). A secret next refuses and live did not writes RefusedMessage, and one next
// refuses for another reason than live did writes it with was. One live refused and next does not
// writes ServableMessage, with now=servable when next serves it and now=absent when next neither
// serves nor refuses it, which is a secret gone from the store. A secret refused for the reason
// live held writes nothing, whatever its owner tag now says: next carries the new tag, and
// broker_unservable_secrets with it. A publish that changes no refusal allocates nothing here.
func (c *Current) publish(next *Set, first bool) {
	var before map[string]Refusal
	if live := c.set.Load(); live != nil {
		before = live.Refused
	}
	var changed []string
	for id, now := range next.Refused {
		if was, refused := before[id]; !refused || was.Reason != now.Reason {
			changed = append(changed, id)
		}
	}
	for id := range before {
		if _, refused := next.Refused[id]; !refused {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)
	for _, id := range changed {
		was, wasRefused := before[id]
		now, refused := next.Refused[id]
		switch {
		case refused && !wasRefused:
			slog.Error(RefusedMessage, "name", id, "reason", now.Reason, "owner", now.Owner, "first_load", first)
		case refused:
			slog.Error(RefusedMessage, "name", id, "reason", now.Reason, "owner", now.Owner, "first_load", false, "was", was.Reason)
		case c.serves(next, id):
			slog.Info(ServableMessage, "name", id, "reason", was.Reason, "owner", was.Owner, "now", "servable")
		default:
			slog.Info(ServableMessage, "name", id, "reason", was.Reason, "owner", was.Owner, "now", "absent")
		}
	}
	c.set.Store(next)
}

// serves reports whether set serves the secret whose whole name in the store is id. One outside
// the prefix, or whose name under it is no slug (ReasonNameMalformed), maps to no name a session
// asks for, so it is served by no set and never taken for the secret its name would fold onto.
func (c *Current) serves(set *Set, id string) bool {
	slug, ok := strings.CutPrefix(id, c.loader.Prefix)
	if !ok || !slugPattern.MatchString(slug) {
		return false
	}
	_, served := set.Secrets[SlugToName(slug)]
	return served
}

// Get answers the live policy.
func (c *Current) Get() *Set { return c.set.Load() }
