package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/githubtest"
	"github.com/sjawhar/envoy/testwait"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// installations answers each App's installation on each repository, by "<role> <owner/name>";
// a repository it does not name is installed for both Apps, fail answers an error for every read
// and failing an error for the reads it names. delay is how long each read takes, as a GitHub
// that answers slowly; reads wait it out side by side.
type installations struct {
	mu      sync.Mutex
	missing map[string]bool
	fail    error
	failing map[string]error
	asked   []string
	delay   time.Duration
}

func (i *installations) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	if i.delay > 0 {
		select {
		case <-time.After(i.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	key := string(role) + " " + repo.String()
	i.asked = append(i.asked, key)
	if i.fail != nil {
		return false, i.fail
	}
	if err := i.failing[key]; err != nil {
		return false, err
	}
	return !i.missing[key], nil
}

// setFailing makes the reads keys names fail with err, or answer again when err is nil.
func (i *installations) setFailing(err error, keys ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failing == nil {
		i.failing = map[string]error{}
	}
	for _, key := range keys {
		i.failing[key] = err
	}
}

// reads is how many installation reads were asked of key ("<role> <owner/name>").
func (i *installations) reads(key string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	count := 0
	for _, asked := range i.asked {
		if asked == key {
			count++
		}
	}
	return count
}

// setMissing makes the reads keys names answer that the App is not installed, or installed when
// missing is false, as a transfer does to the name a repository moved away from.
func (i *installations) setMissing(missing bool, keys ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.missing == nil {
		i.missing = map[string]bool{}
	}
	for _, key := range keys {
		i.missing[key] = missing
	}
}

// asks is every installation read asked of i, as "<role> <owner/name>", sorted and with its
// repeats: what a case that pins the reads a mapping load makes compares, since the reads run at
// once and arrive in no order of their own.
func (i *installations) asks() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	asked := slices.Clone(i.asked)
	slices.Sort(asked)
	return asked
}

// mappingDispatch is a Dispatch whose repository mapping is mappings, whose project listing is
// listed (counted in listings, its next listFailures reads failing), whose issue detail is
// issues' entry for a key it names, the embedded issue for any other, and whose webhook App check
// answers not installed for each repository webhookMissing names, by its "<owner>/<name>" without
// case, and installed for every other (the embedded Dispatch's answer). webhookAsked is each
// repository it was asked the check for: the mapped name checkMapping reads it under, and GitHub's
// name of a repository only a record names (prepareUnmapped, checkRecordedWebhookApps).
type mappingDispatch struct {
	outboxDispatch
	mu             sync.Mutex
	mappings       []dispatch.RepoProject
	listed         []dispatch.IssueSummary
	listings       int
	listFailures   int
	issues         map[string]dispatch.Issue
	err            error
	webhookMissing map[string]bool
	webhookAsked   []string
}

func (d *mappingDispatch) WebhookAppInstalled(_ context.Context, repo string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.webhookAsked = append(d.webhookAsked, repo)
	return !d.webhookMissing[strings.ToLower(repo)], nil
}

// webhookChecks is every repository Dispatch was asked the webhook App check for, sorted.
func (d *mappingDispatch) webhookChecks() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	asked := slices.Clone(d.webhookAsked)
	slices.Sort(asked)
	return asked
}

func (d *mappingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatch.RepoProject{}, d.mappings...), d.err
}

func (d *mappingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listings++
	if d.listFailures > 0 {
		d.listFailures--
		return nil, &dispatch.TransientError{Err: errors.New("Dispatch answered 502 to the listing")}
	}
	return append([]dispatch.IssueSummary{}, d.listed...), nil
}

// listingCount is how many project listings were asked of d.
func (d *mappingDispatch) listingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listings
}

func (d *mappingDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	issue, named := d.issues[key]
	d.mu.Unlock()
	if named {
		return issue, nil
	}
	return d.outboxDispatch.GetIssue(ctx, key)
}

func (d *mappingDispatch) set(mappings ...dispatch.RepoProject) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mappings = mappings
}

// The mapping's own repositories, which are not all the project serves, are served only once both
// Apps are installed on it: a repository missing an App is left out with the App named, the
// owners are the served repositories', lowercased and sorted, and a served repository is held as
// GitHub names it, at its default branch.
func TestTheMappingServesOnlyRepositoriesBothAppsAreInstalledOn(t *testing.T) {
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main"})
	repos := newRepositories("LEGION", ghrepo.Renames{})
	installs := &installations{missing: map[string]bool{"review acme/gadgets": true}}
	client := &mappingDispatch{mappings: []dispatch.RepoProject{
		{Repo: "acme/widgets", Project: "LEGION", Default: true},
		{Repo: "acme/gadgets", Project: "LEGION"},
		{Repo: "zeta/tools", Project: "LEGION"},
		{Repo: "other/thing", Project: "OTHER", Default: true},
	}}
	branches := newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now)

	accepted, refused, _, err := readMapping(context.Background(), client, "LEGION", installs, ghrepo.Renames{})
	if err != nil {
		t.Fatalf("readMapping: %v", err)
	}
	var limited []limitedRead
	if accepted, limited, err = nameAccepted(context.Background(), accepted, branches); err != nil || len(limited) != 0 {
		t.Fatalf("nameAccepted: %v, rate limited %v; want every repository named", err, limited)
	}
	repos.set(accepted, refused)
	if got, want := repos.Owners(), []string{"acme", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Owners = %v, want %v", got, want)
	}
	if got := repos.Accepted(); len(got) != 2 || got[0] != ghrepo.MustParse("Acme/Widgets") || got[1] != ghrepo.MustParse("zeta/tools") {
		t.Fatalf("Accepted = %v, want Acme/Widgets and zeta/tools", got)
	}
	if got, want := repos.Refused(), []refusedRepository{{Repo: "acme/gadgets", Reason: "the review App is not installed on acme/gadgets", apps: appAnswers{answered: true, implement: true}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Refused = %#v, want %#v", got, want)
	}
	if mapped := repos.Mapped(); len(mapped) != 2 || !mapped[0].Default || mapped[1].Default {
		t.Fatalf("Mapped = %#v, want Acme/Widgets the default and zeta/tools not", mapped)
	}

	for _, tc := range []struct {
		resolved string
		want     workflow.Resolution
	}{
		{"acme/widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"Acme/Widgets", workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}},
		{"acme/gadgets", workflow.Resolution{Refusal: "the review App is not installed on acme/gadgets", LeftOut: true}},
		{"other/thing", workflow.Resolution{Refusal: "GitHub has not been read for other/thing yet", Unread: true}},
	} {
		got, err := (&workflowRuntime{repositories: repos, defaultBranches: branches}).ResolveRepository(tc.resolved)
		if err != nil || got != tc.want {
			t.Errorf("ResolveRepository(%s) = %#v, %v; want %#v", tc.resolved, got, err, tc.want)
		}
	}
}

// A repository both Apps lack names both. An installation read that fails, transient or not, is no
// answer: it leaves only its own repository out for now, naming the App, the repository and the
// failure, and marked as a failed read so it is read again, and the rest of the mapping is checked
// as it stands. Each refusal keeps what GitHub answered of both Apps for legion state wherever it
// answered both reads, and nothing where a read failed, since that read answered nothing of its
// App: legion state carried the constant "both installed" on served entries alone before, and a
// reader parsed the reason to learn which App was missing.
func TestTheMappingNamesEveryMissingAppAndLeavesOutOnlyARepositoryWhoseReadFailed(t *testing.T) {
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}}}
	installs := &installations{missing: map[string]bool{"implement acme/gadgets": true, "review acme/gadgets": true}}
	accepted, refused, _, err := readMapping(context.Background(), client, "LEGION", installs, ghrepo.Renames{})
	if err != nil || len(accepted) != 0 || len(refused) != 1 || refused[0].Reason != "the implement and review Apps are not installed on acme/gadgets" || refused[0].readErr != nil {
		t.Fatalf("readMapping = %#v, %#v, %v; want acme/gadgets left out naming both Apps", accepted, refused, err)
	}
	if got, want := refused[0].apps, (appAnswers{answered: true}); got != want {
		t.Fatalf("acme/gadgets' Apps = %+v, want %+v: GitHub answered both, neither installed", got, want)
	}

	for _, failure := range []error{&appauth.TransientError{Err: errors.New("GitHub answered 502")}, errors.New("GitHub answered 403")} {
		client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}}
		installs := &installations{}
		installs.setFailing(failure, "review acme/gadgets")
		accepted, refused, _, err := readMapping(context.Background(), client, "LEGION", installs, ghrepo.Renames{})
		if err != nil {
			t.Fatalf("readMapping with %v on acme/gadgets = %v, want acme/gadgets left out and the rest checked", failure, err)
		}
		if len(accepted) != 1 || accepted[0].Repo != ghrepo.MustParse("acme/widgets") {
			t.Fatalf("accepted = %#v, want acme/widgets", accepted)
		}
		want := "the review App's installation on acme/gadgets could not be read: " + failure.Error() + "; Legion reads it again"
		if len(refused) != 1 || refused[0].Repo != "acme/gadgets" || refused[0].Reason != want || !errors.Is(refused[0].readErr, failure) {
			t.Fatalf("refused = %#v, want acme/gadgets refused for now with %q", refused, want)
		}
		if refused[0].apps.answered {
			t.Fatalf("acme/gadgets' Apps = %+v with its review App read failing (%v), want no answer kept", refused[0].apps, failure)
		}
	}

	// An App GitHub answered is missing leaves the repository out, but a read that failed to
	// answer beside it makes that refusal a refusal for now: Legion reads the entry again until
	// GitHub has answered every read, since a transient failure must never settle an entry for
	// good.
	installs = &installations{missing: map[string]bool{"review acme/gadgets": true}}
	failure := errors.New("GitHub answered 502")
	installs.setFailing(failure, "implement acme/gadgets")
	_, refused, _, err = readMapping(context.Background(), client, "LEGION", installs, ghrepo.Renames{})
	want := "the review App is not installed on acme/gadgets; the implement App's installation on acme/gadgets could not be read: GitHub answered 502; Legion reads it again"
	if err != nil || len(refused) != 1 || refused[0].Reason != want || !errors.Is(refused[0].readErr, failure) || refused[0].apps.answered {
		t.Fatalf("readMapping = %#v, %v; want acme/gadgets refused for now with %q and no answer of the Apps kept", refused, err, want)
	}
}

