package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// The rate reading logs, for each installation GitHub lists for each App the daemon holds a key
// for, one github_rate_reading line (githubrate.Write): GitHub's used count, limit and reset of the
// installation's REST core counter and of its GraphQL counter, and the daemon's own answered calls
// on each since that counter's reset, by the code path that made them (githubrate.Counter). A pass
// runs as the workflow starts, and each next one when githubrate.Next says it is due after the
// last: an Interval later, or BeforeReset before the earliest reset the reading holds of any
// installation, which is how the last minute of each counter's hour is read. GitHub budgets each
// installation, not each program, so every installation an App has is read, whichever project the
// daemon serves; the own counts are calls, not GitHub's units, so the used count less them is no
// measure of anyone else's use.

// The caller kinds of the daemon's code paths that call GitHub, each a githubrate.WithCaller on the
// context of that path's calls, so the reading counts the daemon's own calls on an installation
// by the path that made them. A call no path tagged counts as githubrate.Other. Three are tagged
// in their own packages: the handoff route's read of a pull request (internal/api,
// handoff_pull_request), token mode's check of a released token (internal/appauth,
// token_identity) and the read of the shared leak check (internal/leakguard, leak_check).
const (
	// callerPullRequestReads is the per-issue read (readIssuePullRequest).
	callerPullRequestReads = "pull_request_reads"
	// callerMergeStateReads is the merge-state ladder's reads (readMergeState).
	callerMergeStateReads = "merge_state_reads"
	// callerOutbox is every row the outbox runs (outbox.Run).
	callerOutbox = "outbox"
	// callerReviewBody is the read of a review body Envoy capped (reviewBody).
	callerReviewBody = "review_body"
	// callerReviewPermission is the read of a review author's permission (readPermission).
	callerReviewPermission = "review_permission"
	// callerDefaultBranch is the read of a repository's name and default branch
	// (defaultBranches.fetch).
	callerDefaultBranch = "default_branch"
	// callerRateReading is the rate reading's own calls (rateReader.pass).
	callerRateReading = "rate_reading"
)

// rateReadBound bounds one App's installation list, and on its own each installation's mint and
// reads: each of those calls answers in well under a second, so one that runs this long holds a
// request GitHub never answered, and its bound ends it rather than holding every installation read
// after it in the pass. An installation whose bound ends has its line, carrying the deadline.
const rateReadBound = 30 * time.Second

// rateReadingKeyless is what the reading logs once, as it starts, for each App on token_command:
// such an App holds no key to sign the JWT its installations are listed under, so no reading runs
// for it, and the log says so rather than leave its lines missing unexplained.
const rateReadingKeyless = "github rate reading: the App is on token_command and holds no key to list its installations under, so it is not read"

// rateReadingStarts is what the reading logs once, as it starts, when the daemon holds the key of
// at least one App, with each App it reads and the Interval: Dispatch's main logs the same line,
// so a log store finds a reader's start beside its first lines, and tells a reading that started
// and wrote nothing from one that never ran.
const rateReadingStarts = "github rate reading: reading the GitHub rate counters of each installation of each App the daemon holds a key for"

// countingTokens is the daemon's own token manager for apps at the GitHub root githubAPI (empty is
// api.github.com) and the rate counter it counts on: the process's REST client is put on the
// counter's transport (githubrest.SetTransport), the manager calls GitHub with that same client
// (githubrest.HTTPClient), and it tells the counter each installation token it is given. So every
// call the daemon makes with an installation token, the manager's own and every workflow read's,
// counts on that installation. The caller owns the transport from then on, and gives it back with
// githubrest.SetTransport(nil) when the manager's workflow ends.
func countingTokens(apps config.GitHubApps, githubAPI string, log *slog.Logger) (*appauth.Manager, *githubrate.Counter) {
	counter := githubrate.NewCounter()
	githubrest.SetTransport(counter.Transport(nil))
	tokens := appauth.New(apps, appauth.Options{BaseURL: githubAPI, Log: log, HTTPClient: githubrest.HTTPClient(), Counter: counter})
	return tokens, counter
}

