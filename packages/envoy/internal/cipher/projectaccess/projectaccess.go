// Package projectaccess is the broker's one client of Dispatch's project-access routes: whether a
// project is private, whether a person may see it, and what kind of member a person is. Dispatch
// decides all three — the owner, the access list, a group's members, the full and restricted
// members — and the broker asks rather than keeps a copy, so a decision here is the one Dispatch
// would make for that person's own read.
//
// Every answer is plain data. The rule every caller of this package keeps is that no answer is
// asked for inside a database transaction, inside policy.Set.Evaluate, or while a row lock or an
// advisory lock is held: a caller resolves what it needs first and hands the answers on, so a
// Dispatch that is slow to answer holds no lock and no pooled connection of the broker's. The
// token exchange asks before it opens its transaction (internal/cipher/oauth, exchangeGrant), an
// enrollment before its own (enroll.Service.checkMember), and a secret request and a release
// before theirs (requests.Machine.restrictedOperator).
//
// Answers are cached CacheTTL per project and person and per person, aged from before each
// question was sent, which bounds how long a person removed from a project's access list goes on
// passing a check here; Visible and States ask again for an answer that aged past it while a list
// was read, a caller holding answers across further reads ages them again where it decides
// (Stale), a question asked under Fresh skips the cache, and a slower answer never replaces one to
// a question asked later. A failure to reach Dispatch is never an answer: each caller decides what
// an outage means for its own check. A project's failure is not kept; a person's member-kind read
// that failed is the answer to that person's next reads for failureTTL (2 s), and the person's
// reads in flight together share one call (MemberKind), so an outage costs each person one call
// at a time, not one per caller. A read under Fresh takes neither: it asks Dispatch itself.
package projectaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// The member kinds Dispatch answers for a person.
const (
	// MemberFull is a person who signed in under the deployment's sign-in domain.
	MemberFull = "full"
	// MemberRestricted is a person an access list admitted from outside that domain, who sees
	// only the projects that name them.
	MemberRestricted = "restricted"
	// MemberNone is a person Dispatch holds no row for.
	MemberNone = "none"
)

// CacheTTL is how long one answer is kept. It is the bound on how long a person removed from a
// project's access list goes on passing a check made here, beside the life of a token minted
// before the removal (BROKER_ACCESS_TOKEN_SECONDS).
const CacheTTL = 30 * time.Second

// RelayedReadBound is how long a member-kind read made for a route Dispatch's server relays may
// run: Dispatch waits 5 seconds for the broker's whole answer (internal/dispatch/cipher's
// defaultTimeout), and one call to Dispatch may take requestTimeout, so a read with no bound of its
// own could outlast the call it is made for, and the route would answer nobody rather than the
// rows it lists without the read, the refusal, or the revoke that goes ahead. A caller bounds its
// read with it (context.WithTimeout), and takes a read it cut short as one Dispatch did not answer.
const RelayedReadBound = 2 * time.Second

// requestTimeout bounds one call to Dispatch, the dial included.
const requestTimeout = 10 * time.Second

// failureTTL is how long a member-kind read that failed is the answer to the next reads of the
// same person, counted from when the failure came back: while Dispatch does not answer, those
// reads fail at once rather than each waiting up to requestTimeout for a call of its own. It is
// short beside CacheTTL because a failure is no answer: the read is asked again once it passes.
const failureTTL = 2 * time.Second

// maxAnswerBytes caps the answer read from Dispatch. An answer is a handful of short fields and a
// list of repositories, so anything larger is not Dispatch's and is refused rather than held.
const maxAnswerBytes = 64 << 10

// maxCached bounds how many answers are kept at once, so a caller naming project keys at random
// cannot grow the cache without bound. An answer that does not fit is not kept, which costs that
// question a call to Dispatch and nothing else.
const maxCached = 4096

// The paths of Dispatch's two routes.
const (
	projectPath = "/api/v1/access/projects/"
	personPath  = "/api/v1/access/person"
	// projectNotFoundCode is the code Dispatch's project route answers a project it does not
	// hold with, beside its 404; a 404 with any other code is no answer about a project.
	projectNotFoundCode = "PROJECT_NOT_FOUND"
)