// legion state carries, on each refused repository, what GitHub answered of both role Apps on it
// (api.RefusedRepositoryView.Apps): the App missing named as a field where GitHub answered both
// reads, both installed on one left out for its default-branch read alone, and no answer at all
// where a read failed or Dispatch's name is no <owner>/<name>. A served repository carries both
// Apps as installed, as before.
func TestTheStateNamesEachRefusedRepositorysAppsWhereGitHubAnswered(t *testing.T) {
	client := &mappingDispatch{mappings: []dispatch.RepoProject{
		{Repo: "acme/widgets", Project: "LEGION", Default: true},
		{Repo: "acme/gadgets", Project: "LEGION"},
		{Repo: "acme/tools", Project: "LEGION"},
		{Repo: "not a repository", Project: "LEGION"},
	}}
	installs := &installations{missing: map[string]bool{"implement acme/gadgets": true}}
	installs.setFailing(errors.New("GitHub answered 502"), "review acme/tools")
	accepted, refused, _, err := readMapping(context.Background(), client, "LEGION", installs, ghrepo.Renames{})
	if err != nil {
		t.Fatalf("readMapping: %v", err)
	}
	limited := unreadDefaultBranch(mappedRepository{Repo: ghrepo.MustParse("zeta/limited")}, errors.New("GitHub's rate limit"))
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.set(accepted, append(refused, limited))

	view := repos.view(nil)
	if len(view.Mapped) != 1 || view.Mapped[0].Apps != (api.AppInstallationsView{Implement: true, Review: true}) {
		t.Fatalf("served = %+v, want acme/widgets with both Apps installed", view.Mapped)
	}
	apps := map[string]*api.AppInstallationsView{}
	for _, entry := range view.Refused {
		apps[entry.Repo] = entry.Apps
	}
	for repo, want := range map[string]*api.AppInstallationsView{
		"acme/gadgets":     {Review: true},
		"zeta/limited":     {Implement: true, Review: true},
		"acme/tools":       nil,
		"not a repository": nil,
	} {
		got, held := apps[repo]
		if !held {
			t.Fatalf("refused = %+v, want %s among them", view.Refused, repo)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s's Apps = %+v, want %+v", repo, got, want)
		}
	}
	encoded, err := json.Marshal(view.Refused)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"repo":"acme/gadgets","reason":"the implement App is not installed on acme/gadgets","apps":{"implement":false,"review":true}}`; !strings.Contains(string(encoded), want) {
		t.Errorf("refused on the wire = %s, want it to hold %s", encoded, want)
	}
	if strings.Count(string(encoded), `"apps"`) != 2 {
		t.Errorf("refused on the wire = %s, want apps on acme/gadgets and zeta/limited alone", encoded)
	}
}

// checkMapping bounds each installation read by installReadAttempt and nothing else: twenty slow
// reads, more than two rounds of mappingReads, are each answered and every repository served,
// however long the whole check takes, and a read longer than installReadAttempt is a failed read,
// refused for now. This guards checkMapping's own bound, which the code before this check already
// had; the removed bound on a refresh as a whole, which cut such a check off, is
// TestASlowButAnsweringGitHubIsServedOnARefresh's.
func TestEachInstallationReadIsBoundedOnItsOwn(t *testing.T) {
	read := installReadAttempt
	t.Cleanup(func() { installReadAttempt = read })
	var mappings []dispatch.RepoProject
	for i := range 10 {
		mappings = append(mappings, dispatch.RepoProject{Repo: fmt.Sprintf("acme/repo-%d", i), Project: "LEGION", Default: i == 0})
	}
	installReadAttempt = 400 * time.Millisecond
	accepted, refused, _, err := checkMapping(context.Background(), &mappingDispatch{}, mappings, "LEGION", &installations{delay: 150 * time.Millisecond}, ghrepo.Renames{})
	if err != nil || len(accepted) != 10 || len(refused) != 0 {
		t.Fatalf("checkMapping of 20 slow reads = %d accepted, %#v, %v; want all 10 served", len(accepted), refused, err)
	}

	installReadAttempt = 50 * time.Millisecond
	accepted, refused, _, err = checkMapping(context.Background(), &mappingDispatch{}, mappings[:1], "LEGION", &installations{delay: 150 * time.Millisecond}, ghrepo.Renames{})
	if err != nil || len(accepted) != 0 || len(refused) != 1 || !errors.Is(refused[0].readErr, context.DeadlineExceeded) {
		t.Fatalf("checkMapping of a read longer than installReadAttempt = %#v, %#v, %v; want it refused for now as a failed read", accepted, refused, err)
	}
}

// The boot refuses only a project with no repository mapped. A mapping whose every repository
// lacks an App boots serving none, each left out and reported; a mapping with one served
// repository and one left out boots serving the one.
func TestTheBootRefusesOnlyAnEmptyMapping(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mappings        []dispatch.RepoProject
		missing         map[string]bool
		want            string
		served, leftOut int
	}{
		{name: "no repository mapped", mappings: []dispatch.RepoProject{{Repo: "other/thing", Project: "OTHER"}},
			want: "project LEGION has no repository mapped in Dispatch (Settings → Repositories → Projects)"},
		{name: "no repository with both Apps", mappings: []dispatch.RepoProject{{Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, leftOut: 1},
		{name: "one served, one left out", mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION"}, {Repo: "acme/gadgets", Project: "LEGION"}},
			missing: map[string]bool{"review acme/gadgets": true}, served: 1, leftOut: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{dispatch: &mappingDispatch{mappings: tc.mappings}, dispatchProject: "LEGION", installs: &installations{missing: tc.missing},
				repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
			err := w.loadMapping(context.Background())
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("loadMapping = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || len(w.repositories.Accepted()) != tc.served || len(w.repositories.Refused()) != tc.leftOut {
				t.Fatalf("loadMapping = %v, served %v, left out %v; want it to boot serving %d with %d left out",
					err, w.repositories.Accepted(), w.repositories.Refused(), tc.served, tc.leftOut)
			}
		})
	}
}

// unreadyDispatch is a Dispatch with no ready endpoint for its first refusals reads of the
// repository mapping: each is refused at the dial, as a Service with no ready endpoint refuses it,
// and reported as the real client reports a refused dial (a TransientError around it). Every read
// after them answers the mapping.
type unreadyDispatch struct {
	mappingDispatch
	refusals int64
	reads    atomic.Int64
}

func (d *unreadyDispatch) ListRepoProjects(ctx context.Context) ([]dispatch.RepoProject, error) {
	if d.reads.Add(1) <= d.refusals {
		return nil, &dispatch.TransientError{Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	}
	return d.mappingDispatch.ListRepoProjects(ctx)
}

// A Dispatch with no ready endpoint refuses every dial for as long as it has none. The boot's read
// of the mapping waits that out without limit (readinessRetry), logging each wait, as the boot's
// listing for admission does: bounded at appMintRetry's attempts, it exited the daemon, whose
// supervisor restarted it straight back into the same refusal. Here Dispatch refuses two reads more
// than appMintRetry's budget allows and then answers, and the boot serves the mapped repository.
// GitHub's installation reads never refuse the boot (TestABootServesTheRepositoriesWhoseInstallationReadsAnswer).
func TestTheBootWaitsForTheMappingWhileDispatchRefusesEveryDial(t *testing.T) {
	quickAppMints(t)
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})
	mapped := []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}}
	refusals := appMintRetry.Attempts + 2
	client := &unreadyDispatch{mappingDispatch: mappingDispatch{mappings: mapped}, refusals: int64(refusals)}
	var logged bytes.Buffer
	w := &workflowRuntime{dispatch: client, dispatchProject: "LEGION", installs: &installations{},
		repositories: newRepositories("LEGION", ghrepo.Renames{}), log: slog.New(slog.NewTextHandler(&logged, nil))}
	if err := w.loadMapping(context.Background()); err != nil {
		t.Fatalf("loadMapping = %v, want it to wait out %d refused dials and serve acme/widgets", err, refusals)
	}
	if got := w.repositories.Accepted(); len(got) != 1 || got[0] != ghrepo.MustParse("acme/widgets") {
		t.Fatalf("Accepted = %v, want acme/widgets", got)
	}
	if got, want := client.reads.Load(), int64(refusals)+1; got != want {
		t.Fatalf("Dispatch saw %d mapping reads, want %d: the %d refused and the one that answered", got, want, refusals)
	}
	if got := strings.Count(logged.String(), `probe="repository mapping read"`); got != refusals {
		t.Fatalf("the boot logged %d waits on the mapping read, want one per refused dial (%d):\n%s", got, refusals, logged.String())
	}
	if !strings.Contains(logged.String(), "connect: connection refused") {
		t.Fatalf("the waits do not name the refused dial:\n%s", logged.String())
	}
}

// A boot whose installation read fails for one repository, transient or not, serves the
// repositories GitHub answered for and leaves that one out for now, naming the App, the repository
// and the failure, where it refused the whole boot and its supervisor restarted it straight back
// into GitHub's same failure. The state shows the refusal.
func TestABootServesTheRepositoriesWhoseInstallationReadsAnswer(t *testing.T) {
	for _, failure := range []error{&appauth.TransientError{Err: errors.New("GitHub answered 502")}, errors.New("read whether the implement App is installed on acme/gadgets failed (403)")} {
		installs := &installations{}
		installs.setFailing(failure, "implement acme/gadgets")
		var logged bytes.Buffer
		w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}},
			dispatchProject: "LEGION", installs: installs, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: slog.New(slog.NewTextHandler(&logged, nil))}
		if err := w.loadMapping(context.Background()); err != nil {
			t.Fatalf("loadMapping with %v on acme/gadgets = %v, want the boot to go on", failure, err)
		}
		if got := w.repositories.Accepted(); len(got) != 1 || got[0] != ghrepo.MustParse("acme/widgets") {
			t.Fatalf("Accepted = %v, want acme/widgets", got)
		}
		view := w.repositories.view(nil)
		want := "the implement App's installation on acme/gadgets could not be read: " + failure.Error() + "; Legion reads it again"
		if len(view.Refused) != 1 || view.Refused[0] != (api.RefusedRepositoryView{Repo: "acme/gadgets", Reason: want}) {
			t.Fatalf("state's refused = %#v, want acme/gadgets with %q", view.Refused, want)
		}
		if !strings.Contains(logged.String(), "level=WARN") || strings.Contains(logged.String(), "level=ERROR") {
			t.Fatalf("boot log = %s, want one warning and no error while acme/widgets is served", logged.String())
		}
	}
}

// A boot whose every repository's installation read fails still comes up, serving none, and logs
// one ERROR line naming the first failure: its API and recovery stay up, and the read again heals it.
func TestABootWhoseEveryInstallationReadFailsServesNoneAndLogsOneError(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 403: Resource not accessible by integration"), "implement acme/widgets", "review acme/widgets", "implement acme/gadgets", "review acme/gadgets")
	var logged bytes.Buffer
	w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "acme/gadgets", Project: "LEGION"}}},
		dispatchProject: "LEGION", installs: installs, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: slog.New(slog.NewTextHandler(&logged, nil))}
	if err := w.loadMapping(context.Background()); err != nil {
		t.Fatalf("loadMapping with every installation read failing = %v, want a boot serving none", err)
	}
	if got := w.repositories.Accepted(); len(got) != 0 {
		t.Fatalf("Accepted = %v, want none", got)
	}
	if got := readFailures(w.repositories.Refused()); len(got) != 2 {
		t.Fatalf("refused for a failed read = %#v, want both repositories", got)
	}
	var errorLines []string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "level=ERROR") {
			errorLines = append(errorLines, line)
		}
	}
	if len(errorLines) != 1 || !strings.Contains(errorLines[0], "every mapped repository's GitHub App installation read failed") ||
		!strings.Contains(errorLines[0], "check the implement App's installation on acme/widgets: GitHub answered 403: Resource not accessible by integration") {
		t.Fatalf("ERROR lines = %q, want one naming the first failure (acme/widgets' implement read)", errorLines)
	}
}

// The boot's mapping load says once of each `commit_issue_trailer_repos` name Dispatch does not
// map to this project that the mapping does not hold it, naming the name and the project, and
// says nothing of a name the project is mapped, whatever the case it is configured in. It reads
// the mapping's own equivalence, the one the stamping decision reads (specs.issueTrailer), so a
// configured name the mapping holds only under the other name of a configured rename is said
// nothing about.
//
// It is a statement about the mapping and no more: the stamping decision reads no mapping, so a
// pane working a repository the mapping does not hold stamps its commits all the same, and this
// line catches the typo it exists for. It refuses no boot and widens nothing: the served set is
// Dispatch's mapping either way, and `legion config check` reads no Dispatch at all, so
// the file alone cannot decide it.
func TestTheBootWarnsOfACommitTrailerRepositoryTheProjectIsNotMapped(t *testing.T) {
	const unmapped = "commit_issue_trailer_repos names a repository Dispatch does not map to this project"
	for _, tc := range []struct {
		name     string
		repos    []ghrepo.Repository
		renames  ghrepo.Renames
		warnings int
		named    string
	}{
		{name: "a name the mapping does not hold", repos: []ghrepo.Repository{ghrepo.MustParse("acme/gadgets")},
			warnings: 1, named: "repository=acme/gadgets project=LEGION"},
		{name: "a served name", repos: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}},
		{name: "a served name in another case", repos: []ghrepo.Repository{ghrepo.MustParse("Acme/Widgets")}},
		{name: "the name a configured rename moved the served repository away from",
			repos:   []ghrepo.Repository{ghrepo.MustParse("acme/widgets-legacy")},
			renames: ghrepo.MustRenames(ghrepo.MustRename("acme/widgets-legacy", "acme/widgets"))},
		{name: "one of each", repos: []ghrepo.Repository{ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("acme/gadgets")},
			warnings: 1, named: "repository=acme/gadgets project=LEGION"},
		{name: "none configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			w := &workflowRuntime{dispatch: &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "zeta/tools", Project: "LEGION"}}},
				dispatchProject: "LEGION", installs: &installations{}, commitIssueTrailerRepos: tc.repos,
				repositories: newRepositories("LEGION", tc.renames), log: slog.New(slog.NewTextHandler(&logged, nil))}
			if err := w.loadMapping(context.Background()); err != nil {
				t.Fatalf("loadMapping = %v, want the boot to go on", err)
			}
			if got := repositoryNames(w.repositories.Accepted()); !slices.Equal(got, []string{"acme/widgets", "zeta/tools"}) {
				t.Fatalf("served %v, want Dispatch's mapping alone: the key selects, it never widens", got)
			}
			if got := strings.Count(logged.String(), unmapped); got != tc.warnings {
				t.Fatalf("the boot says %d times that a configured repository is not mapped, want %d:\n%s", got, tc.warnings, logged.String())
			}
			if tc.named != "" && !strings.Contains(logged.String(), tc.named) {
				t.Fatalf("the boot's line does not name %q:\n%s", tc.named, logged.String())
			}
		})
	}
}

// webhookDispatch is a Dispatch over HTTP, read through the real client, that maps acme/widgets
// (its default) and zeta/tools to LEGION and answers its webhook App check for each repository
// installed names, by "owner/name", or with the status and body failing names for it. Without
// route it is a Dispatch older than the check: the route is not found. asked counts each
// repository's checks.
type webhookDispatch struct {
	url string

	mu        sync.Mutex
	route     bool
	installed map[string]bool
	failing   map[string]string
	asked     map[string]int
}

func newWebhookDispatch(t *testing.T, route bool, installed map[string]bool) *webhookDispatch {
	t.Helper()
	d := &webhookDispatch{route: route, installed: installed, failing: map[string]string{}, asked: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, check := strings.CutPrefix(r.URL.Path, "/api/v1/github/app-installation/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/repo-projects":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"repo":"acme/widgets","project":"LEGION","default":true},{"repo":"zeta/tools","project":"LEGION","default":false}]`))
		case r.Method == http.MethodGet && check:
			d.mu.Lock()
			defer d.mu.Unlock()
			d.asked[repo]++
			if !d.route {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if body, failing := d.failing[repo]; failing {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = fmt.Fprintf(w, `{"installed":%t}`, d.installed[repo])
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	return d
}

// serve is the rig option that has the runtime read Dispatch from d, through the real client.
func (d *webhookDispatch) serve(w *workflowRuntime) {
	w.dispatch = dispatch.New(d.url, "dispatch-test-token")
}

// install has the check answer that Dispatch's GitHub App is installed on repo.
func (d *webhookDispatch) install(repo string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.installed[repo] = true
}

// fail has the check answer 502 with body for repo.
func (d *webhookDispatch) fail(repo, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing[repo] = body
}

// checks is how many webhook App checks Dispatch was asked for repo.
func (d *webhookDispatch) checks(repo string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked[repo]
}

// A served repository Dispatch's GitHub App is not installed on is one GitHub sends Legion no
// event of: the daemon serves it all the same and the state names it with why. A repository a
// role App is missing on is named only as refused. Once the App is installed, the next read of the
// mapping names nothing.
func TestARepositoryWithoutTheWebhookAppIsServedAndNamed(t *testing.T) {
	standIn := newWebhookDispatch(t, true, map[string]bool{"acme/widgets": false, "zeta/tools": false})
	w, _, _, _ := mappingRigWith(t, &installations{missing: map[string]bool{"review zeta/tools": true}}, standIn.serve)
	if !served(w, "acme/widgets") || served(w, "zeta/tools") {
		t.Fatalf("served acme/widgets %v, zeta/tools %v; want acme/widgets alone", served(w, "acme/widgets"), served(w, "zeta/tools"))
	}
	view := w.repositories.view(nil)
	want := []api.WebhookAppGapView{{Repo: "acme/widgets", Reason: "Dispatch's GitHub App is not installed on acme/widgets: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed"}}
	if !reflect.DeepEqual(view.WithoutWebhookApp, want) {
		t.Fatalf("state's withoutWebhookApp = %#v, want %#v", view.WithoutWebhookApp, want)
	}
	if len(view.Refused) != 1 || view.Refused[0].Repo != "zeta/tools" {
		t.Fatalf("state's refused = %#v, want zeta/tools alone", view.Refused)
	}

	standIn.install("acme/widgets")
	if err := w.refreshMapping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.repositories.view(nil).WithoutWebhookApp; len(got) != 0 {
		t.Fatalf("state's withoutWebhookApp once the App is installed = %#v, want none", got)
	}
}

// A webhook App check Dispatch fails is no answer: the state names the repository with the
// failure, and the repository is served, never left out for it nor read again of the daemon's own.
func TestAFailedWebhookAppCheckIsNamedAndTheRepositoryKeepsBeingServed(t *testing.T) {
	quickInstallReads(t)
	standIn := newWebhookDispatch(t, true, map[string]bool{"zeta/tools": true})
	message := "read the GitHub App's installation on acme/widgets: GET https://api.github.com/repos/acme/widgets/installation: status 500: Server Error"
	standIn.fail("acme/widgets", `{"code":"GITHUB_UNREADABLE","error":"`+message+`"}`)
	w, _, _, _ := mappingRigWith(t, &installations{}, standIn.serve)
	if !served(w, "acme/widgets") || !served(w, "zeta/tools") {
		t.Fatalf("served acme/widgets %v, zeta/tools %v; want both", served(w, "acme/widgets"), served(w, "zeta/tools"))
	}
	want := []api.WebhookAppGapView{{Repo: "acme/widgets", Reason: "the webhook App check failed: GITHUB_UNREADABLE: " + message}}
	if got := w.repositories.view(nil).WithoutWebhookApp; !reflect.DeepEqual(got, want) {
		t.Fatalf("state's withoutWebhookApp = %#v, want %#v", got, want)
	}
	if waiting := w.repositories.awaitingRead(); len(waiting) != 0 {
		t.Fatalf("awaiting a read again = %q, want none: a failed webhook App check asks for none", waiting)
	}
	// The refresher reads again within installReadRetry.Max of a failed installation read; ten
	// of them pass with no read.
	time.Sleep(10 * installReadRetry.Max)
	if got := standIn.checks("acme/widgets"); got != 1 {
		t.Fatalf("Dispatch was asked the webhook App check of acme/widgets %d times, want once", got)
	}
}

// A Dispatch older than the webhook App check answers its route 404: the state names every
// served repository with that, and serves each.
func TestAnOlderDispatchWithoutTheWebhookAppRouteIsNamed(t *testing.T) {
	standIn := newWebhookDispatch(t, false, nil)
	w, _, _, _ := mappingRigWith(t, &installations{}, standIn.serve)
	if !served(w, "acme/widgets") || !served(w, "zeta/tools") {
		t.Fatalf("served acme/widgets %v, zeta/tools %v; want both", served(w, "acme/widgets"), served(w, "zeta/tools"))
	}
	want := []api.WebhookAppGapView{
		{Repo: "acme/widgets", Reason: "this Dispatch does not answer the webhook App check"},
		{Repo: "zeta/tools", Reason: "this Dispatch does not answer the webhook App check"},
	}
	if got := w.repositories.view(nil).WithoutWebhookApp; !reflect.DeepEqual(got, want) {
		t.Fatalf("state's withoutWebhookApp = %#v, want %#v", got, want)
	}
}

// unmappedWithoutWebhookApp is the state's gap for unmappedNamed while Dispatch's GitHub App is not
// installed on it, named as a mapped repository's is (checkWebhookApp).
var unmappedWithoutWebhookApp = []api.WebhookAppGapView{{Repo: unmappedNamed, Reason: "Dispatch's GitHub App is not installed on " + unmappedNamed +
	": GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed"}}

// withoutWebhookAppFor is the rig option under which Dispatch answers that its GitHub App is not
// installed on unmappedRepo, a repository no mapping names.
func withoutWebhookAppFor(w *workflowRuntime) {
	w.dispatch.(*mappingDispatch).webhookMissing = map[string]bool{unmappedRepo: true}
}

// A repository only a live record names is served as a mapped one is, so the state names it
// without Dispatch's GitHub App, with why, exactly as it names a mapped one (INFRA-2543 R6): from
// the preparation that read GitHub for it, so from the record that first names it, and again at
// each read of the mapping, which takes an App installed since. Only the mapping's repositories
// were ever asked of before, so a tree on such a repository stood still for want of GitHub's
// events while the state said nothing of why. The repository is served throughout and the issue
// recorded on it: a missing Dispatch App refuses no work.
func TestARepositoryOnlyARecordNamesWithoutTheWebhookAppIsServedAndNamed(t *testing.T) {
	const key = "LEGION-951"
	w, client, _, _ := unmappedRig(t, &installations{}, withoutWebhookAppFor)
	deliver(t, w, client, issue(key, unmappedRepo, "todo", true, "", 1))
	if got := recordedIssue(t, w, key); got == nil || got.Repo != unmappedNamed {
		t.Fatalf("%s = %+v, want it recorded on %s whatever Dispatch's GitHub App", key, got, unmappedNamed)
	}
	view := w.repositories.view(nil)
	if listed := servedNames(w); !slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state serves %v, want %s among them: a missing Dispatch App refuses nothing", listed, unmappedNamed)
	}
	if !reflect.DeepEqual(view.WithoutWebhookApp, unmappedWithoutWebhookApp) {
		t.Fatalf("state's withoutWebhookApp = %#v, want %#v: a live record names %s and no mapping does", view.WithoutWebhookApp, unmappedWithoutWebhookApp, unmappedNamed)
	}
	if len(view.Refused) != 0 {
		t.Fatalf("state's refused = %#v, want none: a missing Dispatch App leaves nothing out", view.Refused)
	}

	client.mu.Lock()
	client.webhookMissing = nil
	client.mu.Unlock()
	w.requestMappingRead()
	testwait.Eventually(t, "the state naming no repository without Dispatch's GitHub App once it is installed on "+unmappedNamed, func() bool {
		return len(w.repositories.view(nil).WithoutWebhookApp) == 0
	})
	if listed := servedNames(w); !slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state serves %v once Dispatch's GitHub App is installed, want %s among them", listed, unmappedNamed)
	}
}

