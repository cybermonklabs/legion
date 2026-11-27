package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// Current is the live policy. The first load must succeed; a later load that fails keeps the
// previous set, so a Secrets Manager outage never turns into deny-everything. A secret refused for
// its own tags leaves only that secret out. Refresh re-describes each name it holds an anchor for
// (recent) for as long as the store's metadata may lag a write (Store.ListLag), so a lagging
// listing neither undoes what a single-name read settled nor undoes a change that reload's own
// re-describe caught. Each set it makes live is compared with the live one before it (publish),
// and only a change of a secret's state writes a line: a load that finds what the one before
// found writes nothing, however often it runs. Every set it makes live has the deployment's
// profiles applied to what the store holds (withhold, Profiles.apply), and its first writes
// ProfilesAppliedMessage.
//
// It also reads each type secret's stored value, once for each version and type a load or
// reread lists that this pod has not decoded together, and decodes it with secrets.ParseValue
// under that type (readValues), so a value no mint could use is refused
// (ReasonValueMalformed) from the load that first lists it, at pod start too, rather than at the
// first mint an agent needed. A secret of no type is never read, and a load that lists what the
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
	// channel so a writer waiting for it gives up when the context it waits on ends (lock), which
	// for a reload recording its value reads is one that does not end (readValues).
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
	// values is, for each type secret base serves, by Name, what this pod knows of its stored
	// value: the version and type a read now decodes under, the ones it last decoded under,
	// whether that value is refused, and whether its reads are failing (valueState). A secret base
	// stops serving as a type secret leaves it, with whatever refusal it held.
	values map[string]*valueState
	now    func() time.Time
}

// valueKey is what a type secret's value is decoded under: the version of the value the listing
// names (secrets.Listing.Version), and the type tag that says how it is read, which a
// retag changes and leaves the version as it was.
type valueKey struct{ version, secretType string }

// valueState is what Current knows of one type secret's stored value.
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
	// ends it when the read was of another version listed since. why is the decoder's or the
	// mint's error, the refusal's Detail, which never quotes the value (DecodeValue).
	refused   bool
	refusedAt string
	why       string
	// failing is whether the last read of the value failed: ValueReadFailedMessage was written for
	// it, and ValueReadRecoveredMessage is owed at the next read that succeeds.
	failing bool
}

// valueRead is one value read a load or reread hands readValues: the secret's Name, its store
// source (Secret.Source without the type TypeSource froze into it) and the key its value is
// decoded under.
type valueRead struct {
	name, source string
	key          valueKey
}

// valueOutcome is what one valueRead came to: the read's error, or whether the decoder refused the
// value it read, and its words of why.
type valueOutcome struct {
	valueRead
	err     error
	refused bool
	why     string
}

// NewCurrent loads the policy once, failing when that load fails, reads and decodes the value of
// each type secret it serves (readValues), on ctx like the load, so a value the decoder refuses
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
	// The loader weighs a secret waiting for its value by c's clock, which PendingValue reads too,
	// so the two never answer differently. Load and LoadOne run under the writer lock, which SetNow
	// holds while it changes the clock.
	c.loader.now = func() time.Time { return c.now() }
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
// refresh. Then it reads the value of each type secret the load lists at a version or type
// this pod has not decoded (readValues), on ctx, and makes live what those decodes leave; a value
// read that fails fails no refresh. No caller waits on a reload's value reads, so it records the
// ones it finished however ctx ends, waiting for the writer lock on ctx without its cancellation:
// a reload whose ctx ends during them - the background reload's interval passing, or the broker
// shutting down - fails no refresh either, and the read ctx cut short, and the reads after it,
// are made again at the next load.
func (c *Current) Refresh(ctx context.Context) error {
	reads, first, err := c.reload(ctx)
	if err != nil {
		return err
	}
	_ = c.readValues(ctx, context.WithoutCancel(ctx), reads, first)
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
		set = setOf(c.loader.Profiles, set.Secrets, set.Refused, set.Pending, set.versions)
	}
	first = c.base == nil
	c.base = set
	reads = c.list()
	c.publish(c.withhold(), first)
	// Each profile the live set serves behind its own proxy string whose source's proxy tag names
	// the same kind is a string the file can drop now: one line for each, at each full load, so
	// it stands out until the file drops it, and no reread repeats it. The loop goes with the
	// proxy string, once every source a profile names carries its own proxy tag.
	live := c.Get()
	for _, name := range live.redundantProxy {
		served := live.Secrets[name]
		slog.Warn(ProxyKindRedundantMessage, "profile", name, "source", served.Profile.Source, "kind", served.Proxy)
	}
	// A full load, and only a full load, names its groups to the reader: a group it names for
	// the first time is read at once, and a reread's new group (RefreshOne) waits for the next
	// load, admitting nobody until then. The groups are those of the stored secrets with the
	// profiles applied, before any value refusal: a profile's requesters may name a group its
	// source's do not, and a secret whose value is refused now still names its groups.
	if c.loader.Groups != nil {
		c.loader.Groups.Want(c.loader.Profiles.apply(c.base, c.loader.Prefix).GroupNames())
	}
	return reads, first, nil
}