var (
	// ErrProjectNotFound is a project Dispatch does not hold.
	ErrProjectNotFound = errors.New("projectaccess: dispatch holds no such project")
	// ErrNotVisible is a credential naming a project its person may not see, or one Dispatch
	// does not hold: the two are refused in the same words, so a refusal never says whether a
	// project exists.
	ErrNotVisible = errors.New("projectaccess: names a project its person cannot see")
	// ErrListRequired is a restricted member's credential that names no project: such a person
	// sees only the projects that name them, so their credential names which.
	ErrListRequired = errors.New("projectaccess: a restricted member's credential names its projects")
	// ErrUnknownMemberKind is Dispatch answering a member kind that is none of MemberFull,
	// MemberRestricted and MemberNone. The broker cannot tell whether such a person is held to the
	// projects that name them, so no caller reads it as any of the three, and none mints or
	// enrolls through it.
	ErrUnknownMemberKind = errors.New("projectaccess: dispatch answered a member kind this broker does not know")
	// ErrUnknownVisibility is Dispatch answering a project's visibility as anything but
	// VisibilityPublic or VisibilityPrivate, an answer with no visibility at all included. The
	// broker cannot tell whether such a project is private, so no caller reads it as either: it
	// is never kept, and each caller fails as it does when Dispatch cannot be asked.
	ErrUnknownVisibility = errors.New("projectaccess: dispatch answered a project visibility this broker does not know")
	// ErrUnchecked is ListAllowed's answer for an empty list whose person's member kind Dispatch
	// could not be asked for: the list is allowed, as it is today, but unchecked, so the caller
	// that goes on to mint or enroll logs that it did so. It wraps the failure it stands for.
	ErrUnchecked = errors.New("projectaccess: dispatch could not be asked, so the empty list is allowed unchecked")
	// errUnavailable is a status that says Dispatch is not answering now (Unanswered): a 502, 503
	// or 504 whose body names no code of Dispatch's, a gateway's or a load balancer's page in front
	// of it, or one that names a code Dispatch answers for its own outage (outageCodes); and a 500
	// whose body names no code or names internalCode.
	errUnavailable = errors.New("projectaccess: dispatch is unavailable")
)

// outageCodes are the codes Dispatch answers its access routes with, beside a 503, for an outage
// of its own that says nothing of the person or the project asked about: OIDC_ISSUER_UNAVAILABLE
// while it cannot reach the issuer of the broker's own account token (internal/dispatch/api,
// serviceTokenActor), and DATABASE_UNAVAILABLE, which only a Dispatch with no database configured
// answers (readyPool, begin). A 502, 503 or 504 whose body names any other code, such as
// FEATURE_OFF or GITHUB_APP_UNCONFIGURED, is Dispatch answering, and that answer lasts until
// somebody fixes it.
var outageCodes = []string{"DATABASE_UNAVAILABLE", "OIDC_ISSUER_UNAVAILABLE"}

// internalCode is the code of Dispatch's 500 for a handler that failed (internal/dispatch/api,
// writeHandlerError), which is how its access routes answer while its database fails: a query
// that fails at once, in a failover or a restart, is that 500, not a 503. A 500 naming it, or
// naming no code at all (a page in front of Dispatch), is Dispatch not answering now; a 500 naming
// any other code of Dispatch's, such as IDENTITY_ERROR, is an answer that lasts until somebody
// fixes it. The cost is that a Dispatch bug that answers 500 INTERNAL reads as an outage while it
// lasts, which lets only what an outage lets through: a revoke by the person a grant names as its
// approver, which takes access away.
const internalCode = "INTERNAL"

// Unanswered reports whether err, a failed read of Dispatch, is Dispatch not answering: a failure
// that clears by itself once Dispatch answers again. That is the read's deadline passing, its
// caller's own bound (RelayedReadBound) included; the transport failing before Dispatch's whole
// answer arrived (dispatchread.ErrNoAnswer); a 502, 503 or 504 whose body names no code of
// Dispatch's, or one of the codes Dispatch answers for its own outage (outageCodes); and a 500
// whose body names no code or internalCode, the 500 Dispatch answers while its database fails.
// Every other failure is an answer, one that lasts until somebody fixes it: Dispatch refusing the
// broker's own account (dispatchread.ErrBrokerRefused), answering a member kind this broker does
// not know (ErrUnknownMemberKind), a 404 that answers nothing of the person or the project, a 500
// naming any other code of Dispatch's, a 502, 503 or 504 naming any other code of Dispatch's, any
// other status, or a body that does not decode; a certificate that does not verify and a name that
// does not resolve (dispatchread.Reader.Get); and so is a read its caller gave up on
// (context.Canceled), which says nothing of Dispatch. A caller that lets something through only
// while Dispatch does not answer goes ahead on Unanswered alone
// (requests.Machine.RevokeByApprover, for the grant's named approver).
func Unanswered(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, dispatchread.ErrNoAnswer) || errors.Is(err, errUnavailable)
}

// NoApprover is refusal, a caller's own refusal of a person who may not act as an approver there
// (record.ErrNotApprover, say), for a person Dispatch calls a restricted member (MemberRestricted),
// in words that say so first: "a person Dispatch calls a restricted member is no approver of " and
// then of, which names what and says what such a person does instead, in the verb of what they did.
// It is refusal, so whatever answers refusal answers it alike, while its words are these alone:
// refusal's own would speak of whose the row is, and contradict a reader the row does name.
func NoApprover(refusal error, of string) error {
	return &restrictedRefusal{why: "a person Dispatch calls a restricted member is no approver of " + of, refusal: refusal}
}

