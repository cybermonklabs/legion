// Package projectaccess is the broker's one client of Dispatch's access routes: whether a
// project is private, whether a person may see it, and what kind of member a person is. Dispatch
// decides all three — the owner, the access list, a group's members, the full and restricted
// members — and the broker asks rather than keeps a copy, so a decision here is the one Dispatch
// would make for that person's own read.
//
// The broker asks with its own read token (Credential, ReadToken): a token it issues to itself at
// its own token endpoint from its read entry, for its projected Kubernetes token, bound to a key
// the process holds, presented under the DPoP scheme with a proof signed for each request. It
// never presents the projected token to Dispatch, and nothing here falls back to a bearer.
//
// Every answer is plain data. The rule every caller of this package keeps is that no answer is
// asked for inside a database transaction, inside policy.Set.Evaluate, or while a row lock or an
// advisory lock is held: a caller resolves what it needs first and hands the answers on, so a
// Dispatch that is slow to answer holds no lock and no pooled connection of the broker's. The
// token exchange asks before it opens its transaction (internal/broker/oauth, exchangeGrant).
//
// Answers are cached CacheTTL per project and person, and per person: one answer of the person
// route, which MemberKind and Person both read. Each is aged from before its question was sent,
// which bounds how long a person removed from a project's access list goes on passing a check
// here; Visible asks again for an answer that aged past it while a list was checked, and a slower
// answer never replaces one to a question asked later. Concurrent asks about one person share one
// call to Dispatch. A failure to reach Dispatch is never cached and is never an answer: each
// caller decides what an outage means for its own check.
package projectaccess

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/brokerclient"
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

// requestTimeout bounds one call to Dispatch, the dial included.
const requestTimeout = 10 * time.Second

// maxAnswerBytes caps the answer read from Dispatch. An answer is a handful of short fields and the
// project's Slack channel ids, so anything larger is not Dispatch's and is refused rather than held.
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
	// ErrBrokerRefused is the broker's own credential not working: Dispatch answering 401 or 403
	// to its read token (Dispatch does not list the broker's account as a service that may ask,
	// or does not accept the token, one whose proof names another URL, say), or the broker unable
	// to have that token at all: its projected token unreadable from
	// BROKER_PROJECT_ACCESS_TOKEN_FILE (missing, empty or unreadable), or its trade refused by its
	// own token endpoint in a way no retry cures (no read entry lists the account, say). Either is
	// a misconfiguration and not an outage, so no caller mints or enrolls through it as it would
	// through a failure to reach Dispatch: every check made with that credential would be wrong.
	ErrBrokerRefused = errors.New("projectaccess: the broker's own read token was refused or could not be had")
	// ErrUnknownMemberKind is Dispatch answering a member kind that is none of MemberFull,
	// MemberRestricted and MemberNone. The broker cannot tell whether such a person is held to the
	// projects that name them, so no caller reads it as any of the three, and none mints or
	// enrolls through it.
	ErrUnknownMemberKind = errors.New("projectaccess: dispatch answered a member kind this broker does not know")
	// ErrUnchecked is ListAllowed's answer for an empty list whose person's member kind Dispatch
	// could not be asked for: the list is allowed, as it is today, but unchecked, so the caller
	// that goes on to mint or enroll logs that it did so. It wraps the failure it stands for.
	ErrUnchecked = errors.New("projectaccess: dispatch could not be asked, so the empty list is allowed unchecked")
	// ErrNoHeldField is Person's answer for a person whose answer from Dispatch has no held
	// field: a Dispatch older than the hold of refused people. It is a failure to ask and never
	// read as not held, since during a roll such a Dispatch cannot say whether a newer one holds
	// the person.
	ErrNoHeldField = errors.New("projectaccess: dispatch's answer for a person names no held")
)

