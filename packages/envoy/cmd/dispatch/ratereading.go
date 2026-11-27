package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/redact"
)

// rateReadingCaller is the caller kind of every call the rate reading makes to GitHub
// (githubrate.WithCaller), so its own reads show among the calls it reports.
const rateReadingCaller = "rate_reading"

// runRateReading is Dispatch's GitHub rate reading (githubrate): each pass lists the App's
// installations and logs one github_rate_reading line for each of them through log, with the
// installation's permissions as the list named them, the used count, the limit and the reset of
// its REST core counter and its GraphQL counter, and this process's own calls on each since its
// last reset by caller kind (client.OwnCalls). The first pass runs at once and each next one at
// githubrate.Next of the resets of every listed installation that the reading knows and has not
// yet tried in their last minute: a pass that ends inside a counter's last minute it read the
// installation outside of is followed by one at once, and a reset the reading tried in its last
// minute asks for nothing more, whatever its header drifts to. A pass that fails to read a counter
// keeps the reset read before it. Each installation's line says before_reset when it was written in
// the last minute before one of that installation's known resets, whether the pass was regular or
// scheduled before a reset, and whether the reading succeeded or failed: a failed reading there is
// a before_reset line carrying its error. It runs until ctx ends.
//
// log is a JSON logger of its own (main's slog.NewJSONHandler on stderr), not the process's text
// logger, so that the line is one JSON object a log store reads field by field.
//
// It runs in every process and takes no lease: the own counts are the process's, the used count
// is every holder's, and each process's lines name it by its host name. A pass costs each
// installation one core call, the workflows read; one more where the installation's repository
// is listed again, at its first pass and after GitHub answered the remembered one 404.
func runRateReading(ctx context.Context, client *githubapp.Client, log *slog.Logger) {
	readRates(ctx, client, log, realClock())
}

// readRates is runRateReading on clock, which says what time it is and waits for the next pass.
func readRates(ctx context.Context, client *githubapp.Client, log *slog.Logger, clock rateClock) {
	process, err := os.Hostname()
	if err != nil {
		slog.Warn("dispatch: the GitHub rate reading names no process: the host name could not be read", "error", err)
	}
	newRateReading(client, log, process, clock).run(ctx)
}

// rateClock is the rate reading's clock: now is the time a pass reads and schedules by, and wait
// waits until until, answering false once ctx ended first. The reading's own resets and GitHub's
// answers are compared on it, so a test that moves it runs a pass due at a reset's last minute at
// once.
type rateClock struct {
	now  func() time.Time
	wait func(ctx context.Context, until time.Time) bool
}

// realClock is the clock of the process: time.Now, and a timer.
func realClock() rateClock {
	return rateClock{
		now: time.Now,
		wait: func(ctx context.Context, until time.Time) bool {
			timer := time.NewTimer(time.Until(until))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
				return true
			}
		},
	}
}

// rateReading is what one process's reading keeps from pass to pass: the App's numeric id and
// fallback slug from GET /app, the first repository each installation listed, whose answer the
// core counter is read from, and what it holds of each listed installation (held). A listed
// installation's current slug takes precedence.
type rateReading struct {
	client        *githubapp.Client
	log           *slog.Logger
	process       string
	clock         rateClock
	appID         string
	appSlug       string
	repositories  map[int64]repositoryName
	installations map[int64]readingHeld
}

// readingHeld is what the reading holds of one installation the App's last answered list held:
// its two counters' known resets, and the owner and permissions that list gave it, which the
// line of a pass whose list GitHub refuses names (refusedList).
type readingHeld struct {
	resets      counterResets
	owner       string
	permissions map[string]string
}

// counterResets is one installation's two counters' resets as the reading last read them.
type counterResets struct {
	core, graphql knownReset
}