// restrictedRefusal is NoApprover's refusal.
type restrictedRefusal struct {
	why     string
	refusal error
}

func (e *restrictedRefusal) Error() string { return e.why }

// Unwrap is the caller's own refusal.
func (e *restrictedRefusal) Unwrap() error { return e.refusal }

// NotVisibleError is Visible's refusal: the first project of a list its person may not see, or
// that Dispatch does not hold. It is ErrNotVisible (errors.Is), and its Project is what a caller
// names in its own refusal; its text is the same whichever of the two it was.
type NotVisibleError struct{ Project string }

func (e *NotVisibleError) Error() string { return ErrNotVisible.Error() + ": " + e.Project }

// Unwrap is ErrNotVisible.
func (e *NotVisibleError) Unwrap() error { return ErrNotVisible }

// UncheckableCode is the code every broker point answers 503 with when it refuses what it would
// do for a person because it could not learn whether Dispatch calls that person a restricted
// member (MemberKindUncheckable): an enrollment, a secret request and a release alike.
const UncheckableCode = "MEMBER_KIND_UNCHECKABLE"

// MemberKindUncheckable is the refusal of something done for Person — an enrollment, a secret
// request, a release — because Dispatch could not be asked what kind of member Person is: a
// check that cannot decide refuses rather than answer as if the person were a full member. Err is
// the failure, which a caller logs. Error names the person and why in uncheckableReason's fixed
// words and never Err, which can carry Dispatch's address, so a caller answers it as it is.
type MemberKindUncheckable struct {
	Person string
	Err    error
}

func (e *MemberKindUncheckable) Error() string {
	return "whether Dispatch calls " + e.Person + " a restricted member cannot be read: " + uncheckableReason(e.Err)
}

// Unwrap is the failure, so errors.Is reads dispatchread.ErrBrokerRefused and ErrUnknownMemberKind
// through it.
func (e *MemberKindUncheckable) Unwrap() error { return e.Err }

// CallerEnded reports whether the read failed because the caller gave up (its context was
// cancelled: a client that closed its connection) rather than because Dispatch could not be
// asked. The refusal stands either way; only the log line differs, since such a read says nothing
// about Dispatch. The shared call never fails this way: it runs on none of its readers' contexts.
func (e *MemberKindUncheckable) CallerEnded() bool { return errors.Is(e.Err, context.Canceled) }

// uncheckableReason is why a person's member kind could not be read (err, a MemberKind failure),
// in words a refused caller may read: Dispatch refusing the broker's own account, which the
// deployment fixes; Dispatch answering a kind this broker does not know, which a newer broker
// fixes; the broker not asking Dispatch, while Dispatch does not answer (Unanswered) or the
// caller went away; and otherwise Dispatch answering what the broker cannot read, an answer that
// lasts until somebody fixes it.
func uncheckableReason(err error) string {
	switch {
	case errors.Is(err, dispatchread.ErrBrokerRefused):
		return "Dispatch refused the broker's own account"
	case errors.Is(err, ErrUnknownMemberKind):
		return "Dispatch answered a member kind this broker does not know"
	case Unanswered(err) || errors.Is(err, context.Canceled):
		return "the broker could not ask Dispatch"
	}
	return "Dispatch answered what the broker cannot read"
}

// Restricted reports whether Dispatch calls person a restricted member (MemberRestricted), for a
// caller about to enroll, decide or release something for that person. MemberFull and MemberNone
// are not; any other kind, and any MemberKind failure, is a *MemberKindUncheckable naming person,
// which the caller returns as it is: a check that cannot decide refuses, whatever Access answered.
func Restricted(ctx context.Context, a Access, person string) (bool, error) {
	kind, err := a.MemberKind(ctx, person)
	if err != nil {
		return false, &MemberKindUncheckable{Person: person, Err: err}
	}
	restricted, err := restrictedKind(kind, person)
	if err != nil {
		return false, &MemberKindUncheckable{Person: person, Err: err}
	}
	return restricted, nil
}

// restrictedKind is the one reading of a member kind Dispatch answered for person: MemberRestricted
// is a restricted member, MemberFull and MemberNone are not, and any other kind, an empty one
// included, is ErrUnknownMemberKind naming it. Restricted and ListAllowed both decide by it.
func restrictedKind(kind, person string) (bool, error) {
	switch kind {
	case MemberFull, MemberNone:
		return false, nil
	case MemberRestricted:
		return true, nil
	}
	return false, fmt.Errorf("%w: %q for %s", ErrUnknownMemberKind, kind, person)
}

// ProjectNotFoundError is Access's answer for a project Dispatch does not hold: it is
// ErrProjectNotFound (errors.Is), and it carries when the question it answers was asked, so a
// caller whose decision rests on a project's absence ages that answer as it ages any other
// (Stale). A source that keeps no clock answers a zero AskedAt, or ErrProjectNotFound alone.
type ProjectNotFoundError struct {
	Project string
	AskedAt time.Time
}