// NotVisibleError is Visible's refusal: the first project of a list its person may not see, or
// that Dispatch does not hold. It is ErrNotVisible (errors.Is), and its Project is what a caller
// names in its own refusal; its text is the same whichever of the two it was.
type NotVisibleError struct{ Project string }

func (e *NotVisibleError) Error() string { return ErrNotVisible.Error() + ": " + e.Project }

// Unwrap is ErrNotVisible.
func (e *NotVisibleError) Unwrap() error { return ErrNotVisible }

// Answer is Dispatch's answer for one project and one person.
type Answer struct {
	// Project is the project's key.
	Project string `json:"project"`
	// Visibility is "public" or "private".
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
	// AskedAt is when the question this answers was asked, read before it was sent, so an
	// answer's age is never less than it truly is. *Client always sets it; a zero AskedAt is an
	// answer from a source that keeps no clock, which Visible takes as fresh.
	AskedAt time.Time `json:"-"`
}

// Private reports whether the project is private.
func (a Answer) Private() bool { return a.Visibility == "private" }

// Access is every question a broker reader asks Dispatch; *Client answers all three.
type Access interface {
	// Access is Dispatch's answer for project and person; ErrProjectNotFound for a project it
	// does not hold.
	Access(ctx context.Context, project, person string) (Answer, error)
	// MemberKind is the person's member kind.
	MemberKind(ctx context.Context, person string) (string, error)
	// States answers one fact per key, for the caller that must resolve every answer before it
	// opens a transaction: present for a project Dispatch holds, absent for one it does not. It
	// is one call per key against the cache, not a batch route.
	States(ctx context.Context, projects []string) (map[string]Answer, error)
}

// Credential is the broker's own read token at Dispatch, which the client presents on every call:
// an access token of the broker's read entry under the DPoP scheme, beside a proof of the key it
// is bound to signed for that one request. ReadToken is the broker's; a test hands in one of its
// own (projectaccesstest.Account).
type Credential interface {
	// Token is a live access token, had within ctx: the token in hand, or one traded now. A
	// failure no retry cures is ErrBrokerRefused; any other is a failure to ask.
	Token(ctx context.Context) (string, error)
	// Key is the private key every token Token answers is bound to, which signs each proof.
	Key() *ecdsa.PrivateKey
}

// Client asks Dispatch's access routes at one base URL, presenting the broker's own read token.
type Client struct {
	base       string
	credential Credential
	http       *http.Client
	now        func() time.Time

	mu      sync.Mutex
	answers map[accessKey]cachedAnswer
	people  map[string]cachedPerson
	// asks makes the concurrent asks about one person, MemberKind's and Person's alike, one call
	// to Dispatch (askPerson), keyed by the canonical login.
	asks singleflight.Group
}

type accessKey struct{ project, person string }

type cachedAnswer struct {
	answer          Answer
	projectNotFound bool
	at              time.Time
}

// cachedPerson is one answer of the person route: the member kind, held as Dispatch answered it
// (nil for an answer with no held field, from a Dispatch older than the hold), the as_of it
// answered with (zero when it named none), the not_held_until it answered with (zero when it named
// none), and when it was asked.
type cachedPerson struct {
	kind  string
	held  *bool
	asOf  time.Time
	until time.Time
	at    time.Time
}

// New is a client of Dispatch at baseURL (BROKER_PROJECT_ACCESS_URL, Dispatch's canonical URL,
// which every proof names), presenting credential on every call. Each request goes through a DPoP
// transport of credential's key wrapped around httpClient's transport, so httpClient keeps its
// timeout and whatever it connects through: brokerclient.AtAddress, where the broker reaches
// Dispatch at BROKER_PROJECT_ACCESS_DIAL_URL instead. A nil httpClient is http.DefaultClient; each
// call is bounded by its own 10 s whatever the client, the read token's trade included.
func New(baseURL string, credential Credential, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	proofed := &http.Client{Transport: brokerclient.NewDPoPTransport(credential.Key(), base), CheckRedirect: httpClient.CheckRedirect, Jar: httpClient.Jar, Timeout: httpClient.Timeout}
	return &Client{
		base:       strings.TrimRight(baseURL, "/"),
		credential: credential,
		http:       proofed,
		now:        time.Now,
		answers:    map[accessKey]cachedAnswer{},
		people:     map[string]cachedPerson{},
	}
}

