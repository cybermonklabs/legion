// Package roles resolves a role's holder: the session its claim names, read as the listener's
// role routes, its role lane and Dispatch's outbox all read it, so the three cannot drift.
//
// No resolution here takes anything from a holder. It reads the role's claim and the holder's
// session record and says what it found; it never deletes a claim, and it never writes to the
// role bucket at all. A holder that stopped answering keeps its role until the role-claim reaper
// finds it gone (session.Gone), which is the one place a claim is taken away (dispatch://INFRA-392,
// R4): a lookup, a publish and a role-lane delivery attempt leave the claim and its claimed_at
// exactly as they were, so a stalled session that comes back still holds the role it had.
//
// Every resolution reads the role's claim record from its ClaimStore, the role bucket itself, and
// never from a watched copy of that bucket: a copy can lack a claim the bucket holds, or keep one
// the bucket dropped, and the answer each caller acts on is the record's.
//
// A holder is judged by the state its own last registration puts it in (session.StateOf, on the
// windows the lookup runs with), never by how long a bucket keeps its record and never by a
// staleness compiled into this package.
package roles

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/roleholder"
)

// ClaimStore is where a role's claim is read, with the privacy of the project a claim is read
// under and the deployment's privacy mode: cmd/listener's claim store over store.Registry, its
// role lane's wrapper of it, and Buckets in Dispatch. Privacy is answered by the claim reader
// itself, so no resolution can read a claim without also being able to read privacy, and there is
// no separate privacy reader for a caller to forget or pass as nil.
type ClaimStore interface {
	// RoleClaim returns the claim of role under project, read within ctx at
	// contracts.RoleClaimKey(project, role), or a zero claim without an error when the role is
	// unclaimed there.
	RoleClaim(ctx context.Context, project, role string) (store.RoleClaim, error)
	// Private reports whether project is private, read within ctx: the listener reads the privacy
	// source afresh at each call, Dispatch its own committed rows. An error is an answer that is
	// not known, which holds what it decides.
	Private(ctx context.Context, project string) (bool, error)
	// Mode is the deployment's privacy mode, which the store's own deployment configured.
	Mode() Mode
}

// Mode is a deployment's privacy mode (AllPublic, Required): the listener's from
// ENVOY_PROJECT_PRIVACY_REQUIRED (internal/config), Dispatch's from
// DISPATCH_PROJECT_PRIVACY_REQUIRED (cmd/dispatch's outboxDeps).
type Mode uint8

const (
	// AllPublic resolves as before either mode existed: every project is public to the holder
	// check, so a holder's stored projects are never read.
	AllPublic Mode = iota
	// Required takes a holder only when its stored projects (session.SessionEntry.Projects) see
	// the project the message is of (Sees).
	Required
)

// SessionLookup reads a role holder's session record and judges it: the listener's session
// registry (cmd/listener's roleHolderSessions, which adds Holder to session.SessionRegistry) and
// Dispatch's Buckets.
type SessionLookup interface {
	// Holder returns sessionID's record, read within ctx, whatever state that record is in:
	// judging it is EntryState's. nats.ErrKeyNotFound says no record stands here at all; any
	// other error is a read that did not answer, on which nothing is decided about the holder.
	Holder(ctx context.Context, sessionID string) (session.SessionEntry, error)
	// EntryState is the state entry's last registration puts its session in (session.StateOf on
	// the windows this lookup runs with): session.SessionRegistry.EntryState in the listener,
	// Windows.EntryState in Buckets.
	EntryState(entry session.SessionEntry) session.State
	// LastSeen is the last registration this lookup can still name for a session whose record has
	// gone, and 0 when nothing here can date it.
	LastSeen(sessionID string) int64
}

