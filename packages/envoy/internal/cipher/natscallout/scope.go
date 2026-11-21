package natscallout

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/natsgrant"
)

// scopeGrant is grant, entry's kind's grant, held to the projects entry may reach.
//
// Dispatch's grant gains the write of the private-projects bucket (bus.PrivateProjectsBucket),
// which Dispatch keeps, and a listener's its read, which a listener holds each session to its
// token's projects by. The static grants a hub renders do not change (natsgrant.Dispatch and
// natsgrant.Listener hold neither), so on a hub with no auth service a listener's read is refused
// and reads as no project being private, which is true there: a private project needs the auth
// service.
//
// Every answer reads only its own replies (ownReplies): an all-projects entry takes that grant
// whole but for its reply inboxes. Any other entry is held to the projects the broker's own
// exchange list gives its account (Deps.Exchange), the one list that also decides the projects of
// a token minted for that account, and to what the
// token exchange holds that token to: the person its exchange entry acts for must see each
// project, as projectaccess.Visible requires before a mint. So a connection and that account's
// token cannot disagree. WithinProjects writes that same reply prefix in place of _INBOX.>, and
// keeps the GitHub events whole, since a GitHub event is a message of no project. Every answer the
// grant is built from comes from one complete pass (heldAccess), never two mixed, narrowed by the
// refusals of any later pass. It is refused, naming why: an account no exchange entry lists, or
// whose entry names no project; no complete pass held yet; Dispatch refusing the broker's own
// account token in that pass; a project Dispatch does not hold, or whose answer says the entry's
// person cannot see it; and a grant entry natsgrant.WithinProjects cannot hold. ParseServices
// refuses a dispatch or listener entry with "-" at start, since their grants carry KV calls.
//
// Dispatch is never asked inside a decision: a decision reads only the pass the service holds.
//
// A daemon entry held this way is narrowed to natsgrant.WithinProjects(natsgrant.Daemon(), scope),
// whose consumers are the ones a Legion daemon opens once it opens its consumers per project. So
// no deployment may list a daemon entry with "-" until the release gate for that daemon opens:
// both `packages/envoy/scripts/daemon-release-gate.sh --listeners <the commit that lands it>` and
// `daemon-release-gate.sh <that commit>` print GATE OPEN, and the listener leg's GATE OPEN has been
// read twice, 72 hours apart, because the shared notifications.legion.wake subject keeps other
// projects' wakes for the stream's 72-hour retention after the last listener moves. The change
// that lists daemon entries with "-" waits on that gate, with both readings quoted in its pull
// request.
func scopeGrant(_ context.Context, deps Deps, entry ServiceEntry, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	switch entry.Kind {
	case KindDispatch:
		grant.Publish = append(slices.Clip(grant.Publish), natsgrant.KVWrite(bus.PrivateProjectsBucket)...)
	case KindListener:
		grant.Publish = append(slices.Clip(grant.Publish), natsgrant.KVRead(bus.PrivateProjectsBucket)...)
	}
	if entry.AllProjects {
		return ownReplies(entry, grant), nil
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
	pass := deps.held.latest()
	if pass == nil {
		return natsgrant.Permissions{}, refuse(labelNoCompletePass, "no complete read of Dispatch's project answers is held yet: service "+entry.name())
	}
	if pass.refused {
		// The token exchange refuses every trade that asks Dispatch while it refuses the broker's
		// token (oauth's refusedByDispatch), so every held connection is refused too, whichever
		// question of the pass Dispatch refused.
		return natsgrant.Permissions{}, refuse(labelAccessRefused, "Dispatch refuses the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), so what the account's person may see is not known: service "+entry.name())
	}
	for _, project := range projects {
		held, known := pass.answers[heldKey{project: project, person: person}]
		switch {
		case !known:
			// A pass asks about every project an entry names (newHeldAccess), so a complete one
			// answers each; one that does not is no answer to admit on.
			return natsgrant.Permissions{}, refuse(labelNoCompletePass, "the complete read of Dispatch's answers holds none about project "+project)
		case held.notFound:
			return natsgrant.Permissions{}, refuse(labelProjectNotFound, "project not found: "+project)
		case !held.answer.Visible:
			// The token exchange refuses this entry's token the same way (projectaccess.Visible):
			// its person cannot see the project, so neither may the connection.
			return natsgrant.Permissions{}, refuse(labelProjectNotVisible, "project not visible to the account's person: "+project)
		}
	}
	scoped, err := natsgrant.WithinProjects(grant, natsgrant.Scope{Projects: projects, Identity: entry.Account})
	if err != nil {
		return natsgrant.Permissions{}, refuse(labelGrantNotHoldable, "grant cannot be held to its projects: service "+entry.name()+": "+err.Error())
	}
	return ownReplies(entry, scoped), nil
}

// everyReply is the subscribe nats.go's own inboxes take, where every client's replies arrive.
const everyReply = nats.InboxPrefix + ">"

// ownReplies is grant reading only the replies of entry's own account: each subscribe to _INBOX.>,
// where every client's replies arrive and so where one service would read another's JetStream
// answers and push deliveries, is written as the account's own reply prefix
// (natsgrant.InboxPrefix(entry.Account)+".>"), which its client sets (internal/bus, keptService;
// the daemon's natsauth.ConnectService), as WithinProjects writes it for an entry held to its
// projects; and a listener's and Dispatch's grant also subscribe natsgrant.SessionReplyInbox of
// that account, where the sessions they ask answer (the relay's receipts, the connection probe's
// answer and the agent stream's replay). entry.Account is the account its verified token names, as
// the client reads it from the same token (natscred.Identity). A static grant a hub renders is not
// this one: it keeps _INBOX.>, and a hub that renders static users holds no private project.
//
// A subscribe that is not _INBOX.> is kept as it is: the measurement job's own fixed prefix
// (natsgrant.Measure) among them, which one measure entry alone holds (ParseServices).
func ownReplies(entry ServiceEntry, grant natsgrant.Permissions) natsgrant.Permissions {
	subscribe := make([]string, 0, len(grant.Subscribe)+1)
	for _, subject := range grant.Subscribe {
		if subject == everyReply {
			subject = natsgrant.InboxPrefix(entry.Account) + ".>"
		}
		subscribe = append(subscribe, subject)
	}
	switch entry.Kind {
	case KindListener, KindDispatch:
		subscribe = append(subscribe, natsgrant.SessionReplyInbox(entry.Account)+".>")
	}
	grant.Subscribe = subscribe
	return grant
}

// accessRefresh is how often the service reads Dispatch again, and accessFetchTimeout how long one
// question may take. accessRefresh is a var so a test can shorten it.
var accessRefresh = 20 * time.Second

const accessFetchTimeout = 10 * time.Second

// heldAccess is what the service holds of Dispatch for the service entries held to their
// exchange projects, so that no decision waits on Dispatch: the decision budget is 1.5 s
// (decideBudget), and a Dispatch call may take ten. A pass (refresh) asks, in parallel, Dispatch's
// answer about every project such an entry can name, for the person that entry's exchange entry
// acts for; run makes one at the start and one every accessRefresh after. A held entry's grant is
// built from one complete pass (heldPass) and never from two mixed. A pass every question of which
// Dispatch answered replaces the one held. A pass any question of which failed (Dispatch
// unreachable, a timeout, an answer past its bound) adds nothing, but every refusal any of its
// answered questions yields applies at once (narrow), whatever else failed: a project it answers
// not found or not visible is refused so, since the token exchange refuses a removed person's token
// whatever else Dispatch answers; each failure is logged at warn. Dispatch refusing the broker's
// own account token (a 401 or 403, projectaccess.ErrBrokerRefused) on any question of a pass is
// the pass's outcome whatever its other questions did, failures included: the token exchange
// refuses every trade that asks Dispatch then (oauth's refusedByDispatch), so that pass is held,
// refuses every held connection until a later pass replaces it, and is logged at error once a
// pass. With no complete pass held yet, a held connection is refused, and its client retries; the
// newest complete pass, narrowed so, is used however old it is, so while Dispatch cannot be
// reached a connection is admitted or refused as Dispatch last answered a whole pass, or refused
// as any later answer said. While Dispatch answers every question, the pass held is at most about
// a minute old: the 30 s (projectaccess.CacheTTL) the Dispatch client may have kept an answer, the
// accessRefresh to the next pass, and the accessFetchTimeout its questions may take.
type heldAccess struct {
	access func(ctx context.Context, project, person string) (projectaccess.Answer, error)
	asks   []heldKey
	now    func() time.Time
	log    *slog.Logger

	mu   sync.RWMutex
	pass *heldPass
}

// heldKey is one question the service asks Dispatch: whether person may see project.
type heldKey struct{ project, person string }

// heldAnswer is one question's answer: Dispatch's, or that Dispatch holds no such project.
type heldAnswer struct {
	answer   projectaccess.Answer
	notFound bool
}

// heldPass is one pass a decision reads: every held question's answer, as one pass read them,
// from began, read before its first question was sent; or, when refused, that Dispatch refused the
// broker's own account token on one of them. Nothing changes a pass once it is held.
type heldPass struct {
	began   time.Time
	answers map[heldKey]heldAnswer
	// refused is Dispatch refusing the broker's own account token on any of the pass's questions;
	// the rest of a refused pass is never read.
	refused bool
}

// newHeldAccess holds the passes over every project deps.Exchange gives an entry of services that
// is not an all-projects one, each asked for the person its exchange entry acts for; nil when the
// broker cannot read them (no exchange list, or no Dispatch client) or no entry can name a project.
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
	return &heldAccess{access: deps.Access, asks: asks, now: deps.now, log: log}
}