// Access is Dispatch's answer for project and person, from the cache while it is younger than
// CacheTTL. A person is asked about by their lowercase login; an empty person asks whether the
// project is public. An answer is aged from before its question was sent, so a slow answer is
// kept no longer than CacheTTL from the moment Dispatch could last have changed it.
func (c *Client) Access(ctx context.Context, project, person string) (Answer, error) {
	key := accessKey{project: project, person: record.CanonicalLogin(person)}
	if cached, ok := c.cachedAnswer(key); ok {
		if cached.projectNotFound {
			return Answer{}, fmt.Errorf("%w: %s", ErrProjectNotFound, project)
		}
		return cached.answer, nil
	}
	query := url.Values{}
	if key.person != "" {
		query.Set("person", key.person)
	}
	asked := c.now()
	var answer Answer
	status, err := c.get(ctx, projectPath+url.PathEscape(project), query, &answer)
	switch {
	case err != nil:
		return Answer{}, err
	case status == http.StatusNotFound:
		c.keepAnswer(key, cachedAnswer{projectNotFound: true, at: asked})
		return Answer{}, fmt.Errorf("%w: %s", ErrProjectNotFound, project)
	}
	answer.AskedAt = asked
	c.keepAnswer(key, cachedAnswer{answer: answer, at: asked})
	return answer, nil
}

// MemberKind is the person's member kind, from the cached person answer while it is younger than
// CacheTTL, whether or not that answer has held (Person), so a Dispatch older than the hold still
// answers it: one of MemberFull, MemberRestricted and MemberNone. Any other answer, an empty one
// included, is ErrUnknownMemberKind naming it, and never a failure to ask.
func (c *Client) MemberKind(ctx context.Context, person string) (string, error) {
	person = record.CanonicalLogin(person)
	if person == "" {
		return "", errors.New("projectaccess: a member kind is asked of a person, and none was named")
	}
	answer, err := c.personAnswer(ctx, person, func(cachedPerson) time.Duration { return CacheTTL })
	if err != nil {
		return "", err
	}
	switch answer.kind {
	case MemberFull, MemberRestricted, MemberNone:
		return answer.kind, nil
	}
	return "", fmt.Errorf("%w: %q for %s", ErrUnknownMemberKind, answer.kind, person)
}

// PersonAnswer is Dispatch's answer on its person route for one person, as Person reads it.
type PersonAnswer struct {
	// MemberKind is the member kind Dispatch answered, passed on whatever it is: MemberKind is
	// the check of it.
	MemberKind string
	// Held is whether Dispatch holds the person's credentials until they sign in again.
	Held bool
	// AsOf is the database time of the read Dispatch's answer rests on (as_of): the clock by
	// which a later answer is told from an earlier one, whichever broker process asked.
	AsOf time.Time
	// NotHeldUntil is the deadline an IAP deployment's Dispatch puts on an answer that is not held
	// (not_held_until): the last active Workspace Directory answer plus 24 hours, or AsOf itself
	// once that passed. Past it the answer is no longer served (hold.Gate.CheckUntil). It is the
	// zero time for an answer naming none, as every answer outside IAP and every held one.
	NotHeldUntil time.Time
	// AskedAt is when the question this answers was asked, by the client's clock, read before it
	// was sent. Every read of one cached answer carries the same AskedAt.
	AskedAt time.Time
}