// A daemon that boots over a live tree on a repository no mapping names names it without Dispatch's
// GitHub App from the records, as it lists the repository from them (served): no preparation of
// the restarted process has read anything of it, so the gap stands on no read a restart forgets.
func TestARepositoryOnlyALiveRecordNamesIsNamedWithoutTheWebhookAppAfterARestart(t *testing.T) {
	ctx := context.Background()
	w, _, installs, _ := unmappedRig(t, &installations{}, withoutWebhookAppFor, func(w *workflowRuntime) {
		// The tree an earlier process recorded, which this one boots over.
		if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
			return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-952", Tree: "LEGION-952", Project: "LEGION", Title: "a tree on a repository no project maps",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", Repo: unmappedNamed, DefaultBranch: "trunk", GitHubRepo: unmappedNamed})
		}); err != nil {
			t.Fatal(err)
		}
	})
	testwait.Eventually(t, "the state naming "+unmappedNamed+" without Dispatch's GitHub App from the records", func() bool {
		return reflect.DeepEqual(w.repositories.view(nil).WithoutWebhookApp, unmappedWithoutWebhookApp)
	})
	if listed := servedNames(w); !slices.Contains(listed, unmappedNamed) {
		t.Fatalf("the state serves %v, want %s among them: a live record names it, and a missing Dispatch App refuses nothing", listed, unmappedNamed)
	}
	if reads := installs.reads("implement " + unmappedRepo); reads != 0 {
		t.Fatalf("the restarted daemon made %d installation reads of %s, want none: the gap stands on the records, not on a preparation", reads, unmappedRepo)
	}
}

