package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/redirects"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// probeTimeout bounds one role lookup, so a hung listener answers Unknown rather than holding the
// caller.
const probeTimeout = 10 * time.Second

// Liveness is what the Envoy listener says of the registered controller session.
type Liveness string

const (
	// Live: the listener names the session as the controller role's holder, which it does only
	// for a holder inside its live window.
	Live Liveness = "live"
	// Gone: the role is unheld or expired, held by another session, or its holder reached the
	// listener's gone horizon.
	Gone Liveness = "gone"
	// Unknown: the listener could not answer — unreachable, a refusal other than 404, or a body
	// the probe cannot read — or it named a holder that is only unresponsive. Never a death
	// verdict.
	Unknown Liveness = "unknown"
)

// ProberOptions are what a Prober reads the listener with.
type ProberOptions struct {
	// EnvoyURL is the listener's base URL; EnvoyToken its bearer ("" sends none).
	EnvoyURL, EnvoyToken string
	// Project is the project token (claim.ProjectToken): the lookup is its controller token.
	Project string
	// Log receives each non-alive verdict's reason; nil is slog.Default().
	Log *slog.Logger
}

// Prober reads the registered controller session's liveness from the Envoy role registry, under
// either `controller` mode: the one record of the operator's controller the daemon can read, since
// its session is on the operator's own machine with no process of the daemon's to probe, and the
// role the daemon's own controller must hold to take its wakes.
//
// The listener owns the windows: it answers the lookup only for a holder inside its live window
// (`ENVOY_SESSION_LIVE_WINDOW`), and names the holder's state in the 404 it answers for any
// other, so the daemon keeps no staleness window of its own to drift from it.
type Prober struct {
	// lookup is the role lookup's URL, "" where the Envoy URL does not parse (NewProber).
	lookup string
	token  string
	log    *slog.Logger
	client *http.Client
}

// NewProber is a Prober over opts. Its lookup goes to the controller token's role route joined
// onto the Envoy URL's path (urljoin.Join), the URL's query kept, where the route appended to the
// URL's text would sit inside its query or its fragment and the listener would be asked another
// path; an Envoy URL that does not parse or names no host leaves it no lookup, and each probe then
// answers Unknown in fixed words, since url.Parse's error quotes the URL whole. The lookup follows
// no redirect (redirects.Refused): it carries the listener's bearer, which a followed redirect
// would hand the host the Location names, and a 3xx is the listener's refusal, which the probe
// reads as Unknown.
func NewProber(opts ProberOptions) *Prober {
	p := &Prober{
		token:  opts.EnvoyToken,
		log:    opts.Log,
		client: redirects.Refused(&http.Client{Timeout: probeTimeout}),
	}
	if lookup, err := urljoin.Join(opts.EnvoyURL, "/v1/roles/"+url.PathEscape(string(claim.ControllerToken(opts.Project)))); err == nil {
		p.lookup = lookup.String()
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// The listener's live-holder answer is listenerwire.RoleHolder (roleGetHandler,
// packages/envoy/cmd/listener/api.go), whose `last_seen` is in unix milliseconds, and its 404 for
// a role it will not name a holder of is listenerwire.RoleRefusal (writeRoleHolderError,
// packages/envoy/cmd/listener/role_holder.go); the probe reads them as listenerwire.RoleHolderAs
// and listenerwire.RoleRefusalAs, each with its words and the holder's state held as
// oauth2call.RemoteText, which print as the rule reads them. `reason` is `unclaimed` when nobody
// holds the role and `holder_lapsed` when its holder is past the live window, and `holder_state`
// is that holder's state, `unresponsive` or `gone`. A listener of an earlier release names
// neither. A holder the listener calls unresponsive is stalled, not dead — it keeps its role
// claim, nothing is released for it, and it answers for itself once its heartbeats land — so a
// probe of one is never a death verdict.

// Probe says whether session — the one registered with the current controller capability — is
// alive: Gone and Unknown each log why, naming the lookup as oauth2call.AskedURL names it (scheme,
// host and path; envoy_url may carry the listener's credential as its user information) and, where
// the listener named them, both sessions or the holder's state. A lookup that could not be made or
// got no answer logs its
// failure through oauth2call.Unanswered; one whose URL http.NewRequest refuses (url.URL.String can
// write text url.Parse refuses) logs no other name of that URL, whose text holds its user
// information and its query whole.
func (p *Prober) Probe(ctx context.Context, session string) Liveness {
	if p.lookup == "" {
		p.log.Warn("controller liveness: the Envoy URL is not an absolute URL with a host (it is not printed)")
		return Unknown
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.lookup, nil)
	if err != nil {
		p.log.Warn("controller liveness: build the role lookup", "error", oauth2call.Unanswered(err, nil, ""))
		return Unknown
	}
	asked := oauth2call.AskedURL(request.URL)
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		p.log.Warn("controller liveness: the Envoy listener is unreachable", "url", asked, "error", oauth2call.Unanswered(err, request, ""))
		return Unknown
	}
	defer response.Body.Close()
	read, readErr := oauth2call.ReadAnswer(response, 1<<20)
	if response.StatusCode == http.StatusNotFound {
		// A 404 whose body could not be read to its end may have named an unresponsive holder,
		// which is not gone, so what it said is unknown: a reset mid-body is no answer that the
		// controller is gone.
		if readErr != nil {
			p.log.Warn("controller liveness: the Envoy listener's 404 to the controller role lookup could not be read to its end",
				"url", asked, "session", session, "error", readErr)
			return Unknown
		}
		// The reason, the state and the holder are the listener's words, so they are compared as
		// they came and logged as the rule reads them (listenerwire.RoleRefusalAs).
		var refusal listenerwire.RoleRefusalAs[oauth2call.RemoteText, oauth2call.RemoteText, oauth2call.RemoteText]
		_ = read.Decode(&refusal)
		if refusal.HolderState.Equal(string(notify.StateUnresponsive)) {
			p.log.Info("controller liveness: the controller role's holder is unresponsive, not gone; the controller is left as it is",
				"url", asked, "session", session, "reason", refusal.Reason, "lastSeen", refusal.LastSeen)
			return Unknown
		}
		p.log.Info("controller liveness: the controller role has no live holder; the controller is gone",
			"url", asked, "session", session, "reason", refusal.Reason, "holderState", refusal.HolderState)
		return Gone
	}
	if response.StatusCode != http.StatusOK {
		reason := "the Envoy listener refused the controller role lookup"
		if p.token == "" {
			reason += " with no bearer token sent"
		}
		p.log.Warn("controller liveness: "+reason, "url", asked, "status", response.StatusCode)
		return Unknown
	}
	var answer listenerwire.RoleHolderAs[oauth2call.RemoteText, oauth2call.RemoteText]
	if readErr != nil || read.Decode(&answer) != nil || answer.Holder.IsEmpty() || answer.LastSeen == nil {
		p.log.Warn("controller liveness: the Envoy listener answered the controller role lookup with an unreadable body", "url", asked)
		return Unknown
	}
	if !answer.Holder.Equal(session) {
		p.log.Info("controller liveness: another session holds the controller role; the recorded controller is gone",
			"holder", answer.Holder, "session", session)
		return Gone
	}
	return Live
}
