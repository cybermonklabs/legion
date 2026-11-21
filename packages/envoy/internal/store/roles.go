package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// RoleClaim is the durable ownership record for a role lane. Role claims are
// independent of interest records: a listener restart can temporarily age an
// interest out before its still-running holder re-registers.
type RoleClaim struct {
	HolderSessionID   string `json:"holder_session_id"`
	ClaimedAt         int64  `json:"claimed_at"`
	PreviousSessionID string `json:"previous_session_id"`
	// ClientInstanceID is the client instance of the process that holds the claim, which the
	// claimant named when it took the role. A session id alone does not name that process: a
	// second process can resume a live session's id, and it carries a client instance of its
	// own, so this field is what tells the two apart and keeps the second from taking,
	// releasing or reaping the first's role. Empty where the claimant named none - a claim an
	// earlier build wrote, or one whose caller has no instance - and an empty instance is
	// never evidence that two claims come from the same process.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// RenewedAt is the wall clock in milliseconds at which the holder's process last answered
	// for this claim; a claim write stamps it with that write's own ClaimedAt. Zero means the
	// record carries no renewal, which a claim an earlier build wrote does not, and such a
	// claim is read at the time it was claimed rather than at the epoch: every reader of a
	// renewal reads it through LastAnsweredAt, which says so.
	RenewedAt int64 `json:"renewed_at,omitempty"`
	// RenewedWithoutProcess is the note a registration that states no process leaves on a
	// claim that names none when it renews it (Registry.renewRoleClaim): the claim's holder
	// answers for it without saying which process it is, so from then on no registration that
	// states a process adopts it. It is absent on every other claim. Every grant writes a fresh
	// record without it (SetRoleWithPrevious), and a claim an earlier build stored has none
	// either, so the first registration of its session that states a process adopts it: the
	// one heartbeat in which a claim that names no process is open to any process of its
	// session. A note rather than a comparison of RenewedAt with ClaimedAt, because a clock
	// that does not move, and two listeners whose clocks differ, make those two equal or
	// reversed for a claim that was renewed.
	RenewedWithoutProcess bool `json:"renewed_without_process,omitempty"`
	// HandedBack is the mark a hand-back left on this claim: the release named this role's own
	// lane topic and stated no process, so it was not authorised to free the claim
	// (RoleReleaseAuthority.frees) and marked it instead (Registry.markRoleClaimHandedBack),
	// whether the claim names a process or none. It is written by that release alone - never by
	// the administrator's force, which frees the claim outright, and never by the empty-topics
	// deregistration. Its readers are the renewal, which renews and adopts no marked claim
	// whoever registers (Registry.renewRoleClaim), so the role claim reaper frees the role once
	// that renewal has lapsed, a live window and up to one reap interval later, and GET
	// /v1/roles/<role>, which reports it as handed_back. Absent means no hand-back of that shape
	// has reached this claim: a claim granted since clears the mark, because every grant writes
	// a fresh record (SetRoleWithPrevious).
	HandedBack bool `json:"handed_back,omitempty"`
	// Revision is the role bucket revision the claim was read at, which RoleClaim stamps on
	// every claim it decodes and nothing stores: it belongs to the entry, not to the record,
	// so it is never marshalled back into the bucket. Zero means no claim record was read -
	// an unclaimed role, or a claim built to be written. It is what a conditional
	// claim names as its precondition (SetRoleWithPrevious's ifClaimRevision) and what GET
	// /v1/roles/<role> reports as claim_revision, so a caller can read a role and then claim
	// it only while no other claim has landed.
	Revision uint64 `json:"-"`

	// Created is when the claim's record was stored, on the server's clock, which every read of
	// the bucket stamps on the claim it decodes and nothing stores: it belongs to the entry, not
	// to the record, so it is never marshalled back into the bucket. Zero means no claim record
	// was read, as a zero Revision does. It is what a role claim's grace is weighed by: a claim
	// stored before the machine's service start keeps its grace, one stored since gets none
	// (releaseReadExpiredRoleClaim).
	Created time.Time `json:"-"`

	// unknown holds the JSON of every key a stored claim carries that RoleClaim does not name: a
	// field a newer build writes, which a role claim through this build writes back. It is never
	// modified once decoded, so copies of a claim may share it.
	unknown map[string]json.RawMessage
}