// hangingWebhookDispatch is a Dispatch whose webhook App check answers only once its context
// ends, as one whose own read of GitHub never returns, keeping each repository it was asked of
// (mappingDispatch.webhookChecks).
type hangingWebhookDispatch struct{ mappingDispatch }

func (d *hangingWebhookDispatch) WebhookAppInstalled(ctx context.Context, repo string) (bool, error) {
	d.mu.Lock()
	d.webhookAsked = append(d.webhookAsked, repo)
	d.mu.Unlock()
	<-ctx.Done()
	return false, ctx.Err()
}

// The refresher's check of the repositories only a live record names is bounded as checkMapping's
// reads are: each check by installReadAttempt, mappingReads of them at once, and nothing bounds the
// whole. A Dispatch that never answers costs each repository installReadAttempt, so more
// repositories than one round of mappingReads are each named with the check's deadline, the
// repository the mapping serves is asked nothing apart, and every one of them is still served.
func TestTheCheckOfTheRepositoriesOnlyARecordNamesIsBoundedPerCheck(t *testing.T) {
	read := installReadAttempt
	t.Cleanup(func() { installReadAttempt = read })
	installReadAttempt = 50 * time.Millisecond
	client := &hangingWebhookDispatch{}
	w := &workflowRuntime{dispatch: client, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	live := map[string]ghrepo.Repository{"acme/widgets": ghrepo.MustParse("acme/widgets")}
	var want []webhookAppGap
	for i := range mappingReads + 2 {
		repo := ghrepo.MustParse(fmt.Sprintf("stranger/repo-%02d", i))
		live[repo.String()] = repo
		want = append(want, webhookAppGap{Repo: repo.String(), Reason: "the webhook App check failed: " + context.DeadlineExceeded.Error()})
	}
	w.repositories.setLive(live)
	running, stop := context.WithCancel(context.Background())
	refreshed := make(chan struct{})
	go func() { w.refreshOnMappingChange(running); close(refreshed) }()
	t.Cleanup(func() { stop(); <-refreshed })
	testwait.Eventually(t, "each repository only a record names named with the check's deadline", func() bool {
		return slices.Equal(w.repositories.WithoutWebhookApp(), want)
	})
	if got := len(w.repositories.served()); got != len(live) {
		t.Fatalf("served %d repositories, want all %d: a check that failed refuses nothing", got, len(live))
	}
	if asked := client.webhookChecks(); slices.Contains(asked, "acme/widgets") {
		t.Fatalf("Dispatch was asked of %v, want acme/widgets among none of them: the mapping's own reads ask of it", asked)
	}
}

// namedGitHub answers GET /repos/{owner}/{repo} for each repository it names, as
// "<GitHub's spelling>@<default branch>", or as "301 <owner>/<name>" for one GitHub renamed, which
// it answers as GitHub does: a 301 to the new name's path.
type namedGitHub struct{ url string }

func newNamedGitHub(t *testing.T, repos map[string]string) *namedGitHub {
	t.Helper()
	return &namedGitHub{url: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		named, ok := repos[strings.ToLower(path)]
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if renamed, moved := strings.CutPrefix(named, "301 "); moved {
			http.Redirect(w, r, "/repos/"+renamed, http.StatusMovedPermanently)
			return
		}
		name, branch, _ := strings.Cut(named, "@")
		_, _ = w.Write([]byte(`{"full_name":"` + name + `","default_branch":"` + branch + `"}`))
	}})}
}