// ErrValueUnchecked is a reread (RefreshOne) that merged what it read of the secret's metadata
// into the live policy and whose value read did not finish, since its context ended first. It
// wraps that context's error, so errors.Is matches context.Canceled or context.DeadlineExceeded
// on it too.
var ErrValueUnchecked = errors.New("the reread merged the secret's tags, and its context ended before its value read was recorded")

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
// A type secret whose value this pod has not decoded at the version and type the reread
// lists is then read and decoded (readValues), on ctx, and the answer is the policy's after that:
// a served secret the live policy refuses answers that refusal, ReasonValueMalformed for a value
// refused, or a reason of the profiles file's (ReasonGitHubAppNoProfile, ReasonGitHubInstallationsNoProfile, ReasonProfileNameTaken).
// A stored secret profiles of other names use answers as the store holds it, served, since what
// a reread tells its caller is whether the write it made took; a request names one of its
// profiles. A profile's name (Profiles.Lookup) rereads the profile's source, which is the secret
// the store holds for it, and answers the profile as the live policy then serves or refuses it.
// The value read waits for
// the writer lock to record that read only while ctx lives, and records nothing once ctx has
// ended: a caller may be waiting on it, and a reread nobody waits on is bounded by ctx's own
// deadline. When ctx ends before that read is recorded - its Store.Read cut
// short, or its wait for the writer lock - the reread has merged what it read, and its value read
// did not finish: RefreshOne answers ErrValueUnchecked wrapping ctx's error, with a Lookup that
// holds Moved alone rather than one the value read never checked. What the reread merged stays
// live, and the value is read at the next load or reread. Each of its callers tells that apart
// from a reread that merged nothing: the reread route answers a session's 503 REQUEST_ENDED, and
// logs it as a failed reread when it ran after a 202 that nobody waits on, and tells the other
// pods to reread the name either way, since this pod now serves the tags it read; the miss path
// takes it as its request's own failure; and the reread listener (cmd/cipherd), which a peer's
// notification drives, takes it as a reread that ran.
//
// Once name passes CheckName, the answer's Moved is every name whose value the reread may have
// moved (movedByReread), read against the live set as it stood when RefreshOne began, whatever
// the reread came to: the names whose held values the airlock's proxy drops (cmd/cipherd). It is
// set on an error too, ErrValueUnchecked or a reread that merged nothing, since a peer's
// notification is the signal that a stored value has a new one even where this pod's own reread
// failed. RefreshOne takes that set itself, so no caller reads what moved against another one.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	if err := c.CheckName(name); err != nil {
		return Lookup{}, err
	}
	before := c.Get()
	lk, err := c.refreshOne(ctx, name)
	lk.Moved = c.movedByReread(name, before)
	return lk, err
}