// knownReset is one counter's reset as the last pass that read it read it: zero when none is
// known or the one known has passed. tried is whether a line of the installation was written in
// that reset's last minute since, so the reading has had its last-minute chance at it. A line that
// failed counts as tried: a reading tried again at once would ask GitHub again in a loop until the
// reset, for as long as GitHub refused.
type knownReset struct {
	at    time.Time
	tried bool
}

// observe takes one counter's reset as a line of its installation, written at at, read it: reset
// replaces the one known, with its last-minute try as at says, and a zero reset, a counter the line
// did not read, keeps the one known and marks it tried when at is in its last minute.
func (k *knownReset) observe(reset, at time.Time) {
	if !reset.IsZero() {
		*k = knownReset{at: reset}
	}
	if githubrate.InLastMinute(at, k.at) {
		k.tried = true
	}
}

// untried is the reset when it is known, still ahead of now, and not yet tried in its last minute.
func (k knownReset) untried(now time.Time) (time.Time, bool) {
	return k.at, k.at.After(now) && !k.tried
}

// newRateReading is a reading through client and log, named process, on clock, that knows nothing
// of any installation yet.
func newRateReading(client *githubapp.Client, log *slog.Logger, process string, clock rateClock) *rateReading {
	return &rateReading{
		client: client, log: log, process: process, clock: clock,
		repositories: map[int64]repositoryName{}, installations: map[int64]readingHeld{},
	}
}

// run reads a pass at once and each next one when githubrate.Next says it is due, until ctx ends.
func (r *rateReading) run(ctx context.Context) {
	ctx = githubrate.WithCaller(ctx, rateReadingCaller)
	for {
		resets := r.pass(ctx)
		at, _ := githubrate.Next(r.clock.now(), resets)
		if !r.clock.wait(ctx, at) {
			return
		}
	}
}

// repositoryName is the repository an installation's core counter is read on: the owner and the
// name of the full_name GitHub listed, held to the names GitHub allows (refs.SplitGitHubRepo, the
// rule every place that builds a GitHub path reads) where the listing is read, so the path built
// from them is never one of a name with no owner or no name.
type repositoryName struct {
	owner, name string
}

// pass reads every installation the App lists now and logs a line for each, and returns the
// resets of the installations listed that are ahead and not yet tried in their last minute, for
// githubrate.Next. It reads the installations with such a reset in its last minute first, so the
// reading that must land before a reset is not kept behind the rest, and still reads every one. A
// reading that fails logs its line with the error instead of the counters, so a pass logs as many
// lines as the list held, and keeps the resets read before of every counter it did not read. A
// list that fails logs one line for each installation it holds (refusedList), and one that holds
// no installation logs one line saying so, so no pass is silent: a failed list keeps every known
// reset, marking tried each in its last minute, and an empty one, which lists no installation,
// forgets them all. A pass cut short by the end of ctx, the process stopping, logs nothing
// further.
func (r *rateReading) pass(ctx context.Context) []time.Time {
	installations, err := r.client.ReadInstallations(ctx)
	if ctx.Err() != nil {
		return nil
	}
	r.app(ctx, installations)
	line := githubrate.Reading{Program: "dispatch", Process: r.process, AppID: r.appID, AppSlug: r.appSlug}
	switch {
	case err != nil:
		r.refusedList(line, "list the App's installations: "+err.Error())
		return r.untriedResets()
	case len(installations) == 0:
		line.Error = "GET /app/installations listed no installation of the App"
		clear(r.repositories)
		clear(r.installations)
		githubrate.Write(r.log, line)
		return r.untriedResets()
	}
	line.Installations = len(installations)
	start := r.clock.now()
	slices.SortStableFunc(installations, func(a, b githubapp.Installation) int {
		return lastMinuteFirst(r.inLastMinute(start, a.ID), r.inLastMinute(start, b.ID))
	})
	listed := make(map[int64]repositoryName, len(installations))
	known := make(map[int64]readingHeld, len(installations))
	for _, installation := range installations {
		reading := line
		reading.Owner = installation.AccountLogin
		reading.InstallationID = strconv.FormatInt(installation.ID, 10)
		reading.Permissions = installation.Granted
		if installation.AppID != 0 {
			reading.AppID = strconv.FormatInt(installation.AppID, 10)
		}
		if installation.AppSlug != "" {
			reading.AppSlug = installation.AppSlug
		}
		var err error
		reading.Core, reading.GraphQL, err = r.read(ctx, installation.ID)
		if ctx.Err() != nil {
			return nil
		}
		// The known resets take this reading's: each counter it read replaces its reset, and one
		// it did not read keeps the reset read before. The line says before_reset when the
		// reading was taken in the last minute before one of them, so a failed reading in a last
		// minute is a before_reset line carrying its error: that minute was tried and read nothing.
		at := r.clock.now()
		held := r.installations[installation.ID]
		held.resets.core.observe(reading.Core.Reset, at)
		held.resets.graphql.observe(reading.GraphQL.Reset, at)
		held.owner, held.permissions = installation.AccountLogin, installation.Granted
		reading.BeforeReset = githubrate.InLastMinute(at, held.resets.core.at, held.resets.graphql.at)
		if err != nil {
			reading.Error = err.Error()
		}
		githubrate.Write(r.log, reading)
		known[installation.ID] = held
		if repository, ok := r.repositories[installation.ID]; ok {
			listed[installation.ID] = repository
		}
	}
	// An installation no longer listed is forgotten with its repository and what the reading held.
	r.repositories = listed
	r.installations = known
	return r.untriedResets()
}

