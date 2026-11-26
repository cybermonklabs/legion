package main

// edge_repositories.go is the edge listener's reader of which repositories the issues of each
// project it serves resolve to, as the secrets broker relays Dispatch's one rule
// (GET /v1/project-repositories, internal/cipher/api/project_repositories.go). The edge asks it
// with its own broker account, fresh at each registration that follows a GitHub topic and every
// edgeRepositoryRefresh for each project it serves, and fails closed on any answer that is not a
// whole list: a registration it cannot read for is 503 repositories_unknown, and while no answer
// of a session's projects is younger than edgeRepositoryAnswerLife that session is relayed no
// GitHub frame (project_scope.go, session_relay.go).
//
// The list is the whole of what it knows of a repository. It reads no repository mapping of its
// own, and no project of a repository is ever derived here: a repository is a project's only
// while the broker's answer for that project names it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

const (
	// edgeRepositoriesPath is the broker's route of the repositories of a project's issues,
	// asked as ?project=<KEY>.
	edgeRepositoriesPath = "/v1/project-repositories"
	// edgeRepositoryRefresh is how often the edge reads every project's answer again: at least
	// every 30 seconds.
	edgeRepositoryRefresh = 30 * time.Second
	// edgeRepositoryAnswerLife is how long an answer holds for the relay: a GitHub event is
	// relayed only while its repository is in a list no older than 60 seconds. Two refreshes fit
	// in it, so one failed read alone withholds nothing.
	edgeRepositoryAnswerLife = 60 * time.Second
	// edgeRepositoryReadTimeout bounds one read of the broker: the broker bounds its own read of
	// Dispatch at 2 s, and a registration's budget (sendRequestBudget) bounds the whole of a
	// registration's reads below this.
	edgeRepositoryReadTimeout = 5 * time.Second
	// edgeRepositoryReadsAtOnce bounds the reads one pass runs at once (readEach). Each project
	// is read on its own, so a project whose read hangs holds back no other project's answer,
	// which would otherwise age past edgeRepositoryAnswerLife behind it; and the most projects
	// an edge serves (oauthwire.MaxProjects, 16) are read in two rounds, a pass ending within
	// two read timeouts, well inside edgeRepositoryRefresh.
	edgeRepositoryReadsAtOnce = 8
	// edgeRepositoryBodyLimit bounds an answer's body; a list past it is no answer.
	edgeRepositoryBodyLimit = 1 << 20
	// edgeRepositoryErrorExcerpt bounds how much of a refused answer's body the log line about it
	// quotes.
	edgeRepositoryErrorExcerpt = 512
)

// The reasons envoy_edge_github_withheld_total counts a GitHub frame under.
const (
	// withheldNotInProject is a frame of a repository that no fresh answer of the session's
	// projects names.
	withheldNotInProject = "not_in_project"
	// withheldAnswerUnknown is a frame read while some project of the session has no answer
	// younger than edgeRepositoryAnswerLife, so the edge cannot say the repository is its.
	withheldAnswerUnknown = "answer_unknown"
)

// errRepositoriesUnknown is a read of a project's repositories that gave no whole answer: the
// broker's refusal or 503, a transport failure, a body that does not decode, or the listener's
// own account unread. It is never a list, so nothing is admitted on it.
var errRepositoriesUnknown = errors.New("the repositories of the project's issues could not be read")

// repositoryAnswer is one project's answer and when it was read.
type repositoryAnswer struct {
	repositories []string
	readAt       time.Time
}

// edgeRepositories reads and holds the answer of each project the edge serves.
type edgeRepositories struct {
	brokerURL string
	account   ciphertoken.Account
	projects  []string
	client    *http.Client
	// now is the clock answers are dated and aged by; time.Now outside a test.
	now func() time.Time
	// withheld is envoy_edge_github_withheld_total{reason}.
	withheld *metrics.Counter
	logger   *logging.Logger

	mu      sync.Mutex
	answers map[string]repositoryAnswer
	// unread counts, for each served project whose refresh reads have failed since its last good
	// one, the reads that failed (noteRefresh), so a run of them is logged when it starts and when
	// it ends rather than at every refresh.
	unread map[string]int
	// unlisted counts the refresh passes in a row that could not list the interest records, and
	// unremoved holds each session whose interests in a repository that left its projects the last
	// pass that weighed them could not remove (pruneEdgeInterests), so each run of failures is logged
	// when it starts rather than at every pass.
	unlisted  int
	unremoved map[string]bool
	// passes counts the refresh passes that have finished (runEdgeRepositories), so a test
	// orders a publish after one.
	passes atomic.Uint64
}