// Windows are the session windows a holder's record is judged by and the clock it is judged at:
// what session.StateOf takes. A zero field is the package's own (session.LiveWindow,
// session.GoneHorizon, time.Now), so a lookup given no windows judges as a listener started with
// neither ENVOY_SESSION_LIVE_WINDOW nor ENVOY_SESSION_GONE_HORIZON does.
type Windows struct {
	Live    time.Duration
	Horizon time.Duration
	Now     func() time.Time
}

// EntryState is entry's state on these windows, which is what a SessionLookup that carries
// Windows judges a holder by (Buckets).
func (w Windows) EntryState(entry session.SessionEntry) session.State {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	return session.StateOf(entry, now(), cmp.Or(w.Live, session.LiveWindow), cmp.Or(w.Horizon, session.GoneHorizon))
}

// State is what a role's claim came to when its holder was resolved. The strings are what the
// listener's answers carry: the 404's reason and the publish route's recorded outcome. The two
// no-holder reasons are the shared ones the Legion daemon reads (roleholder), not package-local
// spellings, so the listener's 404, its refusal exception, the daemon's notify.Publish and its
// exception consumer all read one list. It is the listener's wire type (listenerwire.RoleState).
type State = listenerwire.RoleState

const (
	// Live: the role is claimed and its holder's record reads live.
	Live State = "live"
	// Unclaimed: the role has no claim.
	Unclaimed State = roleholder.Unclaimed
	// Lapsed: the claim's holder is not answering - unresponsive, or gone - and the claim stands
	// untouched. Resolution.HolderState says which of the two it is.
	Lapsed State = roleholder.HolderLapsed
)

// Resolution is a resolved role holder. Nothing was taken from that holder to reach it.
type Resolution struct {
	// State is what the claim came to.
	State State
	// Holder is the session the claim named, on Live and Lapsed alike.
	Holder string
	// HolderState is the state the holder's own record puts it in (SessionLookup.EntryState):
	// session.Live on Live, and session.Unresponsive or session.Gone on Lapsed. A holder the
	// lookup holds no record of is session.Gone, since there is no registration to date it from.
	HolderState session.State
	// Entry is the holder's record when one was read, and the zero entry when none stands.
	Entry session.SessionEntry
	// LastSeen is when the holder last registered: its record's own time, or what the lookup
	// still remembers of a session whose record has gone (SessionLookup.LastSeen). It is 0 when
	// nothing can date the holder - no record and no memory of one - which is not the same as a
	// holder whose registration is past the gone horizon: a session bucket's maximum age can be
	// shorter than that horizon, so a caller that gives a dead holder's role up reads this before
	// it counts a holder dead.
	LastSeen int64
	// ClaimRevision is the claim store's revision of the claim record this resolution read, which
	// GET /v1/roles/<role> reports as claim_revision and a conditional claim names as its
	// precondition. A claim record stands behind every resolution that names a holder, since no
	// resolution here takes one away, so it is the revision that record was read at on Live and
	// Lapsed alike, and zero for Unclaimed.
	ClaimRevision uint64
	// HolderClientInstanceID is the client instance of the process the claim this resolution
	// read names as its holder (store.RoleClaim.ClientInstanceID), which GET /v1/roles/<role>
	// reports as client_instance_id beside the holder's session id: it is how a client sees
	// that a role is held under its own session id by another process. It is the claim's own
	// field on Live and Lapsed alike, since no resolution here releases a claim, and empty
	// where the claim names no process - a claim an earlier build wrote, or one whose claimant
	// stated none - and for Unclaimed, where there is no claim at all.
	HolderClientInstanceID string
	// HolderHandedBack is the hand-back mark the claim this resolution read carries
	// (store.RoleClaim.HandedBack), which GET /v1/roles/<role> reports as handed_back on both
	// its answers: the holder handed this role back through a release that could not free the
	// claim, so no registration renews it any more and the role claim reaper frees the role
	// one live window and up to one reap interval later. False where no such release reached
	// the claim, and for Unclaimed, where there is no claim at all.
	HolderHandedBack bool
}

// Step names the read a resolution failed at.
type Step uint8

