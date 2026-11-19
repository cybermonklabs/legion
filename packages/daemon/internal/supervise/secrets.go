package supervise

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Enrollment is one pod generation's enrollment with the secrets broker.
type Enrollment struct {
	ID          string
	Incarnation string
}

// PodEnrollment is what the machine hands the broker for one process generation: the pod UID the
// runtime recorded as the locator's incarnation, the identity the shim's hello carried, and the
// agent's session id. The real pod UID stays the verified runtime identity, while Slot is a
// daemon-derived role/generation label for several private enrollments in one pod; tmux leaves
// Slot empty. It carries no issue: each secret's owner and tier tags pick a request's approver at
// request time, never at enrollment.
type PodEnrollment struct {
	PodUID, Slot, Thumbprint, PodToken, Session string
}

// AgentSecretsIdentity is the pod identity a shim's hello2 carried (stream.AgentSecretsIdentity).
type AgentSecretsIdentity struct {
	Thumbprint, PodToken string
}

// Enroller is the secrets broker's enrollment routes as the machine uses them. Nil in Deps means
// this deployment enrolls no process: no identity is kept, nothing is enrolled or revoked.
//
// Carried is the broker's answer to a release (Machine.ReleaseNames): every enrollment this
// daemon's login launched, of one of sessionIDs, that is live or still carries a grant or a
// request the next enrollment of its session id would take (envoy's POST /v1/enrollments/carried);
// none is an empty answer.
type Enroller interface {
	Enroll(ctx context.Context, e PodEnrollment) (string, error)
	Revoke(ctx context.Context, id string) error
	Carried(ctx context.Context, sessionIDs []string) ([]CarriedEnrollment, error)
}

// CarriedEnrollment is one enrollment the secrets broker still carries something for (Enroller.
// Carried): its session id and its own id, whether it is CarriedLive or CarriedDormant, and when
// its lease ends (a live one, unless its process renews it) or its carry ends (a dormant one).
type CarriedEnrollment struct {
	SessionID, EnrollmentID, State string
	EndsAt                         time.Time
}

const (
	// CarriedLive is an enrollment no end has reached yet: its process may still run, and it can be
	// granted something until it ends.
	CarriedLive = "live"
	// CarriedDormant is an ended enrollment whose end left its grants or requests for the next
	// enrollment of its session id, until its carry ends.
	CarriedDormant = "dormant"
)

// PermanentError is a broker refusal a retry cannot change (a broker 4xx other than an expired
// token): an Enroll refusal drops the identity and waits for the shim's next hello; a Revoke
// refusal is not retried.
type PermanentError interface{ Permanent() bool }

// ExpiredTokenError is an Enroll failure the broker gave because the pod token presented had
// expired (POD_TOKEN_EXPIRED): the key the hello carried stands, its token does not. The machine
// drops the token, keeps the key, and enrolls that key with the token freshToken reads next.
type ExpiredTokenError interface{ TokenExpired() bool }

// Registrar is the secrets broker's pod-session registration as the machine uses it (Deps.Registrar):
// KeepPodSession writes r's registration now, answering that first write's outcome, and keeps it
// written, again and again on r.After, until stop is called or ctx ends. stop returns only once a
// write in flight has been answered, without cancelling it. The daemon's is agentsecrets.Client's
// own KeepPodSession, which logs each write naming the pod and the session.
type Registrar interface {
	KeepPodSession(ctx context.Context, r PodSession) (stop func(), err error)
}

// PodSession is one pod's registration to trade under one session name, bound to the role's own
// key: Thumbprint is the RFC 7638 thumbprint the role's shim hello named, which every write sends.
// Generation is the launch generation of the process it registers (SandboxLocator.Generation, the
// claim's generation, which every launch raises and stores before it starts the process): it rises
// with each process of the claim, a process never sends two, and a daemon restart reads it back
// from the claim, so the broker orders two processes' writes of one (pod, session) by it. Latest
// answers the claim's current generation, read when an answer comes back at a later one: an
// answered generation above it was reached by no process of this claim, so the registration is
// one this daemon never made. It reads the view each decision publishes, never the machine's lock,
// since a renewal's answer comes back on the clock's goroutine while a decision may hold that lock
// waiting on it (stopRegistration). Token answers the pod's own projected token for the broker's
// audience, which is what names the pod to the broker; it is asked once per write, and the first
// time inside the decision that starts the registration. After is the machine's clock, and Bound
// the longest one write may take.
type PodSession struct {
	PodUID, Session, Thumbprint string
	Generation                  uint64
	Latest                      func() uint64
	Token                       func(ctx context.Context) (string, error)
	After                       func(d time.Duration, f func()) (stop func() bool)
	Bound                       time.Duration
}