// rateApps is the installation list and read lease the rate reader needs. The daemon's Manager
// answers both calls, and a test can stand in for it to check the reader's deadlines and stop.
type rateApps interface {
	ListInstallations(context.Context, appauth.AppRole) (appauth.AppInstallations, error)
	ReadToken(context.Context, appauth.AppRole, string) (appauth.Lease, error)
}

// rateApp is one App of the daemon's configuration as the reading takes it: its id, and the role
// whose configuration it is reached through, which for an App the daemon holds a key for is the
// first role holding that key.
type rateApp struct {
	role appauth.AppRole
	id   string
}

// rateKept is what the reading keeps of one installation an App listed, from one pass to the next:
// the first repository the installation listed, zero until it is listed, whose answer the core
// counter is read from; and each counter's reset as the last reading that read that counter found
// it, kept while later readings of the installation fail, which schedules the passes before them.
type rateKept struct {
	repository    ghrepo.Repository
	core, graphQL rateReset
}

// rateReset is one counter's reset as the reading holds it, zero where none is known or the one
// known has passed, and whether a pass read the installation in that reset's last minute. Only a
// reset not so read schedules the reading before it (githubrate.Next), so a reset GitHub names a
// second later at the next read, as it does within one hour, schedules no second early pass.
type rateReset struct {
	at      time.Time
	sampled bool
}

// readAt is the counter's reset once a reading taken at at read reset, zero where it read none: a
// reset read replaces the one held, and the reset held is marked sampled where at is in its last
// minute. A reading that failed there marks it too, so an installation whose reading keeps failing
// is read once in each last minute rather than pass after pass until the reset.
func (h rateReset) readAt(reset, at time.Time) rateReset {
	if !reset.IsZero() {
		h = rateReset{at: reset}
	}
	if githubrate.InLastMinute(at, h.at) {
		h.sampled = true
	}
	return h
}

// rateReader is the rate reading of one daemon. Only run's goroutine touches its fields once
// it runs.
type rateReader struct {
	// apps lists each App's installations under its JWT and mints their read leases.
	apps    rateApps
	counter *githubrate.Counter
	// keyed is each App the daemon holds a key for, once per App id; keyless each App on
	// token_command, which is not read.
	keyed, keyless []rateApp
	// project is the daemon's project key, process the host it runs on, and githubAPI the
	// GitHub REST root the reading's own calls go to (empty is api.github.com).
	project, process, githubAPI string
	log                         *slog.Logger
	// readBound is rateReadBound; a test shortens it.
	readBound time.Duration
	// after is the wait before the next pass, time.After; a test cuts it short.
	after func(time.Duration) <-chan time.Time
	// now is the clock the reading's schedule, its order and its window checks read, time.Now;
	// a test moves it.
	now func() time.Time
	// installations is what the reading keeps of each installation, by App id and then
	// installation id: only those the App's last list that GitHub answered held.
	installations map[string]map[string]rateKept
}

// newRateReader is the reading of configured's Apps through apps, counting on counter, the
// counter apps tells of each token it mints. Two roles that name one App read it once.
func newRateReader(apps rateApps, counter *githubrate.Counter, configured config.GitHubApps, project, githubAPI string, log *slog.Logger) *rateReader {
	process, err := os.Hostname()
	if err != nil {
		log.Warn("github rate reading: the host's name cannot be read, so each line names no process", "error", err)
	}
	r := &rateReader{apps: apps, counter: counter, project: project, process: process, githubAPI: githubAPI, log: log,
		readBound: rateReadBound, after: time.After, now: time.Now, installations: map[string]map[string]rateKept{}}
	byRole := map[appauth.AppRole]config.GitHubApp{appauth.Implement: configured.Implement, appauth.Review: configured.Review}
	seen := map[string]bool{}
	// Every role holding a key is taken before any role on token_command, so a key either role of
	// an App holds reads that App.
	for _, role := range appauth.Roles {
		if app := byRole[role]; app.TokenCommand == "" && !seen[app.AppID] {
			seen[app.AppID] = true
			r.keyed = append(r.keyed, rateApp{role: role, id: app.AppID})
		}
	}
	for _, role := range appauth.Roles {
		if app := byRole[role]; app.TokenCommand != "" && !seen[app.AppID] {
			seen[app.AppID] = true
			r.keyless = append(r.keyless, rateApp{role: role, id: app.AppID})
		}
	}
	return r
}