// noHeldRetry is how long Person answers ErrNoHeldField from a cached answer with no held before
// it asks Dispatch again, while MemberKind goes on reading that answer for its CacheTTL. It is the
// hold gate's memory of a failure to ask (internal/broker/hold), so a Dispatch that starts
// answering held is read at the gate's next ask.
const noHeldRetry = 5 * time.Second

// Person is Dispatch's answer for person: their member kind, whether Dispatch holds them, the
// time its answer rests on, and the deadline an IAP deployment's Dispatch puts on a not-held
// answer, as Dispatch spelled it. It reads the cached person answer MemberKind reads, while it is
// younger than CacheTTL, whatever its deadline: the gate judges that at each use
// (hold.Gate.CheckUntil). An answer with no held, from a Dispatch older than the hold, is
// ErrNoHeldField, answered from the cache for noHeldRetry and asked again after; an answer with
// held and no as_of is a failure too, since no later answer could be told from it. Neither is
// ever read as not held. A member kind the broker does not know is still an answer here, since
// its held decodes. Dispatch refusing the broker's own read token, or that token not to be had,
// is ErrBrokerRefused, as for MemberKind, and every other failure is a failure to ask.
func (c *Client) Person(ctx context.Context, person string) (PersonAnswer, error) {
	person = record.CanonicalLogin(person)
	if person == "" {
		return PersonAnswer{}, errors.New("projectaccess: a person's answer is asked of a person, and none was named")
	}
	answer, err := c.personAnswer(ctx, person, func(cached cachedPerson) time.Duration {
		if cached.held == nil || cached.asOf.IsZero() {
			return noHeldRetry
		}
		return CacheTTL
	})
	switch {
	case err != nil:
		return PersonAnswer{}, err
	case answer.held == nil:
		return PersonAnswer{}, fmt.Errorf("%w: %s", ErrNoHeldField, person)
	case answer.asOf.IsZero():
		return PersonAnswer{}, fmt.Errorf("projectaccess: dispatch's answer for %s names held and no as_of", person)
	}
	return PersonAnswer{MemberKind: answer.kind, Held: *answer.held, AsOf: answer.asOf, NotHeldUntil: answer.until, AskedAt: answer.at}, nil
}

// personAnswer is the cached answer for person while it is younger than keep says for it, and
// else the one a call to Dispatch answers. Concurrent callers share that call (c.asks), which runs
// apart from any one caller's context and is bounded by requestTimeout: a caller whose own
// context ends stops waiting, and the call goes on for every caller still waiting and for the
// cache.
func (c *Client) personAnswer(ctx context.Context, person string, keep func(cachedPerson) time.Duration) (cachedPerson, error) {
	c.mu.Lock()
	cached, ok := c.people[person]
	c.mu.Unlock()
	if ok && c.now().Sub(cached.at) < keep(cached) {
		return cached, nil
	}
	call := c.asks.DoChan(person, func() (any, error) {
		return c.askPerson(context.WithoutCancel(ctx), person)
	})
	select {
	case result := <-call:
		if result.Err != nil {
			return cachedPerson{}, result.Err
		}
		return result.Val.(cachedPerson), nil
	case <-ctx.Done():
		return cachedPerson{}, fmt.Errorf("projectaccess: ask dispatch about %s: %w", person, ctx.Err())
	}
}

// askPerson asks the person route about person and keeps the answer unless the cache already
// holds one to a question asked later. A 404, whatever its code, is no answer about a person.
func (c *Client) askPerson(ctx context.Context, person string) (cachedPerson, error) {
	asked := c.now()
	var answer struct {
		MemberKind   string    `json:"member_kind"`
		Held         *bool     `json:"held"`
		AsOf         time.Time `json:"as_of"`
		NotHeldUntil time.Time `json:"not_held_until"`
	}
	status, err := c.get(ctx, personPath, url.Values{"person": {person}}, &answer)
	if err != nil {
		return cachedPerson{}, err
	}
	if status == http.StatusNotFound {
		return cachedPerson{}, fmt.Errorf("projectaccess: dispatch answered %d with no answer for %s", status, person)
	}
	entry := cachedPerson{kind: answer.MemberKind, held: answer.Held, asOf: answer.AsOf, until: answer.NotHeldUntil, at: asked}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.people) >= maxCached {
		c.sweepLocked()
	}
	if kept, ok := c.people[person]; (!ok || kept.at.Before(asked)) && len(c.people) < maxCached {
		c.people[person] = entry
	}
	return entry, nil
}