// podRegistration is the registration a machine keeps written: the process incarnation and the
// session it is for, and the stop that ends its renewal. Its key is the incarnation's pinned one
// (helloKey), so an incarnation and a session name it whole.
type podRegistration struct {
	incarnation, session string
	stop                 func()
}

// registrationKey is an incarnation and a session ensureRegistered was refused for
// (Machine.registrationRefused).
type registrationKey struct{ incarnation, session string }

// helloKey is the thumbprint the first hello of one incarnation named.
type helloKey struct {
	incarnation, thumbprint string
}

// pinHelloKey pins the thumbprint a hello of incarnation names, if it is the first of that
// incarnation to name one, and logs a later one naming another key, which then changes nothing.
// An honest shim caches its thumbprint, so the same process never names two keys; only the
// holder of the claim's boot token can send a hello at all.
func (m *Machine) pinHelloKey(incarnation, thumbprint string) {
	switch {
	case thumbprint == "":
	case m.helloKey.incarnation != incarnation || m.helloKey.thumbprint == "":
		m.helloKey = helloKey{incarnation: incarnation, thumbprint: thumbprint}
	case thumbprint != m.helloKey.thumbprint:
		m.log.Warn("supervise: agent-secrets: a later hello of the same process names another key; its session's registration keeps the key the first hello named",
			"incarnation", incarnation, "first", m.helloKey.thumbprint, "named", thumbprint)
	}
}

const (
	// revokeAttempts and revokeRetryDelay bound the revocation of a let-go process's enrollment;
	// past them the enrollment's lease, which nothing renews once the pod is gone, ends it.
	revokeAttempts   = 3
	revokeRetryDelay = 10 * time.Second
	// podTokenSkew is how far the broker's clock, which judges a pod token's expiry, may run
	// ahead of the daemon's before a token the daemon judged unexpired is refused anyway.
	podTokenSkew = time.Minute
)

// heldIdentity is the latest hello2 identity for the claim's current incarnation, kept in memory
// until the enrollment lands: a resume enrolls on the hello itself, a fresh launch once the agent
// registers and the session id is known. Its token is the one the hello carried until freshToken
// replaces it with one the shim read later, and empty once the broker found it expired, until
// freshToken reads a fresh one.
type heldIdentity struct {
	incarnation string
	AgentSecretsIdentity
}