// run says once that each keyless App is not read, and, when the daemon holds an App's key, that
// the reading starts (rateReadingStarts); then it takes a pass at once and each next one when
// githubrate.Next says it is due after the last, from the resets the reading holds that no pass
// has read in their last minute (heldResets), until ctx ends. A daemon holding no App's key reads
// nothing.
func (r *rateReader) run(ctx context.Context) {
	for _, app := range r.keyless {
		r.log.Info(rateReadingKeyless, "app", app.role, "app_id", app.id)
	}
	if len(r.keyed) == 0 {
		return
	}
	ids := make([]string, 0, len(r.keyed))
	for _, app := range r.keyed {
		ids = append(ids, app.id)
	}
	r.log.Info(rateReadingStarts, "app_ids", ids, "interval", githubrate.Interval.String())
	for ctx.Err() == nil {
		r.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		now := r.now()
		due, _ := githubrate.Next(now, r.heldResets(now))
		select {
		case <-ctx.Done():
		case <-r.after(due.Sub(now)):
		}
	}
}

// heldResets is every reset the reading holds of any installation that is still to come at now
// and that no pass has read in its last minute: what githubrate.Next schedules the next pass by,
// due at once for one already in its last minute. A reset now past is dropped.
func (r *rateReader) heldResets(now time.Time) []time.Time {
	var resets []time.Time
	for _, installations := range r.installations {
		for id, kept := range installations {
			for _, held := range []*rateReset{&kept.core, &kept.graphQL} {
				if !held.at.After(now) {
					*held = rateReset{}
				} else if !held.sampled {
					resets = append(resets, held.at)
				}
			}
			installations[id] = kept
		}
	}
	return resets
}

// pass writes one line for each installation each keyed App lists, the installations in a
// counter's last minute first by the resets the reading holds of them, and keeps what the next
// pass needs. An installation whose mint or read fails has its line all the same, carrying the
// failure, so a pass writes as many lines as each list held; such a reading keeps the resets the
// installation's last readings read. An App whose list GitHub refuses, or lists no installation,
// writes one line naming no installation and carrying the refusal or the empty list: a pass that
// wrote nothing for it would be silent for an App the daemon holds a key for, with nothing to tell
// a refused list from a reading never taken. A refused list keeps what the reading held of the
// App's installations, and marks each reset among them that is in its last minute, as a failed
// reading does, so a list that keeps failing there schedules no pass at once after pass; a list
// GitHub answered keeps only the installations it holds. Each list, and each installation's mint
// and reads, has readBound of its own, so a stalled call cannot stop every later reading. A pass
// the daemon's stop cuts short writes nothing more: that stop is not GitHub's failure. Every call
// counts as callerRateReading's.
func (r *rateReader) pass(ctx context.Context) {
	ctx = githubrate.WithCaller(ctx, callerRateReading)
	for _, app := range r.keyed {
		if ctx.Err() != nil {
			return
		}
		list, cancel := context.WithTimeout(ctx, r.readBound)
		listed, err := r.apps.ListInstallations(list, app.role)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			at := r.now()
			for id, kept := range r.installations[app.id] {
				kept.core, kept.graphQL = kept.core.readAt(time.Time{}, at), kept.graphQL.readAt(time.Time{}, at)
				r.installations[app.id][id] = kept
			}
			r.write(githubrate.Reading{AppID: app.id, Error: fmt.Sprintf("list the %s App's installations: %v", app.role, err)})
			continue
		}
		if len(listed.Installations) == 0 {
			delete(r.installations, app.id)
			r.write(githubrate.Reading{AppID: app.id, AppSlug: listed.Slug, Error: "GET /app/installations listed no installation of the App"})
			continue
		}
		held := r.installations[app.id]
		now := r.now()
		var lastMinute, rest []appauth.ListedInstallation
		for _, installation := range listed.Installations {
			if githubrate.InLastMinute(now, held[installation.ID].core.at, held[installation.ID].graphQL.at) {
				lastMinute = append(lastMinute, installation)
			} else {
				rest = append(rest, installation)
			}
		}
		kept := make(map[string]rateKept, len(listed.Installations))
		for _, installation := range append(lastMinute, rest...) {
			if ctx.Err() != nil {
				return
			}
			state := held[installation.ID]
			reading := r.readInstallation(ctx, app, installation, &state)
			if ctx.Err() != nil {
				return
			}
			kept[installation.ID] = state
			reading.AppID, reading.AppSlug, reading.Owner, reading.InstallationID = app.id, listed.Slug, installation.Owner, installation.ID
			reading.Installations, reading.Permissions = len(listed.Installations), installation.Permissions
			r.write(reading)
		}
		r.installations[app.id] = kept
	}
}