// refusedList logs the lines of a pass whose list of the App's installations GitHub refused with
// refusal, line being the pass's line before any installation: one error line for each
// installation the reading holds, in the order of their ids, with the owner, the permissions and
// the count the App's last answered list gave, and before_reset from the resets the reading knows
// of that installation, so an owner whose list fails in a last minute still has a line for that
// minute; and the one line naming no installation where the reading holds none. What the reading
// holds is kept, each known reset among it marked tried where this pass is in its last minute, as
// a failed reading marks it, so a list that keeps failing there schedules no pass at once after
// pass. The daemon's reading does the same (internal/daemon's refusedList).
func (r *rateReading) refusedList(line githubrate.Reading, refusal string) {
	line.Error = refusal
	if len(r.installations) == 0 {
		githubrate.Write(r.log, line)
		return
	}
	at := r.clock.now()
	line.Installations = len(r.installations)
	for _, id := range slices.Sorted(maps.Keys(r.installations)) {
		held := r.installations[id]
		reading := line
		reading.Owner, reading.Permissions = held.owner, held.permissions
		reading.InstallationID = strconv.FormatInt(id, 10)
		reading.BeforeReset = githubrate.InLastMinute(at, held.resets.core.at, held.resets.graphql.at)
		held.resets.core.observe(time.Time{}, at)
		held.resets.graphql.observe(time.Time{}, at)
		r.installations[id] = held
		githubrate.Write(r.log, reading)
	}
}

// inLastMinute is whether a counter of installation the reading knows, and has not tried in its
// last minute, is in that minute at at.
func (r *rateReading) inLastMinute(at time.Time, installation int64) bool {
	resets := r.installations[installation].resets
	for _, known := range []knownReset{resets.core, resets.graphql} {
		if reset, untried := known.untried(at); untried && githubrate.InLastMinute(at, reset) {
			return true
		}
	}
	return false
}

// lastMinuteFirst orders an installation in a counter's last minute before one that is not.
func lastMinuteFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

// untriedResets is every known reset still ahead on the reading's clock and not yet tried in its
// last minute; one that has passed is forgotten.
func (r *rateReading) untriedResets() []time.Time {
	now := r.clock.now()
	var untried []time.Time
	for installation, held := range r.installations {
		for _, known := range []*knownReset{&held.resets.core, &held.resets.graphql} {
			if !known.at.After(now) {
				*known = knownReset{}
			}
			if reset, ok := known.untried(now); ok {
				untried = append(untried, reset)
			}
		}
		r.installations[installation] = held
	}
	return untried
}

