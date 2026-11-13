package policy

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Current is the live policy. The first load must succeed; a later load that fails keeps the
// previous set, so a Secrets Manager outage never turns into deny-everything. A secret refused for
// its own tags leaves only that secret out. Refresh re-describes each name it holds an anchor for
// (recent) for as long as the store's metadata may lag a write (Store.ListLag), so a lagging
// listing neither undoes what a single-name read settled nor undoes a change that reload's own
// re-describe caught. Each set it makes live is compared with the live one before it (publish),
// and only a change of a secret's state writes a line: a load that finds what the one before
// found writes nothing, however often it runs.
type Current struct {
	loader Loader
	set    atomic.Pointer[Set]
	// mu is the writer lock, held by every writer, Refresh and RefreshOne, from its read of Secrets
	// Manager to its Store, so neither stores a set built on one the other has since replaced. It
	// guards recent, and orders the state-change lines as the sets they describe are ordered. It
	// is a one-slot channel so a writer waiting for it gives up when its context ends (lock).
	mu chan struct{}
	// recent is, for each name a reread or a reload's own re-describe last changed the live answer
	// for, when it did: RefreshOne's reread and Refresh's own re-describe each record one only when
	// the answer differed from what the live set held, so a reread that confirms the live answer
	// leaves any earlier anchor in place and adds none. Each anchor lives until the store's ListLag
	// has passed.
	recent map[string]time.Time
	now    func() time.Time
}

// NewCurrent loads the policy once, failing when that load fails, and then reloads it every
// interval until ctx ends, each reload given that interval before it fails, so one stuck on a
// Secrets Manager that does not answer holds the writer lock no longer. The first load writes a
// RefusedMessage line for each secret it refuses, marked first_load. A run of reloads that fail
// while ctx is live writes one LoadFailedMessage line, at its first failure, and the reload that
// ends it one LoadRecoveredMessage line, as enroll.ServingClock.Run does for its beats, so an
// outage of any length is two lines and not one for each interval. A reload that fails once ctx
// has ended, the broker shutting down, is no failed load and writes nothing.
func NewCurrent(ctx context.Context, loader Loader, every time.Duration) (*Current, error) {
	c := &Current{loader: loader, mu: make(chan struct{}, 1), recent: map[string]time.Time{}, now: time.Now}
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
// one an earlier reload's own re-describe caught a change to. Any failed read fails the refresh.
func (c *Current) Refresh(ctx context.Context) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	// The window is measured from before the listing is fetched: a listing read inside it may lag
	// a reread however long Load takes to return.
	now := c.now()
	lag := c.loader.Store.ListLag()
	set, err := c.loader.Load(ctx)
	if err != nil {
		return err
	}
	live := c.set.Load()
	if len(c.recent) > 0 {
		for name, at := range c.recent {
			if now.Sub(at) > lag {
				delete(c.recent, name)
				continue
			}
			lk, err := c.loader.LoadOne(ctx, name)
			if err != nil {
				return err
			}
			id := c.storeID(name)
			merge(set.Secrets, set.Refused, name, id, lk)
			// A change this read catches that no reread did - a console delete, a tag that now
			// refuses the secret - is as new to the listing as a reread's answer, so it is kept for
			// the store's ListLag from now; the live set, not this listing, is what it changed.
			if !answers(live, name, id, lk) {
				c.recent[name] = c.now()
			}
		}
		set = setOf(set.Secrets, set.Refused)
	}
	c.publish(live, set)
	return nil
}

// RefreshOne rereads name alone (Loader.LoadOne) and merges the answer into the live policy: a
// served secret replaces or adds name, a refused one takes it out of what is served and records
// its refusal, and an absent one takes it out of both. Every other name stays as the live policy
// had it. A failed read leaves the live policy as it was and writes nothing, and a name no secret
// can carry is ErrNameInvalid before the writer lock (CheckName). A reread that finds what the
// live policy already held - the same secret served, the same refusal, or neither - changes
// nothing and writes nothing; one that changes it is published (publish) and writes the lines that
// change calls for. Refresh keeps a reread for the store's ListLag only when it changed the live
// answer, as Refresh does for its own re-describes: a reread that confirms what the live policy
// already held settled nothing a lagging listing could undo, and anyone may ask for a reread, so
// anchoring it would let each one cost every reload in the next ListLag a describe under the
// writer lock (on Secret Manager, two metadata reads from a quota the whole project shares). A
// served name found absent is a change and is kept, so a lagging listing cannot bring back a
// deleted secret; a secret created and deleted before any read served it can still show from a
// lagging listing until ListLag has passed. A refusal the reread changed is kept the same way, so a
// lagging listing does not undo a reason the reread settled and write the line back.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	if err := c.CheckName(name); err != nil {
		return Lookup{}, err
	}
	if err := c.lock(ctx); err != nil {
		return Lookup{}, err
	}
	defer c.unlock()
	lk, err := c.loader.LoadOne(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	live, id := c.set.Load(), c.storeID(name)
	if answers(live, name, id, lk) {
		return lk, nil
	}
	secrets, refused := maps.Clone(live.Secrets), maps.Clone(live.Refused)
	merge(secrets, refused, name, id, lk)
	c.publish(live, setOf(secrets, refused))
	c.recent[name] = c.now()
	return lk, nil
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

// merge sets name, whose whole name in the store is id, as lk found it: the served secret or no
// entry in secrets, and its refusal or no entry in refused.
func merge(secrets map[string]Secret, refused map[string]Refusal, name, id string, lk Lookup) {
	if lk.Served {
		secrets[name] = lk.Secret
	} else {
		delete(secrets, name)
	}
	if refusal := lk.refusal(); refusal.Reason != "" {
		refused[id] = refusal
	} else {
		delete(refused, id)
	}
}

// answers reports whether set already answers name, whose whole name in the store is id, as lk
// does: the same secret served, or the same refusal, or neither.
func answers(set *Set, name, id string, lk Lookup) bool {
	secret, served := set.Secrets[name]
	return served == lk.Served && secret == lk.Secret && set.Refused[id] == lk.refusal()
}

// publish makes next the live policy, first writing a line for each secret whose state next
// changes from live's, in the order of the secrets' whole names in the store, so whoever reads
// next has had its lines written already; the writer lock is held, so the lines of two sets come
// in the order the sets do. live is nil on the pod's first load, every line of which says so
// (first_load). A secret next refuses and live did not writes RefusedMessage, and one next
// refuses for another reason than live did writes it with was. One live refused and next does not
// writes ServableMessage, with now=servable when next serves it and now=absent when next neither
// serves nor refuses it, which is a secret gone from the store. A secret refused for the reason
// live held writes nothing, whatever its owner tag now says: next carries the new tag, and
// broker_unservable_secrets with it. A publish that changes no refusal allocates nothing here.
func (c *Current) publish(live, next *Set) {
	var before map[string]Refusal
	if live != nil {
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
			slog.Error(RefusedMessage, "name", id, "reason", now.Reason, "owner", now.Owner, "first_load", live == nil)
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