const (
	// ClaimRead: the role's claim could not be read, or the resolution's context ended before the
	// read began.
	ClaimRead Step = iota
	// SessionRead: the holder's session record could not be read (an error other than
	// nats.ErrKeyNotFound), in the all-public mode.
	SessionRead
	// PrivacyRead: the privacy of the project the role was asked under could not be read, so
	// neither the fallback to the role's claim under no project nor the holder check can be
	// decided. The message is held, never sent to the claim under no project.
	PrivacyRead
	// HolderRead: in the required mode, the holder's session record, whose stored projects the
	// holder check reads, could not be read.
	HolderRead
)

// ReadError is a resolution that failed at Step, about Holder for SessionRead. Its message is
// what an HTTP route names in its answer, so it names the read rather than the resolution; Err is
// the read's own error, which the role lane logs under a line of its own per step. It unwraps to
// Err, so a caller still tells store.ErrMalformedRoleClaim and a refused or oversized read by the
// read's own error.
type ReadError struct {
	Step   Step
	Holder string
	Err    error
}

func (e *ReadError) Error() string {
	switch e.Step {
	case SessionRead, HolderRead:
		return "read role holder session: " + e.Err.Error()
	case PrivacyRead:
		return "read the project's privacy: " + e.Err.Error()
	}
	return e.Err.Error()
}

func (e *ReadError) Unwrap() error { return e.Err }

// PatternString is the shape of a role token: the pattern POST /v1/roles/set accepts, so it is
// every token a claim can name. A role message under a token it refuses can reach no holder and
// is kept for none (Valid).
const PatternString = `^[a-z0-9][a-z0-9_-]*$`

var pattern = regexp.MustCompile(PatternString)

// Valid reports whether role is a token a claim can name (PatternString). The listener's role
// routes and Dispatch's outbox both ask it, so the two cannot drift over which role a message
// may be kept for.
func Valid(role string) bool {
	return pattern.MatchString(role)
}

// ForwardFenceAge is how old the resolution that chose a role message's holder may be when the
// message's forward to that holder starts: a forward from the listener's role lane, its publish
// route or Dispatch's outbox whose resolution is this old or older resolves the holder again
// first. It is the listener's keep window (one hour, cmd/listener roleKeepWindow) less one
// interval of its kept lane's sweep (five minutes, roleWakeInterval): a takeover's copy of the
// previous holder's role messages reads them for as long as its record lives, and a forward
// fenced so is stored inside the hour the copy reads from the takeover on.
const ForwardFenceAge = 55 * time.Minute

// ResolutionTooOld reports whether a forward whose holder was resolved at resolvedAt must resolve
// the holder again before its publish starts at now: the resolution is ForwardFenceAge old or
// older. A zero resolvedAt - a forward that read the claim itself just before, such as the kept
// lane's walk - is never too old. The role lane, the publish route and Dispatch's outbox each ask
// it, so the fence is one rule.
func ResolutionTooOld(resolvedAt, now time.Time) bool {
	return !resolvedAt.IsZero() && now.Sub(resolvedAt) >= ForwardFenceAge
}

// ValidClaimKey reports whether key is the key a claim can be keyed by (store.RoleClaimKeyOf):
// a role token a claim can name (Valid), alone or, for a free-form role under a project, after
// that project's key and a dot (contracts.RoleClaimKey, read back by contracts.ParseRoleClaimKey).
// The listener's kept lane asks it of the claim key a give-up path holds, so a role message under
// a key no claim can carry is kept for none.
func ValidClaimKey(key string) bool {
	project, role := contracts.ParseRoleClaimKey(key)
	return Valid(role) && contracts.RoleClaimKey(project, role) == key
}