// roleClaimKeys are RoleClaim's JSON keys; any other key of a stored claim is kept in its unknown
// map, and written back by every rewrite of the claim, so pods of two builds taking turns at one
// record during a roll do not drop each other's fields.
var roleClaimKeys = JSONKeys(reflect.TypeFor[RoleClaim]())

// LastAnsweredAt is the wall clock in milliseconds at which this claim's holder last answered for
// it: RenewedAt where the record carries one, and ClaimedAt, the moment the claim was written,
// where it does not. It is the one reading of a claim's renewal, so a claim stored before this
// build, which carries no RenewedAt, is read at the time it was claimed rather than at the epoch,
// where every window would already have passed. A claim carrying neither is zero, which is older
// than any window.
func (c RoleClaim) LastAnsweredAt() int64 {
	if c.RenewedAt > 0 {
		return c.RenewedAt
	}
	return c.ClaimedAt
}

// HeldByProcess reports whether instance names the very process this claim records as the
// holder's. It is the one strict test of a claim against a process, which every decision that
// weighs the two reads through: the release (RoleReleaseAuthority.frees), a claim of a role held
// under the claimant's own session id (POST /v1/roles/set), and the renewal
// (Registry.renewRoleClaim, which also adopts a claim that names no process at its first
// heartbeat).
//
// An absent id on either side names no process, so it matches nothing: a claim whose claimant
// stated none names none, and a caller that states none says nothing about which process it is.
// That is the opposite of session.InstanceMatches, the loose test the predecessor rule reads,
// where an absent id on either side is a client predating the field and the claim goes through.
func (c RoleClaim) HeldByProcess(instance string) bool {
	return instance != "" && c.ClientInstanceID == instance
}

// ExpiredRoleClaimRelease reports what releaseReadExpiredRoleClaim's conditional delete did to
// an expired role claim: ExpiredRoleClaimReleased is the delete that landed, the one outcome the
// sweep counts as reaped (ReapRoleClaims); ExpiredRoleClaimMissing a role whose record was gone
// before the delete, or that never had one; ExpiredRoleClaimSuperseded a claim written again
// between the caller's read and the delete, which the delete's own revision kept; and
// ExpiredRoleClaimRetained a claim left standing, either for the grace a claim stored before the
// machine's service start keeps (sessionTTL) or by a delete that failed, which the error beside
// it names.
type ExpiredRoleClaimRelease uint8

const (
	ExpiredRoleClaimReleased ExpiredRoleClaimRelease = iota
	ExpiredRoleClaimMissing
	ExpiredRoleClaimSuperseded
	ExpiredRoleClaimRetained
)

// scannedRoleClaim is one role claim record as a scan of the role bucket delivered it: the key it
// is stored under, the claim decoded from its value with the entry's revision and stored time
// stamped on it, and the error DecodeRoleClaim answered, nil when the record decoded. A sweep
// reading from the scan has of each record exactly what a Get of its key gave it before - the
// value, the revision and the stored time - so the decode failure travels with the record rather
// than failing the scan: a stored record this build cannot read costs its own role and no more,
// as it did when each key was read on its own.
type scannedRoleClaim struct {
	Role  string
	Claim RoleClaim
	Err   error
}