// refreshOne is RefreshOne for a name CheckName passed, but for Moved.
func (c *Current) refreshOne(ctx context.Context, name string) (Lookup, error) {
	// A profile's name is no secret in the store: the secret a reread of it reads is its source,
	// and the answer is the profile as the policy then serves or refuses it.
	read, profile := c.loader.Profiles.SourceOf(name)
	lk, reads, err := c.reread(ctx, read)
	if err != nil {
		return Lookup{}, err
	}
	if err := c.readValues(ctx, ctx, reads, false); err != nil {
		return Lookup{}, fmt.Errorf("%w: %w", ErrValueUnchecked, err)
	}
	live := c.Get()
	if profile {
		return profileLookup(live, name), nil
	}
	if lk.Served {
		if refusal, refused := live.Refused[c.storeID(name)]; refused {
			return Lookup{Reason: refusal.Reason, Owner: refusal.Owner}, nil
		}
	}
	return lk, nil
}

// profileLookup is the Lookup of the profile name as set holds it: served, as the Secret its
// source makes and at its source's value version, or refused, with that refusal.
func profileLookup(set *Set, name string) Lookup {
	if secret, served := set.Secrets[name]; served {
		return Lookup{Secret: secret, Served: true, version: set.versions[name]}
	}
	refusal := set.Refused[profileKey(name)]
	return Lookup{Reason: refusal.Reason, Owner: refusal.Owner}
}

// RereadNames is every name a reread of name may have given a new value: the stored secret the
// reread reads, which is name or, for a profile's name, the profile's source (RefreshOne), and
// each profile of that source (Profiles.Of), since every one of them is served from that one
// stored value. The airlock's proxy holds a grant's values by the name the grant was released
// under, a profile's own name for a profile, so a reread that moved that value (Lookup.Moved)
// drops each of these from it (cmd/cipherd), and a new value of a source reaches every profile of
// it rather than only a grant of the name that was reread. A name no profile is of, or that the
// policy has never heard of, is itself alone.
func (c *Current) RereadNames(name string) []string {
	source, _ := c.loader.Profiles.SourceOf(name)
	names := []string{source}
	for _, profile := range c.loader.Profiles.Of(source) {
		if !slices.Contains(names, profile) {
			names = append(names, profile)
		}
	}
	if !slices.Contains(names, name) {
		names = append(names, name)
	}
	return names
}

// movedByReread is the names whose value a reread of name may have moved, read against before,
// the live set as it stood when RefreshOne began: RereadNames(name) where the reread moved the
// value version (Set.ValueVersion) of a name served from the stored secret it read, or where
// neither set lists a version of any of them, and none where both list the same versions. A type
// secret's served names each carry its value version, which a reread moves only when the store
// holds a new value, so a reread that finds the value the policy held gave no name a new one:
// what a proxy holds of it was minted from that same value and is spent until it nears its
// expiry, rather than minted again for each held grant of each profile of the source at every
// reread anyone may ask for. A stored value has no version the policy lists, and nor does a name
// neither set serves, so for those the reread is the only signal of a new value, and every one of
// its names is moved. The live set is read after the reread, so a reload that moved a version in
// between counts as moved too.
func (c *Current) movedByReread(name string, before *Set) []string {
	names := c.RereadNames(name)
	live := c.Get()
	listed := false
	for _, served := range names {
		was := before.ValueVersion(served)
		if was != live.ValueVersion(served) {
			return names
		}
		listed = listed || was != ""
	}
	if listed {
		return nil
	}
	return names
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
		merged := &Set{Secrets: maps.Clone(c.base.Secrets), Refused: maps.Clone(c.base.Refused), Pending: maps.Clone(c.base.Pending), versions: maps.Clone(c.base.versions)}
		merge(merged, name, id, lk)
		c.base = setOf(c.loader.Profiles, merged.Secrets, merged.Refused, merged.Pending, merged.versions)
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

// PendingValue reports whether set holds the secret a session asks for as name waiting for its
// first value (Set.Pending, ReasonPendingValue), not served, and still waiting by c's clock
// (pendingAt), which c's loads and rereads weigh it by too (NewCurrent): a request for it is refused
// ErrPendingValue rather than ErrUnknownSecret. The age is checked here, not only at load, so a set
// no load or reread has replaced since (a reread the miss-path limit refused, or one that failed)
// stops answering pending at the window, as the policy's own reason would.
func (c *Current) PendingValue(set *Set, name string) bool {
	if _, served := set.Secrets[name]; served || c.CheckName(name) != nil {
		return false
	}
	waiting, ok := set.Pending[c.storeID(name)]
	return ok && pendingAt(waiting.Created, c.now())
}

// merge sets name, whose whole name in the store is id, in set's maps as lk found it: the served
// secret or no entry in Secrets, its value's version or no entry in versions, its refusal or no
// entry in Refused, and its wait for a value or no entry in Pending. set's Version is not
// recomputed.
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
	if lk.Reason == ReasonPendingValue {
		if set.Pending == nil {
			set.Pending = map[string]Pending{}
		}
		set.Pending[id] = lk.pending()
	} else {
		delete(set.Pending, id)
	}
}