// ResolveHolder resolves the holder of role for a message of project within ctx. It reads the
// claim of role under project (ResolveClaim), and, when that claim is unclaimed, project is not
// NoProject and project is not the role's own (contracts.RoleProject), it asks claims.Private of
// project: only an answer of false reads the role's claim under NoProject, the role's claim
// every holder of a free-form role made before claims named their project. An error is a
// *ReadError at PrivacyRead, and the message is held as for any read that did not answer - never
// sent to the holder under NoProject on an unknown answer. A Legion role token never reaches the
// fallback, since its project is always contracts.RoleProject(role).
//
// In the required mode (claims.Mode()) a holder, the one claimed under project or the one the
// fallback finds, is taken only when its stored projects see project under the same privacy
// answer the relay decides by (Sees); otherwise the resolution answers Unclaimed, so the
// message goes to the kept lane and reaches a holder that can see it when one claims. A holder
// whose record cannot be read is a *ReadError at HolderRead. In the all-public mode the holder's
// projects are never read and it resolves as before the modes existed.
//
// This is the one place the fallback is written: every reader that delivers a role's message
// (the listener's publish route, its role lane and kept lanes, Dispatch's outbox) resolves through
// it, and a read that answers a role's holder (GET /v1/roles/<role>, the claim route) reads the
// claim under its own project alone (ResolveClaim).
func ResolveHolder(ctx context.Context, claims ClaimStore, sessions SessionLookup, project, role string) (Resolution, error) {
	resolved, err := ResolveClaim(ctx, claims, sessions, project, role)
	if err != nil || resolved.State != Unclaimed || project == contracts.NoProject || project == contracts.RoleProject(role) {
		return resolved, err
	}
	private, err := claims.Private(ctx, project)
	if err != nil {
		return Resolution{}, &ReadError{Step: PrivacyRead, Err: fmt.Errorf("read project %s's privacy for role %s's holder: %w", project, role, err)}
	}
	if private {
		return resolved, nil
	}
	fallback, err := ResolveClaim(ctx, claims, sessions, contracts.NoProject, role)
	if err != nil || fallback.State == Unclaimed || claims.Mode() != Required {
		return fallback, err
	}
	// The holder under NoProject is held to project, the message's, and not to NoProject: the
	// claim it holds says nothing of the projects it sees.
	return holderSeesProject(ctx, claims, fallback, project)
}

