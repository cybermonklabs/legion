package natscallout

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/natsgrant"
)

// scopeGrant is grant, entry's kind's grant (serviceGrant), held to the projects entry may reach.
//
// An all-projects entry takes that grant whole.
//
// Any other entry is held to the projects the broker's own exchange list gives its account
// (Deps.Exchange), the one list that also decides the projects of a token minted for that
// account, and to what the token exchange holds that token to: the person its exchange entry acts
// for must see each project, as projectaccess.Visible requires before a mint. So a connection and
// that account's token cannot disagree. WithinProjects writes the account's own reply prefix
// (natsgrant.InboxPrefix) in place of _INBOX.>, and keeps the GitHub events whole, since a GitHub
// event is a message of no project, so a held daemon hears every repository's events.
// Each project's answer is the one Dispatch last gave about it (heldAccess), held on its own. It
// is refused, naming why: an account no exchange entry lists, or whose entry names no project;
// Dispatch refusing the broker's own account token, until Dispatch answers a question asked after
// the refusal was seen (heldAccess); a project Dispatch has
// not yet answered about; a project Dispatch does not hold, or whose answer says the entry's
// person cannot see it; and a grant entry natsgrant.WithinProjects cannot hold. ParseServices
// refuses a dispatch or listener entry with "-" at start, since their grants carry KV calls.
//
// Dispatch is never asked inside a decision: a decision reads only the answers the service holds.
//
// A daemon entry held this way is narrowed to natsgrant.WithinProjects(natsgrant.Daemon(), scope),
// whose consumers are the ones a Legion daemon opens once it opens its consumers per project, and
// which holds a daemon to those consumers by name, not by their filter. So no deployment may list
// a daemon entry with "-" until the plan's precondition holds. Here it is word for word, with
// each plan task it names written as what it does and each note of ours in brackets:
//
//	Held until `daemon-release-gate.sh --listeners <the commit that landed [the daemon that
//	opens its consumers per project]>` and `daemon-release-gate.sh <the commit that landed [the
//	daemon that opens its consumers per project]>` [each with `--deployment <owner/repo>`, which
//	the script requires] both print `GATE OPEN`, each read once and
//	quoted where this step is turned on, and the stored-wake read below finds nothing a
//	`legion-go-<P>-ready` consumer created then would deliver but `listener_ready`. The step
//	also waits on `daemon-running-gate.sh --deployment <owner/repo> <the deploy-4 commit>`
//	printing that every running daemon is at or after deploy 4, and on the consumer-filter read
//	below; the pin gates alone read the repository, not the daemons running, and are extra
//	evidence only.
//
//	And the consumer-filter read, [that daemon's] deploy-4 production read, quoted where this
//	step is turned on: every daemon durable `legion-go-<P>-{dispatch,wake,ready,github}` carries
//	exactly its kind's filter set (`-dispatch` and `-wake` the project's own lane, `-ready` the
//	shared wake subject's `listener_ready` only [the filter is the subject
//	notifications.legion.wake itself, which carries only listener_ready once every listener
//	publishes each project's wakes on that project's lane], `-github` `notifications.github.>`),
//	none on `notifications.dispatch.issue.>` or the whole shared wake subject [for -dispatch and
//	-wake], and every daemon runs a build at or after deploy 4 (`daemon-release-gate.sh` prints
//	`GATE OPEN` for that commit), so no static-grant daemon can widen one again; a failed read
//	prints UNKNOWN and holds the step.
//
// daemon-release-gate.sh is in packages/envoy/scripts; daemon-running-gate.sh comes with the
// change that lands the daemon that opens its consumers per project. The change that lists daemon
// entries with "-" waits on that precondition, with those readings quoted in its pull request.
func scopeGrant(deps Deps, entry ServiceEntry, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	if entry.AllProjects {
		return grant, nil
	}
	if deps.Exchange == nil || deps.Access == nil {
		return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to exchange projects, which this broker cannot read: service "+entry.name())
	}
	person, projects, listed := deps.Exchange(entry.Account)
	switch {
	case !listed:
		return natsgrant.Permissions{}, refuse(labelNoExchangeEntry, "account has no exchange entry: "+entry.Account)
	case len(projects) == 0:
		return natsgrant.Permissions{}, refuse(labelNoProjects, "account's exchange entry names no project to hold it to: "+entry.Account)
	}
	if err := deps.held.refusal(entry, person, projects); err != nil {
		return natsgrant.Permissions{}, err
	}
	scoped, err := natsgrant.WithinProjects(grant, natsgrant.Scope{Projects: projects, Identity: entry.Account})
	if err != nil {
		return natsgrant.Permissions{}, refuse(labelGrantNotHoldable, "grant cannot be held to its projects: service "+entry.name()+": "+err.Error())
	}
	return scoped, nil
}