// answers reports whether set already answers name, whose whole name in the store is id, as lk
// does: the same secret served at the same value version, the same refusal, the same wait for a
// value, or none of them.
func answers(set *Set, name, id string, lk Lookup) bool {
	secret, served := set.Secrets[name]
	return served == lk.Served && secret == lk.Secret && set.versions[name] == lk.version && set.Refused[id] == lk.refusal() && set.Pending[id] == lk.pending()
}

// list records, for each type secret base serves, the key a read of its value decodes under now
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

// listOne is list for name alone: it records the version and type base lists for it as
// the key a read of its value decodes under, or forgets it when base serves it as no type secret
// or not at all, and answers the read of its value, with true, when this pod has not decoded the
// value under that key. The caller holds mu.
func (c *Current) listOne(name string) (valueRead, bool) {
	secret, served := c.base.Secrets[name]
	secretType, source, minted, err := ParseTypeSource(secret.Source)
	if !served || !minted || err != nil {
		delete(c.values, name)
		return valueRead{}, false
	}
	key := valueKey{version: c.base.versions[name], secretType: secretType}
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
// read=value), on ctx, and decodes it under its type (DecodeValue), dropping the value at
// once; the reads are made without the writer lock. A read that fails once ctx has ended stops
// them: it and the reads after it record nothing, and the next load or reread makes them again.
// It then takes the writer lock for as long as wait lives (lock) to record the reads it finished.
// Refresh waits on ctx without its cancellation, so a reload records what it read however ctx
// ends; RefreshOne waits on ctx itself, as the reread before it did and as RefuseValue does, so a
// reread whose caller has gone does not wait out a reload stuck on a store that does not answer,
// and once ctx has ended it records nothing (lock answers an ended ctx at once): each secret keeps
// the state it had, and the version it read, still not decoded, is read again at the next load or
// reread. It answers ctx's error when ctx cut a read short, and wait's when wait ended before it
// took the lock, and nil once it has recorded every read it was handed. Holding the lock, it
// records each outcome whose key is still the one listed (a load or reread since that lists
// another reads that itself):
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
func (c *Current) readValues(ctx, wait context.Context, reads []valueRead, first bool) error {
	if len(reads) == 0 {
		return nil
	}
	var cut error
	outcomes := make([]valueOutcome, 0, len(reads))
	for _, read := range reads {
		c.loader.StoreReads.Inc(readValue)
		value, err := c.loader.Store.Read(ctx, read.source)
		if err != nil && ctx.Err() != nil {
			cut = ctx.Err()
			break
		}
		outcome := valueOutcome{valueRead: read}
		switch {
		case errors.Is(err, secrets.ErrValueNotText):
			// The sentinel's own words: the store's error around it names the call and the
			// secret, which the refusal's line names already.
			outcome.refused, outcome.why = true, secrets.ErrValueNotText.Error()
		case err != nil:
			outcome.err = err
		default:
			if err := DecodeValue(read.key.secretType, value); err != nil {
				outcome.refused, outcome.why = true, err.Error()
			}
		}
		outcomes = append(outcomes, outcome)
	}
	if len(outcomes) == 0 {
		return cut
	}
	if err := c.lock(wait); err != nil {
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
				state.refused, state.why, changed = false, "", true
			}
			continue
		}
		if state.failing {
			slog.Info(ValueReadRecoveredMessage, "name", id, "version", o.key.version)
			state.failing = false
		}
		state.decoded = o.key
		if state.refused != o.refused || state.why != o.why {
			changed = true
		}
		state.refused, state.refusedAt, state.why = o.refused, o.key.version, o.why
	}
	if changed {
		c.publish(c.withhold(), first)
	}
	return cut
}