// write logs reading as this daemon's.
func (r *rateReader) write(reading githubrate.Reading) {
	reading.Program, reading.Process, reading.Project = "daemon", r.process, r.project
	githubrate.Write(r.log, reading)
}

// readInstallation reads installation's two counters under a read lease of its own, the core
// counter as rateReader.core reads it and the GraphQL counter from GET /rate_limit, then the
// daemon's own calls on each (githubrate.Counter.Own), read last so the reading's own calls are
// among them. It returns the counters, or the failure alone. The line says before_reset when the
// reading was taken in the last minute before one of the installation's known resets, the ones it
// read or, where it read none, the ones the reading holds from before: so a failed try in a last
// minute is a before_reset line carrying its error, which says that minute was tried and read
// nothing, rather than a line that reads as taken outside it. A counter whose reset passed before
// the own counts were read is a failure: the counter GitHub answered is the old hour's and the own
// counts the new one's.
//
// state is what the reading keeps of the installation: the first repository the core read lists
// and forgets, and each counter's reset, which a reading that read it replaces, a failed reading
// that read the core counter before it failed among them, and which is marked read in its last
// minute where this reading was taken there (rateReset.readAt). The mint and every read share
// readBound, independently of every other installation.
func (r *rateReader) readInstallation(ctx context.Context, app rateApp, installation appauth.ListedInstallation, state *rateKept) githubrate.Reading {
	ctx, cancel := context.WithTimeout(ctx, r.readBound)
	defer cancel()
	core, graphQL, err := r.readCounters(ctx, app, installation, state)
	if err == nil {
		core.Own, graphQL.Own = r.counter.Own(installation.ID)
	}
	at := r.now()
	state.core = state.core.readAt(core.Reset, at)
	state.graphQL = state.graphQL.readAt(graphQL.Reset, at)
	beforeReset := githubrate.InLastMinute(at, state.core.at, state.graphQL.at)
	if err != nil {
		return githubrate.Reading{Error: err.Error(), BeforeReset: beforeReset}
	}
	if !at.Before(core.Reset) || !at.Before(graphQL.Reset) {
		return githubrate.Reading{Error: "a counter reset before its own-call snapshot; the reading spans two rate windows", BeforeReset: beforeReset}
	}
	return githubrate.Reading{Core: core, GraphQL: graphQL, BeforeReset: beforeReset}
}