// ensureEnrolled enrolls the claim's current process once the machine holds both halves of its
// identity: the hello's thumbprint and token for this incarnation, and the agent's session id,
// the broker's wake address. It enrolls only a session every name of which the claim holds
// (holdRecorded): a registration holds its names before it records them, so this asks nothing
// more then, and the names a resumed claim enrolls under on its hello were held by an earlier
// registration, or recorded before the daemon held sessions, when the migration that made the
// hold gave each to the claim recording it, or to no claim where two did. It never fails a
// transition: a refusal is logged with its code, a transient one retried on the next Alive
// observation, a permanent one on the shim's next hello. An expired token is dropped and the key
// kept, and the next attempt reads a fresh one (freshToken). Once enrolled, the id is handed to
// the shim over the claim's connection, once per connection. The pod's session registration
// comes first (ensureRegistered), and holds its session the same way before its first write:
// every path that may enroll may register, and the registration's first write presents the
// hello's token before the enrollment drops it.
func (m *Machine) ensureEnrolled(ctx context.Context) error {
	m.ensureRegistered(ctx)
	if m.deps.Secrets == nil || m.claim.Locator == nil {
		return nil
	}
	incarnation := m.claim.Locator.Incarnation
	podUID, slot := incarnation, ""
	if loc := m.claim.Locator; loc.Runtime == runtime.RuntimeSandbox && loc.Sandbox != nil {
		podUID = loc.Sandbox.PodUID
		slot = fmt.Sprintf("%s-g%d", m.claim.Role, loc.Sandbox.Generation)
	}
	if m.claim.Enrollment == nil || m.claim.Enrollment.Incarnation != incarnation {
		if m.identity == nil || m.identity.incarnation != incarnation || m.claim.Session == "" {
			return nil
		}
		if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
			var refusal claim.Refusal
			if errors.As(err, &refusal) {
				// The refusal says why: another claim holds a name of the session, it is held for no
				// claim, or no row can hold it (holdRecorded). The line names none of them, so an
				// operator reads the error rather than a cause the line would guess (review of #4355).
				m.log.Error("supervise: agent-secrets: not enrolling a session this claim may not take; waiting for the shim's next hello", "incarnation", incarnation, "error", err)
				m.identity = nil
			} else {
				m.log.Warn("supervise: agent-secrets: the store did not hold the session; retrying on the next observation", "incarnation", incarnation, "error", err)
			}
			return nil
		}
		if !m.freshToken(ctx, incarnation) {
			return nil
		}
		enrolling, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
		id, err := m.deps.Secrets.Enroll(enrolling, PodEnrollment{
			PodUID: podUID, Slot: slot, Thumbprint: m.identity.Thumbprint, PodToken: m.identity.PodToken, Session: m.claim.Session,
		})
		cancel()
		if err != nil {
			var expired ExpiredTokenError
			var permanent PermanentError
			switch {
			case errors.As(err, &expired) && expired.TokenExpired():
				m.log.Warn("supervise: agent-secrets: the broker found the pod token expired; keeping the key for a fresh token", "incarnation", incarnation, "error", err)
				m.identity.PodToken = ""
			case errors.As(err, &permanent) && permanent.Permanent():
				m.log.Error("supervise: agent-secrets: enrollment refused; waiting for the shim's next hello", "incarnation", incarnation, "error", err)
				m.identity = nil
			default:
				m.log.Warn("supervise: agent-secrets: enrollment failed; retrying on the next observation", "incarnation", incarnation, "error", err)
			}
			return nil
		}
		m.claim.Enrollment = &Enrollment{ID: id, Incarnation: incarnation}
		m.identity.PodToken = "" // held no longer than the enrollment needs it
		m.enrollmentSent = false
		m.log.Info("supervise: agent-secrets: enrolled", "incarnation", incarnation, "enrollment", id, "thumbprint", m.identity.Thumbprint)
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	// An enrollment this claim already holds is kept only while the claim holds its session: one a
	// daemon from before the hold enrolled under a name another claim holds, or no claim does, is
	// revoked at the broker, since its process, which the boot could not suspend, keeps that
	// enrollment's file and renewer across connections and would go on reading what it adopted
	// (review rounds 12 to 14 of #4220, revokeUnheld). The hold is asked once per session when held
	// (Machine.heldNames), so a claim holding its session pays nothing here. A store that does not
	// answer revokes nothing and hands nothing over, and the next observation asks again.
	if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
		var refusal claim.Refusal
		if !errors.As(err, &refusal) {
			m.log.Warn("supervise: agent-secrets: the store did not hold the session; handing the enrollment over at the next observation",
				"enrollment", m.claim.Enrollment.ID, "error", err)
			return nil
		}
		return m.revokeUnheld(ctx, err)
	}
	if m.enrollmentSent {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return nil
	}
	sending, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	err := conn.AgentSecretsEnrollment(sending, m.claim.Enrollment.ID)
	cancel()
	if err != nil {
		m.log.Warn("supervise: agent-secrets: the shim did not take the enrollment; retrying on the next observation", "enrollment", m.claim.Enrollment.ID, "error", err)
		return nil
	}
	m.enrollmentSent = true
	return nil
}