// RefuseValue refuses the value of the type secret name at version, read as type, as
// a load's decode refuses one (ReasonValueMalformed): the mint path calls it when
// secrets.ParseValue refused the value a release read, with the version the live policy listed
// for name when that release read it (Set.ValueVersion), the type that release decoded it under,
// and the minter's error, the refusal's Detail, which never quotes the value; so a pod whose own
// load could not read the value refuses the secret from its first mint. A key that is no longer
// the one listed - a load or reread has listed a new value since, or a retag a new type for the
// same value - is not refused, since that key is decoded afresh; nor is a name Current holds no
// type secret for. A secret it newly refuses, or refuses for other words, is published, writing
// one RefusedMessage line. It waits on the writer lock for as long as ctx lives, the release's
// own request: one whose caller has gone marks nothing, and the next mint of the secret, or the
// next load that reads a new version, marks it then.
func (c *Current) RefuseValue(ctx context.Context, name, version, secretType string, cause error) {
	// A profile has no value of its own: what a release of it decoded is its source's, so the
	// refusal is the source's, as a load's would be, and every profile of the source is refused
	// with it (withhold).
	name, _ = c.loader.Profiles.SourceOf(name)
	if c.lock(ctx) != nil {
		return
	}
	defer c.unlock()
	state, known := c.values[name]
	if !known || state.listed != (valueKey{version: version, secretType: secretType}) {
		return
	}
	why := ""
	if cause != nil {
		why = cause.Error()
	}
	was, wasWhy := state.refused, state.why
	state.refused, state.refusedAt, state.why = true, version, why
	if !was || wasWhy != why {
		c.publish(c.withhold(), false)
	}
}

// withhold is base as Current serves it: each type secret whose value is refused
// (valueState.refused) taken out of Secrets and put in Refused under ReasonValueMalformed, with
// its owner tag, and then the profiles applied (Profiles.apply), with none refused too, so every
// set the four publishes make live (reload, reread, readValues and RefuseValue) holds them, and a
// profile of a source whose value is refused is refused with that source's reason. The caller
// holds mu.
func (c *Current) withhold() *Set {
	set := c.base
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
		refused[c.storeID(name)] = Refusal{Reason: ReasonValueMalformed, Owner: secret.Owner, Detail: state.why, SupersededBy: secret.SupersededBy}
	}
	if served != nil {
		set = setOf(c.loader.Profiles, served, refused, c.base.Pending, c.base.versions)
	}
	return c.loader.Profiles.apply(set, c.loader.Prefix)
}

