// Package githubproxy is a Legion role's GitHub forwarder (`legion forwarder`): a loopback HTTP
// CONNECT proxy that the role's environment names in HTTPS_PROXY and https_proxy, and the keeper
// of the grant it forwards under.
//
// The role holds its own key (the shim makes it in the role's key directory) and the daemon
// enrolls it; the forwarder asks the secrets broker, with that key, for a grant of the role's
// GitHub secrets, which the deployment names by role (runtime.kubernetes.agent_secrets.github).
// The grant lasts an hour, names no repository, and holds one placeholder for each account the
// role's App is installed on, and no token. The forwarder writes each placeholder where the daemon
// writes a token for a role whose secrets are not behind the proxy: one gh directory per account
// (ghconfig.WriteBundle), whose hosts.yml `legion gh` and `legion git-credential` read. A CONNECT
// to a host the grant binds goes to the broker's proxy port with the grant's proxy password; the
// broker swaps each placeholder a request carries for its account's token. Every other CONNECT
// the forwarder opens itself, so a broker outage stops only the calls that need a credential.
//
// The forwarder asks for the next grant with ten minutes of the hour left, and asks again at once
// when the broker's proxy answers 407, which it does for a grant the broker ended early; that one
// connection fails, and the role's next call reads the new grant's placeholders. Before its first
// grant, a CONNECT to one of GitHub's hosts waits for it while an ask can still bring it, and every
// other CONNECT goes out at once. It owns a grant from the moment the broker names it, and every
// revocation it makes, of a grant it cannot use as at its stop, reads that one ledger and drops a
// grant from it only once the broker ended it. It revokes the grants it holds when it stops, at
// the end of the role's generation: first every grant held then, then, once an ask still in
// flight lands or its stop stops waiting for it, every grant held by then, within its stop bound.
//
// A grant it fails to revoke (its stop cut short, its process killed) is not ended by the daemon:
// the daemon's revocation of the generation's enrollment ends the enrollment, and the broker
// carries a grant of a session's enrollment to that session's next one (enroll.endMayCarry). It
// stays live until its own expiry, at most the hour of Lifetime, which is the bound the spec gives
// a missed revocation. The role's next generation holds none of its placeholders, since the
// bundle is the generation's own and goes with it; its forwarder's first ask is handed that grant
// back while it is in the first half of its life (requests' reuseLiveGrant), and revokes it at its
// own stop, and otherwise leaves it unused.
package githubproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Lifetime is a role grant's lifetime: an hour, the life of an App installation token, so a
// placeholder a missed revocation leaves works no longer than the token it replaced did.
const Lifetime = time.Hour

// RenewBefore is how long before a grant expires the forwarder asks for the next one: ten minutes
// of the hour, which leaves room for retries. The broker hands an automatic grant back only in the
// first half of its life (requests' reuseLiveGrant), so the ask gets a new grant, and the old one
// lives on until its own expiry, which lets calls made under it finish.
const RenewBefore = 10 * time.Minute

// githubSlotPrefix opens the slot of one installation of a github-app secret's release, followed
// by the account's login, lowercased (the broker's requests.SlotGitHubPrefix).
const githubSlotPrefix = "github:"

// Broker is the role's session at the secrets broker, as the forwarder uses it
// (*cipher.Session).
type Broker interface {
	Request(ctx context.Context, names []string, reason string, lifetime int) (cipher.RequestAnswer, error)
	WaitRequest(ctx context.Context, id string, wait time.Duration) (cipher.RequestAnswer, error)
	Values(ctx context.Context, id string) (cipher.Release, error)
	Revoke(ctx context.Context, id string) error
}

// Grant is one grant of the role's secrets as the forwarder holds it: its id and expiry, the
// broker's proxy and the password a connection to it opens with, the hosts the grant binds, and
// each account's placeholder, by the account's lowercased login.
type Grant struct {
	ID        string
	ExpiresAt time.Time
	ProxyURL  *url.URL
	Password  string
	CA        string
	Hosts     []string
	Accounts  map[string]string
}

// Binds reports whether the grant sends a CONNECT to host:port through the broker (bindsHost).
func (g *Grant) Binds(host, port string) bool { return bindsHost(g.Hosts, host, port) }