// revokeUnheld revokes at the broker, while its process lives, the enrollment of a claim that does
// not hold its session (ensureEnrolled; refused is the hold's refusal), and records the claim
// enrolled no longer only once the broker has answered. revoke's retries end after revokeAttempts
// and leave the rest to the lease, which a stopped pod no longer renews; this pod's shim renews it
// (its startRenewer), so a revocation that fails here keeps the enrollment recorded and handed to
// no connection, and the next observation tries it again (review round 14 of #4220). The claim
// then has no enrollment, so the next decision asks the hold again before any new one. Once the
// claim holds its session, its next hello or observation enrolls the same process again and
// hands it the new enrollment, but its shim runs one renewer, which ended at the broker's refusal
// of the revoked enrollment, so the new one lapses at its lease; the claim's next launch enrolls
// with a renewer (review rounds 15 and 16 of #4220). A broker that refuses the revocation for good
// is not asked again, since it would refuse again: the enrollment is recorded no longer, the
// refusal is logged at Error naming it, and the process renews it until it stops.
func (m *Machine) revokeUnheld(ctx context.Context, refused error) error {
	e := *m.claim.Enrollment
	revoking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	err := m.deps.Secrets.Revoke(revoking, e.ID)
	cancel()
	var permanent PermanentError
	switch {
	case err == nil:
		m.log.Warn("supervise: agent-secrets: revoked the enrollment of a claim that does not hold its session; once the claim holds it, its next hello or observation enrolls the process again, and its next launch enrolls it with a renewer",
			"enrollment", e.ID, "incarnation", e.Incarnation, "session", m.claim.Session, "error", refused)
	case errors.As(err, &permanent) && permanent.Permanent():
		m.log.Error("supervise: agent-secrets: the broker refused to revoke the enrollment of a claim that does not hold its session; it is recorded no longer, and the process renews it until it stops",
			"enrollment", e.ID, "incarnation", e.Incarnation, "session", m.claim.Session, "error", refused, "revocation", err)
	default:
		m.log.Warn("supervise: agent-secrets: the revocation of the enrollment of a claim that does not hold its session failed; trying again at the next observation",
			"enrollment", e.ID, "incarnation", e.Incarnation, "session", m.claim.Session, "error", err)
		return nil
	}
	ClearEnrollment(&m.claim)
	m.enrollmentSent = false
	return m.persist(ctx)
}

// ensureRegistered registers the claim's current pod to trade under the claim's session at the
// broker, bound to the role's own key, and keeps that registration written while the process lives
// (Deps.Registrar), once the machine holds what the registration names: a process in a pod (the
// Sandbox locator's pod uid), the session, and the shim's hello for this incarnation, which names
// the thumbprint of the role's key and whose connection reads the pod's token. Nothing is
// registered before that hello, nor for a hello that names no thumbprint: a registration without
// one would leave the session to every role of the pod. The session is the one the claim records —
// the agent's registration, or the session a relaunch resumes, so a relaunched pod's uid is
// registered for the session it resumes at its shim's first hello, with that hello's thumbprint,
// before the resumed agent registers again — or else the one the runtime chose for this launch
// (runtime.LaunchSessionNamer): a Claude Code role's, registered at its shim's hello, before its
// agent registers. An Oh My Pi role's fresh session is known at its registration, which its agent
// makes after its shim's hello, and is registered there before the registration is answered
// (register). The first write is made in this decision, bounded by the RPC timeout, and its refusal
// changes nothing the machine decides; the renewal goes on on the machine's clock until the process
// is let go (letGo, stopRegistration) or the claim moves to another session or key. Each pod,
// session and key is registered once per incarnation: a later hello or observation of the same one
// starts nothing.
//
// The session is held to the claim before the first write, as an enrollment's is (holdRecorded,
// plan E's t-e-54): every name the claim's record gives it, or, for the id
// the runtime chose for this launch, that id. The broker answers an exchange naming the pod and
// the session with the role's own token, so a registration of another claim's session is that
// session's trade. A hold refused — another claim holds a name, no claim does, or the claim is at
// MaxHeldNamesPerClaim — registers nothing and is logged at Error; a store that does not answer
// registers nothing and is logged at Warn, and the next decision that reaches here asks again. Any
// registration the machine kept for another session is stopped first either way, since the claim
// has moved off that session. A name of the claim's own session it holds already asks nothing
// (Machine.heldNames), so a registration after the agent's own registration costs no store call.
func (m *Machine) ensureRegistered(ctx context.Context) {
	loc := m.claim.Locator
	if m.deps.Registrar == nil || loc == nil || loc.Runtime != runtime.RuntimeSandbox || loc.Sandbox == nil {
		return
	}
	if m.identity == nil || m.identity.incarnation != loc.Incarnation {
		return
	}
	if m.helloKey.incarnation != loc.Incarnation || m.helloKey.thumbprint == "" {
		return
	}
	thumbprint := m.helloKey.thumbprint
	session := m.claim.Session
	if session == "" {
		if namer, ok := m.deps.Runtime.(runtime.LaunchSessionNamer); ok {
			session, _ = namer.LaunchSession(*loc)
		}
	}
	if session == "" {
		return
	}
	if r := m.registration; r != nil && r.incarnation == loc.Incarnation && r.session == session {
		return
	}
	m.stopRegistration()
	file := ""
	if session == m.claim.Session {
		file = m.claim.SessionFile
	}
	if err := m.holdRecorded(ctx, session, file); err != nil {
		var refusal claim.Refusal
		switch {
		case !errors.As(err, &refusal):
			m.log.Warn("supervise: agent-secrets: the store did not hold the session; registering it at the next decision",
				"incarnation", loc.Incarnation, "session", session, "error", err)
		case m.registrationRefused != registrationKey{loc.Incarnation, session}:
			// Once per incarnation and session (review round 14 of #4220, note 42): a contained
			// claim keeps its hello identity, so every observation reaches here and is refused
			// again, and each such line says nothing the first did not.
			m.registrationRefused = registrationKey{loc.Incarnation, session}
			m.log.Error("supervise: agent-secrets: not registering a session this claim may not take for its pod to trade",
				"incarnation", loc.Incarnation, "session", session, "error", err)
		}
		return
	}
	m.registrationRefused = registrationKey{}
	// The renewal's context is the machine's own, not the decision's: the request that brought
	// the session in ending does not end it, the process's let-go (its stop) or the machine's end
	// does. The first write's outcome is the registrar's to log; nothing here decides on it.
	stop, _ := m.deps.Registrar.KeepPodSession(m.ctx, PodSession{
		PodUID: loc.Sandbox.PodUID, Session: session, Thumbprint: thumbprint, Generation: loc.Sandbox.Generation, Bound: m.deps.Timeouts.RPC,
		Latest: m.latestGeneration(loc.Sandbox.Generation),
		Token:  m.podToken(m.identity.PodToken),
		After:  func(d time.Duration, f func()) func() bool { return m.deps.Clock.AfterFunc(d, f).Stop },
	})
	m.registration = &podRegistration{incarnation: loc.Incarnation, session: session, stop: stop}
}