// publish makes next the live policy, first writing a line for each secret whose state next
// changes from the live one's, in the order of the secrets' whole names in the store, so whoever
// reads next has had its lines written already; the writer lock is held, so the lines of two sets
// come in the order the sets do. first is the pod's first load, every line of which says so
// (first_load). A secret next refuses and live did not writes RefusedMessage, and one next
// refuses for another reason than live did writes it with was. One live refused and next does not
// writes ServableMessage, with now=servable when next serves it, now=pending when next holds it
// waiting for its value, and now=absent when next neither serves, refuses nor holds it, which is a
// secret gone from the store. A secret next holds waiting for its value (Set.Pending) that live
// neither held so nor refused writes PendingMessage, once: the loads that find it still waiting
// write nothing, nor does its value landing, since a secret coming to be served writes no line,
// nor its leaving the store. A secret refused for the reason live held writes nothing, whatever its
// owner tag now says: next carries the new tag, and broker_unservable_secrets with it. A publish
// that changes no refusal and no wait allocates nothing here. A refused profile is named by its
// key (profileKey), and a refusal with a Detail writes it as detail.
//
// The first publish of c, the one with no live set before it, writes ProfilesAppliedMessage after
// those lines, once: c is the process's one policy, so that is once per process.
func (c *Current) publish(next *Set, first bool) {
	var before map[string]Refusal
	var waited map[string]Pending
	live := c.set.Load()
	if live != nil {
		before, waited = live.Refused, live.Pending
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
	for id := range next.Pending {
		_, waiting := waited[id]
		_, refused := before[id]
		if !waiting && !refused {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)
	for _, id := range changed {
		was, wasRefused := before[id]
		now, refused := next.Refused[id]
		pending, waits := next.Pending[id]
		switch {
		case refused && !wasRefused:
			slog.Error(RefusedMessage, refusedAttrs(id, now, first, "")...)
		case refused:
			slog.Error(RefusedMessage, refusedAttrs(id, now, false, was.Reason)...)
		case waits && !wasRefused:
			slog.Info(PendingMessage, "name", id, "reason", ReasonPendingValue, "owner", pending.Owner, "first_load", first)
		case waits:
			slog.Info(ServableMessage, "name", id, "reason", was.Reason, "owner", was.Owner, "now", "pending")
		case c.serves(next, id):
			slog.Info(ServableMessage, "name", id, "reason", was.Reason, "owner", was.Owner, "now", "servable")
		default:
			slog.Info(ServableMessage, "name", id, "reason", was.Reason, "owner", was.Owner, "now", "absent")
		}
	}
	if live == nil {
		c.profilesApplied(next)
	}
	// Every set made live carries the loader's group reader, the reread's as much as the load's,
	// so no decision reads another; publish names no group to it (reload does, for a full load).
	next.groups = c.loader.Groups
	c.set.Store(next)
}

// refusedAttrs are RefusedMessage's fields for the refusal of id: name, reason, owner, first_load,
// was where the reason before it is not "", and detail where the refusal carries one.
func refusedAttrs(id string, refusal Refusal, first bool, was string) []any {
	attrs := []any{"name", id, "reason", refusal.Reason, "owner", refusal.Owner, "first_load", first}
	if was != "" {
		attrs = append(attrs, "was", was)
	}
	if refusal.Detail != "" {
		attrs = append(attrs, "detail", refusal.Detail)
	}
	return attrs
}

// profilesApplied writes ProfilesAppliedMessage for next, the first set c makes live: how many
// of the profiles file's entries it serves and how many it refuses, and the file's digest.
func (c *Current) profilesApplied(next *Set) {
	served, refused := 0, 0
	for _, name := range c.loader.Profiles.Names() {
		if _, held := next.Refused[profileKey(name)]; held {
			refused++
		} else if next.Secrets[name].Profile != nil {
			served++
		}
	}
	slog.Info(ProfilesAppliedMessage, "served", served, "refused", refused, "file_digest", c.loader.Profiles.fileDigest)
}

// serves reports whether set serves the secret whose whole name in the store is id, or, for a
// profile's key (profileKey), the profile of that name. One outside the prefix, or whose name
// under it is no slug (ReasonNameMalformed), maps to no name a session asks for, so it is served
// by no set and never taken for the secret its name would fold onto.
func (c *Current) serves(set *Set, id string) bool {
	if name, profile := strings.CutPrefix(id, profileKeyPrefix); profile {
		_, served := set.Secrets[name]
		return served
	}
	slug, ok := strings.CutPrefix(id, c.loader.Prefix)
	if !ok || !slugPattern.MatchString(slug) {
		return false
	}
	_, served := set.Secrets[SlugToName(slug)]
	return served
}

// Get answers the live policy.
func (c *Current) Get() *Set { return c.set.Load() }