// bindsHost reports whether hosts bind host:port: port 443 and a host of hosts, by its exact name
// or, for an entry `*.<domain>`, under that domain, as the broker's proxy matches them.
func bindsHost(hosts []string, host, port string) bool {
	if port != "443" {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, bound := range hosts {
		bound = strings.ToLower(bound)
		if suffix, wildcard := strings.CutPrefix(bound, "*."); wildcard {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == bound {
			return true
		}
	}
	return false
}

// grantOf reads a release into a Grant, refusing one the forwarder cannot stand behind: a name of
// the role's released as a value, which would put a credential in the role's files; a release
// with no proxy object, no proxy password or a proxy URL that is no http(s) URL; and two names
// holding a placeholder for one account, since a gh directory holds one token. A name of the
// role's with no github slot is no refusal: its hosts go through the proxy all the same.
func grantOf(id string, names []string, release cipher.Release) (*Grant, error) {
	for _, name := range names {
		if _, released := release.Values[name]; released {
			return nil, fmt.Errorf("the broker released %s as its value, not behind its proxy: a role's GitHub secret must have a kind", name)
		}
	}
	if release.Proxy == nil {
		return nil, errors.New("the release holds no proxy object")
	}
	if release.ExpiresAt == nil {
		return nil, errors.New("the grant has no expiry: a role's grant lasts an hour")
	}
	proxy, err := url.Parse(release.Proxy.ProxyURL)
	if err != nil || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.Host == "" {
		return nil, errors.New("the release's proxy URL is no http(s) URL with a host")
	}
	if release.Proxy.ProxyPassword == "" {
		return nil, errors.New("the release names no proxy password")
	}
	grant := &Grant{ID: id, ExpiresAt: *release.ExpiresAt, ProxyURL: proxy, Password: release.Proxy.ProxyPassword,
		CA: release.Proxy.CACertificate, Accounts: map[string]string{}}
	owner := map[string]string{}
	for _, name := range names {
		held, ok := release.Proxy.Names[name]
		if !ok {
			return nil, fmt.Errorf("the release holds no placeholder for %s", name)
		}
		for _, host := range held.Hosts {
			if !slices.Contains(grant.Hosts, host) {
				grant.Hosts = append(grant.Hosts, host)
			}
		}
		for slot, placeholder := range held.Slots {
			account, ok := strings.CutPrefix(slot, githubSlotPrefix)
			if !ok {
				continue
			}
			account = strings.ToLower(account)
			if other, clash := owner[account]; clash {
				return nil, fmt.Errorf("%s and %s both hold a placeholder for account %s, and a gh directory holds one token", other, name, account)
			}
			owner[account] = name
			grant.Accounts[account] = placeholder
		}
	}
	slices.Sort(grant.Hosts)
	return grant, nil
}

// keeper holds the role's current grant, asks for the next one when it nears its expiry or when
// the broker ended it early, and writes each grant's placeholders into the role's gh bundle.
type keeper struct {
	open         func() (Broker, error)
	names        []string
	reason       string
	githubHosts  []string // the hosts the forwarder was told the GitHub kind binds (Config.GitHubHosts)
	ghDir        string
	defaultOwner string
	renames      []ghrepo.Rename
	renewBefore  time.Duration
	log          *slog.Logger

	mu      sync.Mutex
	current *Grant
	held    map[string]time.Time // every grant the broker made for the forwarder that no revocation ended, by id, with its expiry
	asking  *ask                 // the ask in flight, which every caller joins
	// lastErr is how the last ask ended, nil for a grant or before any ask, lastEnded when it
	// ended, and changed is closed and replaced each time an ask ends, so awaitFirst learns of the
	// next one.
	lastErr   error
	lastEnded time.Time
	changed   chan struct{}
	// stopping is set once revokeHeld began: no ask starts after it, so none lands a grant it
	// cannot revoke.
	stopping bool
	// saidNotEnrolled is whether the wait for the role's enrollment was logged, which is said
	// once.
	saidNotEnrolled bool
}

// errStopping is the answer to an ask the forwarder's stop came before.
var errStopping = errors.New("the forwarder is stopping")

// ask is one ask for a grant, which every caller that needs one while it runs joins. landed is
// closed once its broker work and bundle write are done, whatever the outcome, before any line
// about it is written; done is closed once its outcome is published. Its grant is owned before
// either (keeper.own).
type ask struct {
	landed chan struct{}
	done   chan struct{}
	grant  *Grant
	err    error
}

// newKeeper is the keeper of a forwarder of cfg, which Listen has checked and given its defaults,
// logging to log.
func newKeeper(cfg Config, log *slog.Logger) *keeper {
	return &keeper{open: cfg.Open, names: cfg.Secrets, reason: cfg.Reason, githubHosts: cfg.GitHubHosts, ghDir: cfg.GHDir,
		defaultOwner: cfg.DefaultOwner, renames: cfg.Renames, renewBefore: cfg.RenewBefore, log: log,
		held: map[string]time.Time{}, changed: make(chan struct{})}
}

// unconfiguredHosts are the hosts grant binds that the configured github_hosts do not cover, as
// the forwarder routes them (bindsHost: an exact entry, or a `*.` entry for every name under its
// domain): such a host goes out directly before a later forwarder's first grant, where it should
// wait for that grant, so each grant that binds one is warned of.
func (k *keeper) unconfiguredHosts(grant *Grant) []string {
	var lacking []string
	for _, host := range grant.Hosts {
		if !bindsHost(k.githubHosts, host, "443") {
			lacking = append(lacking, host)
		}
	}
	return lacking
}

// Current is the grant held now, nil before the first.
func (k *keeper) Current() *Grant {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.current
}

// awaitFirst waits for the first grant while one may come soon: while an ask runs, and while the
// role's key awaits its enrollment or no ask has run yet, until ctx ends. Once an ask has failed
// for any other reason, with none running, it answers that failure at once: a broker that is down
// or refuses the role's names answers a GitHub call with its reason, not with a minute's wait.
func (k *keeper) awaitFirst(ctx context.Context) (*Grant, error) {
	for {
		k.mu.Lock()
		current, asking, last, changed := k.current, k.asking, k.lastErr, k.changed
		k.mu.Unlock()
		switch {
		case current != nil:
			return current, nil
		case asking != nil:
			changed = asking.done
		case last != nil && !errors.Is(last, cipher.ErrNotEnrolled):
			return nil, fmt.Errorf("the forwarder's last ask for a grant failed: %w", last)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, fmt.Errorf("no grant of the role's GitHub secrets yet: %w", context.Cause(ctx))
		}
	}
}

// replaceCooldown is how long after a failed ask Replace makes no new one: a 407'd connection in
// that time is answered with the failed ask's reason. A broker that ended the role's grant and
// refuses its names then writes one denied request for the role's calls, not one for each call,
// and the role's calls still work again within the 60 seconds R13 gives once the broker grants
// again (the next 407 after the cooldown asks, and the keeper's own renewal asks on its backoff).
const replaceCooldown = 10 * time.Second

// Replace asks for a new grant in place of failed, the grant whose password the broker's proxy
// refused, and answers the grant held once that ask ends. The choice is made under the keeper's
// lock, so asks coalesce across every 407'd connection: a caller whose failed grant was already
// replaced gets the current one without asking; a caller that comes while an ask runs joins it;
// and a caller within replaceCooldown of a failed ask gets that ask's failure without asking.
func (k *keeper) Replace(ctx context.Context, failed *Grant) (*Grant, error) {
	k.mu.Lock()
	switch {
	case k.current != nil && failed != nil && k.current.ID != failed.ID:
		current := k.current
		k.mu.Unlock()
		return current, nil
	case k.asking == nil && k.lastErr != nil && time.Since(k.lastEnded) < replaceCooldown:
		err := k.lastErr
		k.mu.Unlock()
		return nil, fmt.Errorf("the last ask for a grant failed less than %s ago: %w", replaceCooldown, err)
	}
	a, err := k.askLocked("the broker's proxy refused the grant's password (407)")
	k.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return a.wait(ctx)
}

// obtain makes one ask for a grant, or joins the one in flight, and on success makes it current
// and writes its placeholders into the bundle. Once the stop began it starts none (errStopping).
func (k *keeper) obtain(ctx context.Context, why string) (*Grant, error) {
	k.mu.Lock()
	a, err := k.askLocked(why)
	k.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return a.wait(ctx)
}

// askLocked is the ask in flight, started now for why when none runs, under k.mu. Once the stop
// began it starts none (errStopping).
func (k *keeper) askLocked(why string) (*ask, error) {
	if k.asking == nil {
		if k.stopping {
			return nil, errStopping
		}
		k.asking = &ask{landed: make(chan struct{}), done: make(chan struct{})}
		go k.run(k.asking, why)
	}
	return k.asking, nil
}

// wait answers how a ends, or ctx's cause if ctx ends first.
func (a *ask) wait(ctx context.Context) (*Grant, error) {
	select {
	case <-a.done:
		return a.grant, a.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// askBound bounds one ask: the request, its wait for a decision, the release and the bundle's
// write. An agent-tier secret is granted at once, so most asks are two broker calls, the request
// and the release.
const askBound = 50 * time.Second

// run makes ask a, and lands it as ask returns, its broker work and bundle write done, whatever
// the outcome; the forwarder already owns the ask's grant from the moment the broker named it
// (keeper.own, inside ask). Only once the ask landed does run log how it ended, the revocation of
// a grant it could not use among it, and only after that does it publish the outcome (the current
// grant, the last error, a.done), so every line about a grant is in the log before any caller can
// use that grant or read that failure, and no line is written before the ask landed.
func (k *keeper) run(a *ask, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), askBound)
	defer cancel()
	grant, cleanup, err := k.ask(ctx)
	close(a.landed)
	switch {
	case errors.Is(err, cipher.ErrNotEnrolled):
		k.mu.Lock()
		said := k.saidNotEnrolled
		k.saidNotEnrolled = true
		k.mu.Unlock()
		if !said {
			k.log.Info("forwarder: waiting for the daemon to enroll the role's key", "names", k.names)
		}
	case err != nil:
		k.log.Warn("forwarder: no grant of the role's GitHub secrets", "why", why, "names", k.names, "err", err)
	default:
		if _, held := grant.Accounts[k.defaultOwner]; !held && k.defaultOwner != "" {
			k.log.Warn("forwarder: the grant holds no account for GH_CONFIG_DIR's directory", "default", k.defaultOwner,
				"accounts", slices.Sorted(maps.Keys(grant.Accounts)))
		}
		k.log.Info("forwarder: holds a grant of the role's GitHub secrets", "why", why, "grant", grant.ID,
			"expires_at", grant.ExpiresAt.UTC().Format(time.RFC3339), "accounts", slices.Sorted(maps.Keys(grant.Accounts)), "hosts", grant.Hosts)
		if lacking := k.unconfiguredHosts(grant); len(lacking) > 0 {
			k.log.Warn("forwarder: the grant binds hosts runtime.kubernetes.agent_secrets.github_hosts lacks; before a forwarder's first grant a call to one goes out directly, so add them to that key",
				"grant", grant.ID, "lacks", lacking, "configured", k.githubHosts)
		}
	}
	if cleanup.openErr != nil {
		k.log.Warn("forwarder: could not revoke the grant it refused; the stop tries again", "grants", cleanup.ids, "err", cleanup.openErr)
	}
	for _, id := range cleanup.ids {
		if failed, ok := cleanup.failed[id]; ok {
			k.log.Warn("forwarder: could not revoke the grant it refused; the stop tries again", "grant", id, "err", failed)
		}
	}
	k.mu.Lock()
	if err == nil {
		k.current = grant
	}
	a.grant, a.err = grant, err
	k.asking, k.lastErr, k.lastEnded = nil, err, time.Now()
	close(k.changed)
	k.changed = make(chan struct{})
	k.mu.Unlock()
	close(a.done)
}

// own records the grant the broker just named as the forwarder's, until a revocation ends it or
// it expires: from here the stop revokes it whatever comes next, before its release is asked for,
// its bundle written or any line about it logged. expires is an upper bound until the release
// names the grant's own expiry.
func (k *keeper) own(id string, expires time.Time) {
	k.mu.Lock()
	k.held[id] = expires
	k.mu.Unlock()
}

// ask is one request of the role's names, its wait while it is pending, the forwarder's ownership
// of the grant it makes (own), the grant's release and the bundle's write. A grant the forwarder
// cannot use (a release that fails or that it refuses, a bundle it cannot write) is revoked through
// revokeOwned, the one routine every revocation goes through, and stays owned where the broker
// would not end it, so the stop tries again; that revocation's outcome is answered for run to
// log. It writes no line.
func (k *keeper) ask(ctx context.Context) (*Grant, revocation, error) {
	broker, err := k.open()
	if err != nil {
		return nil, revocation{}, err
	}
	answer, err := broker.Request(ctx, k.names, k.reason, int(Lifetime/time.Second))
	if err != nil {
		return nil, revocation{}, fmt.Errorf("ask for %v: %w", k.names, err)
	}
	for answer.State == "pending" {
		id := answer.RequestID
		if answer, err = broker.WaitRequest(ctx, id, 20*time.Second); err != nil {
			return nil, revocation{}, fmt.Errorf("wait for request %s: %w", id, err)
		}
	}
	if answer.State != "granted" || answer.GrantID == nil {
		detail := ""
		if answer.Detail != nil {
			detail = ": " + *answer.Detail
		}
		return nil, revocation{}, fmt.Errorf("request %s for %v is %s%s", answer.RequestID, k.names, answer.State, detail)
	}
	id := *answer.GrantID
	lifetime := Lifetime
	if answer.LifetimeSeconds != nil {
		lifetime = time.Duration(*answer.LifetimeSeconds) * time.Second
	}
	k.own(id, time.Now().Add(lifetime))
	release, err := broker.Values(ctx, id)
	var grant *Grant
	if err != nil {
		err = fmt.Errorf("release: %w", err)
	} else if grant, err = grantOf(id, k.names, release); err == nil {
		err = k.write(grant)
	}
	if err != nil {
		return nil, k.revokeOwned([]string{id}, revokeBound), fmt.Errorf("grant %s: %w", id, err)
	}
	k.mu.Lock()
	if _, owned := k.held[id]; owned {
		k.held[id] = grant.ExpiresAt
	}
	k.mu.Unlock()
	return grant, revocation{}, nil
}

// write brings the bundle to grant: one gh directory per account holding that account's
// placeholder (ghconfig.Render, with the grant's expiry), the configured renames, and the default
// directory GH_CONFIG_DIR names. Where the grant holds no account for that default, the bundle's
// own default is its first account: GH_CONFIG_DIR's directory holds no token, as a role whose App
// is not installed on its issue's owner holds none today, and run warns of it once the ask has
// landed. It writes no line.
func (k *keeper) write(grant *Grant) error {
	if len(grant.Accounts) == 0 {
		return errors.New("the grant holds a placeholder for no GitHub account")
	}
	bundle := ghconfig.Bundle{Default: k.defaultOwner, Dirs: map[string]ghconfig.Rendered{}, Pairs: k.renames}
	for account, placeholder := range grant.Accounts {
		bundle.Dirs[account] = ghconfig.Render(placeholder, "proxy", grant.ExpiresAt)
	}
	if _, held := bundle.Dirs[bundle.Default]; !held {
		bundle.Default = bundle.DirNames()[0]
	}
	_, err := ghconfig.WriteBundle(k.ghDir, bundle)
	return err
}

// keep asks for the first grant, then the next one each time the current one nears its expiry,
// until ctx ends; a failed ask is retried with a backoff from one second to thirty, except while
// the role's key awaits its enrollment, which comes seconds after the agent registers and is
// looked for each second with no backoff, so the first grant follows it closely. An ask that
// gets the current grant back — a broker from before it handed an automatic grant back only in
// the first half of its life — is no new grant, and is asked again a minute later, or just past
// that grant's expiry where that comes first, rather than at once.
func (k *keeper) keep(ctx context.Context) {
	wait := time.Second
	for {
		var next time.Duration
		previous := k.Current()
		if previous != nil {
			next = time.Until(previous.ExpiresAt.Add(-k.renewBefore))
		}
		if next > 0 && backoff.Sleep(ctx, next) != nil {
			return
		}
		why := "the role's generation started"
		if previous != nil {
			why = "the grant nears its expiry"
		}
		grant, err := k.obtain(ctx, why)
		switch {
		case errors.Is(err, cipher.ErrNotEnrolled):
			if backoff.Sleep(ctx, time.Second) != nil {
				return
			}
		case err != nil:
			if ctx.Err() != nil || backoff.Sleep(ctx, wait) != nil {
				return
			}
			wait = min(2*wait, 30*time.Second)
		case previous != nil && grant.ID == previous.ID:
			k.log.Warn("forwarder: the broker handed the expiring grant back rather than a new one", "grant", grant.ID)
			if backoff.Sleep(ctx, min(time.Minute, time.Until(grant.ExpiresAt)+time.Second)) != nil {
				return
			}
		default:
			wait = time.Second
		}
	}
}

// revokeHeld ends the grants the forwarder owns, at the generation's end, in two passes, each
// under a time limit of its own, so that neither an ask that hangs nor a log that stalls holds a
// revocation back. The first pass revokes every grant held now, the current one among them, within
// revokeBound. The second pass runs where an ask was in flight or the first pass left a grant
// unended: where an ask was in flight, it first waits for that ask to land (ask.landed, within
// landedBound), then it revokes every grant held then, whether or not the ask landed: the one that
// ask made, owned as soon as the broker named it, and any the first pass could not end. Its limit
// is revokeBound, and landedBound too where it waited for no ask, so the stop spends at most
// stopBound either way and a stop with no ask in flight spends all of it on revocations. No ask
// starts once revokeHeld began (keeper.stopping), so only the ask in flight can add a grant after
// the first pass. A grant the broker names only after the second pass lives until its own expiry,
// as a grant of a stop cut short does (see the package doc). Every line revokeHeld writes comes
// after both passes.
func (k *keeper) revokeHeld() {
	k.mu.Lock()
	k.stopping = true
	asking := k.asking
	first := slices.Sorted(maps.Keys(k.held))
	k.mu.Unlock()
	passes := []revocation{k.revokeOwned(first, revokeBound)}
	outlived := false
	if asking != nil || len(passes[0].failed) > 0 || passes[0].openErr != nil {
		bound := revokeBound + landedBound
		if asking != nil {
			bound = revokeBound
			landed := time.NewTimer(landedBound)
			select {
			case <-asking.landed:
			case <-landed.C:
				outlived = true
			}
			landed.Stop()
		}
		k.mu.Lock()
		second := slices.Sorted(maps.Keys(k.held))
		k.mu.Unlock()
		passes = append(passes, k.revokeOwned(second, bound))
	}
	if outlived {
		k.log.Warn("forwarder: an ask in flight had not ended when the stop stopped waiting; a grant it is named later lives to its expiry")
	}
	for _, pass := range passes {
		if pass.openErr != nil {
			k.log.Warn("forwarder: could not revoke its grants at its stop", "grants", pass.ids, "err", pass.openErr)
		}
		for _, id := range pass.ids {
			if err, ok := pass.failed[id]; ok {
				k.log.Warn("forwarder: could not revoke a grant at its stop", "grant", id, "err", err)
			}
		}
		for _, id := range pass.revoked {
			k.log.Info("forwarder: revoked a grant at its stop", "grant", id)
		}
	}
}

// revocation is how one revocation of owned grants went: the ids it was asked to end, those the
// broker ended, those it would not end, and, where the broker could not be reached at all, why.
type revocation struct {
	ids     []string
	revoked []string
	failed  map[string]error
	openErr error
}

// revokeOwned is every revocation the forwarder makes, of a grant an ask could not use as at the
// stop: it revokes each of ids the keeper still owns, under a time limit of its own (bound), and
// removes an id from held only once the broker ended it or it expired, so an id the broker would
// not end stays owned for the stop to try again. An id no longer owned was ended by another
// revocation and is passed over. It writes no line: its caller does, after the work that must not
// wait on a log.
func (k *keeper) revokeOwned(ids []string, bound time.Duration) revocation {
	r := revocation{ids: ids, failed: map[string]error{}}
	if len(ids) == 0 {
		return r
	}
	broker, err := k.open()
	if err != nil {
		r.openErr = err
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	for _, id := range ids {
		k.mu.Lock()
		expires, owned := k.held[id]
		if owned && time.Now().After(expires) {
			delete(k.held, id)
			owned = false
		}
		k.mu.Unlock()
		if !owned {
			continue
		}
		if err := broker.Revoke(ctx, id); err != nil {
			r.failed[id] = err
			continue
		}
		k.mu.Lock()
		delete(k.held, id)
		k.mu.Unlock()
		r.revoked = append(r.revoked, id)
	}
	return r
}