// readCounters is readInstallation's reads: the counters as far as GitHub answered them, a core
// counter read before the GraphQL read failed among them, and the failure. An installation GitHub
// lists as suspended is read no further, since GitHub mints no token of it.
func (r *rateReader) readCounters(ctx context.Context, app rateApp, installation appauth.ListedInstallation, state *rateKept) (core, graphQL githubrate.Bucket, err error) {
	if installation.Suspended {
		return core, graphQL, errors.New("GitHub lists the installation as suspended, and mints no token of a suspended installation")
	}
	lease, err := r.apps.ReadToken(ctx, app.role, installation.Owner)
	if err != nil {
		return core, graphQL, fmt.Errorf("mint the %s App's read token on %s: %w", app.role, installation.Owner, err)
	}
	if lease.Installation != installation.ID {
		return core, graphQL, fmt.Errorf("the %s App's read token on %s is of installation %q, not of the installation GitHub lists", app.role, installation.Owner, lease.Installation)
	}
	if core, err = r.core(ctx, lease.Token, state); err != nil {
		return core, graphQL, err
	}
	var limits json.RawMessage
	if err := (githubrest.Client{Token: lease.Token, API: githubrest.APIRoot(r.githubAPI)}).Get(ctx, "/rate_limit", &limits); err != nil {
		return core, graphQL, fmt.Errorf("read the GraphQL counter: GET /rate_limit: %w", err)
	}
	if graphQL, err = githubrate.GraphQLBucket(limits); err != nil {
		return core, graphQL, fmt.Errorf("read the GraphQL counter: %w", err)
	}
	return core, graphQL, nil
}

// core is the installation's REST core counter, read with token, as the headers of
// GET /repos/OWNER/REPO/actions/workflows?per_page=1 on its first repository name it, whatever
// GitHub answered the read with: a refusal at the limit names the counter as any answer does, and
// it is the answer of a spent hour the reading most needs. Where the listing of the installation's
// repositories gives no repository to read, though GitHub answered it (a refusal, no repository, a
// name that is no repository's, a body that is no JSON), the counter is read from the listing's
// own answer instead, which is a core call's too, as Dispatch reads it. A repository GitHub
// answers 404 is the installation's no longer, and is forgotten for the next pass to list another.
func (r *rateReader) core(ctx context.Context, token string, state *rateKept) (githubrate.Bucket, error) {
	if state.repository.IsZero() {
		repository, answered, err := r.firstRepository(ctx, token)
		if err != nil {
			if core, unread := githubrate.CoreBucket(answered); unread == nil {
				return core, nil
			}
			return githubrate.Bucket{}, fmt.Errorf("read the installation's first repository: %w", err)
		}
		state.repository = repository
	}
	repository := state.repository
	headers, err := githubrest.Client{Token: token, API: githubrest.RepositoryAPI(r.githubAPI, repository)}.GetHeaders(ctx, "/actions/workflows?per_page=1", nil)
	var refused *githubrest.Answer
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		state.repository = ghrepo.Repository{}
	}
	core, unread := githubrate.CoreBucket(headers)
	if unread != nil {
		if err == nil {
			err = unread
		}
		return githubrate.Bucket{}, fmt.Errorf("read the core counter from GET /repos/%s/actions/workflows: %w", repository, err)
	}
	return core, nil
}

// firstRepository is the first repository GET /installation/repositories?per_page=1 lists for
// token's installation, and the headers of GitHub's answer, whatever it answered, nil where there
// was no answer: the core counter can be read from them where the listing names no repository.
func (r *rateReader) firstRepository(ctx context.Context, token string) (repository ghrepo.Repository, answered http.Header, err error) {
	var listed struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	answered, err = githubrest.Client{Token: token, API: githubrest.APIRoot(r.githubAPI)}.GetHeaders(ctx, "/installation/repositories?per_page=1", &listed)
	if err != nil {
		return ghrepo.Repository{}, answered, err
	}
	if len(listed.Repositories) == 0 {
		return ghrepo.Repository{}, answered, errors.New("the installation holds no repository to read the core counter on")
	}
	repository, err = ghrepo.Parse("the installation's first repository", listed.Repositories[0].FullName)
	if err != nil {
		return ghrepo.Repository{}, answered, err
	}
	return repository, answered, nil
}
