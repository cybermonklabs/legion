package main

// project_privacy.go is the listener's required privacy mode (ENVOY_PROJECT_PRIVACY_REQUIRED,
// internal/config): the one reader of the private-projects answer, K7, which every decision a
// project's privacy turns on calls afresh, and the per-request decision of what a caller sees.
//
// K7 is read from Dispatch through the secrets broker's GET /v1/project-privacy, with the
// listener's own platform identity: the broker answers Dispatch's committed rows unchanged,
// {"projects":[{"project":"KEY","slack_channels":[...],"slack_channels_changed_at":"<RFC3339>"}]},
// {"projects":[]} when no project is private, and an error (502 dispatch_unavailable, 403
// not_listener, 503 busy) rather than an empty list when it cannot. The reader keeps nothing
// between decisions: a read serves only the decisions that began before its request was sent
// (k7Reader.Read), and any answer that is not a whole one is unknown for every project. An unknown
// project counts as private: a frame of it is held for a full member's session whose token does
// not list it, a route that needs it answers 503 refilling, the Slack intake 503, and a role
// message is held.
//
// In the all-public mode (both variables unset) nothing here reads anything: every project is
// public and a token's list restricts nothing, as before either mode existed (R9).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// k7ReadTimeout bounds one read of the privacy source, the broker's round trip to Dispatch
	// included. It is the cap the first build ships; plan C's t-c-27 measures the round trip as
	// the listener's own identity and sets this to twice the longest correct read it measured.
	k7ReadTimeout = 2 * time.Second
	// k7Path is the broker's route the privacy source is read at (plan E's t-e-63).
	k7Path = "/v1/project-privacy"
	// k7BodyLimit bounds the answer's body; an answer past it is no answer.
	k7BodyLimit = 1 << 20
	// k7UnknownLogEvery is how often an unknown answer is logged: the counter
	// (envoy_listener_project_unknown_total) counts every one.
	k7UnknownLogEvery = time.Hour
)

// The reasons an unknown privacy answer is counted under (envoy_listener_project_unknown_total)
// and named by in its error.
const (
	unknownTimeout     = "timeout"
	unknownTransport   = "transport"
	unknownAccount     = "account"
	unknownRefused     = "refused"
	unknownUnavailable = "unavailable"
	unknownBusy        = "busy"
	unknownRateLimited = "rate_limited"
	unknownStatus      = "status"
	unknownUndecodable = "undecodable"
	unknownIncomplete  = "incomplete"
)

// unknownReasons are every reason, each counted at 0 from the start.
var unknownReasons = []string{unknownTimeout, unknownTransport, unknownAccount, unknownRefused, unknownUnavailable,
	unknownBusy, unknownRateLimited, unknownStatus, unknownUndecodable, unknownIncomplete}

// The answers a decision the relay takes from a read is counted under
// (envoy_listener_project_decisions_total).
const (
	decisionPublic  = "public"
	decisionPrivate = "private"
	decisionUnknown = "unknown"
)

// errPrivacyUnknown is every unknown privacy answer: a read that failed or answered no whole
// value. It is never a set, so nothing is decided public on it.
var errPrivacyUnknown = errors.New("the project privacy answer is unknown")

// privacyUnknown is an unknown answer and why (one of the unknown* reasons).
type privacyUnknown struct {
	reason string
	err    error
}

func (e *privacyUnknown) Error() string {
	return fmt.Sprintf("%s (%s): %v", errPrivacyUnknown, e.reason, e.err)
}

func (e *privacyUnknown) Unwrap() []error { return []error{errPrivacyUnknown, e.err} }

// unknownAnswer is an unknown answer of reason, naming what went wrong.
func unknownAnswer(reason string, format string, args ...any) error {
	return &privacyUnknown{reason: reason, err: fmt.Errorf(format, args...)}
}

// unknownReasonOf is the reason err is counted under: its own, or transport for an error that
// names none.
func unknownReasonOf(err error) string {
	if unknown, ok := errors.AsType[*privacyUnknown](err); ok {
		return unknown.reason
	}
	return unknownTransport
}

// privateSet is one whole privacy answer: each private project and its Slack channels, sorted
// upper-case channel ids. A project it does not list is public, and an empty set makes every
// project public.
type privateSet struct {
	channels map[string][]string
}

// private reports whether project is private in this answer.
func (s privateSet) private(project string) bool {
	_, listed := s.channels[project]
	return listed
}