// app keeps the App's numeric id and slug the reading names a line by, from installations, the
// list this pass read: each installation object names both (app_id, app_slug), and the last one
// named wins, so a renamed App's lines take its new slug. GET /app (githubapp.Client.AppIdentity)
// is read only where the list named either of them nowhere and the reading holds none, as a failed
// or empty list does. A failure there is logged and fails no reading: a line then names the App
// by what the list named, or by nothing.
func (r *rateReading) app(ctx context.Context, installations []githubapp.Installation) {
	for _, installation := range installations {
		if installation.AppID != 0 {
			r.appID = strconv.FormatInt(installation.AppID, 10)
		}
		if installation.AppSlug != "" {
			r.appSlug = installation.AppSlug
		}
	}
	if r.appID != "" && r.appSlug != "" {
		return
	}
	id, slug, err := r.client.AppIdentity(ctx)
	if err != nil {
		slog.Warn("dispatch: the GitHub rate reading names the App by what its installation list named: GET /app failed", "error", err)
		return
	}
	r.appID = cmp.Or(r.appID, strconv.FormatInt(id, 10))
	r.appSlug = cmp.Or(r.appSlug, slug)
}

// read reads installation's two counters with a token of its own, REST core as core reads it and
// GraphQL from GET /rate_limit, then this process's own calls on each, read last so the reading's
// own calls are among them. A counter whose reset passed before that snapshot is an error rather
// than an old reading paired with the new hour's own calls. Every read is sent past the shared
// ledger's hold on the installation (githubapp.Client.ReadPastHold): the installation the ledger
// holds is one that spent its budget, and its reading is the one that says so. A secondary limit
// GitHub answered the installation's token with is the one hold the reading obeys (held).
func (r *rateReading) read(ctx context.Context, installation int64) (core, graphql githubrate.Bucket, err error) {
	if err := r.held(installation); err != nil {
		return core, graphql, err
	}
	token, err := r.client.Token(ctx, installation)
	if err != nil {
		return core, graphql, fmt.Errorf("mint a token of installation %d: %w", installation, err)
	}
	core, err = r.core(ctx, installation, token)
	if err != nil {
		return core, graphql, err
	}
	body, status, header, err := r.pastHold(ctx, installation, token, "/rate_limit")
	if err == nil {
		err = githubapp.CheckResponse(status, header, body)
	}
	if err != nil {
		return core, graphql, fmt.Errorf("read the GraphQL counter: GET /rate_limit: %w", err)
	}
	var answer githubrate.RateLimitAnswer
	if err := body.Decode(&answer); err != nil {
		return core, graphql, fmt.Errorf("read the GraphQL counter: %w", err)
	}
	graphql, err = githubrate.GraphQLBucketOf(answer)
	if err != nil {
		return core, graphql, fmt.Errorf("read the GraphQL counter: %w", err)
	}
	core.Own, graphql.Own = r.client.OwnCalls(installation)
	if at := r.clock.now(); !at.Before(core.Reset) || !at.Before(graphql.Reset) {
		return core, graphql, errors.New("a counter reset before its own-call snapshot; the reading spans two rate windows")
	}
	return core, graphql, nil
}

// held is the error of a secondary limit that holds installation's reads on the reading's clock,
// and nil when none does. GitHub asks a client to send nothing while a secondary limit holds, and
// warns that it may ban an integration that goes on. The limit is the one the process's Counter
// noted from GitHub's own answer to one of the installation's tokens, whichever client of the
// process sent it (githubapp.Client.SecondaryUntil), so a secondary limit another process met is
// not seen here. A primary limit is not one: its refusal still names the spent counter, which is
// what the reading reads.
func (r *rateReading) held(installation int64) error {
	if until := r.client.SecondaryUntil(installation); until.After(r.clock.now()) {
		return fmt.Errorf("a GitHub secondary rate limit holds installation %d until %s, as GitHub answered this process; nothing was sent", installation, until.UTC().Format(time.RFC3339))
	}
	return nil
}