// latestGeneration is a registration's reader of the claim's current generation: the generation of
// the claim in the view the last decision published, or the registering process's own generation
// when that view still predates it (a first write answered inside the decision that launched the
// process), since the claim is never behind a process it launched. It takes no lock.
func (m *Machine) latestGeneration(own uint64) func() uint64 {
	return func() uint64 {
		return max(m.view.Load().Claim.Generation, own)
	}
}

// stopRegistration ends the renewal of the registration the machine keeps, if any, and returns
// once its write in flight, if any, has been answered: that write is not cancelled, so a new
// incarnation's first write, which follows this in the same decision, reaches the broker after the
// old incarnation's last and the row ends bound to the new key. The wait is bounded by the write's
// own bound, the RPC timeout. The broker honours the last write for a day, past which an exchange
// naming the pod and session is refused.
func (m *Machine) stopRegistration() {
	if m.registration != nil {
		m.registration.stop()
		m.registration = nil
	}
}

// podToken is a registration's source of the pod's token: held, the token the hello carried while
// it is far enough from its expiry for the write to present it (as freshToken judges it), and the
// first time only; then, at each write, the token the shim reads from its mount at that moment,
// over the claim's connection. It reads nothing of the machine's own state, so a renewal on the
// clock's goroutine calls it without the machine's lock.
func (m *Machine) podToken(held string) func(ctx context.Context) (string, error) {
	token, conns, clock, skew := m.claim.Token, m.deps.Conns, m.deps.Clock, m.deps.Timeouts.RPC+podTokenSkew
	return func(ctx context.Context) (string, error) {
		if held != "" {
			first := held
			held = ""
			if expires, ok := podTokenExpiry(first); ok && expires.After(clock.Now().Add(skew)) {
				return first, nil
			}
		}
		conn, ok := conns.Conn(token)
		if !ok {
			return "", errors.New("the claim's shim is not connected to read its projected token")
		}
		return conn.AgentSecretsToken(ctx)
	}
}