// accessRefresh is how often the service reads Dispatch again, and accessFetchTimeout how long one
// question may take. accessRefresh is a var so a test can shorten it.
var accessRefresh = 20 * time.Second

const accessFetchTimeout = 10 * time.Second

// heldAccess is what the service holds of Dispatch for the service entries held to their
// exchange projects, so that no decision waits on Dispatch: the decision budget is 1.5 s
// (decideBudget), and a Dispatch call may take ten. A refresh asks, in parallel, Dispatch's answer
// about every project such an entry can name, for the person that entry's exchange entry acts
// for; run makes one at the start and one every accessRefresh after.
//
// Each project's answer is held on its own: an answer replaces the one held about its question,
// whatever the refresh's other questions did, and a question that fails (Dispatch unreachable, a
// timeout, an answer past its bound) keeps the answer held about it, logged at warn. A held answer
// is used however old it is. A question no refresh has answered yet has no answer, and a held
// connection that needs it is refused until one does. The one exception joins a whole refresh:
// Dispatch refusing the broker's own account token (a 401 or 403, projectaccess.ErrBrokerRefused)
// on any question of a refresh refuses every held connection, since the token exchange refuses
// every trade that asks Dispatch then; it is logged at error once a refresh. The refusal clears at
// the first later refresh that gets no refusal of the broker's token and holds an answer whose
// question was asked after the refusing refresh had all its answers (refusalSeen): Dispatch then
// answered with the token after it refused it. An answer the Dispatch client kept from before the
// refusal (projectaccess.CacheTTL; cmd/cipherd shares that client with the token exchange, so a
// person's trade can fill its cache) does not clear it, nor does a not-found, whose age the client
// does not report, nor a refresh that reached Dispatch for no question. An answer a person's trade
// left in that cache after the refusal does clear it. When Dispatch takes the token back inside the
// refusing refresh's own window, a trade's answer from that window, kept up to
// projectaccess.CacheTTL, does not count, so the clear can wait up to that long for an answer asked
// later. While Dispatch answers, an answer held is at most about a minute old: the 30 s
// (projectaccess.CacheTTL) the Dispatch client may have kept it, the accessRefresh to the next
// refresh, and the accessFetchTimeout its question may take.
type heldAccess struct {
	access func(ctx context.Context, project, person string) (projectaccess.Answer, error)
	asks   []heldKey
	log    *slog.Logger

	mu      sync.RWMutex
	answers map[heldKey]heldAnswer
	// brokerRefused is whether Dispatch refused the broker's own account token in a refresh and
	// no later refresh has cleared it; refusalSeen is when the last refresh that got such a
	// refusal had all its answers, after which an answer's question must have been asked to clear
	// it.
	brokerRefused bool
	refusalSeen   time.Time
}

// heldKey is one question the service asks Dispatch: whether person may see project.
type heldKey struct{ project, person string }

// heldAnswer is one question's answer: that Dispatch holds no such project, or whether the
// person may see it.
type heldAnswer struct{ notFound, visible bool }