// The state's mapping carries each mapped repository's cache, matched by repository whatever its
// case, with its last health read; a cache whose repository the mapping no longer names is listed
// apart with when the sweep retires it; a mapped repository with no cache carries none.
func TestTheMappingsViewCarriesTheRepositoryCaches(t *testing.T) {
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.set([]mappedRepository{
		{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true},
		{Repo: ghrepo.MustParse("zeta/tools")},
	}, nil)
	fetched := time.Date(2026, 10, 16, 19, 20, 30, 0, time.UTC)
	retained := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	view := repos.view([]sandbox.CacheStatus{
		{Repo: ghrepo.MustParse("acme/legacy"), Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing",
			Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained},
		{Repo: ghrepo.MustParse("acme/widgets"), Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
			Health: &repocache.Health{Ready: true, Repo: "acme/widgets", Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678}},
	})
	want := api.RepositoriesView{
		Owners: []string{"acme", "zeta"},
		Mapped: []api.MappedRepositoryView{
			{Repo: "Acme/Widgets", Default: true, Apps: api.AppInstallationsView{Implement: true, Review: true}, Cache: &api.CacheView{
				Repo: "acme/widgets", Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
				Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678,
			}},
			{Repo: "zeta/tools", Apps: api.AppInstallationsView{Implement: true, Review: true}},
		},
		Caches: []api.CacheView{{Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing", Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained}},
	}
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("view = %+v\nwant %+v", view, want)
	}
}

// The state's repositories are every repository the daemon serves: the ones Dispatch's mapping
// names and the ones the project's own live records name, each carrying both role Apps as
// installed, which is what each of the two doors admits a repository on (INFRA-2543 R6). A
// repository only a record names is no repository the daemon serves no longer, so its cache is
// its own entry's and not one of the caches listed apart, and the project's default is the
// mapping's alone. A read a preparation kept for a repository no record names puts nothing in
// the state: the preparation of a person's own todo issue keeps one, and no tree works there. A
// repository the mapping gains after an issue was recorded on it is served once, as the
// mapping's entry, since that is the entry a default can stand on, and one the mapping leaves
// out stands under the refusals alone, with why, never beside them as one both Apps are
// installed on.
func TestTheStatesRepositoriesHoldTheOnesOnlyARecordNames(t *testing.T) {
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true}}, nil)
	repos.addLive(ghrepo.MustParse("Zeta/Tools"))
	repos.recordRepository("stranger/tools", ghbranch.Repository{Name: ghrepo.MustParse("Stranger/Tools"), DefaultBranch: "trunk"})
	retained := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	caches := []sandbox.CacheStatus{
		{Repo: ghrepo.MustParse("zeta/tools"), Sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e", State: "cloning"},
		{Repo: ghrepo.MustParse("acme/legacy"), Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "starting", RetainedUntil: retained},
	}
	want := api.RepositoriesView{
		Owners: []string{"acme"},
		Mapped: []api.MappedRepositoryView{
			{Repo: "Acme/Widgets", Default: true, Apps: api.AppInstallationsView{Implement: true, Review: true}},
			{Repo: "Zeta/Tools", Apps: api.AppInstallationsView{Implement: true, Review: true}, Cache: &api.CacheView{
				Repo: "zeta/tools", Sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e", State: "cloning",
			}},
		},
		Caches: []api.CacheView{{Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "starting", RetainedUntil: retained}},
	}
	if view := repos.view(caches); !reflect.DeepEqual(view, want) {
		t.Fatalf("view = %+v\nwant %+v", view, want)
	}

	repos.set([]mappedRepository{
		{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true},
		{Repo: ghrepo.MustParse("Zeta/Tools")},
	}, nil)
	view := repos.view(nil)
	if len(view.Mapped) != 2 || view.Mapped[1].Repo != "Zeta/Tools" {
		t.Fatalf("the mapping's own entries = %+v, want acme/widgets and zeta/tools once each", view.Mapped)
	}
	if !slices.Equal(view.Owners, []string{"acme", "zeta"}) {
		t.Fatalf("owners = %v, want the owners of the repositories the mapping names", view.Owners)
	}

	missing := "the review App is not installed on zeta/tools"
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true}}, []refusedRepository{{Repo: "zeta/tools", Reason: missing}})
	view = repos.view(nil)
	if len(view.Mapped) != 1 || view.Mapped[0].Repo != "Acme/Widgets" || !reflect.DeepEqual(view.Refused, []api.RefusedRepositoryView{{Repo: "zeta/tools", Reason: missing}}) {
		t.Fatalf("view = %+v, want acme/widgets served and zeta/tools, which a live record names, left out for its review App alone", view)
	}
}

