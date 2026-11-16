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
type Enroller interface {
	Enroll(ctx context.Context, e PodEnrollment) (string, error)
	Revoke(ctx context.Context, id string) error
}

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
// written, again and again on r.After, until ctx ends. The daemon's is agentsecrets.Client's own
// KeepPodSession, which logs each write naming the pod and the session.
type Registrar interface {
	KeepPodSession(ctx context.Context, r PodSession) error
}

// PodSession is one pod's registration to trade under one session name. Token answers the pod's
// own projected token for the broker's audience, which is what names the pod to the broker; it is
// asked once per write, and the first time inside the decision that starts the registration.
// After is the machine's clock, and Bound the longest one write may take.
type PodSession struct {
	PodUID, Session string
	Token           func(ctx context.Context) (string, error)
	After           func(d time.Duration, f func()) (stop func() bool)
	Bound           time.Duration
}

// podRegistration is the registration a machine keeps written: the process incarnation and the
// session it is for, and the cancel that ends its renewal.
type podRegistration struct {
	incarnation, session string
	cancel               context.CancelFunc
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
// the broker's wake address. It never fails a transition: a refusal is logged with its code, a
// transient one retried on the next Alive observation, a permanent one on the shim's next hello.
// An expired token is dropped and the key kept, and the next attempt reads a fresh one
// (freshToken). Once enrolled, the id is handed to the shim over the claim's connection, once per
// connection. The pod's session registration comes first (ensureRegistered): every path that may
// enroll may register, and the registration's first write presents the hello's token before the
// enrollment drops it.
func (m *Machine) ensureEnrolled(ctx context.Context) error {
	m.ensureRegistered()
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

// ensureRegistered registers the claim's current pod to trade under the claim's session at the
// broker, and keeps that registration written while the process lives (Deps.Registrar), once the
// machine holds what the registration names: a process in a pod (the Sandbox locator's pod uid),
// the session, and the shim's hello for this incarnation, whose connection reads the pod's token.
// The session is the one the claim records — the agent's registration, or the session a relaunch
// resumes, so a relaunched pod's uid is registered for the session it resumes at its shim's first
// hello, before the resumed agent registers again — or else the one the runtime chose for this
// launch (runtime.LaunchSessionNamer): a Claude Code role's, registered at its shim's hello,
// before its agent registers. An Oh My Pi role's fresh session is known at its registration, and
// is registered there before the registration is answered (register). The first write is made
// in this decision, bounded by the RPC timeout, and its refusal changes nothing the machine
// decides; the renewal goes on on the machine's clock until the process is let go (letGo,
// stopRegistration) or the claim moves to another session. Each pod and session is registered
// once per incarnation: a later hello or observation of the same one starts nothing.
func (m *Machine) ensureRegistered() {
	loc := m.claim.Locator
	if m.deps.Registrar == nil || loc == nil || loc.Runtime != runtime.RuntimeSandbox || loc.Sandbox == nil {
		return
	}
	if m.identity == nil || m.identity.incarnation != loc.Incarnation {
		return
	}
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
	// The renewal's context is the machine's own, not the decision's: the request that brought
	// the session in ending does not end it, the process's let-go or the machine's end does. The
	// first write's outcome is the registrar's to log; nothing here decides on it.
	keeping, cancel := context.WithCancel(m.ctx)
	m.registration = &podRegistration{incarnation: loc.Incarnation, session: session, cancel: cancel}
	_ = m.deps.Registrar.KeepPodSession(keeping, PodSession{
		PodUID: loc.Sandbox.PodUID, Session: session, Bound: m.deps.Timeouts.RPC,
		Token: m.podToken(m.identity.PodToken),
		After: func(d time.Duration, f func()) func() bool { return m.deps.Clock.AfterFunc(d, f).Stop },
	})
}

// stopRegistration ends the renewal of the registration the machine keeps, if any. The broker
// honours its last write for a day, past which an exchange naming the pod and session is refused.
func (m *Machine) stopRegistration() {
	if m.registration != nil {
		m.registration.cancel()
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