// newHeldAccess holds the answers about every project deps.Exchange gives an entry of services
// that is not an all-projects one, each asked for the person its exchange entry acts for; nil when
// the broker cannot read them (no exchange list, or no Dispatch client) or no entry can name a
// project.
func newHeldAccess(deps Deps, services []ServiceEntry) *heldAccess {
	if deps.Exchange == nil || deps.Access == nil {
		return nil
	}
	var asks []heldKey
	for _, entry := range services {
		if entry.AllProjects {
			continue
		}
		person, projects, _ := deps.Exchange(entry.Account)
		for _, project := range projects {
			asks = append(asks, heldKey{project: project, person: person})
		}
	}
	slices.SortFunc(asks, func(a, b heldKey) int {
		return cmp.Or(cmp.Compare(a.project, b.project), cmp.Compare(a.person, b.person))
	})
	asks = slices.Compact(asks)
	if len(asks) == 0 {
		return nil
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &heldAccess{access: deps.Access, asks: asks, log: log, answers: make(map[heldKey]heldAnswer, len(asks))}
}

// run makes a refresh now and every accessRefresh after, until ctx ends, one at a time: each
// refresh returns before the next begins, so no two overlap. On a nil heldAccess (no entry held to
// projects) it returns at once.
func (h *heldAccess) run(ctx context.Context) {
	if h == nil {
		return
	}
	ticker := time.NewTicker(accessRefresh)
	defer ticker.Stop()
	for {
		h.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh asks Dispatch every held question, all at once, each bounded by accessFetchTimeout, and
// holds each answer it gets in place of the one held about its question, whatever its other
// questions did, a refusal of the broker's token among them. A refusal of the broker's token sets
// brokerRefused and refusalSeen; a refresh with none clears brokerRefused when one of its answers
// was asked after refusalSeen (askedAfter). It holds all of that before it writes any log line,
// since a write can block (report.go). It logs a refusal at error, naming the project whose
// question Dispatch refused, since every held connection is then refused; else each failed
// question at warn, since the answer held about it is kept. refresh is not safe to run beside
// itself; run calls it one at a time.
func (h *heldAccess) refresh(ctx context.Context) {
	answers := make([]heldAnswer, len(h.asks))
	// errs is each question's failure, nil for one answered; askedAt is when Dispatch was asked the
	// question an answer answers (projectaccess.Answer.AskedAt), zero for a not-found or a source
	// that keeps no clock.
	errs := make([]error, len(h.asks))
	askedAt := make([]time.Time, len(h.asks))
	var wait sync.WaitGroup
	for i, ask := range h.asks {
		wait.Go(func() {
			fetch, cancel := context.WithTimeout(ctx, accessFetchTimeout)
			defer cancel()
			answer, err := h.access(fetch, ask.project, ask.person)
			switch {
			case errors.Is(err, projectaccess.ErrProjectNotFound):
				answers[i] = heldAnswer{notFound: true}
			case err != nil:
				errs[i] = err
			default:
				answers[i], askedAt[i] = heldAnswer{visible: answer.Visible}, answer.AskedAt
			}
		})
	}
	wait.Wait()
	answersIn := time.Now()
	refused := slices.IndexFunc(errs, func(err error) bool { return errors.Is(err, projectaccess.ErrBrokerRefused) })
	h.mu.Lock()
	for i, ask := range h.asks {
		if errs[i] == nil {
			h.answers[ask] = answers[i]
		}
	}
	switch {
	case refused >= 0:
		h.brokerRefused, h.refusalSeen = true, answersIn
	case h.brokerRefused && askedAfter(answers, errs, askedAt, h.refusalSeen):
		h.brokerRefused = false
	}
	h.mu.Unlock()
	if refused >= 0 {
		h.log.Error("natscallout: Dispatch refuses the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), so every daemon held to its exchange projects is refused until Dispatch answers a question asked after this refresh with no such refusal", "question", "project "+h.asks[refused].project, "error", errs[refused].Error())
		return
	}
	if ctx.Err() != nil {
		return
	}
	for i, err := range errs {
		if err != nil {
			h.log.Warn("natscallout: Dispatch could not be asked about a project, so the answer held about it is kept", "question", "project "+h.asks[i].project, "error", err.Error())
		}
	}
}

// refusal is entry's refusal by the answers held for person about projects, decided under one
// read lock, and nil when they admit it: a refusal of the broker's own account token no later
// refresh has cleared (brokerRefused), before any project; then the first project no refresh has
// answered yet, one Dispatch does not hold, or one the person cannot see. projects is not empty.
// On a nil heldAccess no project has an answer.
func (h *heldAccess) refusal(entry ServiceEntry, person string, projects []string) error {
	if h == nil {
		return refuse(labelProjectAccessUnknown, "project access not yet known: "+projects[0])
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.brokerRefused {
		// The token exchange refuses every trade that asks Dispatch while it refuses the broker's
		// token (oauth's refusedByDispatch), so every held connection is refused too, whichever
		// question of the refresh Dispatch refused.
		return refuse(labelAccessRefused, "Dispatch refuses the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), so what the account's person may see is not known: service "+entry.name())
	}
	for _, project := range projects {
		held, known := h.answers[heldKey{project: project, person: person}]
		switch {
		case !known:
			// No refresh has answered about this project yet (the first is still running, or
			// Dispatch was down from the start); the client retries.
			return refuse(labelProjectAccessUnknown, "project access not yet known: "+project)
		case held.notFound:
			return refuse(labelProjectNotFound, "project not found: "+project)
		case !held.visible:
			// The token exchange refuses this entry's token the same way (projectaccess.Visible):
			// its person cannot see the project, so neither may the connection.
			return refuse(labelProjectNotVisible, "project not visible to the account's person: "+project)
		}
	}
	return nil
}

// askedAfter reports whether one of a refresh's answers shows Dispatch answering the broker's token
// after seen: an answer (not a not-found, whose age the client does not report) whose question was
// asked after seen, or that comes from a source that keeps no clock (a zero AskedAt, which
// projectaccess.Visible also takes as fresh). An answer the Dispatch client kept from before seen
// is not one, since it says nothing about whether Dispatch refuses the broker's token now.
func askedAfter(answers []heldAnswer, errs []error, askedAt []time.Time, seen time.Time) bool {
	for i := range answers {
		if errs[i] == nil && !answers[i].notFound && (askedAt[i].IsZero() || askedAt[i].After(seen)) {
			return true
		}
	}
	return false
}