// roleClaimsUntil lists every role claim the role bucket holds, sorted by role, from one scan of
// its existing keys (kvwatch.ScanExistingKeys) with delete markers left out: nats.go's Keys runs
// its legacy watcher (bus.KeyValue says why none may run). An empty bucket lists no claim. A scan
// short of the bucket fails the caller rather than list part of it, where nats.go's Keys would
// hand over what had arrived when its watcher gave up, so a sweep never takes a partial list for
// every claim.
//
// The scan carries each record's value, and not its metadata alone, because its callers want the
// claim: a listing of keys costs every caller a Get of its own per key, which is a round trip per
// stored claim per pass against the bucket whose read load is what this reads one pass of instead.
//
// It refuses at the first key it is handed once ctx has ended, so a listing its caller walked
// away from ends there. The scan must end on its own account, because it has a context of its
// own (kvwatch.ScanExistingKeys builds one): one left running goes on holding an ordered
// consumer and appending to a map nobody reads until the bucket's last key arrives or its idle
// bound elapses (kvwatch.ScanIdleTimeout, 10 s), while the client's retry starts a second scan
// of the whole bucket - against the bucket already too far behind to serve the claim view, which
// is the only reason the deregistration's listing runs at all. The error a visit returns ends
// the scan, and the scan then stops and drains its watcher (kvwatch.drainScan), so the listing
// costs the bucket nothing past the answer its caller already has.
//
// The claims are collected into a map keyed by the stored key, the higher revision winning, and
// only then sorted into a slice. The map is what makes a key delivered twice one claim: an ordered
// consumer delivers a key's last-per-subject entry and then any update to it that lands while the
// scan runs, and a sweep handed that key twice would act on the role twice, the second time at the
// older of the two revisions. The slice is sorted by role because that is the order a listing of
// the bucket's keys returned, which the sweeps' callers read their logs and their fixtures in.
//
// Nothing an entry's value aliases is retained: kvwatch.Entry.Value shares the read entry's bytes
// (kvwatch/entry.go), while RoleClaim holds only strings encoding/json allocated out of them. What
// is held to the end of the scan is one scannedRoleClaim per stored claim - a role string, the
// claim's two strings, its two numbers and its stored time - which at the fleet's ~5,000 claims is
// under about 1 MB.
func (r *Registry) roleClaimsUntil(ctx context.Context) ([]scannedRoleClaim, error) {
	claims := map[string]scannedRoleClaim{}
	_, err := kvwatch.ScanExistingKeys(context.Background(), r.roles(), jetstream.AllKeys, "role claims", kvwatch.ScanIdleTimeout, r.logger(), func(entry kvwatch.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scanned, ok := claims[entry.Key]; ok && scanned.Claim.Revision >= entry.Revision {
			return nil
		}
		claim, decodeErr := DecodeRoleClaim(entry.Value)
		claim.Revision, claim.Created = entry.Revision, entry.Created
		claims[entry.Key] = scannedRoleClaim{Role: entry.Key, Claim: claim, Err: decodeErr}
		return nil
	}, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(maps.Values(claims), func(a, b scannedRoleClaim) int { return strings.Compare(a.Role, b.Role) }), nil
}

// ErrMalformedRoleClaim is a role claim record the bucket holds and this build cannot read: one
// that names no holder, or JSON that is not a claim. The bus answered it, so it is not a read that
// failed, and reading it again returns the same record; each error wrapping it names which.
var ErrMalformedRoleClaim = errors.New("malformed role claim")

// DecodeRoleClaim reads a stored role claim record: the fields RoleClaim names, every other key
// into its unknown map, or a bare session id an earlier build stored. Any other record is
// ErrMalformedRoleClaim. Dispatch's outbox reads the role bucket through it too (roles.Buckets),
// so the two readers cannot drift.
//
// A record that carries no client instance id, no renewal, or neither is read, not refused: a
// claim an earlier build wrote names no process and no renewal, and both fields are left zero, as
// they are for the bare session id. A missing field is never ErrMalformedRoleClaim, which is a
// record this build cannot read at all.
func DecodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	unknown, err := DecodeKeeping(value, &claim, roleClaimKeys)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("%w: it names no holder", ErrMalformedRoleClaim)
		}
		claim.unknown = unknown
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("%w: %w", ErrMalformedRoleClaim, err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("%w: a legacy record with an empty holder", ErrMalformedRoleClaim)
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

// encodeRoleClaim writes claim for the bucket: its known fields, and the unknown keys it was
// decoded with (DecodeRoleClaim).
func encodeRoleClaim(claim RoleClaim) ([]byte, error) {
	return EncodeKeeping(claim, claim.unknown)
}

// newRoleHolderCopy is an empty copy of the role claim bucket, which keeps the session each claim
// it can read names. A claim this build cannot read (ErrMalformedRoleClaim) gives its role to
// nobody, so the copy keeps no entry for it, as a delete marker leaves none. Each entry it removes
// is handed to r's repair (roleClaimRepair.swept), which counts the ones a repairing read's sweep
// took out.
func newRoleHolderCopy(r *Registry) kvwatch.Copy[string] {
	return kvwatch.NewCopy(kvwatch.Rows[string]{Decode: func(entry kvwatch.Entry) (string, bool, error) {
		claim, err := DecodeRoleClaim(entry.Value)
		if err != nil {
			return "", false, err
		}
		return claim.HolderSessionID, true, nil
	}, Removed: r.roleRepair.swept})
}

// roleCache is the registry as the role claim cache its watcher feeds (kvwatch.Cache). The hooks
// are its own type's, not the registry's methods, so nothing but the watcher calls them. Beside the
// copy they count what each whole read of the bucket repaired in it (roleClaimRepair), which
// RoleClaimRepair reports.
type roleCache struct{ r *Registry }

// BeginRead starts a read of the role claim bucket (kvwatch.Copy.BeginRead). Every holder stays.
// Whether the read repairs the copy, and so counts what it repairs, is the repair's to say
// (roleClaimRepair.begin).
func (c roleCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.BeginRead(read)
	r.roleRepair.begin(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a claim it could
// not read, which gave its role to nobody, as the interest watcher logs a value it cannot decode.
// What the record did to the role's entry is handed to the repair while a read that repairs is
// under way (roleClaimRepair.delivered); a record delivered while the copy follows its bucket
// costs no look at the entry beside its own write.
func (c roleCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	var malformed error
	if r.roleRepair.repairing {
		before, held := r.roleHolders.Get(entry.Key)
		malformed = r.roleHolders.Delivered(entry)
		after, holds := r.roleHolders.Get(entry.Key)
		r.roleRepair.delivered(entry.Key, before, held, after, holds)
	} else {
		malformed = r.roleHolders.Delivered(entry)
	}
	r.holdersMu.Unlock()
	if malformed != nil {
		r.logger().Warn("role claim watcher found a claim it cannot read; it names no holder",
			slog.String("role", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every holder and fence entry the read found no evidence of: the bucket no
// longer holds that role's claim. The repair runs that sweep (roleClaimRepair.sweep), so a read
// that repairs counts each holder the sweep removes as taken out (roleClaimRepair.swept), and no
// removal outside the sweep is counted so. Every completed read, whether it repaired or filled the
// copy, is recorded as the last on the registry's clock, with what it repaired
// (roleClaimRepair.complete); a read that repaired anything logs it at INFO, with the counts and
// the first roleClaimRepairLoggedRoles of the roles it repaired.
func (c roleCache) CompleteRead(read kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	r.roleRepair.sweep(&r.roleHolders)
	found := r.roleRepair.complete(read, r.now())
	r.holdersMu.Unlock()
	if found.putBack > 0 || found.tookOut > 0 {
		r.logger().Info(fmt.Sprintf("role claim repair put back %d claims and took out %d", found.putBack, found.tookOut),
			slog.Int("put_back", found.putBack),
			slog.Int("took_out", found.tookOut),
			slog.Any("put_back_roles", found.putBackRoles),
			slog.Any("took_out_roles", found.tookOutRoles),
		)
	}
}

// setOwnRoleHolder records a claim write this registry made through a handle of epoch
// (kvwatch.Copy.Put): a write through a handle on a bucket since recreated is dropped rather than
// fencing out the new bucket's lower revisions, and so is one the fence finds older than the
// holder the copy already took.
func (r *Registry) setOwnRoleHolder(role, holder string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Put(r.roleWatcher, epoch, role, holder, revision)
}

// forgetRoleHolder records that the claim of role at revision was deleted through a handle of
// epoch. A delete does not return its marker's revision, so the view is fenced one past the
// revision deleted, as deleteInterest fences the interest cache: a claim written since is later
// still, and applies.
func (r *Registry) forgetRoleHolder(role string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Evict(r.roleWatcher, epoch, role, revision+1)
}

// roleViewEpoch is the epoch a claim write through the role handle carries, and 0 when no watcher
// follows the claims, which only a registry a unit test built leaves: such a write reaches no
// view (setOwnRoleHolder, forgetRoleHolder).
func (r *Registry) roleViewEpoch() uint64 {
	if r.roleWatcher == nil {
		return 0
	}
	_, epoch := r.roleWatcher.Handle()
	return epoch
}

// roleClaimWrite is one conditional write of one role's claim, as its caller means it: which
// records the write may be made over, the write itself, and what the view owes a write that
// landed. Three callers make such a write - the release's delete (releaseReadRoleClaim), the
// hand-back's mark (markRoleClaimHandedBack) and the renewal (renewRoleClaim) - and what they
// share is the protocol, not the meaning: the bounded call, the bucket's answers read one way,
// the bounded re-read, and the bound on attempts. writeRoleClaim runs that, each caller states
// the rest here, and nothing is flattened: a lost race means something different to each of
// them, which each says by what it does with the outcome.
type roleClaimWrite struct {
	// role is the claim's key: what the write is sent against and what a re-read reads.
	role string
	// attempts is how many conditional writes may be sent in all, counting the first. One is a
	// write that is never retried, which is what a lost race means to the renewal: the record
	// is another claimant's now, and a renewal written into it would stamp this process's
	// heartbeat on a claim it does not hold.
	attempts int
	// writes reports whether this write may be made over claim, the record as it was last
	// read. It is asked before every attempt, so a retry is made only over a record its caller
	// still means to write, and a caller comparing the re-read against the claim it first read
	// closes over that one.
	writes func(claim RoleClaim) bool
	// send sends the write itself, conditional on the revision claim was read at, and answers
	// the revision the record stands at after it, as the bucket's writes do; a delete answers
	// none and its caller reads the revision it deleted off claim.
	send func(claim RoleClaim) (uint64, error)
	// landed moves this registry's own copy of the role's holder to what the write left
	// (setOwnRoleHolder, forgetRoleHolder), over the claim the write was made at, the revision
	// it answered and the epoch of the handle it was sent through, read before the write.
	landed func(claim RoleClaim, revision, epoch uint64)
}

// roleClaimWriteOutcome is what one run of writeRoleClaim did to the role's record, for the
// callers that answer for each of them differently. A failure is the zero value and comes with
// the error that caused it; every other outcome carries none.
type roleClaimWriteOutcome uint8

const (
	// roleClaimWriteFailed is a bucket call that did not answer, a re-read that failed, or an
	// answer that is neither a landed write nor a lost race. Nothing was written.
	roleClaimWriteFailed roleClaimWriteOutcome = iota
	// roleClaimWriteLanded is the conditional write the bucket took; landed has been called.
	roleClaimWriteLanded
	// roleClaimWriteStood is a record this write may not be made over: the caller's own test
	// refused it, or no record claims the role at all (a zero revision), which no conditional
	// write can be made over either. The claim writeRoleClaim answers beside it is the record
	// it stopped at, which is the caller's to read.
	roleClaimWriteStood
	// roleClaimWriteMissing is a write the bucket answered nats.ErrKeyNotFound: the record was
	// gone when it arrived.
	roleClaimWriteMissing
	// roleClaimWriteRaced is every attempt lost to a write that landed since the read it was
	// conditional on (nats.ErrKeyExists), the last of them at the attempt bound.
	roleClaimWriteRaced
)

// writeRoleClaim sends w over claim, the record its caller read, conditional on the revision
// that read returned, and sends it again over a re-read of the record while the write loses
// that condition and w allows another attempt. Each bucket call waits no longer than ctx
// (bus.BoundedOn); a call ctx abandoned may still land after this answer, and it is conditional on
// the revision it was sent at, so it touches no record written since - each caller says what
// that costs it.
//
// It answers what became of the record (roleClaimWriteOutcome), the claim it stopped at, and the
// error of a call that failed. The claim is the caller's own when nothing was re-read, and the
// re-read's record otherwise, so a caller that must weigh the record it left reads it here
// rather than reading the bucket again.
func (r *Registry) writeRoleClaim(ctx context.Context, claim RoleClaim, w roleClaimWrite) (roleClaimWriteOutcome, RoleClaim, error) {
	for attempt := 1; ; attempt++ {
		if claim.Revision == 0 || !w.writes(claim) {
			return roleClaimWriteStood, claim, nil
		}
		at := claim
		epoch := r.roleViewEpoch()
		revision, answered, err := bus.BoundedOn(ctx, r.roles(), func() (uint64, error) { return w.send(at) })
		switch {
		case !answered:
			return roleClaimWriteFailed, claim, err
		case err == nil:
			w.landed(at, revision, epoch)
			return roleClaimWriteLanded, at, nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return roleClaimWriteMissing, claim, nil
		case !errors.Is(err, nats.ErrKeyExists):
			return roleClaimWriteFailed, claim, err
		case attempt == w.attempts:
			return roleClaimWriteRaced, claim, nil
		}
		if claim, err = r.RoleClaim(ctx, w.role); err != nil {
			return roleClaimWriteFailed, claim, err
		}
	}
}

// ErrRoleViewNotReady is RolesHeldBy's answer when its view of the role claims cannot be served,
// so it cannot say which roles a session holds: the watcher has not completed a read of the bucket,
// its copy has not followed the bucket for longer than the stale bound
// (kvwatch.Watcher.Servable), the watcher has ended and not been replaced, or no watcher follows
// the claims. It is a read that did not answer, never the answer that a session holds no roles.
var ErrRoleViewNotReady = errors.New("the role claim view cannot be served")

// RolesHeldBy returns the roles whose claims name sessionID, sorted, from the role claim
// watcher's copy of the bucket, so it makes no bucket call. A session holds a role exactly when
// the role's claim names it; its interest record's topics say nothing about that. It answers only
// while that copy can be served (kvwatch.Watcher.Servable): a read of the bucket has completed and
// the copy follows it, or stopped following it no longer ago than the stale bound. Otherwise it
// returns ErrRoleViewNotReady, wrapped, with the reason RoleViewErr gives, the one test of the
// view's state, which /healthz makes too. A read whose scan timed out is read again by the
// watcher itself, so a view it refuses becomes servable without a Rewatch.
//
// The copy keeps every holder through a read and removes the ones the read found no evidence of
// only once it completes (roleCache), so a read of it during a refill answers the copy it had, not
// a part of the bucket, and no answer is ever a session's roles stripped by a read in flight.
func (r *Registry) RolesHeldBy(sessionID string) ([]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	roles := []string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		if holder == sessionID {
			roles = append(roles, role)
		}
	}
	r.holdersMu.RUnlock()
	sort.Strings(roles)
	return roles, nil
}

// RolesBySession is RolesHeldBy for every session at once: each holder's roles, sorted, read in
// one pass over the claim copy under one lock, for a caller that would otherwise ask per row
// (GET /v1/sessions, whose answer carries every listed session's roles). It refuses on the same
// terms, and a session no claim names is simply absent from the map, which a caller reads as no
// roles.
func (r *Registry) RolesBySession() (map[string][]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	held := map[string][]string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		held[holder] = append(held[holder], role)
	}
	r.holdersMu.RUnlock()
	for _, roles := range held {
		sort.Strings(roles)
	}
	return held, nil
}

// RoleViewErr is nil while the role claim view would answer a read, and otherwise the wrapped
// ErrRoleViewNotReady that RolesHeldBy refuses that read with, naming the same reason: it is the
// one test of the view's state, so a report that the view answers and a read it refuses can never
// disagree. It makes no bucket call, so a caller reports the view without waiting on the bus.
//
// /healthz reports it as a degraded dependency, and it is deliberately not part of Ping or
// WatchErr: those feed the listener's self-health monitor, which restarts the listener over a
// terminal failure that survives its recovery attempts, and no rebuild completes a read the
// bucket keeps timing out on. What does complete it is the role claim watcher's own re-read,
// which needs nothing from the listener.
func (r *Registry) RoleViewErr() error {
	if r.roleWatcher == nil {
		return fmt.Errorf("%w: no watcher follows the role claims", ErrRoleViewNotReady)
	}
	if err := r.roleWatcher.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	if err := r.roleWatcher.Servable(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	return nil
}

// ErrRoleHeld reports a soft claim that found the role held by another session.
// The holder is carried on the error so callers can report who has it.
type ErrRoleHeld struct {
	Role   string
	Holder string
}

func (e *ErrRoleHeld) Error() string {
	return fmt.Sprintf("role %s is held by %s", e.Role, e.Holder)
}

// ErrRoleClaimRevisionMismatch reports a conditional claim the role bucket did not hold the
// revision for: the caller named the revision it had read the role's claim at
// (SetRoleWithPrevious's ifClaimRevision) and the bucket holds another one, either when the claim
// was read or by the time its conditional write reached the bucket. Expected is the revision the
// caller named, Actual the revision the bucket holds as this call last read it - zero on either
// side names no claim record at all - and Holder the session the stored claim names, empty when
// there is none. No claim record was written: the role is left exactly as the claim that stands
// left it, and a claim that lost the write's compare-and-swap rolled its own topic back unless
// the winning claim names the claimant itself.
type ErrRoleClaimRevisionMismatch struct {
	Role     string
	Expected uint64
	Actual   uint64
	Holder   string
}

func (e *ErrRoleClaimRevisionMismatch) Error() string {
	return fmt.Sprintf("role %s's claim is at revision %d, not the %d this claim required", e.Role, e.Actual, e.Expected)
}

// RoleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.BoundedOn). The
// claim it returns carries the entry's revision (RoleClaim.Revision), which is what a conditional
// claim compares against and what GET /v1/roles/<role> reports as claim_revision, and the time
// the entry was stored (RoleClaim.Created), which is what a claim's grace is weighed by; a role
// with no claim record returns a zero claim without an error, so a zero revision names a role no
// record claims, and a JSON record that is not a claim returns ErrMalformedRoleClaim. A value
// that is not JSON is read as an earlier build's bare holder id (DecodeRoleClaim), not as
// malformed.
//
// Those two are the whole of what the entry the read answered says beyond the claim itself,
// which is why this hands back no entry: a caller testing whether the bucket holds a record for
// the role reads claim.Revision == 0, and one writing conditionally over the record it read names
// claim.Revision, as releaseReadRoleClaim already does.
func (r *Registry) RoleClaim(ctx context.Context, role string) (RoleClaim, error) {
	roles := r.roles()
	entry, answered, err := bus.BoundedOn(ctx, roles, func() (nats.KeyValueEntry, error) { return roles.Get(role) })
	if !answered {
		return RoleClaim{}, fmt.Errorf("read role %s's claim: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil
	}
	if err != nil {
		return RoleClaim{}, err
	}
	return decodeReadRoleClaim(entry)
}

// roleClaimToWrite is RoleClaim for the claim SetRoleWithPrevious is about to write over the
// record it reads, with the revision that write is conditional on. The key is read with
// bus.KeyValue.GetLatest, so that revision is the claim's own where a claim record stands, the
// revision of the delete or purge marker that ended the last claim where none does, and 0 where
// the key holds no entry at all, and the conditional write lands only over that very entry. The
// claim hides the marker, as RoleClaim does: its zero Revision is what a caller reading the role
// compares (IfClaimRevision).
func (r *Registry) roleClaimToWrite(ctx context.Context, role string) (RoleClaim, uint64, error) {
	roles := r.roles()
	read, answered, err := bus.BoundedOn(ctx, roles, func() (bus.LatestEntry, error) { return roles.GetLatest(role) })
	if !answered {
		return RoleClaim{}, 0, fmt.Errorf("read role %s's claim: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, read.Revision, nil
	}
	if err != nil {
		return RoleClaim{}, 0, err
	}
	claim, err := decodeReadRoleClaim(read.Entry)
	return claim, read.Revision, err
}

// decodeReadRoleClaim is the claim entry holds, stamped with the entry's revision and the time it
// was stored (RoleClaim.Revision, RoleClaim.Created).
func decodeReadRoleClaim(entry nats.KeyValueEntry) (RoleClaim, error) {
	claim, err := DecodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, err
	}
	claim.Revision, claim.Created = entry.Revision(), entry.Created()
	return claim, nil
}

// RoleHolder returns the authoritative session ID for role, read with no deadline of its own. Only
// tests call it, to read what a claim left behind; the listener reads RoleClaim within a deadline,
// its routes and its role lane through the one resolution (roles.ResolveHolder, internal/roles)
// and its kept lane directly. An unclaimed role returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}