func (e *ProjectNotFoundError) Error() string { return ErrProjectNotFound.Error() + ": " + e.Project }

// Unwrap is ErrProjectNotFound.
func (e *ProjectNotFoundError) Unwrap() error { return ErrProjectNotFound }

// Answer is Dispatch's answer for one project and one person.
type Answer struct {
	// Project is the project's key.
	Project string `json:"project"`
	// Visibility is VisibilityPublic or VisibilityPrivate: *Client answers ErrUnknownVisibility
	// for any other, and never an Answer.
	Visibility string `json:"visibility"`
	// Owner is the owner's login.
	Owner string `json:"owner"`
	// Person is the person asked about, empty for a question that named none.
	Person string `json:"person"`
	// MemberKind is the person's MemberFull, MemberRestricted or MemberNone.
	MemberKind string `json:"member_kind"`
	// Visible is whether the person may see the project, or, for a question naming no person,
	// whether it is public.
	Visible bool `json:"visible"`
	// Repositories is every "owner/name" repository Dispatch maps to the project, renames
	// included, nil when the answer carries none.
	Repositories []string `json:"repositories"`
	// SlackChannels is every Slack channel id the project's owner named, sorted, and empty for a
	// project that names none (dispatch://INFRA-3862's #t-a-31). SlackChannelsPresent says
	// whether the answer carried the key at all: a Dispatch from before that task carries none,
	// and the broker fails closed on it rather than reading it as "no limit" (requests'
	// AirlockSecret.Limited), so the two deploy in either order safely. Decoded through a
	// pointer, which is the one way a missing key and an empty list read differently.
	SlackChannels []string `json:"-"`
	// SlackChannelsPresent is whether the answer carried slack_channels.
	SlackChannelsPresent bool `json:"-"`
	// AskedAt is when the question this answers was asked, read before it was sent, so an
	// answer's age is never less than it truly is. *Client always sets it; a zero AskedAt is an
	// answer from a source that keeps no clock, which Stale takes as fresh.
	AskedAt time.Time `json:"-"`
	// Missing is States' entry for a project Dispatch does not hold: Project and AskedAt are set
	// beside it, and nothing else. Access never answers one; it answers a *ProjectNotFoundError.
	Missing bool `json:"-"`
}

// The two visibilities Dispatch answers a project with.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Private reports whether the project is private.
func (a Answer) Private() bool { return a.Visibility == VisibilityPrivate }

// Public reports whether the project is public. A project is public only when Dispatch says so:
// a caller reads no other visibility, an empty one included, as public.
func (a Answer) Public() bool { return a.Visibility == VisibilityPublic }

// Access is every question a broker reader asks Dispatch; *Client answers all three.
type Access interface {
	// Access is Dispatch's answer for project and person; ErrProjectNotFound for a project it
	// does not hold.
	Access(ctx context.Context, project, person string) (Answer, error)
	// MemberKind is the person's member kind.
	MemberKind(ctx context.Context, person string) (string, error)
	// States answers one Answer per key, for the caller that must resolve every answer before it
	// opens a transaction: Missing for a project Dispatch does not hold. Each answer was asked
	// less than CacheTTL before it returns. It is one call per key against the cache, not a batch
	// route.
	States(ctx context.Context, projects []string) (map[string]Answer, error)
}

// Client asks Dispatch's routes at one base URL, presenting the broker's own account token.
type Client struct {
	read dispatchread.Reader
	now  func() time.Time

	mu      sync.Mutex
	answers map[accessKey]cachedAnswer
	kinds   map[string]cachedKind
	// failures holds each person's member-kind read that failed, for failureTTL from when it
	// came back, and flights each person's read in flight, which a read of the same person
	// joins rather than making a call of its own.
	failures map[string]failedKind
	flights  map[string]*kindFlight
	// joined, when set, runs each time a member-kind read joins one already in flight for the
	// same person: a test's signal that the read waits on that call rather than making its own.
	joined func()
}

type accessKey struct{ project, person string }

type cachedAnswer struct {
	answer          Answer
	projectNotFound bool
	at              time.Time
}

type cachedKind struct {
	kind string
	at   time.Time
}

type failedKind struct {
	err error
	at  time.Time
}

// kindFlight is one call for a person's member kind, which every read of that person made while
// it is in flight waits on: kind and err are its outcome, set before done closes.
type kindFlight struct {
	done chan struct{}
	kind string
	err  error
}

// New is a client of Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL), presenting account
// (BROKER_PROJECT_ACCESS_TOKEN_FILE's reader) on every call, through the broker's one read of
// Dispatch (dispatchread). A nil httpClient is http.DefaultClient; each call is bounded by its own
// 10 s whatever the client.
func New(baseURL string, account ciphertoken.Account, httpClient *http.Client) *Client {
	return &Client{
		read:     dispatchread.New(baseURL, account, httpClient, requestTimeout, maxAnswerBytes),
		now:      time.Now,
		answers:  map[accessKey]cachedAnswer{},
		kinds:    map[string]cachedKind{},
		failures: map[string]failedKind{},
		flights:  map[string]*kindFlight{},
	}
}