// none reports whether this answer lists no private project.
func (s privateSet) none() bool { return len(s.channels) == 0 }

// channelProjects is the private projects that list channel, trimmed and upper-cased, sorted.
func (s privateSet) channelProjects(channel string) []string {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	var projects []string
	for project, channels := range s.channels {
		if slices.Contains(channels, channel) {
			projects = append(projects, project)
		}
	}
	slices.Sort(projects)
	return projects
}

// projectKey is a project key's shape (contracts.ProjectKeyPattern).
var projectKey = regexp.MustCompile(contracts.ProjectKeyPattern)

// parsePrivateSet reads the broker's answer body: a whole answer is an object whose projects is an
// array, each element naming a project key and a slack_channels array. A body that does not parse,
// projects that is null, missing or not an array, an element without a key or without
// slack_channels, and a key named twice are each incomplete: no part of such an answer is used.
func parsePrivateSet(body []byte) (privateSet, error) {
	var answer struct {
		Projects *[]struct {
			Project       string    `json:"project"`
			SlackChannels *[]string `json:"slack_channels"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return privateSet{}, unknownAnswer(unknownUndecodable, "decode the privacy answer: %w", err)
	}
	if answer.Projects == nil {
		return privateSet{}, unknownAnswer(unknownIncomplete, "the privacy answer's projects is null or missing")
	}
	set := privateSet{channels: make(map[string][]string, len(*answer.Projects))}
	for _, project := range *answer.Projects {
		if !projectKey.MatchString(project.Project) {
			return privateSet{}, unknownAnswer(unknownIncomplete, "the privacy answer names %q, which is no project key", project.Project)
		}
		if project.SlackChannels == nil {
			return privateSet{}, unknownAnswer(unknownIncomplete, "the privacy answer gives project %s no slack_channels", project.Project)
		}
		if _, twice := set.channels[project.Project]; twice {
			return privateSet{}, unknownAnswer(unknownIncomplete, "the privacy answer names project %s twice", project.Project)
		}
		channels := make([]string, 0, len(*project.SlackChannels))
		for _, channel := range *project.SlackChannels {
			channels = append(channels, strings.ToUpper(strings.TrimSpace(channel)))
		}
		slices.Sort(channels)
		set.channels[project.Project] = channels
	}
	return set, nil
}

// k7Result is one read's answer, handed to every decision that read serves.
type k7Result struct {
	set privateSet
	err error
}

// k7Reader is K7's one reader: one GET of the privacy source per read, with the listener's own
// platform identity as its bearer, bounded by timeout. It keeps nothing between reads.
//
// Decisions that arrive together share a read, and none shares a read that began before it: Read
// queues the decision, and the one loop that reads (run) takes every decision queued so far,
// sends its request, and answers them all with that one answer, so at most one read is in flight
// per listener (the broker bounds its concurrent reads at 64) and a decision is always served by
// a read whose request was sent after the decision began. A decision that arrives while a read is
// in flight waits for the next one.
type k7Reader struct {
	url     string
	source  string
	account ciphertoken.Account
	client  *http.Client
	timeout time.Duration
	logger  *logging.Logger

	mu      sync.Mutex
	waiting []chan k7Result
	reading bool
	// loggedAt is when an unknown answer was last logged, under mu.
	loggedAt time.Time
	// reads counts the requests sent, so a test proves each decision read afresh.
	reads atomic.Uint64
}

// newK7Reader is the reader of the privacy source at source (ENVOY_PROJECT_PRIVACY_URL, the
// broker's base URL), asking as account.
func newK7Reader(source string, account ciphertoken.Account, logger *logging.Logger) *k7Reader {
	return &k7Reader{
		url:     strings.TrimSuffix(source, "/") + k7Path,
		source:  source,
		account: account,
		client:  &http.Client{},
		timeout: k7ReadTimeout,
		logger:  logger,
	}
}

// Read is K7's answer for a decision that begins now: a whole answer, or an error that is unknown
// for every project (errPrivacyUnknown). It waits for a read whose request is sent after this call
// began, at most until ctx ends, which is an unknown answer too.
func (k *k7Reader) Read(ctx context.Context) (privateSet, error) {
	answer := make(chan k7Result, 1)
	k.mu.Lock()
	k.waiting = append(k.waiting, answer)
	start := !k.reading
	k.reading = true
	k.mu.Unlock()
	if start {
		go k.run()
	}
	select {
	case result := <-answer:
		return result.set, result.err
	case <-ctx.Done():
		return privateSet{}, unknownAnswer(unknownTimeout, "the decision ended before the privacy source answered: %w", ctx.Err())
	}
}

// run reads for every decision queued, one read at a time, until none is waiting.
func (k *k7Reader) run() {
	for {
		k.mu.Lock()
		batch := k.waiting
		k.waiting = nil
		if len(batch) == 0 {
			k.reading = false
			k.mu.Unlock()
			return
		}
		k.mu.Unlock()
		set, err := k.readOnce()
		if err != nil {
			k.noteUnknown(err)
		}
		for _, answer := range batch {
			answer <- k7Result{set: set, err: err}
		}
	}
}

// readOnce sends one request to the privacy source and reads its answer.
func (k *k7Reader) readOnce() (privateSet, error) {
	k.reads.Add(1)
	bearer, err := k.account.Read()
	if err != nil {
		return privateSet{}, unknownAnswer(unknownAccount, "read the listener's own platform identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return privateSet{}, unknownAnswer(unknownTransport, "build the privacy read: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := k.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return privateSet{}, unknownAnswer(unknownTimeout, "the privacy source did not answer within %s: %w", k.timeout, err)
		}
		return privateSet{}, unknownAnswer(unknownTransport, "ask the privacy source: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, k7BodyLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return privateSet{}, unknownAnswer(unknownTimeout, "the privacy answer did not arrive within %s: %w", k.timeout, err)
		}
		return privateSet{}, unknownAnswer(unknownTransport, "read the privacy answer: %w", err)
	}
	switch {
	case response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized:
		return privateSet{}, unknownAnswer(unknownRefused, "the privacy source refused this listener's identity (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	case response.StatusCode == http.StatusBadGateway:
		return privateSet{}, unknownAnswer(unknownUnavailable, "the privacy source could not read Dispatch (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	case response.StatusCode == http.StatusServiceUnavailable:
		return privateSet{}, unknownAnswer(unknownBusy, "the privacy source is busy (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	case response.StatusCode == http.StatusTooManyRequests:
		return privateSet{}, unknownAnswer(unknownRateLimited, "the privacy source refused this listener for its rate (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	case response.StatusCode != http.StatusOK:
		return privateSet{}, unknownAnswer(unknownStatus, "the privacy source answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	case len(body) > k7BodyLimit:
		return privateSet{}, unknownAnswer(unknownIncomplete, "the privacy answer is longer than %d bytes", k7BodyLimit)
	}
	return parsePrivateSet(body)
}

// noteUnknown logs an unknown answer at most once an hour.
func (k *k7Reader) noteUnknown(err error) {
	k.mu.Lock()
	due := k.loggedAt.IsZero() || time.Since(k.loggedAt) >= k7UnknownLogEvery
	if due {
		k.loggedAt = time.Now()
	}
	k.mu.Unlock()
	if due {
		k.logger.Warn("listener could not read the project privacy answer; every project is unknown and counts as private until a read answers (logged at most once an hour)",
			slog.String("source", k.source), slog.String("reason", unknownReasonOf(err)), slog.String("error", err.Error()))
	}
}

// privacyCounters are the required mode's counters, each series created at 0 when the listener
// starts, so a missing series means a failed scrape and not a quiet listener. Every counter is
// nil-safe, so a test that builds the privacy with none counts nothing.
type privacyCounters struct {
	// decisions is envoy_listener_project_decisions_total{answer}: each frame the relay decides
	// from a read, by the answer it took.
	decisions *metrics.Counter
	// unknown is envoy_listener_project_unknown_total{reason}: each frame held, and each decision
	// refused, on an unknown answer.
	unknown *metrics.Counter
	// withheld is envoy_listener_project_withheld_total{family}: each frame acknowledged
	// undelivered because the session's token does not see its project.
	withheld *metrics.Counter
	// heldDropped is envoy_listener_held_dropped_total{reason}: each held frame dropped.
	heldDropped *metrics.Counter
	// slackWithheld is envoy_listener_slack_withheld_total{reason}: each Slack event published on
	// no lane.
	slackWithheld *metrics.Counter
}

// The reasons a held frame is dropped under (envoy_listener_held_dropped_total).
const (
	heldDroppedLane     = "lane_full"
	heldDroppedSession  = "session_full"
	heldDroppedTotal    = "listener_full"
	heldDroppedTooLarge = "too_large"
	heldDroppedLimit    = "undecided"
)

// withheldFamilies are the families envoy_listener_project_withheld_total counts under: the
// subject's second token (notifications.<family>.…).
var withheldFamilies = []string{"dispatch", "agent", "role", "legion", "envoy"}

// newPrivacyCounters registers the required mode's counters on met, each label value at 0.
func newPrivacyCounters(met *metrics.Registry) privacyCounters {
	counters := privacyCounters{
		decisions:     met.NewCounter("envoy_listener_project_decisions_total", "Frames the relay decided from a privacy read, by the answer it took (public, private, unknown)"),
		unknown:       met.NewCounter("envoy_listener_project_unknown_total", "Decisions taken while the project privacy answer was unknown, each counting as private, by why the answer was unknown"),
		withheld:      met.NewCounter("envoy_listener_project_withheld_total", "Frames acknowledged and not relayed because the session's token does not see the frame's project, by subject family"),
		heldDropped:   met.NewCounter("envoy_listener_held_dropped_total", "Frames the relay held while their project's privacy was unknown and then dropped, by reason; the session's next frame carries Envoy-Gap"),
		slackWithheld: met.NewCounter("envoy_listener_slack_withheld_total", "Slack events published on no lane, by reason (shared_channel: two private projects list the channel)"),
	}
	for _, answer := range []string{decisionPublic, decisionPrivate, decisionUnknown} {
		counters.decisions.Declare([][2]string{{"answer", answer}})
	}
	for _, reason := range unknownReasons {
		counters.unknown.Declare([][2]string{{"reason", reason}})
	}
	for _, family := range withheldFamilies {
		counters.withheld.Declare([][2]string{{"family", family}})
	}
	for _, reason := range []string{heldDroppedLane, heldDroppedSession, heldDroppedTotal, heldDroppedTooLarge, heldDroppedLimit} {
		counters.heldDropped.Declare([][2]string{{"reason", reason}})
	}
	counters.slackWithheld.Declare([][2]string{{"reason", "shared_channel"}})
	return counters
}

// subjectFamily is a subject's family as envoy_listener_project_withheld_total names it: its
// second token.
func subjectFamily(subject string) string {
	rest, _ := strings.CutPrefix(subject, "notifications.")
	family, _, _ := strings.Cut(rest, ".")
	return family
}

// listenerPrivacy is the listener's privacy mode and what it reads with: nil, or required false,
// is the all-public mode, in which nothing is read and every project is public.
type listenerPrivacy struct {
	required bool
	// k7 is the privacy source's reader, nil in the all-public mode.
	k7 *k7Reader
	// allProjects is ENVOY_ALL_PROJECTS_ACCOUNTS: the service-account subjects of the projected
	// token credential the listener answers as a caller of every project (Dispatch's own), and the
	// only callers of GET /v1/privacy-readiness.
	allProjects []string
	counters    privacyCounters
	// logger writes the lines a decision makes of what it withheld; nil in a test that builds
	// the privacy with none.
	logger *logging.Logger
}

// isRequired reports the required mode.
func (p *listenerPrivacy) isRequired() bool { return p != nil && p.required }

// read is a decision's privacy answer: in the all-public mode an empty set with no read, and in
// the required mode K7's fresh answer.
func (p *listenerPrivacy) read(ctx context.Context) (privateSet, error) {
	if !p.isRequired() {
		return privateSet{}, nil
	}
	return p.k7.Read(ctx)
}

// mode is the roles package's name of this mode.
func (p *listenerPrivacy) mode() roles.Mode {
	if p.isRequired() {
		return roles.Required
	}
	return roles.AllPublic
}

// counted is p's counters, none for a nil p.
func (p *listenerPrivacy) counted() privacyCounters {
	if p == nil {
		return privacyCounters{}
	}
	return p.counters
}

// EnvAllProjectsAccounts names the service-account subjects the listener treats as listing every
// project (listenerPrivacy.allProjects).
const EnvAllProjectsAccounts = "ENVOY_ALL_PROJECTS_ACCOUNTS"

// resolveAllProjectsAccounts reads ENVOY_ALL_PROJECTS_ACCOUNTS: system:serviceaccount:<ns>:<name>
// subjects separated by commas or white space, read by ENVOY_OIDC_ACCOUNTS' rule
// (splitAccountList); unset or blank is none.
func resolveAllProjectsAccounts(getenv func(string) string) ([]string, error) {
	value := getenv(EnvAllProjectsAccounts)
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	accounts, err := splitAccountList(EnvAllProjectsAccounts, value)
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		rest, prefixed := strings.CutPrefix(account, serviceAccountSubjectPrefix)
		namespace, name, found := strings.Cut(rest, ":")
		if !prefixed || !found || namespace == "" || name == "" || strings.Contains(name, ":") {
			return nil, fmt.Errorf("%s must be system:serviceaccount:<namespace>:<name> subjects separated by commas or spaces, got %q", EnvAllProjectsAccounts, account)
		}
	}
	return accounts, nil
}

// projectScope is what one request's caller sees: a broker caller its token's list and whether its
// person is a restricted member (ciphertoken.Caller.Restricted), an ENVOY_ALL_PROJECTS_ACCOUNTS
// caller every project, and every other caller (the shared bearer, no bearer, any other service
// account) no list and no member list holding it, which sees the public projects.
type projectScope struct {
	listed      []string
	restricted  bool
	allProjects bool
}

// scopeOf is r's caller's scope.
func scopeOf(r *http.Request, privacy *listenerPrivacy) projectScope {
	if caller, ok := brokerCaller(r); ok {
		return projectScope{listed: caller.Projects, restricted: caller.Restricted}
	}
	if subject := verifiedPrincipal(r); subject != "" && privacy != nil && slices.Contains(privacy.allProjects, subject) {
		return projectScope{allProjects: true}
	}
	return projectScope{}
}

// registrantScope is what a registration records of its token on its session's entry: the token's
// projects (registeringProjects, session.SessionEntry.Projects) and whether its person is a
// restricted member (session.SessionEntry.Restricted).
type registrantScope struct {
	projects   []string
	restricted *bool
}

// registrantOf is r's registrantScope: its broker token's list and restricted claim, or no list
// and false for a registration that presented no broker token, which no member list holds.
func registrantOf(r *http.Request) registrantScope {
	caller, _ := brokerCaller(r)
	return registrantScope{projects: registeringProjects(r), restricted: new(caller.Restricted)}
}

// scopeDecision is one request's privacy decisions: its caller's scope, and the one privacy read
// the request makes, at its first decision that needs one.
type scopeDecision struct {
	ctx     context.Context
	scope   projectScope
	privacy *listenerPrivacy
	read    bool
	set     privateSet
	err     error
}

// decide is r's decisions, made within ctx.
func (p *listenerPrivacy) decide(ctx context.Context, r *http.Request) *scopeDecision {
	return &scopeDecision{ctx: ctx, scope: scopeOf(r, p), privacy: p}
}

// answer is the request's privacy answer, read once.
func (d *scopeDecision) answer() (privateSet, error) {
	if !d.read {
		d.read = true
		d.set, d.err = d.privacy.read(d.ctx)
		if d.err != nil {
			d.privacy.counted().unknown.Inc([2]string{"reason", unknownReasonOf(d.err)})
		}
	}
	return d.set, d.err
}

// sees reports whether the caller sees project: in the all-public mode every project; for an
// all-projects caller every project; for every caller a message of no project and a GitHub
// subject's (AnyToken); otherwise by the member rule (roles.Sees): a project its token lists, and,
// for a caller that is no restricted member, a project a fresh read says is public, an unknown
// project counting as private. A read that fails is its error.
func (d *scopeDecision) sees(project string) (bool, error) {
	if !d.privacy.isRequired() || d.scope.allProjects {
		return true, nil
	}
	return roles.Sees(d.ctx, d.privateOf, d.scope.listed, d.scope.restricted, project)
}

// seesSession reports whether the caller sees the session whose record is entry, read by the
// required mode's rule (session.SessionEntry.RequiredScope): one of its stored projects (K6) it
// sees, or, for a session that lists none, a caller that is no restricted member, or that lists a
// public project.
func (d *scopeDecision) seesSession(entry session.SessionEntry, sessionID string) (bool, error) {
	if !d.privacy.isRequired() || d.scope.allProjects {
		return true, nil
	}
	projects, _ := entry.RequiredScope(sessionID)
	for _, project := range projects {
		if sees, err := d.sees(project); err != nil || sees {
			return sees, err
		}
	}
	if len(projects) > 0 {
		return false, nil
	}
	if !d.scope.restricted {
		return true, nil
	}
	set, err := d.answer()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(d.scope.listed, func(project string) bool { return !set.private(project) }), nil
}

// projectNotListed is the code of a refusal of a project the caller does not see.
const projectNotListed = "project_not_listed"

// writeProjectNotListed answers 403 project_not_listed naming project.
func writeProjectNotListed(w http.ResponseWriter, project string) {
	writeJSON(w, http.StatusForbidden, projectNotListedAnswer(project, ""))
}

// projectNotListedAnswer is the 403 of a project a caller, or the session it names, does not see.
func projectNotListedAnswer(project, session string) listenerwire.APIError {
	if session != "" {
		return listenerwire.APIError{Error: fmt.Sprintf("project %s is not one the access token of session %s lists", project, session), Code: projectNotListed}
	}
	return listenerwire.APIError{Error: fmt.Sprintf("project %s is not one this caller's access token lists", project), Code: projectNotListed}
}

// privacyUnknownMessage is the 503 of a route whose decision could not read the privacy answer. It
// carries the refilling code and Retry-After the list routes give, so a client retries it as it
// retries them.
const privacyUnknownMessage = "envoy: the project privacy answer could not be read; try again in a moment"

// writePrivacyUnknown answers a route whose decision could not read the privacy answer.
func writePrivacyUnknown(w http.ResponseWriter) {
	w.Header().Set("Retry-After", listRefillingRetryAfter)
	writeJSON(w, http.StatusServiceUnavailable, listenerwire.APIError{Error: privacyUnknownMessage, Code: listRefillingCode})
}

// privateOf is project's privacy in the request's one answer: what roles.Sees reads for a session
// the request names.
func (d *scopeDecision) privateOf(_ context.Context, project string) (bool, error) {
	set, err := d.answer()
	return set.private(project), err
}

// sendProject is a send's project as KC1 chooses it: requested when the body names one, else the
// sender's when its token lists exactly one, else the target's when its token lists exactly one,
// else no project's.
func sendProject(requested string, sender, target []string) string {
	switch {
	case requested != "":
		return requested
	case len(sender) == 1:
		return sender[0]
	case len(target) == 1:
		return target[0]
	}
	return contracts.NoProject
}

// logPayload is the payload_summary attribute of a log line about a message (the "listener send"
// and "listener publish" records, and "listener received"), or none: in the all-public mode always,
// as before the modes existed; in the required mode never, so no line carries a comment's or a
// direct message's text whatever the message's project.
func logPayload(privacy *listenerPrivacy, summary string) []slog.Attr {
	if privacy.isRequired() {
		return nil
	}
	return []slog.Attr{slog.String("payload_summary", summary)}
}

// listenerClaims is the listener's roles.ClaimStore: a role's claim read from the role bucket
// through store.Registry at contracts.RoleClaimKey(project, role), a project's privacy through the
// privacy source, read afresh at each call and never from a copy, and the listener's mode.
type listenerClaims struct {
	registry *store.Registry
	privacy  *listenerPrivacy
}

// RoleClaim reads the claim of role under project.
func (c listenerClaims) RoleClaim(ctx context.Context, project, role string) (store.RoleClaim, error) {
	return c.registry.RoleClaim(ctx, contracts.RoleClaimKey(project, role))
}

// Private reads project's privacy afresh: false for every project in the all-public mode, the
// answer's word in the required mode, and its error on an unknown answer.
func (c listenerClaims) Private(ctx context.Context, project string) (bool, error) {
	set, err := c.privacy.read(ctx)
	if err != nil {
		return false, err
	}
	return set.private(project), nil
}

// Mode is the listener's privacy mode.
func (c listenerClaims) Mode() roles.Mode { return c.privacy.mode() }

// claims is the listener's claim store over d's registry and privacy.
func (d *listenerDeps) claims() listenerClaims {
	return listenerClaims{registry: d.registry, privacy: d.privacy}
}

// claims is the claim store the delivery paths read: the registry's, under the listener's privacy.
func (cfg listenerDeliveryHandlerConfig) claims() listenerClaims {
	return listenerClaims{registry: cfg.registry, privacy: cfg.privacy}
}

// newListenerPrivacy is the listener's privacy mode from cfg: in the required mode a reader of
// cfg.PrivacyURL asking as the listener's platform identity, account; in the all-public mode
// none. allProjects is ENVOY_ALL_PROJECTS_ACCOUNTS. Its counters are registered on met in both
// modes, each at 0.
func newListenerPrivacy(cfg config.Service, account ciphertoken.Account, allProjects []string, met *metrics.Registry, logger *logging.Logger) *listenerPrivacy {
	privacy := &listenerPrivacy{required: cfg.PrivacyRequired, allProjects: allProjects, counters: newPrivacyCounters(met), logger: logger}
	if cfg.PrivacyRequired {
		privacy.k7 = newK7Reader(cfg.PrivacyURL, account, logger)
	}
	return privacy
}

// busOptions are the options each of the listener's bus clients connects with for its privacy
// mode: the suffix in the required mode, nothing in the all-public mode.
func (p *listenerPrivacy) busOptions() []bus.ConnectOption {
	if !p.isRequired() {
		return nil
	}
	return []bus.ConnectOption{bus.WithPrivacyRequired(natsgrant.PrivacyRequiredSuffix)}
}

// SlackChannelProjects is the private projects listing channel in a fresh privacy answer, for the
// Slack intake (webhook.SlackPrivacy); an unknown answer is its error, counted.
func (p *listenerPrivacy) SlackChannelProjects(ctx context.Context, channel string) ([]string, error) {
	set, err := p.read(ctx)
	if err != nil {
		p.counted().unknown.Inc([2]string{"reason", unknownReasonOf(err)})
		return nil, err
	}
	return set.channelProjects(channel), nil
}

// SlackWithheld counts and logs a Slack event of a channel that two or more private projects
// list, which the intake publishes on no lane: Dispatch refuses such a list at every write, so
// only a direct write of its database makes one.
func (p *listenerPrivacy) SlackWithheld(channel string, projects []string) {
	p.counted().slackWithheld.Inc([2]string{"reason", "shared_channel"})
	if p != nil && p.logger != nil {
		p.logger.Error("listener published a Slack event on no lane: more than one private project lists its channel",
			slog.String("channel", channel), slog.Any("projects", projects))
	}
}

// seenOf is items less those whose project (projectOf) the caller does not see, in their order:
// items itself in the all-public mode and for an all-projects caller.
func seenOf[T any](d *scopeDecision, items []T, projectOf func(T) string) ([]T, error) {
	if !d.privacy.isRequired() || d.scope.allProjects {
		return items, nil
	}
	kept := make([]T, 0, len(items))
	for _, item := range items {
		sees, err := d.sees(projectOf(item))
		if err != nil {
			return nil, err
		}
		if sees {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

// rolesSeen is roles, each named by its claim's key, less those whose project the caller does not
// see (contracts.ParseRoleClaimKey).
func (d *scopeDecision) rolesSeen(roles []string) ([]string, error) {
	return seenOf(d, roles, func(role string) string {
		project, _ := contracts.ParseRoleClaimKey(role)
		return project
	})
}

// hidesSession answers a read route for the session whose record is entry: false when the caller
// sees it (seesSession); otherwise it writes the answer of a session no listener has heard of, or
// the 503 refilling when the privacy read did not answer, and reports true.
func (d *scopeDecision) hidesSession(w http.ResponseWriter, sessionID string, entry session.SessionEntry) bool {
	seen, err := d.seesSession(entry, sessionID)
	switch {
	case err != nil:
		writePrivacyUnknown(w)
		return true
	case !seen:
		writeJSON(w, http.StatusNotFound, unknownSession(sessionID))
		return true
	}
	return false
}

// interestRowSeen is row less every topic whose project the caller does not see
// (contracts.SubjectProject), so an interest row names no topic of a project the caller's token
// does not list.
func (d *scopeDecision) interestRowSeen(row listenerwire.InterestRow) (listenerwire.InterestRow, error) {
	topics, err := seenOf(d, row.Topics, func(topic string) string {
		project, _ := contracts.SubjectProject(topic)
		return project
	})
	if err != nil {
		return listenerwire.InterestRow{}, err
	}
	row.Topics = topics
	return row, nil
}

// legionAgentsSeen is agents less those of an issue whose project the caller does not see.
func (d *scopeDecision) legionAgentsSeen(agents []legionagents.Agent) ([]legionagents.Agent, error) {
	return seenOf(d, agents, func(agent legionagents.Agent) string {
		if project, ok := contracts.IssueProject(agent.Issue); ok {
			return project
		}
		return contracts.NoProject
	})
}