// newEdgeRepositories is the reader of the broker at brokerURL, asking as account, for the
// projects the edge serves.
func newEdgeRepositories(brokerURL string, account ciphertoken.Account, projects []string, withheld *metrics.Counter, logger *logging.Logger) *edgeRepositories {
	return &edgeRepositories{
		brokerURL: strings.TrimSuffix(brokerURL, "/"),
		account:   account,
		projects:  projects,
		client:    &http.Client{Timeout: edgeRepositoryReadTimeout},
		now:       time.Now,
		withheld:  withheld,
		logger:    logger,
		answers:   map[string]repositoryAnswer{},
	}
}

// newEdgeWithheldCounter registers envoy_edge_github_withheld_total, which counts every frame the
// edge withholds: a GitHub frame by the repository rule, any frame of a project the session's
// token does not list (withheldProjectNotHeld, edge_frames.go), any frame that names a field the
// edge judges by under a key readers disagree on (withheldAmbiguousKey, withheldPrototypeKey), and
// a Dispatch frame the edge cannot place in a project (withheldDispatchProjectUnread).
func newEdgeWithheldCounter(met *metrics.Registry) *metrics.Counter {
	return met.NewCounter("envoy_edge_github_withheld_total",
		"Frames the edge listener withheld from a session, by reason: not_in_project, project_not_held, ambiguous_key, prototype_key and dispatch_project_unread acknowledged undelivered, answer_unknown acknowledged undelivered with its gap mark owed to the session's next frame")
}