// ResolveRepository reads the mapping first and the recorded set second, as its own doc says: a
// repository the mapping names is answered from the read the mapping's own preparation made
// (defaultBranches.Kept), which a later read replaces as its TTL runs out, and never from the
// recorded set, whose read was made once at a preparation and stands for the life of the
// process. Over a configured rename it matters twice over: the recorded set is read under
// either name of a pair (ghrepo.Under), so a read kept under the name a repository moved away
// from would answer the mapped name with the name GitHub no longer holds the repository under.
// A repository no mapping names is answered from the recorded set exactly as before.
func TestResolveRepositoryAnswersAMappedRepositoryFromTheMappingsOwnRead(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1"})
	renames := ghrepo.MustRenames(ghrepo.MustRename("acme/widgets-legacy", "acme/widgets"))
	w := &workflowRuntime{repositories: newRepositories("LEGION", renames),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true}}, nil)
	if _, err := w.defaultBranches.Named(ctx, ghrepo.MustParse("Acme/Widgets")); err != nil {
		t.Fatalf("read the mapped repository's default branch: %v", err)
	}
	// What a preparation made while no mapping named the pair's old name kept: the name GitHub
	// no longer holds the repository under, at the branch it had then.
	w.repositories.recordRepository("acme/widgets-legacy", ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets-legacy"), DefaultBranch: "trunk"})
	want := workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}
	for _, resolved := range []string{"acme/widgets", "Acme/Widgets"} {
		got, err := w.ResolveRepository(resolved)
		if err != nil || got != want {
			t.Fatalf("ResolveRepository(%q) = %+v, %v; want the mapping's own read %+v", resolved, got, err, want)
		}
	}

	w.repositories.recordRepository("zeta/tools", ghbranch.Repository{Name: ghrepo.MustParse("Zeta/Tools"), DefaultBranch: "main"})
	unmapped := workflow.Resolution{Repo: "Zeta/Tools", DefaultBranch: "main", GitHubRepo: "Zeta/Tools"}
	if got, err := w.ResolveRepository("zeta/tools"); err != nil || got != unmapped {
		t.Fatalf("ResolveRepository of a repository no mapping names = %+v, %v; want its recorded read %+v", got, err, unmapped)
	}
}