// freshToken reports whether the held token can be presented, first asking the shim to read it
// again when it cannot. The shim reads its projected token at each hello, the token lives at most
// an hour from when the kubelet wrote it, and an enrollment can wait much longer after the hello
// (the daemon's token login failing and retrying, a broker outage); the broker refuses an expired token as
// POD_TOKEN_EXPIRED. So a token that could expire before the broker verifies it (within the
// Enroll call's RPC bound plus podTokenSkew), or whose expiry cannot be read, is replaced by the
// one the shim reads from its mount now. That includes a token the broker already refused as
// expired, which ensureEnrolled drops, so a broker whose clock runs further ahead than
// podTokenSkew costs one refusal, not a wait for the next hello. A shim that cannot read it again,
// one built before the request among them, drops the identity to wait for its next hello, as a
// refused enrollment does; a lost request is asked again on the next observation, and with no
// connection the next hello carries a token read then.
func (m *Machine) freshToken(ctx context.Context, incarnation string) bool {
	if expires, ok := podTokenExpiry(m.identity.PodToken); ok && expires.After(m.deps.Clock.Now().Add(m.deps.Timeouts.RPC+podTokenSkew)) {
		return true
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return false
	}
	reading, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	token, err := conn.AgentSecretsToken(reading)
	cancel()
	switch {
	case errors.Is(err, runtime.ErrTokenRefused):
		m.log.Error("supervise: agent-secrets: the shim did not read its projected token again; waiting for its next hello", "incarnation", incarnation, "error", err)
		m.identity = nil
		return false
	case err != nil:
		m.log.Warn("supervise: agent-secrets: the shim did not answer for its projected token; retrying on the next observation", "incarnation", incarnation, "error", err)
		return false
	}
	m.identity.PodToken = token
	return true
}

// podTokenExpiry is a JWT's exp claim, read without verifying anything: the broker verifies the
// token, and the machine only judges whether presenting it could still succeed. false for a token
// whose expiry cannot be read.
func podTokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// revoke ends the let-go process's enrollment at the broker on a goroutine of the machine's. Each
// later attempt is a clock callback; Wait covers only the first call.
func (m *Machine) revoke(e Enrollment) {
	m.goroutines++
	go func() {
		defer func() {
			m.mu.Lock()
			m.goroutines--
			m.idle.Broadcast()
			m.mu.Unlock()
		}()
		RevokeEnrollment(m.ctx, m.deps, m.log, e)
	}()
}

// RevokeEnrollment ends a stale enrollment, retrying transient failures on deps.Clock. It is shared
// by a machine that lets a process go and boot repair that finds an enrollment whose process was
// never recorded.
func RevokeEnrollment(ctx context.Context, deps Deps, log *slog.Logger, e Enrollment) {
	if deps.Secrets == nil {
		log.Warn("supervise: agent-secrets: enrollment recorded with no broker configured; its lease ends it", "enrollment", e.ID)
		return
	}
	revokeEnrollmentAttempt(ctx, deps, log, e, 1)
}

func revokeEnrollmentAttempt(ctx context.Context, deps Deps, log *slog.Logger, e Enrollment, attempt int) {
	revoking, cancel := context.WithTimeout(ctx, deps.Timeouts.RPC)
	err := deps.Secrets.Revoke(revoking, e.ID)
	cancel()
	if err == nil {
		log.Info("supervise: agent-secrets: enrollment revoked", "enrollment", e.ID, "incarnation", e.Incarnation, "attempt", attempt)
		return
	}
	var permanent PermanentError
	if errors.As(err, &permanent) && permanent.Permanent() {
		log.Warn("supervise: agent-secrets: enrollment revocation refused; its lease ends it once its pod stops renewing", "enrollment", e.ID, "error", err)
		return
	}
	log.Warn("supervise: agent-secrets: revocation failed", "enrollment", e.ID, "attempt", attempt, "error", err)
	if attempt >= revokeAttempts || ctx.Err() != nil {
		log.Error("supervise: agent-secrets: enrollment not revoked after every attempt; the lease ends it", "enrollment", e.ID, "attempts", attempt)
		return
	}
	deps.Clock.AfterFunc(revokeRetryDelay, func() { revokeEnrollmentAttempt(ctx, deps, log, e, attempt+1) })
}