// read asks the broker for project's repositories now, and keeps the answer when it is a whole
// one. Every other outcome is errRepositoriesUnknown, naming what went wrong, for the log alone:
// what a client is told is the listener's own sentence (edgeTopicHold.answerRepositoryOutage).
func (e *edgeRepositories) read(ctx context.Context, project string) ([]string, error) {
	bearer, err := e.account.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: read the listener's own account: %w", errRepositoriesUnknown, err)
	}
	ctx, cancel := context.WithTimeout(ctx, edgeRepositoryReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.brokerURL+edgeRepositoriesPath+"?project="+url.QueryEscape(project), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRepositoriesUnknown, err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := e.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: ask the broker: %w", errRepositoriesUnknown, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, edgeRepositoryBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read the broker's answer: %w", errRepositoriesUnknown, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: the broker answered %d: %s", errRepositoriesUnknown, response.StatusCode, excerpt(body))
	}
	if len(body) > edgeRepositoryBodyLimit {
		return nil, fmt.Errorf("%w: the broker's answer is longer than %d bytes", errRepositoriesUnknown, edgeRepositoryBodyLimit)
	}
	var answer struct {
		Project      string    `json:"project"`
		Repositories *[]string `json:"repositories"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%w: decode the broker's answer: %w", errRepositoriesUnknown, err)
	}
	if answer.Project != project || answer.Repositories == nil {
		return nil, fmt.Errorf("%w: the broker answered for project %q with repositories %v, not a list for %s", errRepositoriesUnknown, answer.Project, answer.Repositories, project)
	}
	for _, repository := range *answer.Repositories {
		if owner, name, ok := strings.Cut(repository, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("%w: the broker's answer names %q, which is no owner/name", errRepositoriesUnknown, repository)
		}
	}
	e.mu.Lock()
	e.answers[project] = repositoryAnswer{repositories: *answer.Repositories, readAt: e.now()}
	e.mu.Unlock()
	// A good read ends the project's run of failed refresh reads, whoever made it: a
	// registration's read keeps an answer as a refresh's does (noteRefresh).
	e.noteRefresh(project, nil)
	return *answer.Repositories, nil
}

// readFresh reads the answer of each of projects now, for a registration: the union of their
// repositories, or errRepositoriesUnknown when any one of them could not be read.
func (e *edgeRepositories) readFresh(ctx context.Context, projects []string) ([]string, error) {
	answers, errs := e.readEach(ctx, projects)
	var repositories []string
	for i := range projects {
		if errs[i] != nil {
			return nil, errs[i]
		}
		repositories = append(repositories, answers[i]...)
	}
	return repositories, nil
}

// refresh reads the answer of every project the edge serves again. A read that fails keeps the
// last answer, which ages out at edgeRepositoryAnswerLife, and a run of failed reads of a project
// is logged when it starts and when a good read of the project, a refresh's or a registration's,
// ends it (noteRefresh).
func (e *edgeRepositories) refresh(ctx context.Context) {
	_, errs := e.readEach(ctx, e.projects)
	for i, err := range errs {
		if err != nil {
			e.noteRefresh(e.projects[i], err)
		}
	}
}

// noteRefresh logs what a read of project, which ended with err, starts or ends: a WARN for a
// refresh's failed read that starts a run of them, an INFO for a good read (read, whoever asked
// it) that ends one, and nothing for any other read, so a broker that stays away logs two lines per
// project, not one at every refresh. The WARN names when the project's sessions lose their GitHub
// events (withheld_from): the last answer decides them until it is edgeRepositoryAnswerLife old,
// so a failed read withholds nothing while a fresh answer stands, and with no answer read they are
// withheld from the read on.
func (e *edgeRepositories) noteRefresh(project string, err error) {
	e.mu.Lock()
	failed, failing := e.unread[project]
	answer, answered := e.answers[project]
	now := e.now()
	switch {
	case err != nil:
		if e.unread == nil {
			e.unread = map[string]int{}
		}
		e.unread[project] = failed + 1
	case failing:
		delete(e.unread, project)
	}
	e.mu.Unlock()
	switch {
	case err != nil && !failing:
		withheldFrom := now
		if answered {
			withheldFrom = answer.readAt.Add(edgeRepositoryAnswerLife)
		}
		e.logger.Warn("edge listener could not read its project's repositories; its last answer, if any, decides its sessions' GitHub events until withheld_from, they are withheld from then until a read succeeds, and the listener logs again once one does",
			slog.String("project", project), slog.Time("withheld_from", withheldFrom), slog.String("error", err.Error()))
	case err == nil && failing:
		e.logger.Info("edge listener read its project's repositories again; its sessions' GitHub events are decided by the new answer",
			slog.String("project", project), slog.Int("failed_reads", failed))
	}
}

// readEach reads the answer of each of projects, each on its own and at most
// edgeRepositoryReadsAtOnce at once, and returns once every read has ended: each project's
// repositories and error, in projects' order.
func (e *edgeRepositories) readEach(ctx context.Context, projects []string) ([][]string, []error) {
	answers := make([][]string, len(projects))
	errs := make([]error, len(projects))
	slots := make(chan struct{}, edgeRepositoryReadsAtOnce)
	var reads sync.WaitGroup
	for i, project := range projects {
		reads.Add(1)
		go func() {
			defer reads.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			answers[i], errs[i] = e.read(ctx, project)
		}()
	}
	reads.Wait()
	return answers, errs
}

// excerpt is body as a log line quotes it: trimmed, and cut at edgeRepositoryErrorExcerpt bytes.
func excerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > edgeRepositoryErrorExcerpt {
		return text[:edgeRepositoryErrorExcerpt] + "…"
	}
	return text
}

// standing is the repositories the fresh answers of projects name, and whether every one of
// projects has an answer younger than edgeRepositoryAnswerLife. With complete false the list is
// only what the fresh answers name, so a caller may admit by it but never take an absence from it
// as the repository's leaving.
func (e *edgeRepositories) standing(projects []string) (repositories []string, complete bool) {
	now := e.now()
	complete = len(projects) > 0
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, project := range projects {
		answer, ok := e.answers[project]
		if !ok || now.Sub(answer.readAt) >= edgeRepositoryAnswerLife {
			complete = false
			continue
		}
		repositories = append(repositories, answer.repositories...)
	}
	return repositories, complete
}

// relayVerdict is whether the relay withholds a GitHub frame on subject, whose stored data is
// data, from a session whose token lists projects, and why: its only caller (edgeFrameVerdict)
// has already read that subject is a GitHub topic. The frame is relayed while a fresh answer of
// the session's projects names the repository the event itself names (eventRepository), withheld
// not_in_project when the event names none or once every one of those answers is fresh and none
// names it, withheld ambiguous_key or prototype_key when its payload or repo key is one readers
// disagree on (frameKeyWithheld), and withheld answer_unknown otherwise. The event's own
// repository decides, not the subject's tokens: a subject writes a dot in a name as an underscore
// (contracts.SanitizeSubjectSegment), so acme/a.b and acme/a_b publish on one subject, and an
// answer naming one must not admit the other's events.
func (e *edgeRepositories) relayVerdict(projects []string, subject string, data []byte) frameVerdict {
	owner, name, named, err := eventRepository(data)
	if err != nil {
		return frameKeyWithheld(err)
	}
	if !named {
		return frameDrops(withheldNotInProject)
	}
	repositories, complete := e.standing(projects)
	if repositoryNamed(repositories, owner, name) {
		return frameVerdict{action: framePass, answered: true}
	}
	if complete {
		return frameVerdict{action: frameDrop, reason: withheldNotInProject, answered: true}
	}
	return frameAnswerUnknownVerdict()
}

// eventRepository is the owner and name of the repository a GitHub event names, data being its
// stored envelope: the payload's repo, which every GitHub event carries (contracts' githubPayload,
// and the CI settlement's summary), and false for an event that names none, whose envelope or
// payload is no JSON object encoding/json reads (frameIsJSON, json.Valid: text JSON.parse refuses
// too, which the plugin shows raw, or a document nested deeper than 10,000), or whose repo is no
// owner/name. The payload and its repo are each read by their exact keys, as the plugin reads the
// event it shows the session (exactFramePayload, exactJSONString), and an event that names either
// under more than one key, or under a key that differs from its name only in case, is
// errAmbiguousFrameKey: encoding/json, which read both before, takes the last key of any case, so
// a later "Payload" or "Repo" key passed an event under a repository of the session's projects
// that the session then read as another repository's. An envelope or payload with a top-level
// "__proto__" key is errPrototypeFrameKey.
func eventRepository(data []byte) (owner, name string, named bool, err error) {
	if !frameIsJSON(data) {
		return "", "", false, nil
	}
	payload, named, err := exactFramePayload(data)
	if err != nil || !named {
		return "", "", false, err
	}
	repo, named, err := exactJSONString(payload, "repo")
	if err != nil || !named {
		return "", "", false, err
	}
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false, nil
	}
	return owner, name, true, nil
}

// repositoryNamed reports whether one of repositories, each owner/name as Dispatch answers it,
// is owner/name itself, owner and name each compared ignoring case and nothing else: Dispatch
// answers a repository lower-cased (canonicalRepo) while the event keeps GitHub's own case, and
// GitHub's names are case-insensitive, so Acme/Pub's events are acme/pub's; acme/a_b's are not
// acme/a.b's.
func repositoryNamed(repositories []string, owner, name string) bool {
	for _, repository := range repositories {
		answerOwner, answerName, _ := strings.Cut(repository, "/")
		if strings.EqualFold(answerOwner, owner) && strings.EqualFold(answerName, name) {
			return true
		}
	}
	return false
}

// githubSubjectRepository is the owner and name a GitHub subject or topic names, its third and
// fourth tokens, and false for one that names no single repository: fewer tokens, or a wildcard
// or an empty token in either place.
func githubSubjectRepository(subject string) (owner, name string, ok bool) {
	rest, found := strings.CutPrefix(subject, contracts.GithubTopicPrefix)
	if !found {
		return "", "", false
	}
	tokens := strings.SplitN(rest, ".", 3)
	if len(tokens) < 2 || !isRepositoryToken(tokens[0]) || !isRepositoryToken(tokens[1]) {
		return "", "", false
	}
	return tokens[0], tokens[1], true
}

// isRepositoryToken reports whether token names one owner or one name: neither empty nor a
// wildcard.
func isRepositoryToken(token string) bool {
	return token != "" && token != "*" && token != ">"
}

// repositoryAmong reports whether one of repositories, each owner/name as Dispatch answers it,
// is one a topic whose repository tokens are owner and name can follow: the tokens
// contracts.GithubRepositoryPrefix writes for it, compared ignoring case. A topic is all a
// registration names, and it cannot write a dot in a name (acme/a.b is followed as acme.a_b), so
// a topic is taken on these tokens; each event it brings is then held to the repository it names
// itself at relay (relayVerdict).
func repositoryAmong(repositories []string, owner, name string) bool {
	for _, repository := range repositories {
		answerOwner, answerName, _ := strings.Cut(repository, "/")
		tokens := strings.Split(strings.TrimPrefix(contracts.GithubRepositoryPrefix(answerOwner, answerName), contracts.GithubTopicPrefix), ".")
		if len(tokens) == 2 && strings.EqualFold(tokens[0], owner) && strings.EqualFold(tokens[1], name) {
			return true
		}
	}
	return false
}