// One lookup says which set answered for a repository, and every reader that treats a mapped
// repository and one only a preparation read apart takes it from that lookup: what an issue on
// it is recorded with (ResolveRepository), whether release_children waits for it
// (repositoryUnread), and whether the preparation of a todo issue on it reads GitHub itself
// (prepareUnrecorded). Each asked the mapping a second question of its own before, which the
// pair's old name answered otherwise than the lookup had: a read kept under it from before the
// mapping named the pair answered the lookup, while the mapping answered the second question
// from the pair's other name, so the issue was prepared and resolved under a name GitHub no
// longer holds the repository under. A repository neither set names is the one lookup answers
// unread that release_children does not wait for, since nothing is reading GitHub for it.
func TestEveryReaderOfARepositoryTakesTheSetThatAnsweredFromOneLookup(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "other/thing": "Other/Thing@main"})
	installs := &installations{}
	// The preparation of a todo issue on a repository neither set names asks Dispatch of its GitHub
	// App once GitHub has named it (prepareUnmapped), so the runtime has a Dispatch to ask.
	w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.MustRenames(ghrepo.MustRename("acme/widgets-legacy", "acme/widgets"))),
		dispatch: &mappingDispatch{}, installs: installs, log: quietLogger(), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	unanswered := "the implement App's installation on acme/gadgets could not be read: GitHub answered 502; Legion reads it again"
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("Acme/Widgets"), Default: true}},
		[]refusedRepository{{Repo: "acme/gadgets", Reason: unanswered, readErr: errors.New("GitHub answered 502")}})
	if _, err := w.defaultBranches.Named(ctx, ghrepo.MustParse("Acme/Widgets")); err != nil {
		t.Fatalf("read the mapped repository's default branch: %v", err)
	}
	w.repositories.recordRepository("acme/widgets-legacy", ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets-legacy"), DefaultBranch: "trunk"})
	w.repositories.recordRepository("zeta/tools", ghbranch.Repository{Name: ghrepo.MustParse("Zeta/Tools"), DefaultBranch: "main"})
	mapped := workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}
	for _, tc := range []struct {
		name, resolved string
		source         repositorySource
		resolution     workflow.Resolution
		// unread is release_children's answer, and reads how many installation reads the
		// preparation of a todo issue on the repository makes of its own.
		unread bool
		reads  int
	}{
		{name: "a served repository", resolved: "acme/widgets", source: fromMapping, resolution: mapped},
		{name: "the pair's old name, which a preparation once kept a read under", resolved: "acme/widgets-legacy", source: fromMapping, resolution: mapped},
		{name: "a mapped repository GitHub did not answer for", resolved: "acme/gadgets", source: fromMapping,
			resolution: workflow.Resolution{Refusal: unanswered, LeftOut: true, Unread: true}, unread: true},
		{name: "a repository only a preparation read", resolved: "zeta/tools", source: fromRecorded,
			resolution: workflow.Resolution{Repo: "Zeta/Tools", DefaultBranch: "main", GitHubRepo: "Zeta/Tools"}},
		{name: "a repository neither set names", resolved: "other/thing", source: fromNeither,
			resolution: workflow.Resolution{Refusal: "GitHub has not been read for other/thing yet", Unread: true}, reads: len(appauth.Roles)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.repositories.lookup(tc.resolved).source; got != tc.source {
				t.Fatalf("lookup(%s) answered from set %d, want %d", tc.resolved, got, tc.source)
			}
			if got, err := w.ResolveRepository(tc.resolved); err != nil || got != tc.resolution {
				t.Fatalf("ResolveRepository(%s) = %+v, %v; want %+v", tc.resolved, got, err, tc.resolution)
			}
			if got := w.repositoryUnread(tc.resolved); got != tc.unread {
				t.Fatalf("repositoryUnread(%s) = %v, want %v", tc.resolved, got, tc.unread)
			}
			asked := len(installs.asks())
			fact := intake.DispatchIssue{Key: "LEGION-7", Seq: 1, Status: "todo", Title: "todo", Rank: "A", HandedOver: true, ResolvedRepo: tc.resolved}
			if _, err := w.prepareUnrecorded(ctx, fact, false, 0, false); err != nil {
				t.Fatalf("prepare a todo issue on %s: %v", tc.resolved, err)
			}
			if got := len(installs.asks()) - asked; got != tc.reads {
				t.Fatalf("the preparation of a todo issue on %s made %d installation reads of its own, want %d", tc.resolved, got, tc.reads)
			}
		})
	}
}

// A recovery's pass counts a repository as recovered, and tells its release waits, only where
// GitHub answered for it, has not failed for it again, and no issue fell silent for it while the
// pass ran. It lets a repository GitHub does not answer for go only where no preparation will
// read GitHub for it again: no unrecorded todo issue is left on it, or the mapping answers for it
// by now, and no issue fell silent for it since the pass began, with why it did. What a release
// left to that repository's recovery leaves with its marker, for the architects to be told once
// (tellLetGoWaits), and stays exactly while the marker does. Every listed repository counted as
// recovered before, and nothing but an answered read ever let one go.
//
// Nothing is settled for a repository a preparation marked while the pass ran, a marker held
// already or not. An issue a person moves to todo after the pass read the listing is prepared,
// finds GitHub failing and the marker held, and notes its silence only after the pass settles,
// its apply waiting on the fact lock the pass's reconcile holds: let go, the repository was listed
// by no later pass and that issue was never recorded.
func TestARecoverySettlesOnlyTheRepositoriesGitHubAnsweredFor(t *testing.T) {
	const repo = "stranger/tools"
	waits := map[string]map[string]releaseWait{repo: {"LEGION-2": {tree: "LEGION-1", parent: "LEGION-1"}}}
	for _, tc := range []struct {
		name string
		// unread and recovering are how the repository stands once the pass's listing has
		// applied; silent is whether an issue fell silent for it since the pass began, todo
		// whether the listing holds an unrecorded todo issue on it, and mapped whether the
		// mapping names it by now. failed and answeredNow are what a preparation that ran
		// while the pass did found: GitHub failing (leaveUnread), or GitHub answering
		// (readAnswered), after the pass took its set.
		unread, recovering, silent, todo, mapped, failed, answeredNow bool
		// answered is whether the pass recovered it, which is when its release waits are told,
		// kept whether it is still left unread after the pass, and gone whether the pass took
		// the release wait on it out with its marker, to tell, for why.
		answered, kept, gone bool
		why                  letGoWhy
	}{
		{name: "GitHub answered and the pass left nothing silent", recovering: true, answered: true},
		{name: "GitHub answered after an issue of the pass fell silent", recovering: true, silent: true},
		{name: "GitHub answered once and fails again", recovering: true, unread: true, todo: true, kept: true},
		{name: "GitHub does not answer for its todo issue", unread: true, todo: true, kept: true},
		{name: "GitHub does not answer and no todo issue is left on it", unread: true, gone: true, why: letGoNoTodo},
		{name: "GitHub does not answer and an issue fell silent after the listing", unread: true, silent: true, kept: true},
		{name: "GitHub does not answer and the mapping names it by now", unread: true, todo: true, mapped: true, gone: true, why: letGoAnsweredElsewhere},
		{name: "GitHub does not answer and the mapping names it with no todo issue left", unread: true, mapped: true, gone: true, why: letGoNoTodo},
		{name: "an issue moved to todo after the listing finds GitHub failing", unread: true, failed: true, kept: true},
		{name: "GitHub answers an issue of its own while the pass runs", unread: true, answeredNow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
			if tc.mapped {
				w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("Stranger/Tools")}}, nil)
			}
			if tc.unread {
				w.unread = map[string]string{repo: "GitHub answered 502"}
			}
			if tc.recovering {
				w.recovering = map[string]bool{repo: true}
			}
			if tc.silent {
				w.noteUnread("LEGION-3", repo)
			}
			w.releaseWaits = map[string]map[string]releaseWait{repo: maps.Clone(waits[repo])}
			// The pass takes its set, the repository's mark among it, before anything below.
			marks := map[string]uint64{repo: w.unreadMarks[repo]}
			if tc.failed {
				w.leaveUnread(ghrepo.MustParse(repo), "the implement App's installation on stranger/tools could not be read: GitHub answered 502")
			}
			if tc.answeredNow {
				w.readAnswered(ghrepo.MustParse(repo))
			}
			answered, told, dropped := w.settleRecovery([]string{repo}, waits, map[string]bool{repo: tc.todo}, marks)
			if got := slices.Contains(answered, repo); got != tc.answered || (len(told) != 0) != tc.answered {
				t.Fatalf("the pass recovered %v and tells %v, want %s recovered: %v, with its release wait told exactly then", answered, told, repo, tc.answered)
			}
			if _, kept := w.unread[repo]; kept != tc.kept {
				t.Fatalf("%s is left unread after the pass: %v, want %v", repo, kept, tc.kept)
			}
			taken := len(waitingLetGo(dropped)) == 1 && len(w.releaseWaits[repo]) == 0
			if taken != tc.gone {
				t.Fatalf("the pass took %v out to tell, leaving %d waits on %s; want its release wait taken out with its marker: %v",
					dropped, len(w.releaseWaits[repo]), repo, tc.gone)
			}
			if tc.gone && dropped[0].why != tc.why {
				t.Fatalf("%s was let go for %d, want %d: its architects are told why", repo, dropped[0].why, tc.why)
			}
		})
	}
}