// ResolveClaim resolves the holder of role's claim under project alone, within ctx, with no
// fallback to the claim under NoProject: what GET /v1/roles/<project>.<role> answers. It reads the
// claim (Unclaimed when there is none), then that holder's session record through sessions, and
// judges the record by the state it puts its session in (SessionLookup.EntryState): Live when it
// reads live, and Lapsed with that state otherwise - a holder past the live window, one past the
// gone horizon, and one with no record at all. The claim is left exactly as it is on every one of
// them, so a stalled holder's role and its claimed_at outlive any number of resolutions. In the
// required mode its holder is held to project as ResolveHolder holds one (Sees).
//
// A read that fails returns a *ReadError naming its step, with the claim's holder in the
// Resolution when one was read. Every read waits no longer than ctx, and no resolution starts
// once ctx has ended: that is ctx's error, wrapped to name the role, at ClaimRead.
//
// One resolution is one claim read: there is no release for a second read to see past.
//
// It is the one judgement of a holder every role route makes - the role lookup, the role publish,
// the role lane, the kept lane's handover and Dispatch's outbox - so a holder inside the live
// window is live on each of them, whatever its relay subscriber does, and none of them probes its
// relay subject. Only the send route probes a session's relay subject, and its answer goes in the
// send's own state, never in a role answer's holder_state.
func ResolveClaim(ctx context.Context, claims ClaimStore, sessions SessionLookup, project, role string) (Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Resolution{}, &ReadError{Step: ClaimRead, Err: fmt.Errorf("resolve role %s's holder: %w", role, err)}
	}
	claim, err := claims.RoleClaim(ctx, project, role)
	if err != nil {
		return Resolution{}, &ReadError{Step: ClaimRead, Err: err}
	}
	holder := claim.HolderSessionID
	if holder == "" {
		return Resolution{State: Unclaimed}, nil
	}
	entry, err := sessions.Holder(ctx, holder)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		step := SessionRead
		if claims.Mode() == Required {
			step = HolderRead
		}
		return Resolution{Holder: holder}, &ReadError{Step: step, Holder: holder, Err: err}
	}
	// No record to read is no moment to measure the holder from: session.Gone, as session.StateOf
	// judges an undated record, and dated by whatever the lookup still remembers. In the required
	// mode a record that does not name its key (session.SessionEntry.BoundTo: one the server
	// stored at the holder's key, a copy of another session's record, or an earlier build's) is no
	// record: its holder holds no role, and never the scope of the record it copies.
	if err == nil && claims.Mode() == Required && !entry.BoundTo(holder) {
		err = nats.ErrKeyNotFound
	}
	state := session.Gone
	if err == nil {
		state = sessions.EntryState(entry)
	} else {
		entry = session.SessionEntry{}
	}
	lastSeen := entry.UpdatedAt
	if lastSeen == 0 {
		lastSeen = sessions.LastSeen(holder)
	}
	// The claim record stands whatever the holder's state: nothing here releases one. So the
	// revision read is the revision a conditional claim can be made against, on Lapsed as on
	// Live, which is how a scheduler takes a role from a holder that never came back, the
	// process the claim names is the process holding the role on either state, and a claim its
	// holder handed back carries that mark on either state too, since nothing here renews or
	// frees one.
	resolved := Resolution{
		State:                  Lapsed,
		Holder:                 holder,
		HolderState:            state,
		Entry:                  entry,
		LastSeen:               lastSeen,
		ClaimRevision:          claim.Revision,
		HolderClientInstanceID: claim.ClientInstanceID,
		HolderHandedBack:       claim.HandedBack,
	}
	if state == session.Live {
		resolved.State = Live
	}
	if claims.Mode() != Required {
		return resolved, nil
	}
	return holderSeesProject(ctx, claims, resolved, project)
}

// holderSeesProject is resolved when its holder's stored scope sees project (Sees over the
// entry's Projects and Restricted), and Unclaimed otherwise, so the message is kept for a holder
// that can see it. An unknown privacy answer is a *ReadError at PrivacyRead.
func holderSeesProject(ctx context.Context, claims ClaimStore, resolved Resolution, project string) (Resolution, error) {
	projects, restricted := resolved.Entry.RequiredScope(resolved.Holder)
	sees, err := Sees(ctx, claims.Private, projects, restricted, project)
	if err != nil {
		return Resolution{Holder: resolved.Holder}, &ReadError{Step: PrivacyRead, Holder: resolved.Holder,
			Err: fmt.Errorf("read project %s's privacy for holder %s: %w", project, resolved.Holder, err)}
	}
	if !sees {
		return Resolution{State: Unclaimed}, nil
	}
	return resolved, nil
}

// Sees is the required privacy mode's rule for whether a session or a caller sees project, the
// rule the relay decides a frame by: a message of no project, and a GitHub subject's
// (contracts.AnyToken), is everyone's; a project its token lists (listed, the registering
// token's list) it sees in every state; and a session of a full member (restricted false: a
// person Dispatch answered a full member, or a token naming no person) also sees every project
// a fresh read says is public. A restricted member's session sees its list alone, in every state,
// and so does a session whose record or token carries no restricted claim. private reads
// project's privacy, only when the answer turns on it.
func Sees(ctx context.Context, private func(context.Context, string) (bool, error), listed []string, restricted bool, project string) (bool, error) {
	if project == contracts.NoProject || project == contracts.AnyToken || slices.Contains(listed, project) {
		return true, nil
	}
	if restricted {
		return false, nil
	}
	isPrivate, err := private(ctx, project)
	if err != nil {
		return false, err
	}
	return !isPrivate, nil
}