// freshKey marks a context whose questions are asked of Dispatch whatever the cache holds (Fresh).
type freshKey struct{}

// Fresh is ctx for questions Dispatch must answer now rather than the cache: Access and
// MemberKind under it ask Dispatch even while the cache holds a young answer, and keep what it
// answers as any other answer is kept. It is for a check that must see a change younger than
// CacheTTL, as the model judge's approval does when it checks the project rule again before it
// is written (requests.Machine.ApplyJudgeDecision): the run's own reads filled the cache well
// inside CacheTTL, so a check that read them back could not see a project that turned private
// while the model was asked.
func Fresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshKey{}, true)
}

// fresh reports whether ctx is one Fresh made, or derived from one.
func fresh(ctx context.Context) bool {
	marked, _ := ctx.Value(freshKey{}).(bool)
	return marked
}

// askedNowKey marks a context whose member-kind reads take no failure an earlier read kept and
// join no call already in flight (AskedNow), though they still take a young answer from the
// cache: a read whose failure lets something through, as ListAllowed's empty list does and an
// approver's revoke does (requests.Machine.RevokeByApprover). It is a context marker rather than a
// method, so an Access that hands the context on, a wrapper included, keeps the rule.
type askedNowKey struct{}

// AskedNow is ctx for a member-kind read that is asked of Dispatch itself whenever the cache
// holds no young answer (askedNowKey).
func AskedNow(ctx context.Context) context.Context {
	return context.WithValue(ctx, askedNowKey{}, true)
}

// isAskedNow reports whether ctx is one AskedNow or Fresh made, or derived from one.
func isAskedNow(ctx context.Context) bool {
	marked, _ := ctx.Value(askedNowKey{}).(bool)
	return marked || fresh(ctx)
}

// Access is Dispatch's answer for project and person, from the cache while it is younger than
// CacheTTL, unless ctx is one Fresh made. A person is asked about by their lowercase login; an
// empty person asks whether the project is public. An answer is aged from before its question was
// sent, so a slow answer is kept no longer than CacheTTL from the moment Dispatch could last have
// changed it.
func (c *Client) Access(ctx context.Context, project, person string) (Answer, error) {
	key := accessKey{project: project, person: record.CanonicalLogin(person)}
	if cached, ok := c.cachedAnswer(key); ok && !fresh(ctx) {
		if cached.projectNotFound {
			return Answer{}, &ProjectNotFoundError{Project: project, AskedAt: cached.at}
		}
		return cached.answer, nil
	}
	query := url.Values{}
	if key.person != "" {
		query.Set("person", key.person)
	}
	asked := c.now()
	// The answer's own fields, and slack_channels beside them through a pointer, in one decode: a
	// Dispatch from before dispatch://INFRA-3862's #t-a-31 carries no such key, and "no key" must
	// read differently from "an empty list", which a []string alone could not say. Answer's own
	// SlackChannels is json:"-", so the key reaches this field and no other.
	var wire struct {
		Answer
		SlackChannels *[]string `json:"slack_channels"`
	}
	status, err := c.get(ctx, projectPath+url.PathEscape(project), query, &wire)
	switch {
	case err != nil:
		return Answer{}, err
	case status == http.StatusNotFound:
		c.keepAnswer(key, cachedAnswer{projectNotFound: true, at: asked})
		return Answer{}, &ProjectNotFoundError{Project: project, AskedAt: asked}
	}
	answer := wire.Answer
	// A visibility that is neither of the two is no answer: it is never kept, and no caller can
	// read it as public, which is what a missing field would otherwise decode to beside Private.
	if !answer.Public() && !answer.Private() {
		return Answer{}, fmt.Errorf("%w: %q for %s", ErrUnknownVisibility, answer.Visibility, project)
	}
	if wire.SlackChannels != nil {
		answer.SlackChannels, answer.SlackChannelsPresent = *wire.SlackChannels, true
	}
	answer.AskedAt = asked
	c.keepAnswer(key, cachedAnswer{answer: answer, at: asked})
	return answer, nil
}