// run makes a pass now and every accessRefresh after, until ctx ends.
func (h *heldAccess) run(ctx context.Context) {
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

// refresh makes one pass: it asks Dispatch every held question, all at once, each bounded by
// accessFetchTimeout, and holds the pass once every one has come back answered, or once any came
// back refused, whatever the others did (hold). It logs what it did with the pass: a refusal at
// error, naming the project whose question Dispatch refused, since every held daemon is then
// refused; else each failed question at warn, since the last complete pass is kept.
func (h *heldAccess) refresh(ctx context.Context) {
	pass := &heldPass{began: h.now()}
	answers := make([]heldAnswer, len(h.asks))
	// errs is each question's failure, nil for one answered.
	errs := make([]error, len(h.asks))
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
				answers[i] = heldAnswer{answer: answer}
			}
		})
	}
	wait.Wait()
	if i := slices.IndexFunc(errs, func(err error) bool { return errors.Is(err, projectaccess.ErrBrokerRefused) }); i >= 0 {
		h.log.Error("natscallout: Dispatch refuses the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE), so every daemon held to its exchange projects is refused until a later pass is answered", "question", "project "+h.asks[i].project, "error", errs[i].Error())
		h.hold(&heldPass{began: pass.began, refused: true})
		return
	}
	answered := make(map[heldKey]heldAnswer, len(h.asks))
	for i, ask := range h.asks {
		if errs[i] == nil {
			answered[ask] = answers[i]
		}
	}
	if slices.ContainsFunc(errs, func(err error) bool { return err != nil }) {
		if ctx.Err() == nil {
			for i, err := range errs {
				if err != nil {
					h.log.Warn("natscallout: Dispatch could not be asked a question of this pass, so the last complete pass is kept, narrowed by this pass's refusals", "question", "project "+h.asks[i].project, "error", err.Error())
				}
			}
		}
		h.narrow(pass.began, answered)
		return
	}
	pass.answers = answered
	h.hold(pass)
}

// narrow applies to the pass held the refusals an incomplete pass, begun at began, got in
// answers: a held key it answers not found or not visible is refused so at once. It adds nothing:
// a key answered visible again waits for a complete pass, so the grant still never holds what one
// complete pass did not. A pass held as refused stays refused.
func (h *heldAccess) narrow(began time.Time, answers map[heldKey]heldAnswer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.pass
	if held == nil || held.refused || began.Before(held.began) {
		return
	}
	narrowed := &heldPass{began: began, answers: maps.Clone(held.answers)}
	for key, fresh := range answers {
		kept, ok := narrowed.answers[key]
		if ok && !kept.notFound && kept.answer.Visible && (fresh.notFound || !fresh.answer.Visible) {
			narrowed.answers[key] = fresh
		}
	}
	h.pass = narrowed
}

// hold holds pass in place of the one held, unless that one began later, which a slower pass must
// not replace.
func (h *heldAccess) hold(pass *heldPass) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pass == nil || !pass.began.Before(h.pass.began) {
		h.pass = pass
	}
}

// latest is the pass held, nil when none is: on a nil heldAccess too.
func (h *heldAccess) latest() *heldPass {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pass
}
