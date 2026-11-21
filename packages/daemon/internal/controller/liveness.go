package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
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
	// lookup is the role lookup's URL without envoy_url's userinfo, the one URL the request and
	// every log line use, so no line can name the listener's credential; user is that userinfo,
	// sent as Basic auth when no bearer token is (as the HTTP client sends a URL's own).
	lookup string
	user   *url.Userinfo
	token  string
	log    *slog.Logger
	client *http.Client
}

// NewProber is a Prober over opts. An envoy_url that does not parse, which the loader refuses
// (config.validURL), leaves no lookup, and every probe of it is Unknown. Its client follows no
// redirect: the lookup carries its credential in an explicit Authorization header, which Go's
// client copies to a redirect's target on the same host or a subdomain, and the listener answers
// the lookup itself, so a redirect is an answer the probe reads as Unknown.
func NewProber(opts ProberOptions) *Prober {
	p := &Prober{token: opts.EnvoyToken, log: opts.Log, client: &http.Client{
		Timeout:       probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if parsed, err := url.Parse(strings.TrimRight(opts.EnvoyURL, "/")); err == nil {
		p.user, parsed.User = parsed.User, nil
		p.lookup = parsed.String() + "/v1/roles/" + url.PathEscape(string(claim.ControllerToken(opts.Project)))
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// The listener's live-holder answer is listenerwire.RoleHolder (roleGetHandler,
// packages/envoy/cmd/listener/api.go), whose `last_seen` is in unix milliseconds, and its 404 for
// a role it will not name a holder of is listenerwire.RoleRefusal (writeRoleHolderError,
// packages/envoy/cmd/listener/role_holder.go): `reason` is `unclaimed` when nobody holds the role
// and `holder_lapsed` when its holder is past the live window, and `holder_state` is that
// holder's state, `unresponsive` or `gone`. A listener of an earlier release names neither. A
// holder the listener calls unresponsive is stalled, not dead — it keeps its role claim, nothing
// is released for it, and it answers for itself once its heartbeats land — so a probe of one is
// never a death verdict.

// Probe says whether session — the one registered with the current controller capability — is
// alive: Gone and Unknown each log why, naming the listener and, where the listener named them,
// both sessions or the holder's state.
func (p *Prober) Probe(ctx context.Context, session string) Liveness {
	if p.lookup == "" {
		p.log.Warn("controller liveness: envoy_url does not parse as a URL, so the controller role lookup cannot be sent")
		return Unknown
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.lookup, nil)
	if err != nil {
		p.log.Warn("controller liveness: build the role lookup", "url", p.lookup, "error", err)
		return Unknown
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	} else if p.user != nil {
		password, _ := p.user.Password()
		request.SetBasicAuth(p.user.Username(), password)
	}
	response, err := p.client.Do(request)
	if err != nil {
		p.log.Warn("controller liveness: the Envoy listener is unreachable", "url", p.lookup, "error", err)
		return Unknown
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound {
		// A 404 whose body could not be read to its end may have named an unresponsive holder,
		// which is not gone, so what it said is unknown: a reset mid-body is no answer that the
		// controller is gone.
		if readErr != nil {
			p.log.Warn("controller liveness: the Envoy listener's 404 to the controller role lookup could not be read to its end",
				"url", p.lookup, "session", session, "error", readErr)
			return Unknown
		}
		var refusal listenerwire.RoleRefusal
		_ = json.Unmarshal(body, &refusal)
		if refusal.HolderState == notify.StateUnresponsive {
			p.log.Info("controller liveness: the controller role's holder is unresponsive, not gone; the controller is left as it is",
				"url", p.lookup, "session", session, "reason", refusal.Reason, "lastSeen", refusal.LastSeen)
			return Unknown
		}
		p.log.Info("controller liveness: the controller role has no live holder; the controller is gone",
			"url", p.lookup, "session", session, "reason", refusal.Reason, "holderState", refusal.HolderState)
		return Gone
	}
	if response.StatusCode != http.StatusOK {
		reason := "the Envoy listener refused the controller role lookup"
		if p.token == "" {
			reason += " with no bearer token sent"
		}
		p.log.Warn("controller liveness: "+reason, "url", p.lookup, "status", response.StatusCode)
		return Unknown
	}
	var answer listenerwire.RoleHolder
	if readErr != nil || json.Unmarshal(body, &answer) != nil || answer.Holder == "" || answer.LastSeen == nil {
		p.log.Warn("controller liveness: the Envoy listener answered the controller role lookup with an unreadable body", "url", p.lookup)
		return Unknown
	}
	if answer.Holder != session {
		p.log.Info("controller liveness: another session holds the controller role; the recorded controller is gone",
			"holder", answer.Holder, "session", session)
		return Gone
	}
	return Live
}