// MemberKind is the person's member kind, from the cache while it is younger than CacheTTL, unless
// ctx is one Fresh made: one of MemberFull, MemberRestricted and MemberNone. Any other answer, an
// empty one included, is ErrUnknownMemberKind naming it, never kept as an answer; like any other
// failure of a call to Dispatch, it is kept failureTTL as that person's failure.
//
// Reads of one person in flight together share one call to Dispatch, which runs on none of their
// contexts, so a reader that gives up ends only its own wait; and a read that failed is the
// answer to that person's reads for failureTTL from when it came back. So while Dispatch does not
// answer, a person's reads wait on one call at a time, each for no longer than that call's
// requestTimeout, rather than each making a call of its own. A read under Fresh, or one under
// AskedNow, joins no call already in flight, which may have been asked before the change it must
// see, and takes no kept failure: it asks Dispatch itself (memberKindAskedNow).
func (c *Client) MemberKind(ctx context.Context, person string) (string, error) {
	if isAskedNow(ctx) {
		return c.memberKindAskedNow(ctx, person)
	}
	person, err := askedPerson(person)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if cached, ok := c.kinds[person]; ok && c.now().Sub(cached.at) < CacheTTL {
		c.mu.Unlock()
		return cached.kind, nil
	}
	if failed, ok := c.failures[person]; ok && c.now().Sub(failed.at) < failureTTL {
		c.mu.Unlock()
		return "", failed.err
	}
	flight, joined := c.flights[person]
	if !joined {
		flight = &kindFlight{done: make(chan struct{})}
		c.flights[person] = flight
		go c.askKind(context.WithoutCancel(ctx), person, flight)
	}
	hook := c.joined
	c.mu.Unlock()
	if joined && hook != nil {
		hook()
	}
	select {
	case <-flight.done:
		return flight.kind, flight.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// memberKindAskedNow is MemberKind with no failure an earlier read kept and no call already in
// flight: an answer from the cache younger than CacheTTL unless ctx is one Fresh made, else a call
// of its own on ctx, settled as any other. A read under Fresh asks it, and so does a read whose
// failure lets something through (ListAllowed), so that what it lets through lasts no longer than
// Dispatch does not answer. The call runs on the caller's own context, so a call that failed
// because that context ended, its deadline passed or its caller cancelled, keeps no failure for the
// person's next reads: that ending says nothing of Dispatch.
func (c *Client) memberKindAskedNow(ctx context.Context, person string) (string, error) {
	person, err := askedPerson(person)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if cached, ok := c.kinds[person]; ok && !fresh(ctx) && c.now().Sub(cached.at) < CacheTTL {
		c.mu.Unlock()
		return cached.kind, nil
	}
	c.mu.Unlock()
	asked := c.now()
	kind, err := c.readKind(ctx, person)
	if err != nil && ctx.Err() != nil {
		return "", err
	}
	c.mu.Lock()
	c.settleKindLocked(person, asked, kind, err)
	c.mu.Unlock()
	return kind, err
}

// askedPerson is person as a member-kind read asks Dispatch about them: their canonical login,
// and a refusal when it names no one.
func askedPerson(person string) (string, error) {
	person = record.CanonicalLogin(person)
	if person == "" {
		return "", errors.New("projectaccess: a member kind is asked of a person, and none was named")
	}
	return person, nil
}

// askKind makes flight's one call for person's member kind and settles it (settleKindLocked). The
// flight leaves c.flights under the same lock, so a read that finds no flight finds what this one
// settled.
func (c *Client) askKind(ctx context.Context, person string, flight *kindFlight) {
	asked := c.now()
	kind, err := c.readKind(ctx, person)
	c.mu.Lock()
	delete(c.flights, person)
	c.settleKindLocked(person, asked, kind, err)
	c.mu.Unlock()
	flight.kind, flight.err = kind, err
	close(flight.done)
}

// settleKindLocked keeps the outcome of a member-kind call for person asked at asked: an answer
// is kept CacheTTL, aged from before its question, unless one to a question asked later is
// already held, and clears a kept failure; a failure is kept failureTTL from when it came back.
// A caller whose own context ended settles nothing (memberKindAskedNow), and the shared call runs
// on no caller's context (askKind). c.mu is held.
func (c *Client) settleKindLocked(person string, asked time.Time, kind string, err error) {
	if len(c.kinds) >= maxCached || len(c.failures) >= maxCached {
		c.sweepLocked()
	}
	if err != nil {
		if len(c.failures) < maxCached {
			c.failures[person] = failedKind{err: err, at: c.now()}
		}
		return
	}
	delete(c.failures, person)
	if held, ok := c.kinds[person]; (!ok || held.at.Before(asked)) && len(c.kinds) < maxCached {
		c.kinds[person] = cachedKind{kind: kind, at: asked}
	}
}

// readKind asks Dispatch's person route for person's member kind, a call bounded by
// requestTimeout.
func (c *Client) readKind(ctx context.Context, person string) (string, error) {
	var answer struct {
		MemberKind string `json:"member_kind"`
	}
	status, err := c.get(ctx, personPath, url.Values{"person": {person}}, &answer)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound {
		return "", fmt.Errorf("projectaccess: dispatch answered %d with no member kind for %s", status, person)
	}
	switch answer.MemberKind {
	case MemberFull, MemberRestricted, MemberNone:
	default:
		return "", fmt.Errorf("%w: %q for %s", ErrUnknownMemberKind, answer.MemberKind, person)
	}
	return answer.MemberKind, nil
}

// States answers each of projects as it is to a question naming no person, and Missing for each
// Dispatch does not hold. Any other failure is the whole answer's.
//
// The projects are asked one at a time, so, as in Visible, an answer that was young when the call
// began can be older than CacheTTL by its end: a project read first and turned private while the
// rest were read would otherwise be answered public past the bound. States asks again for each
// such answer, going over the list at most maxRecheckPasses times, so every answer it returns was
// asked less than CacheTTL before it returns, and a list whose answers still age out faster than
// Dispatch answers them is a failure, never an aged answer.
func (c *Client) States(ctx context.Context, projects []string) (map[string]Answer, error) {
	states := make(map[string]Answer, len(projects))
	pending := projects
	for pass := 0; len(pending) > 0; pass++ {
		if pass == maxRecheckPasses {
			return nil, fmt.Errorf("projectaccess: the states of %d projects aged past %s while they were read", len(projects), CacheTTL)
		}
		for _, project := range pending {
			answer, err := c.Access(ctx, project, "")
			var missing *ProjectNotFoundError
			switch {
			case errors.As(err, &missing):
				answer = Answer{Project: project, Missing: true, AskedAt: missing.AskedAt}
			case err != nil:
				return nil, err
			}
			states[project] = answer
		}
		pending = nil
		for _, project := range projects {
			if Stale(c, states[project].AskedAt) {
				pending = append(pending, project)
			}
		}
	}
	return states, nil
}

func (c *Client) cachedAnswer(key accessKey) (cachedAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.answers[key]
	if !ok || c.now().Sub(cached.at) >= CacheTTL {
		return cachedAnswer{}, false
	}
	return cached, true
}

// keepAnswer caches answer under key unless the cache already holds an answer to a question asked
// later, which an answer that was slower to arrive must not replace.
func (c *Client) keepAnswer(key accessKey, answer cachedAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.answers[key]; ok && !held.at.Before(answer.at) {
		return
	}
	if len(c.answers) >= maxCached {
		c.sweepLocked()
	}
	if len(c.answers) < maxCached {
		c.answers[key] = answer
	}
}

// sweepLocked drops every answer older than CacheTTL and every failure older than failureTTL.
// c.mu is held.
func (c *Client) sweepLocked() {
	now := c.now()
	for key, cached := range c.answers {
		if now.Sub(cached.at) >= CacheTTL {
			delete(c.answers, key)
		}
	}
	for person, cached := range c.kinds {
		if now.Sub(cached.at) >= CacheTTL {
			delete(c.kinds, person)
		}
	}
	for person, failed := range c.failures {
		if now.Sub(failed.at) >= failureTTL {
			delete(c.failures, person)
		}
	}
}

// get asks one route through dispatchread and decodes a 200 into into. A token of the broker's
// own that it cannot read, or that Dispatch refuses 401 or 403, is dispatchread.ErrBrokerRefused
// (dispatchread.Reader.Get), which a caller never mints through. A 404 is an answer, returned as
// its status with no error, only when its body's code is PROJECT_NOT_FOUND, the code Dispatch's
// project route answers a project it does not hold with; any other 404 (a Dispatch that does not
// serve the route, a wrong path behind an ingress) is a failure naming its code. A 502, 503 or 504
// whose body names no code, or one of outageCodes, and a 500 whose body names no code or
// internalCode, are errUnavailable, Dispatch not answering now (Unanswered); one naming any other
// code is Dispatch's answer. Every other status, a transport failure and a body it could not read
// or decode are errors naming what failed and never the token.
func (c *Client) get(ctx context.Context, path string, query url.Values, into any) (int, error) {
	answer, err := c.read.Get(ctx, path, query)
	if err != nil {
		return 0, err
	}
	if answer.Status != http.StatusOK {
		code := dispatchread.RefusalCode(answer.Body)
		switch answer.Status {
		case http.StatusNotFound:
			if code == projectNotFoundCode {
				return http.StatusNotFound, nil
			}
			return 0, fmt.Errorf("projectaccess: dispatch answered %s 404 with the code %q, which is no answer about a project", path, code)
		case http.StatusInternalServerError:
			if code == "" || code == internalCode {
				return 0, fmt.Errorf("%w: dispatch answered %s %d with the code %q", errUnavailable, path, answer.Status, code)
			}
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			if code == "" || slices.Contains(outageCodes, code) {
				return 0, fmt.Errorf("%w: dispatch answered %s %d with the code %q", errUnavailable, path, answer.Status, code)
			}
		}
		return 0, fmt.Errorf("projectaccess: dispatch answered %s %d with the code %q", path, answer.Status, code)
	}
	if err := json.Unmarshal(answer.Body, into); err != nil {
		return 0, fmt.Errorf("projectaccess: decode dispatch's answer to %s: %w", path, err)
	}
	return http.StatusOK, nil
}

// Visible answers nil when person may see every one of projects, else a *NotVisibleError (which
// is ErrNotVisible) naming the first it may not see. A project Dispatch does not hold is the same
// refusal, so the text never says whether a project exists. Any other failure is returned as it
// is, and is no refusal. It is VisibleAsOf without the time.
func Visible(ctx context.Context, a Access, person string, projects []string) error {
	_, err := VisibleAsOf(ctx, a, person, projects)
	return err
}

// VisibleAsOf is Visible, answering beside a nil error when the oldest answer it passed was
// asked: zero for an empty list and for a source that keeps no clock. A caller that reads more
// before it decides ages its decision from it (Stale).
//
// The projects are asked one at a time, so an answer that was young when the check began can be
// older than CacheTTL by its end. VisibleAsOf asks again for each such answer before it returns,
// so every answer it passes was asked less than CacheTTL before it returns: the bound the
// exchange's mint rests on. It goes over the list at most maxRecheckPasses times, and a list whose
// answers still age out faster than Dispatch answers them is a failure, not a pass.
func VisibleAsOf(ctx context.Context, a Access, person string, projects []string) (time.Time, error) {
	asked := make(map[string]time.Time, len(projects))
	pending := projects
	for pass := 0; len(pending) > 0; pass++ {
		if pass == maxRecheckPasses {
			return time.Time{}, fmt.Errorf("projectaccess: the answers for %s aged past %s while %d projects were checked", person, CacheTTL, len(projects))
		}
		for _, project := range pending {
			answer, err := a.Access(ctx, project, person)
			if errors.Is(err, ErrProjectNotFound) {
				return time.Time{}, &NotVisibleError{Project: project}
			}
			if err != nil {
				return time.Time{}, err
			}
			if !answer.Visible {
				return time.Time{}, &NotVisibleError{Project: project}
			}
			asked[project] = answer.AskedAt
		}
		pending = nil
		for _, project := range projects {
			if Stale(a, asked[project]) {
				pending = append(pending, project)
			}
		}
	}
	var oldest time.Time
	for _, at := range asked {
		oldest = Oldest(oldest, at)
	}
	return oldest, nil
}

// maxRecheckPasses bounds how many times Visible and States go over a list for answers that aged
// out while it was read.
const maxRecheckPasses = 3

// Stale reports whether an answer to a question asked at asked is too old for a decision to rest
// on: asked CacheTTL or longer ago, by a's own clock where a is a *Client and by the wall clock
// otherwise. A zero asked is an answer from a source that keeps no clock, and is never stale. It
// is the one statement of an answer's age, for Visible, States and every caller that holds
// answers across further reads before it decides on them.
func Stale(a Access, asked time.Time) bool {
	if asked.IsZero() {
		return false
	}
	now := time.Now()
	if clocked, ok := a.(interface{ clock() time.Time }); ok {
		now = clocked.clock()
	}
	return now.Sub(asked) >= CacheTTL
}

// Oldest is the earlier of two ask times, a zero one standing for none: the age of a decision
// resting on both answers.
func Oldest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// clock is the time c reads, for Stale to age c's answers by the same clock that stamped them.
func (c *Client) clock() time.Time { return c.now() }

// ListAllowed is the one statement of the rule for a person's list, used by the exchange and by
// an enrollment alike. A non-empty list must pass Visible. An empty list is refused with
// ErrListRequired when MemberKind answers MemberRestricted, and with ErrUnknownMemberKind when it
// answers a kind that is none of the three (restrictedKind); MemberFull and MemberNone answer nil,
// so a person Dispatch has never seen mints and enrolls as today. A MemberKind failure that is
// dispatchread.ErrBrokerRefused or ErrUnknownMemberKind is returned as it is, and no caller mints
// through either. Any other MemberKind failure answers an error that is ErrUnchecked and wraps it:
// the list is allowed, as it is today while Dispatch cannot be reached, and the caller that goes
// on logs that it did so unchecked. Because that failure lets the list through, the member kind is
// asked afresh (AskedNow, which any Access that hands the context on honours), never answered by
// a failure an earlier read kept: the list goes through unchecked only while Dispatch does not
// answer.
func ListAllowed(ctx context.Context, a Access, person string, projects []string) error {
	if len(projects) > 0 {
		return Visible(ctx, a, person, projects)
	}
	kind, err := a.MemberKind(AskedNow(ctx), person)
	switch {
	case errors.Is(err, dispatchread.ErrBrokerRefused), errors.Is(err, ErrUnknownMemberKind):
		return err
	case err != nil:
		return fmt.Errorf("%w: %w", ErrUnchecked, err)
	}
	restricted, err := restrictedKind(kind, person)
	switch {
	case err != nil:
		return err
	case restricted:
		return fmt.Errorf("%w: %s", ErrListRequired, person)
	}
	return nil
}