// pastHold is githubapp.Client.ReadPastHold of path with token, sent unless a secondary limit
// holds installation (held), as one GitHub answered an earlier read of this pass with can.
func (r *rateReading) pastHold(ctx context.Context, installation int64, token, path string) (redact.Answer, int, http.Header, error) {
	if err := r.held(installation); err != nil {
		return redact.Answer{}, 0, nil, err
	}
	return r.client.ReadPastHold(ctx, token, path)
}

// core is installation's REST core counter as the headers of
// GET /repos/OWNER/REPO/actions/workflows?per_page=1 on its first repository name it, whatever
// GitHub answered the read with: a refusal at the limit names the counter as any answer does.
// Where the listing of the installation's repositories gives no repository to read, though GitHub
// answered it (a refusal, no repository, a name that is no repository's, a body that is no JSON),
// the counter is read from the listing's own answer instead, which is a core call's too.
func (r *rateReading) core(ctx context.Context, installation int64, token string) (githubrate.Bucket, error) {
	repository, answered, err := r.repository(ctx, installation, token)
	if err != nil {
		if answered != nil {
			if core, unread := githubrate.CoreBucket(answered); unread == nil {
				return core, nil
			}
		}
		return githubrate.Bucket{}, err
	}
	path := "/repos/" + url.PathEscape(repository.owner) + "/" + url.PathEscape(repository.name) + "/actions/workflows?per_page=1"
	_, status, header, err := r.pastHold(ctx, installation, token, path)
	if err != nil {
		return githubrate.Bucket{}, fmt.Errorf("read the core counter: %w", err)
	}
	if status == http.StatusNotFound {
		// The repository is gone or out of the installation; the next pass lists another.
		delete(r.repositories, installation)
	}
	core, err := githubrate.CoreBucket(header)
	if err != nil {
		return githubrate.Bucket{}, fmt.Errorf("read the core counter from GET %s (status %d): %w", path, status, err)
	}
	return core, nil
}

// repository is the first repository GET /installation/repositories?per_page=1 lists for
// installation, read with token once and remembered. Where GitHub answered the listing and it
// gives no repository, answered is the headers of that answer, which the core counter can be read
// from; it is nil where GitHub gave no answer, and where the repository was remembered.
func (r *rateReading) repository(ctx context.Context, installation int64, token string) (repository repositoryName, answered http.Header, err error) {
	if repository, ok := r.repositories[installation]; ok {
		return repository, nil, nil
	}
	body, status, header, err := r.pastHold(ctx, installation, token, "/installation/repositories?per_page=1")
	if err != nil {
		return repositoryName{}, nil, fmt.Errorf("list installation %d's repositories: %w", installation, err)
	}
	if err := githubapp.CheckResponse(status, header, body); err != nil {
		return repositoryName{}, header, fmt.Errorf("list installation %d's repositories: %w", installation, err)
	}
	var listed struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := body.Decode(&listed); err != nil {
		return repositoryName{}, header, fmt.Errorf("decode installation %d's repositories: %w", installation, err)
	}
	if len(listed.Repositories) == 0 {
		return repositoryName{}, header, errors.New("the installation lists no repository to read the core counter on")
	}
	owner, name, err := refs.SplitGitHubRepo(listed.Repositories[0].FullName)
	if err != nil {
		return repositoryName{}, header, fmt.Errorf("the installation lists no repository to read the core counter on: its first %w", err)
	}
	repository = repositoryName{owner: owner, name: name}
	r.repositories[installation] = repository
	return repository, nil, nil
}