// release_children leaves a child to a repository's recovery only while the daemon holds the
// repository unread, and is told so: a wait on a repository its recovery has stopped waiting on
// (let go, or answered) is refused and holds nothing, so the route falls back to the
// child-release retry. Before, the wait was held all the same, on a repository no pass lists
// again, where no recovery read it and no architect was told of it.
func TestAReleaseWaitIsHeldOnlyWhileItsRepositoryIsUnread(t *testing.T) {
	const repo = "stranger/tools"
	for _, tc := range []struct {
		name string
		// unread is whether the marker is held; mapped is the mapping's answer, "" for none.
		unread bool
		mapped string
		held   bool
	}{
		{name: "a repository no mapping names that GitHub did not answer for", unread: true, held: true},
		{name: "a repository no mapping names that a pass has let go"},
		{name: "a mapped repository left out for a read GitHub did not answer", mapped: "unread", held: true},
		{name: "a mapped repository served", mapped: "served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
			switch tc.mapped {
			case "unread":
				w.repositories.set(nil, []refusedRepository{{Repo: repo, Reason: "GitHub answered 502; Legion reads it again", readErr: errors.New("GitHub answered 502")}})
			case "served":
				w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("Stranger/Tools")}}, nil)
			}
			if tc.unread {
				w.unread = map[string]string{repo: "GitHub answered 502"}
			}
			if got := w.awaitRecovery("Stranger/Tools", "LEGION-1", "LEGION-2", "LEGION-1"); got != tc.held {
				t.Fatalf("awaitRecovery = %v, want %v", got, tc.held)
			}
			if got := len(w.releaseWaits[repo]) == 1; got != tc.held {
				t.Fatalf("%d waits held on %s, want one exactly when the wait is accepted", len(w.releaseWaits[repo]), repo)
			}
		})
	}
}

// A release wait for a mapped repository that is in flight while a read of the mapping publishes
// the repository served, and its recovery's pass takes its set, is refused and holds nothing,
// while a wait filed before the publication is in that pass's set and told by it. So every wait the
// daemon holds is one a pass reads, and every one it refuses goes to the route's child-release
// retry (TestWaveReleaseRetriesAChildWhoseRepositoryTheDaemonStoppedHoldingUnread, in the api
// package, shows that retry records the child). Before, the call read the mapping before taking
// the lock: descheduled between the two, it read the repository unread, then filed the wait after
// the pass had taken its set, or committed, and no pass ever read it or told its architect.
//
// The call is held at recoveringMu by holding it here, as the pass's own lock is held while it
// takes its set. The pause only lets the call reach that lock: with the mapping read under it,
// there is nothing the call does before it, so the answer does not depend on the pause.
func TestAReleaseWaitInFlightAcrossAPublicationIsRefusedRatherThanStranded(t *testing.T) {
	const repo, tree, filed, inFlight = "zeta/tools", "LEGION-1", "LEGION-2", "LEGION-3"
	w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
	w.repositories.set(nil, []refusedRepository{{Repo: repo, Reason: "the implement App's installation on zeta/tools could not be read: GitHub answered 502; Legion reads it again", readErr: errors.New("GitHub answered 502")}})
	if !w.awaitRecovery(repo, tree, filed, tree) {
		t.Fatalf("the wait for %s was refused while zeta/tools is left out for a read GitHub did not answer", filed)
	}

	w.recoveringMu.Lock()
	answered := make(chan bool, 1)
	go func() { answered <- w.awaitRecovery(repo, tree, inFlight, tree) }()
	time.Sleep(100 * time.Millisecond)
	// While the call waits: a read of the mapping publishes zeta/tools served and marks it to
	// recover (markRecovered, which takes this lock, so it is written here), and the recovery's
	// pass takes its set and its waits under the same lock (recoverRepositories).
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse(repo)}}, nil)
	w.recovering = map[string]bool{repo: true}
	taken := maps.Clone(w.releaseWaits[repo])
	w.recoveringMu.Unlock()
	held := <-answered

	if _, told := taken[filed]; !told {
		t.Fatalf("the pass took the waits %v, want %s, filed before the publication, among them to tell", taken, filed)
	}
	w.recoveringMu.Lock()
	_, stranded := w.releaseWaits[repo][inFlight]
	w.recoveringMu.Unlock()
	if held || stranded {
		t.Fatalf("the wait for %s in flight across the publication was held (%v, holding it: %v) though the pass had taken its set %v: no pass will read it, so it must be refused, for the route to retry the child",
			inFlight, held, stranded, taken)
	}
}

// An issue the workflow does not record, such as an operator's claim, works the project's default
// repository, as Dispatch resolves an issue that names none, at its default branch as GitHub names
// it: a pod provisions its workspace from it, and the claim's commit identity is minted for its
// owner. A project serving no default repository refuses the launch, naming the fix.
func TestAnUnrecordedIssueWorksTheProjectsDefaultRepository(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main"})
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatchProject: "LEGION", repositories: newRepositories("LEGION", ghrepo.Renames{}),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}, {Repo: ghrepo.MustParse("zeta/tools")}}, nil)
	if repo, branch, err := w.issueRepository(ctx, "LEGION-9"); err != nil || repo.String() != "Acme/Widgets" || branch != "release/v7.1" {
		t.Fatalf("an unrecorded issue works %s@%s, %v; want the default Acme/Widgets@release/v7.1", repo, branch, err)
	}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("zeta/tools")}}, []refusedRepository{{Repo: "acme/widgets", Reason: "the review App is not installed on acme/widgets"}})
	want := "LEGION-9 is not recorded, so it works the project's default repository, and project LEGION serves none: set the project's default repository in Dispatch (Settings → Repositories → Projects) to one both GitHub Apps are installed on"
	if repo, _, err := w.issueRepository(ctx, "LEGION-9"); err == nil || err.Error() != want {
		t.Fatalf("an unrecorded issue with no default served works %s, %v; want %q", repo, err, want)
	}
}

// A tmux pane adopted after a restart holds its write token for the repository its claim's issue
// works, the one its launch was minted for and whose owner names the directory it writes
// through: its record's, or, for an issue the workflow does not record, the project's default. A
// token no claim holds is refused.
func TestAnAdoptedPanesRepositoryIsTheOneItsIssueWorks(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatchProject: "LEGION", repositories: newRepositories("LEGION", ghrepo.Renames{}),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}}, nil)
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-2", Tree: "LEGION-2", Project: "LEGION", Title: "root", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "zeta/tools", DefaultBranch: "main", GitHubRepo: "zeta/tools"})
	}); err != nil {
		t.Fatal(err)
	}
	issues := map[claim.Token]string{}
	for _, issue := range []string{"LEGION-2", "LEGION-9"} {
		token, err := claim.NewToken("legion", issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		issues[token] = issue
	}
	issueOf := func(_ context.Context, token claim.Token) (string, error) {
		issue, ok := issues[token]
		if !ok {
			return "", errors.New("no claim holds token " + string(token))
		}
		return issue, nil
	}
	repository := claimRepository(issueOf, w.issueRepository)
	want := map[string]string{"LEGION-2": "zeta/tools", "LEGION-9": "Acme/Widgets"}
	for token, issue := range issues {
		if got, err := repository(ctx, token); err != nil || got.String() != want[issue] {
			t.Errorf("the repository of %s = %q, %v; want %q", token, got, err, want[issue])
		}
	}
	if got, err := repository(ctx, claim.ControllerToken("legion")); err == nil {
		t.Errorf("the repository of a token no claim holds = %q, want a refusal", got)
	}
}