// UseClock makes c read now instead of the wall clock, for every answer's age and AskedAt, so a
// test moves an answer past CacheTTL rather than waiting it out. It is set before c is first used.
func (c *Client) UseClock(now func() time.Time) { c.now = now }

// States answers each of projects Dispatch holds, as it is to a question naming no person, and
// leaves out each it does not. Any other failure is the whole answer's.
func (c *Client) States(ctx context.Context, projects []string) (map[string]Answer, error) {
	states := make(map[string]Answer, len(projects))
	for _, project := range projects {
		answer, err := c.Access(ctx, project, "")
		if errors.Is(err, ErrProjectNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		states[project] = answer
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

// sweepLocked drops every answer older than CacheTTL. c.mu is held.
func (c *Client) sweepLocked() {
	now := c.now()
	for key, cached := range c.answers {
		if now.Sub(cached.at) >= CacheTTL {
			delete(c.answers, key)
		}
	}
	for person, cached := range c.people {
		if now.Sub(cached.at) >= CacheTTL {
			delete(c.people, person)
		}
	}
}

// get asks one route and decodes a 200 into into. It has the broker's read token first, within
// this call's bound: a token it cannot have for good (ErrBrokerRefused from the credential) is
// ErrBrokerRefused, asked nothing, since the broker's own credential is broken and not Dispatch,
// and any other failure to have one is a failure to ask, asked nothing either; it never asks with
// anything else. A 401 or a 403 is ErrBrokerRefused too, Dispatch refusing that token, decided by
// the status alone before any of the body is read: a body cut short or past the bound only leaves
// the error without the code it names, and never turns the refusal into a failure to ask, which a
// caller mints through. A 404 is an answer, returned as its status with no error, only when its
// body's code is PROJECT_NOT_FOUND, the code Dispatch's project route answers a project it does
// not hold with; any other 404 (a Dispatch that does not serve the route, a wrong path behind an
// ingress) is a failure naming its code. Every other status, a transport failure and a body it
// could not read or decode are errors naming what failed and never the token.
func (c *Client) get(ctx context.Context, path string, query url.Values, into any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	token, err := c.credential.Token(ctx)
	if err != nil {
		return 0, err
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("projectaccess: build the request: %w", err)
	}
	request.Header.Set("Authorization", "DPoP "+token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("projectaccess: ask dispatch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return 0, fmt.Errorf("%w: dispatch answered %s %d with the code %q", ErrBrokerRefused, path, response.StatusCode, refusalCode(response.Body))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes+1))
	if err != nil {
		return 0, fmt.Errorf("projectaccess: read dispatch's answer: %w", err)
	}
	if len(body) > maxAnswerBytes {
		return 0, fmt.Errorf("projectaccess: dispatch's answer to %s is over %d bytes", path, maxAnswerBytes)
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		code := refusalCode(bytes.NewReader(body))
		if code == projectNotFoundCode {
			return http.StatusNotFound, nil
		}
		return 0, fmt.Errorf("projectaccess: dispatch answered %s 404 with the code %q, which is no answer about a project", path, code)
	default:
		return 0, fmt.Errorf("projectaccess: dispatch answered %s %d with the code %q", path, response.StatusCode, refusalCode(bytes.NewReader(body)))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return 0, fmt.Errorf("projectaccess: decode dispatch's answer to %s: %w", path, err)
	}
	return http.StatusOK, nil
}

// refusalCode is the code a refusal's body names, read from at most maxAnswerBytes of it: empty
// for a body that is cut short, runs past the bound or names none, since the status has already
// decided what the answer is and the code only adds to its error.
func refusalCode(body io.Reader) string {
	var refusal struct {
		Code string `json:"code"`
	}
	raw, _ := io.ReadAll(io.LimitReader(body, maxAnswerBytes))
	_ = json.Unmarshal(raw, &refusal)
	return refusal.Code
}

// Visible answers nil when person may see every one of projects, else a *NotVisibleError (which
// is ErrNotVisible) naming the first it may not see. A project Dispatch does not hold is the same
// refusal, so the text never says whether a project exists. Any other failure is returned as it
// is, and is no refusal.
//
// The projects are asked one at a time, so an answer that was young when the check began can be
// older than CacheTTL by its end. Visible asks again for each such answer before it returns, so
// every answer it passes was asked less than CacheTTL before it returns: the bound the exchange's
// mint rests on. It goes over the list at most maxRecheckPasses times, and a list whose answers
// still age out faster than Dispatch answers them is a failure, not a pass.
func Visible(ctx context.Context, a Access, person string, projects []string) error {
	asked := make(map[string]time.Time, len(projects))
	pending := projects
	for pass := 0; len(pending) > 0; pass++ {
		if pass == maxRecheckPasses {
			return fmt.Errorf("projectaccess: the answers for %s aged past %s while %d projects were checked", person, CacheTTL, len(projects))
		}
		for _, project := range pending {
			answer, err := a.Access(ctx, project, person)
			if errors.Is(err, ErrProjectNotFound) {
				return &NotVisibleError{Project: project}
			}
			if err != nil {
				return err
			}
			if !answer.Visible {
				return &NotVisibleError{Project: project}
			}
			asked[project] = answer.AskedAt
		}
		pending = nil
		now := time.Now()
		if clocked, ok := a.(interface{ clock() time.Time }); ok {
			now = clocked.clock()
		}
		for _, project := range projects {
			if at := asked[project]; !at.IsZero() && now.Sub(at) >= CacheTTL {
				pending = append(pending, project)
			}
		}
	}
	return nil
}

// maxRecheckPasses bounds how many times Visible goes over a list for answers that aged out
// while it was checked.
const maxRecheckPasses = 3

// clock is the time c reads, for Visible to age c's answers by the same clock that stamped them.
func (c *Client) clock() time.Time { return c.now() }

// ListAllowed is the one statement of the rule for a person's list, used by the exchange and by
// an enrollment alike. A non-empty list must pass Visible. An empty list is refused with
// ErrListRequired when MemberKind answers MemberRestricted, and with ErrUnknownMemberKind when it
// answers a kind that is none of the three; MemberFull and MemberNone answer nil, so a person
// Dispatch has never seen mints and enrolls as today. A MemberKind failure that is
// ErrBrokerRefused or ErrUnknownMemberKind is returned as it is, and no caller mints through
// either. Any other MemberKind failure answers an error that is ErrUnchecked and wraps it: the
// list is allowed, as it is today while Dispatch cannot be reached, and the caller that goes on
// logs that it did so unchecked.
func ListAllowed(ctx context.Context, a Access, person string, projects []string) error {
	if len(projects) > 0 {
		return Visible(ctx, a, person, projects)
	}
	kind, err := a.MemberKind(ctx, person)
	switch {
	case errors.Is(err, ErrBrokerRefused), errors.Is(err, ErrUnknownMemberKind):
		return err
	case err != nil:
		return fmt.Errorf("%w: %w", ErrUnchecked, err)
	}
	switch kind {
	case MemberFull, MemberNone:
		return nil
	case MemberRestricted:
		return fmt.Errorf("%w: %s", ErrListRequired, person)
	}
	return fmt.Errorf("%w: %q for %s", ErrUnknownMemberKind, kind, person)
}
