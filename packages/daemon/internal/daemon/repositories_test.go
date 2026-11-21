package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// installations answers each App's installation on each repository, by "<role> <owner/name>";
// a repository it does not name is installed for both Apps, fail answers an error for every read
// and failing an error for the reads it names. delay is how long each read takes, as a GitHub
// that answers slowly; reads wait it out side by side. It answers a name for itself, as GitHub
// answers a name it redirects nowhere, so InstalledUnder answers as InstalledOn does and is asked
// as it is.
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

func (i *installations) InstalledUnder(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	return i.InstalledOn(ctx, role, repo)
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
// repeats: what a case that pins the reads a preparation makes compares, since the reads run at
// once and arrive in no order of their own.
func (i *installations) asks() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	asked := slices.Clone(i.asked)
	slices.Sort(asked)
	return asked
}

// repositoryDispatch is a Dispatch whose project listing is listed (counted in listings, its next
// listFailures reads failing), whose issue detail is issues' entry for a key it names, failing's
// error for a key it names, and the embedded issue for any other, each key it is asked recorded in
// detailAsked, and whose webhook App check answers not installed for each repository
// webhookMissing names, by its "<owner>/<name>" without case, and installed for every other (the
// embedded Dispatch's answer). webhookAsked is each repository it was asked the check for: the
// name each preparation read a repository under, in GitHub's spelling (prepareName), and each
// repository a read of the live records added (refreshLive).
type repositoryDispatch struct {
	outboxDispatch
	mu             sync.Mutex
	listed         []dispatch.IssueSummary
	listings       int
	listFailures   int
	issues         map[string]dispatch.Issue
	failing        map[string]error
	detailAsked    []string
	webhookMissing map[string]bool
	webhookAsked   []string
}

func (d *repositoryDispatch) WebhookAppInstalled(_ context.Context, repo string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.webhookAsked = append(d.webhookAsked, repo)
	return !d.webhookMissing[strings.ToLower(repo)], nil
}

// webhookChecks is every repository Dispatch was asked the webhook App check for, sorted.
func (d *repositoryDispatch) webhookChecks() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	asked := slices.Clone(d.webhookAsked)
	slices.Sort(asked)
	return asked
}

func (d *repositoryDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
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
func (d *repositoryDispatch) listingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listings
}

func (d *repositoryDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	d.detailAsked = append(d.detailAsked, key)
	issue, named := d.issues[key]
	failure := d.failing[key]
	d.mu.Unlock()
	if failure != nil {
		return dispatch.Issue{}, failure
	}
	if named {
		return issue, nil
	}
	return d.outboxDispatch.GetIssue(ctx, key)
}

// detailReads is how many times Dispatch was asked the detail of key.
func (d *repositoryDispatch) detailReads(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, asked := range d.detailAsked {
		if asked == key {
			count++
		}
	}
	return count
}

// The preparation of the repositories the records name bounds each installation read by
// installReadAttempt and nothing else: ten repositories, twenty slow reads, two rounds of
// repositoryReads that take longer as a whole than installReadAttempt, are each answered and every
// repository recorded on, however long the whole preparation takes, and a read longer than
// installReadAttempt is a read that did not answer, kept as the repository's read error for the
// refresher to read again.
func TestEachInstallationReadIsBoundedOnItsOwn(t *testing.T) {
	read := installReadAttempt
	t.Cleanup(func() { installReadAttempt = read })
	var repos []ghrepo.Repository
	named := map[string]string{}
	for i := range 10 {
		repo := fmt.Sprintf("acme/repo-%d", i)
		repos, named[repo] = append(repos, ghrepo.MustParse(repo)), repo+"@main"
	}
	github := newNamedGitHub(t, named)
	prepare := func(repos []ghrepo.Repository) *workflowRuntime {
		w := &workflowRuntime{dispatch: &repositoryDispatch{}, installs: &installations{delay: 150 * time.Millisecond},
			repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
			defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
		eachRepository(repos, func(repo ghrepo.Repository) {
			_, _ = w.prepareLive(context.Background(), repo, w.onceRecordedBranches(context.Background()))
		})
		return w
	}

	installReadAttempt = 400 * time.Millisecond
	w := prepare(repos)
	for _, repo := range repos {
		if _, recorded := w.repositories.lookup(repo.String()); !recorded {
			t.Fatalf("%s is not recorded on after 20 slow reads (entry %+v), want every one answered", repo, w.repositories.entryOf(repo.String()))
		}
	}

	installReadAttempt = 50 * time.Millisecond
	w = prepare(repos[:1])
	entry := w.repositories.entryOf(repos[0].String())
	if _, recorded := w.repositories.lookup(repos[0].String()); recorded || entry.answered() || !strings.Contains(entry.readError, context.DeadlineExceeded.Error()) {
		t.Fatalf("%s after a read longer than installReadAttempt = %+v, want it unanswered for now, its read error the read's deadline", repos[0], entry)
	}
}

// A read of a repository's default branch that GitHub never answers is bounded by
// installReadAttempt, as every other read of a preparation is (prepareName), so it holds up neither
// the refresher's first job nor any job after it. Here GitHub holds zeta/tools' read open until its
// reader goes: the first job keeps the read's deadline as zeta/tools' read error and records on
// acme/widgets, and the refresher's loop then runs its sixth job at the close of zeta/tools'
// tree, which takes the repository out of the cache set. Control: with prepareName's read made on
// the caller's context, the first job never returns and zeta/tools keeps no answer.
func TestADefaultBranchReadGitHubNeverAnswersHoldsUpNoJobOfTheRefresher(t *testing.T) {
	attempt := installReadAttempt
	installReadAttempt = 300 * time.Millisecond
	t.Cleanup(func() { installReadAttempt = attempt })
	silent := githubtest.Server(t, githubtest.Spec{Handle: func(rw http.ResponseWriter, r *http.Request) {
		repo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		if repo == "zeta/tools" {
			<-r.Context().Done()
			return
		}
		_, _ = rw.Write([]byte(`{"full_name":"` + repo + `","default_branch":"main"}`))
	}})
	w, _, _, _ := repositoryRigWith(t, &installations{}, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, silent, defaultBranchTTL, time.Now)
	})
	if entry := w.repositories.entryOf("zeta/tools"); !strings.Contains(entry.readError, "the name and default branch of zeta/tools could not be read") ||
		!strings.Contains(entry.readError, context.DeadlineExceeded.Error()) {
		t.Fatalf("zeta/tools' entry = %+v, want the bounded read's deadline as its read error", entry)
	}
	if !recordsOn(w, "acme/widgets") {
		t.Fatalf("acme/widgets is not recorded on (entry %+v), want it prepared beside zeta/tools' unanswered read", w.repositories.entryOf("acme/widgets"))
	}
	ctx := context.Background()
	closed := anchorOn(anchorTools, "zeta/tools")
	closed.Phase, closed.Status, closed.DispatchStatus = phase.Done, "done", "done"
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, w.pool, func(tx pgx.Tx) error { return w.records.PutIssue(scoped, tx, closed) }); err != nil {
		t.Fatal(err)
	}
	committed()
	testwait.Eventually(t, "the refresher's sixth job taking zeta/tools out of the cache set at its tree's close", func() bool {
		return slices.Equal(repositoryNames(w.cacheRepositories()), []string{"acme/widgets"})
	})
}

// An admission's read of the name and default branch of a repository no entry records issues on
// is bounded by installReadAttempt, as the refresher's and a launch's are: all three run the one
// preparation step (prepareName). Here GitHub holds zeta/tools' read open until its reader goes,
// and the preparation of a todo issue on it answers within the bound, leaving the repository unread
// with the read's deadline as its reason, kept once as its entry's read error, for the recovery to
// read again. Before the one step, an admission's read waited for as long as its caller's context
// allowed. Control: with prepareName's read made on the caller's context, the preparation waits
// until the test's own deadline and names that.
func TestAnAdmissionsReadOfANameGitHubNeverAnswersIsBounded(t *testing.T) {
	attempt := installReadAttempt
	installReadAttempt = 300 * time.Millisecond
	t.Cleanup(func() { installReadAttempt = attempt })
	silent := githubtest.Server(t, githubtest.Spec{Handle: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }})
	w := &workflowRuntime{dispatch: &repositoryDispatch{}, installs: &installations{}, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, silent, defaultBranchTTL, time.Now)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := w.prepareUnrecorded(ctx, intake.DispatchIssue{Key: "LEGION-2", Seq: 1, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}, false); err != nil {
		t.Fatalf("prepare a todo issue on zeta/tools = %v, want it prepared to be applied and acknowledged", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the preparation took %s, want it bounded by installReadAttempt", took)
	}
	if waiting := strings.Join(w.unreadReasons(), "\n"); !strings.Contains(waiting, "the name and default branch of zeta/tools could not be read") ||
		!strings.Contains(waiting, context.DeadlineExceeded.Error()) {
		t.Fatalf("the daemon waits on %q, want zeta/tools unread for the bounded read's deadline", waiting)
	}
	if entry := w.repositories.entryOf("zeta/tools"); !entry.answered() || !strings.Contains(entry.readError, "the name and default branch of zeta/tools could not be read") {
		t.Fatalf("zeta/tools' entry = %+v, want both Apps answered and the bounded read's failure as its read error", entry)
	}
}

// The refresher's first job, the boot's preparation of the repositories the records name, records
// on each repository whose installation reads GitHub answered and keeps, for one whose read failed,
// transient or not, the App, the repository and the failure as its read error, which legion state
// shows once a live record names it: no boot waits on it or is refused for it. It says so at WARN,
// never at ERROR, and the line is true of a repository only finished records name, as both are
// here: it says no live record names it and names no retry, since neither legion state nor the
// refresher's second job reads such a repository (view, badLive). Control: with the line logged
// as a live repository's whatever the live set holds, naming a retry, it fails.
func TestTheBootsPreparationRecordsOnTheRepositoriesWhoseInstallationReadsAnswer(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "acme/gadgets": "acme/gadgets@main"})
	pool := isolatedOutboxPool(t)
	records := projectRecords{Store: record.NewStore(), project: "LEGION"}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, issue := range []record.Issue{doneOn("LEGION-1", "LEGION", "acme/widgets", "main"), doneOn("LEGION-2", "LEGION", "acme/gadgets", "main")} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{&appauth.TransientError{Err: errors.New("GitHub answered 502")}, errors.New("read whether the implement App is installed on acme/gadgets failed (403)")} {
		installs := &installations{}
		installs.setFailing(failure, "implement acme/gadgets")
		var logged bytes.Buffer
		w := &workflowRuntime{pool: pool, records: records, dispatch: &repositoryDispatch{},
			dispatchProject: "LEGION", installs: installs, repositories: newRepositories("LEGION", ghrepo.Renames{}), log: slog.New(slog.NewTextHandler(&logged, nil)),
			defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
		w.prepareAtStart(ctx)
		if _, recorded := w.repositories.lookup("acme/widgets"); !recorded {
			t.Fatalf("acme/widgets is not recorded on, want it answered beside acme/gadgets' failed read (%v)", failure)
		}
		if _, recorded := w.repositories.lookup("acme/gadgets"); recorded {
			t.Fatalf("acme/gadgets is recorded on while its implement App's read fails (%v)", failure)
		}
		want := "the implement App's installation on acme/gadgets could not be read: " + failure.Error()
		view := w.repositories.view(liveOf(t, "acme/gadgets"), nil)
		if len(view.Live) != 1 || view.Live[0].ReadError != want || view.Live[0].Apps != nil {
			t.Fatalf("state's entry for acme/gadgets = %#v, want no Apps and the read error %q", view.Live, want)
		}
		if !strings.Contains(logged.String(), "level=WARN") || strings.Contains(logged.String(), "level=ERROR") {
			t.Fatalf("log = %s, want one warning and no error while acme/widgets is worked", logged.String())
		}
		var warned []string
		for line := range strings.SplitSeq(logged.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "leaves it unworkable for now") {
				warned = append(warned, line)
			}
		}
		if len(warned) != 1 || !strings.Contains(warned[0], "repo=acme/gadgets") || !strings.Contains(warned[0], "while a live record names it") ||
			!strings.Contains(warned[0], "live=false") || strings.Contains(warned[0], "retryIn=") {
			t.Fatalf("WARN lines for acme/gadgets = %q, want one saying no live record names it, with no retry", warned)
		}
	}
}

// webhookDispatch is a Dispatch over HTTP, read through the real client, that answers its webhook
// App check for each repository installed names, by "owner/name", or with the status and body
// failing names for it. Without route it is a Dispatch older than the check: the route is not
// found. asked counts each repository's checks.
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
	if d.installed == nil {
		d.installed = map[string]bool{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, check := strings.CutPrefix(r.URL.Path, "/api/v1/github/app-installation/")
		switch {
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

// answer has the check for repo answered again, as installed names it, after fail.
func (d *webhookDispatch) answer(repo string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.failing, repo)
}

// checks is how many webhook App checks Dispatch was asked for repo.
func (d *webhookDispatch) checks(repo string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked[repo]
}

// liveView is the state's repositories block over the repositories w's live records name, as
// legion state reads it (repositories.view).
func liveView(w *workflowRuntime) api.RepositoriesView {
	return w.repositories.view(w.repositories.liveNames(), nil)
}

// liveOf is the live map a read of records naming repos, one record each, hands view
// (repositoriesOf): each repository once, by its lowercased <owner>/<name>, in the first spelling.
func liveOf(t *testing.T, repos ...string) map[string]ghrepo.Repository {
	t.Helper()
	issues := make([]record.Issue, 0, len(repos))
	for index, repo := range repos {
		issues = append(issues, record.Issue{Key: fmt.Sprintf("LEGION-%d", index+1), GitHubRepo: repo})
	}
	live, err := repositoriesOf(issues)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

// liveEntry is the state's entry for repo, compared without case, and whether the state lists it.
func liveEntry(w *workflowRuntime, repo string) (api.LiveRepositoryView, bool) {
	for _, entry := range liveView(w).Live {
		if strings.EqualFold(entry.Repo, repo) {
			return entry, true
		}
	}
	return api.LiveRepositoryView{}, false
}

// webhookGap is the reason the state's withoutWebhookApp gives for repo, compared without case,
// and "" where the list does not name it: what the state says of Dispatch's GitHub App on repo.
func webhookGap(w *workflowRuntime, repo string) string {
	for _, gap := range liveView(w).WithoutWebhookApp {
		if strings.EqualFold(gap.Repo, repo) {
			return gap.Reason
		}
	}
	return ""
}

// notInstalledWebhookApp is the state's webhook App gap for repo while Dispatch's GitHub App is not
// installed on it (checkWebhookApp).
func notInstalledWebhookApp(repo string) string {
	return "Dispatch's GitHub App is not installed on " + repo + ": GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed"
}

// A repository a record names that Dispatch's GitHub App is not installed on is one GitHub sends
// Legion no event of: the daemon works it all the same, recording issues on it, and the state names
// it with why; a repository a role App is missing on is named with the App too, and Dispatch is
// asked nothing of it, since a preparation checks Dispatch's webhook App only once both role Apps
// answered installed (prepareName). Both are bad answers the refresher reads again
// (rereadBadAnswers), so once the App is installed the state names nothing more, with no restart.
// Control: with the webhook App checked whatever the role Apps answered, Dispatch is asked of
// zeta/tools.
func TestARepositoryWithoutTheWebhookAppIsWorkedAndNamed(t *testing.T) {
	quickInstallReads(t)
	standIn := newWebhookDispatch(t, true, map[string]bool{"acme/widgets": false, "zeta/tools": false})
	w, _, _, _ := repositoryRigWith(t, &installations{missing: map[string]bool{"review zeta/tools": true}}, standIn.serve)
	if !recordsOn(w, "acme/widgets") || recordsOn(w, "zeta/tools") {
		t.Fatalf("recorded on acme/widgets %v, zeta/tools %v; want acme/widgets alone", recordsOn(w, "acme/widgets"), recordsOn(w, "zeta/tools"))
	}
	if gap := webhookGap(w, "acme/widgets"); gap != notInstalledWebhookApp("acme/widgets") {
		t.Fatalf("state's webhook App gap for acme/widgets = %q, want it named without Dispatch's GitHub App", gap)
	}
	if entry, _ := liveEntry(w, "zeta/tools"); entry.NotInstalled() != "the review App is not installed on zeta/tools" {
		t.Fatalf("state's entry for zeta/tools = %+v, want the review App it lacks named", entry)
	}

	standIn.install("acme/widgets")
	testwait.Eventually(t, "acme/widgets named without Dispatch's GitHub App no longer", func() bool {
		return webhookGap(w, "acme/widgets") == ""
	})
	if asked := standIn.checks("zeta/tools"); asked != 0 {
		t.Fatalf("Dispatch was asked %d times of its webhook App on zeta/tools, want never: the review App is not installed there", asked)
	}
}

// A webhook App check Dispatch fails is no answer: the state names the repository with the
// failure, and the repository is worked throughout, issues recorded on it. A gap is a bad answer,
// so the refresher checks again while it stands (rereadBadAnswers, INFRA-4401 D6), and the state
// names nothing once Dispatch answers.
func TestAFailedWebhookAppCheckIsNamedAndCheckedAgainWhileTheRepositoryIsWorked(t *testing.T) {
	quickInstallReads(t)
	standIn := newWebhookDispatch(t, true, map[string]bool{"zeta/tools": true, "acme/widgets": true})
	message := "read the GitHub App's installation on acme/widgets: GET https://api.github.com/repos/acme/widgets/installation: status 500: Server Error"
	standIn.fail("acme/widgets", `{"code":"GITHUB_UNREADABLE","error":"`+message+`"}`)
	w, _, _, _ := repositoryRigWith(t, &installations{}, standIn.serve)
	if !recordsOn(w, "acme/widgets") || !recordsOn(w, "zeta/tools") {
		t.Fatalf("recorded on acme/widgets %v, zeta/tools %v; want both", recordsOn(w, "acme/widgets"), recordsOn(w, "zeta/tools"))
	}
	want := "the webhook App check failed: GITHUB_UNREADABLE: " + message
	if gap := webhookGap(w, "acme/widgets"); gap != want {
		t.Fatalf("state's webhook App gap for acme/widgets = %q, want %q", gap, want)
	}
	testwait.Eventually(t, "acme/widgets' webhook App checked again while the check fails", func() bool { return standIn.checks("acme/widgets") >= 3 })
	if !recordsOn(w, "acme/widgets") {
		t.Fatal("acme/widgets is no longer recorded on while its webhook App check fails, want it worked throughout")
	}
	standIn.answer("acme/widgets")
	testwait.Eventually(t, "acme/widgets named with no gap once Dispatch answers", func() bool {
		return webhookGap(w, "acme/widgets") == ""
	})
}

// A Dispatch older than the webhook App check answers its route 404: the state names every
// repository a live record names with that, and works each.
func TestAnOlderDispatchWithoutTheWebhookAppRouteIsNamed(t *testing.T) {
	standIn := newWebhookDispatch(t, false, nil)
	w, _, _, _ := repositoryRigWith(t, &installations{}, standIn.serve)
	if !recordsOn(w, "acme/widgets") || !recordsOn(w, "zeta/tools") {
		t.Fatalf("recorded on acme/widgets %v, zeta/tools %v; want both", recordsOn(w, "acme/widgets"), recordsOn(w, "zeta/tools"))
	}
	for _, repo := range []string{"acme/widgets", "zeta/tools"} {
		if gap := webhookGap(w, repo); gap != "this Dispatch does not answer the webhook App check" {
			t.Fatalf("state's webhook App gap for %s = %q, want it named for the check this Dispatch does not answer", repo, gap)
		}
	}
}

// withoutWebhookAppFor is the rig option under which Dispatch answers that its GitHub App is not
// installed on strangerRepo, a repository no record names before the test's own.
func withoutWebhookAppFor(w *workflowRuntime) {
	w.dispatch.(*repositoryDispatch).webhookMissing = map[string]bool{strangerRepo: true}
}

// A repository only the record of one issue names is worked as any other is, so the state names
// it without Dispatch's GitHub App, with why (INFRA-2543 R6): from the preparation that read GitHub
// for it, so from the record that first names it, and again at each read again of its bad answer,
// which takes an App installed since. The repository is worked throughout and the issue recorded
// on it: a missing Dispatch App refuses no work.
func TestARepositoryOnlyARecordNamesWithoutTheWebhookAppIsWorkedAndNamed(t *testing.T) {
	quickInstallReads(t)
	const key = "LEGION-951"
	w, client, _, _ := preparationRig(t, &installations{}, withoutWebhookAppFor)
	deliver(t, w, client, issue(key, strangerRepo, "todo", true, "", 1))
	if got := recordedIssue(t, w, key); got == nil || got.Repo != strangerNamed {
		t.Fatalf("%s = %+v, want it recorded on %s whatever Dispatch's GitHub App", key, got, strangerNamed)
	}
	entry, listed := liveEntry(w, strangerNamed)
	if gap := webhookGap(w, strangerNamed); !listed || gap != notInstalledWebhookApp(strangerNamed) || entry.Apps == nil || !entry.Apps.Implement || !entry.Apps.Review {
		t.Fatalf("state's entry for %s = %+v (listed %v), webhook App gap %q, want both Apps installed and Dispatch's GitHub App named missing", strangerNamed, entry, listed, gap)
	}

	client.mu.Lock()
	client.webhookMissing = nil
	client.mu.Unlock()
	testwait.Eventually(t, "the state naming "+strangerNamed+" with no gap once Dispatch's GitHub App is installed on it", func() bool {
		_, listed := liveEntry(w, strangerNamed)
		return listed && webhookGap(w, strangerNamed) == ""
	})
}

// A daemon that boots over a live tree on a repository no other record names names it without
// Dispatch's GitHub App from the refresher's first job, which checks Dispatch's webhook App on each
// repository a record names (prepareAtStart, INFRA-4401 D10): no preparation of an issue's event
// is needed for it, so the gap stands on what the restarted process read itself.
func TestARepositoryALiveRecordNamesIsNamedWithoutTheWebhookAppAfterARestart(t *testing.T) {
	ctx := context.Background()
	w, _, installs, _ := preparationRig(t, &installations{}, withoutWebhookAppFor, func(w *workflowRuntime) {
		// The tree an earlier process recorded, which this one boots over.
		if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
			return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-952", Tree: "LEGION-952", Project: "LEGION", Title: "a tree on a repository no other record names",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", Repo: strangerNamed, DefaultBranch: "trunk", GitHubRepo: strangerNamed})
		}); err != nil {
			t.Fatal(err)
		}
	})
	testwait.Eventually(t, "the state naming "+strangerNamed+" without Dispatch's GitHub App from the first job", func() bool {
		_, listed := liveEntry(w, strangerNamed)
		return listed && webhookGap(w, strangerNamed) == notInstalledWebhookApp(strangerNamed)
	})
	if reads := installs.reads("implement " + strangerNamed); reads != 1 {
		t.Fatalf("the restarted daemon made %d installation reads of %s, want the first job's one", reads, strangerNamed)
	}
}

// hangingWebhookDispatch is a Dispatch whose webhook App check answers only once its context
// ends, as one whose own read of GitHub never returns, keeping each repository it was asked of
// (repositoryDispatch.webhookChecks).
type hangingWebhookDispatch struct{ repositoryDispatch }

func (d *hangingWebhookDispatch) WebhookAppInstalled(ctx context.Context, repo string) (bool, error) {
	d.mu.Lock()
	d.webhookAsked = append(d.webhookAsked, repo)
	d.mu.Unlock()
	<-ctx.Done()
	return false, ctx.Err()
}

// The read again of the live records checks Dispatch's webhook App on each repository it added
// that no preparation checked (refreshLive, uncheckedLive), each check bounded by
// installReadAttempt, repositoryReads of them at once, and nothing bounds the whole. A Dispatch
// that never answers costs each repository installReadAttempt, so more repositories than one
// round of repositoryReads are each named with the check's deadline, and a repository a
// preparation already checked is asked nothing again.
func TestTheCheckOfTheRepositoriesTheLiveRecordsAddIsBoundedPerCheck(t *testing.T) {
	read := installReadAttempt
	t.Cleanup(func() { installReadAttempt = read })
	installReadAttempt = 50 * time.Millisecond
	ctx := context.Background()
	client := &hangingWebhookDispatch{}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "LEGION"}, dispatch: client,
		dispatchProject: "LEGION", repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
	w.repositories.keepWebhookAnswer(ghrepo.MustParse("acme/widgets"), "")
	recorded := []record.Issue{{Key: "LEGION-1", Repo: "acme/widgets"}}
	var want []string
	for i := range repositoryReads + 2 {
		repo := fmt.Sprintf("stranger/repo-%02d", i)
		recorded = append(recorded, record.Issue{Key: fmt.Sprintf("LEGION-%d", 10+i), Repo: repo})
		want = append(want, repo)
	}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for _, issue := range recorded {
			issue.Tree, issue.Project, issue.Title, issue.Phase, issue.Generation, issue.Status, issue.Rank = issue.Key, "LEGION", issue.Key, phase.Planning, 1, "in_progress", "A"
			issue.DefaultBranch, issue.GitHubRepo = "main", issue.Repo
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.refreshLive(ctx); err != nil {
		t.Fatalf("refreshLive: %v", err)
	}
	for _, repo := range want {
		if got := w.repositories.entryOf(repo).webhookApp; got != "the webhook App check failed: "+context.DeadlineExceeded.Error() {
			t.Fatalf("%s's webhook App gap = %q, want the check's deadline", repo, got)
		}
	}
	if asked := client.webhookChecks(); !slices.Equal(asked, want) {
		t.Fatalf("Dispatch was asked of %v, want %v: acme/widgets was checked by a preparation already", asked, want)
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

// The state's repositories block carries each repository a live record names with its cache,
// matched by repository whatever its case, with its last health read; a cache of a repository no
// live record names is listed apart with when the sweep retires it; a repository with no cache
// carries none.
func TestTheStatesViewCarriesTheRepositoryCaches(t *testing.T) {
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("Acme/Widgets"), ghrepo.MustParse("zeta/tools"))
	fetched := time.Date(2026, 10, 16, 19, 20, 30, 0, time.UTC)
	retained := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	view := repos.view(liveOf(t, "Acme/Widgets", "zeta/tools"), []sandbox.CacheStatus{
		{Repo: ghrepo.MustParse("acme/legacy"), Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing",
			Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained},
		{Repo: ghrepo.MustParse("acme/widgets"), Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
			Health: &repocache.Health{Ready: true, Repo: "acme/widgets", Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678}},
	})
	want := api.RepositoriesView{
		Live: []api.LiveRepositoryView{
			{Repo: "Acme/Widgets", Apps: &api.AppInstallationsView{Implement: true, Review: true}, Cache: &api.CacheView{
				Repo: "acme/widgets", Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true, Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
				Head: "34a7ac80", LastFetchAt: fetched, Bytes: 6612345678,
			}},
			{Repo: "zeta/tools", Apps: &api.AppInstallationsView{Implement: true, Review: true}},
		},
		Caches: []api.CacheView{{Repo: "acme/legacy", Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "failing", Reason: "container cache ImagePullBackOff: pull access denied (restarted 0 times)", RetainedUntil: retained}},
	}
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("view = %+v\nwant %+v", view, want)
	}
}

// The state's repositories are exactly the repositories the live records of its own read name
// (INFRA-4401 D9), once each whatever their case, each from what GitHub last answered for the name
// it is served under: both role Apps installed, one App missing, or nothing yet where GitHub has
// not answered. A read a preparation kept for a repository no live record names puts nothing in
// the state: the preparation of a person's own todo issue keeps one, and no tree works there.
func TestTheStatesRepositoriesAreTheOnesTheLiveRecordsName(t *testing.T) {
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("Acme/Widgets"))
	repos.keepRead(ghrepo.MustParse("zeta/tools"), appInstallations{implement: appInstallation{answered: true, installed: true}, review: appInstallation{answered: true}}, "")
	repos.recordRepository("stranger/tools", ghbranch.Repository{Name: ghrepo.MustParse("Stranger/Tools"), DefaultBranch: "trunk"})
	view := repos.view(liveOf(t, "Acme/Widgets", "acme/widgets", "zeta/tools", "orbit/gadgets"), nil)
	want := []api.LiveRepositoryView{
		{Repo: "Acme/Widgets", Apps: &api.AppInstallationsView{Implement: true, Review: true}},
		{Repo: "orbit/gadgets"},
		{Repo: "zeta/tools", Apps: &api.AppInstallationsView{Implement: true}},
	}
	if !reflect.DeepEqual(view.Live, want) {
		t.Fatalf("state's live repositories = %+v\nwant %+v", view.Live, want)
	}
	if view.Caches != nil {
		t.Fatalf("state's caches = %+v, want none", view.Caches)
	}
}

// The state's withoutWebhookApp is each repository the live records name whose served name's entry
// holds Dispatch's webhook App gap, under the name the records keep it under with that gap as its
// reason, in the order of the block's live entries: the one place the block reports Dispatch's
// webhook App (INFRA-4401 A6). A gap kept for a repository no live record names is in no list, and
// a state whose live repositories hold no gap lists none.
func TestTheStatesWebhookAppGapsAreTheLiveRepositoriesThatHoldOne(t *testing.T) {
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("Acme/Widgets"), ghrepo.MustParse("zeta/tools"), ghrepo.MustParse("orbit/gadgets"))
	failed := "the webhook App check failed: GITHUB_UNREADABLE: GitHub answered 500"
	repos.keepWebhookAnswer(ghrepo.MustParse("zeta/tools"), notInstalledWebhookApp("zeta/tools"))
	repos.keepWebhookAnswer(ghrepo.MustParse("acme/widgets"), failed)
	repos.keepWebhookAnswer(ghrepo.MustParse("orbit/gadgets"), "")
	repos.keepWebhookAnswer(ghrepo.MustParse("stranger/tools"), notInstalledWebhookApp("stranger/tools"))
	view := repos.view(liveOf(t, "zeta/tools", "Acme/Widgets", "orbit/gadgets"), nil)
	want := []api.WebhookAppGapView{
		{Repo: "Acme/Widgets", Reason: failed},
		{Repo: "zeta/tools", Reason: notInstalledWebhookApp("zeta/tools")},
	}
	if !reflect.DeepEqual(view.WithoutWebhookApp, want) {
		t.Fatalf("state's withoutWebhookApp = %+v\nwant %+v", view.WithoutWebhookApp, want)
	}
	if gaps := repos.view(liveOf(t, "orbit/gadgets"), nil).WithoutWebhookApp; len(gaps) != 0 {
		t.Fatalf("state's withoutWebhookApp over orbit/gadgets alone = %+v, want none", gaps)
	}
}

// One lookup of the entries answers for a repository, and every reader takes it from that lookup:
// what an issue on it is recorded with (ResolveRepository), whether release_children waits for it
// (repositoryUnread), and whether the preparation of a todo issue on it reads GitHub itself
// (prepareUnrecorded). A repository an entry records issues on is answered from that entry, under
// either name of a configured pair, and its preparation asks GitHub nothing of its installations;
// one no entry records issues on is answered unread and its preparation reads both role Apps, and
// release_children waits for it only while GitHub's reads for it went unanswered (the marker).
func TestEveryReaderOfARepositoryTakesItsAnswerFromOneLookup(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "acme/gadgets": "acme/gadgets@main", "other/thing": "Other/Thing@main"})
	installs := &installations{}
	w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.MustRenames(ghrepo.MustRename("acme/widgets-legacy", "acme/widgets"))),
		dispatch: &repositoryDispatch{}, installs: installs, log: quietLogger(), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	read := ghbranch.Repository{Name: ghrepo.MustParse("Acme/Widgets"), DefaultBranch: "release/v7.1"}
	w.repositories.fillRead(read)
	if _, err := w.defaultBranches.Named(ctx, ghrepo.MustParse("acme/widgets")); err != nil {
		t.Fatalf("read acme/widgets' default branch: %v", err)
	}
	w.unread = map[string]string{"acme/gadgets": "the implement App's installation on acme/gadgets could not be read: GitHub answered 502; Legion reads it again"}
	recorded := workflow.Resolution{Repo: "Acme/Widgets", DefaultBranch: "release/v7.1", GitHubRepo: "Acme/Widgets"}
	for _, tc := range []struct {
		name, resolved string
		recorded       bool
		resolution     workflow.Resolution
		// unread is release_children's answer, and reads how many installation reads the
		// preparation of a todo issue on the repository makes of its own.
		unread bool
		reads  int
	}{
		{name: "a repository an entry records issues on", resolved: "acme/widgets", recorded: true, resolution: recorded},
		{name: "the pair's other name, which no read of its own names", resolved: "acme/widgets-legacy", recorded: true, resolution: recorded},
		{name: "a repository GitHub did not answer for", resolved: "acme/gadgets",
			resolution: workflow.Resolution{Refusal: "GitHub has not been read for acme/gadgets yet", Unread: true}, unread: true, reads: len(appauth.Roles)},
		{name: "a repository no entry names", resolved: "other/thing",
			resolution: workflow.Resolution{Refusal: "GitHub has not been read for other/thing yet", Unread: true}, reads: len(appauth.Roles)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := w.repositories.lookup(tc.resolved); got != tc.recorded {
				t.Fatalf("lookup(%s) answered recorded %v, want %v", tc.resolved, got, tc.recorded)
			}
			if got, err := w.ResolveRepository(tc.resolved); err != nil || got != tc.resolution {
				t.Fatalf("ResolveRepository(%s) = %+v, %v; want %+v", tc.resolved, got, err, tc.resolution)
			}
			if got := w.repositoryUnread(tc.resolved); got != tc.unread {
				t.Fatalf("repositoryUnread(%s) = %v, want %v", tc.resolved, got, tc.unread)
			}
			asked := len(installs.asks())
			fact := intake.DispatchIssue{Key: "LEGION-7", Seq: 1, Status: "todo", Title: "todo", Rank: "A", HandedOver: true, ResolvedRepo: tc.resolved}
			if _, err := w.prepareUnrecorded(ctx, fact, false); err != nil {
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
// read GitHub for it again: no unrecorded todo issue is left on it, or an entry records issues on
// it by now, and no issue fell silent for it since the pass began, with why it did. What a release
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
		// whether the listing holds an unrecorded todo issue on it, and recorded whether an entry
		// records issues on it by now. failed and answeredNow are what a preparation that ran
		// while the pass did found: GitHub failing (leaveUnread), or GitHub answering
		// (readAnswered), after the pass took its set.
		unread, recovering, silent, todo, recorded, failed, answeredNow bool
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
		{name: "GitHub does not answer and an entry records issues on it by now", unread: true, todo: true, recorded: true, gone: true, why: letGoAnsweredElsewhere},
		{name: "GitHub does not answer and an entry records issues on it with no todo issue left", unread: true, recorded: true, gone: true, why: letGoNoTodo},
		{name: "an issue moved to todo after the listing finds GitHub failing", unread: true, failed: true, kept: true},
		{name: "GitHub answers an issue of its own while the pass runs", unread: true, answeredNow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
			if tc.recorded {
				w.repositories.fill(ghrepo.MustParse("Stranger/Tools"))
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
		// unread is whether the marker is held; entry is what the entries hold for the
		// repository: "" nothing, "recorded" a read issues are recorded on, "unanswered" a read
		// GitHub did not answer.
		unread bool
		entry  string
		held   bool
	}{
		{name: "a repository GitHub did not answer an admission's reads for", unread: true, held: true},
		{name: "a repository a pass has let go"},
		{name: "a repository whose refresher read did not answer, with no admission waiting on it", entry: "unanswered"},
		{name: "a repository an entry records issues on, its marker still held", unread: true, entry: "recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
			switch tc.entry {
			case "unanswered":
				w.repositories.keepRead(ghrepo.MustParse(repo), appInstallations{}, "GitHub answered 502")
			case "recorded":
				w.repositories.fill(ghrepo.MustParse("Stranger/Tools"))
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

// A release wait in flight while a preparation records the repository GitHub answered for, and
// its recovery's pass takes its set, is refused and holds nothing, while a wait filed before the
// answer is in that pass's set and told by it. So every wait the daemon holds is one a pass reads,
// and every one it refuses goes to the route's child-release retry
// (TestWaveReleaseRetriesAChildWhoseRepositoryTheDaemonStoppedHoldingUnread, in the api package,
// shows that retry records the child). Before, the call read the answer before taking the lock:
// descheduled between the two, it read the repository unread, then filed the wait after the pass
// had taken its set, or committed, and no pass ever read it or told its architect.
//
// The call is held at recoveringMu by holding it here, as the pass's own lock is held while it
// takes its set. The pause only lets the call reach that lock: with the entries read under it,
// there is nothing the call does before it, so the answer does not depend on the pause.
func TestAReleaseWaitInFlightAcrossAnAnswerIsRefusedRatherThanStranded(t *testing.T) {
	const repo, tree, filed, inFlight = "zeta/tools", "LEGION-1", "LEGION-2", "LEGION-3"
	w := &workflowRuntime{repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger()}
	w.unread = map[string]string{repo: "the implement App's installation on zeta/tools could not be read: GitHub answered 502; Legion reads it again"}
	if !w.awaitRecovery(repo, tree, filed, tree) {
		t.Fatalf("the wait for %s was refused while zeta/tools is unread", filed)
	}

	w.recoveringMu.Lock()
	answered := make(chan bool, 1)
	go func() { answered <- w.awaitRecovery(repo, tree, inFlight, tree) }()
	time.Sleep(100 * time.Millisecond)
	// While the call waits: a preparation records zeta/tools on GitHub's answer and moves it to
	// the repositories to recover (readAnswered, which takes this lock, so it is written here),
	// and the recovery's pass takes its set and its waits under the same lock
	// (recoverRepositories).
	w.repositories.fill(ghrepo.MustParse(repo))
	delete(w.unread, repo)
	w.recovering = map[string]bool{repo: true}
	taken := maps.Clone(w.releaseWaits[repo])
	w.recoveringMu.Unlock()
	held := <-answered

	if _, told := taken[filed]; !told {
		t.Fatalf("the pass took the waits %v, want %s, filed before the answer, among them to tell", taken, filed)
	}
	w.recoveringMu.Lock()
	_, stranded := w.releaseWaits[repo][inFlight]
	w.recoveringMu.Unlock()
	if held || stranded {
		t.Fatalf("the wait for %s in flight across the answer was held (%v, holding it: %v) though the pass had taken its set %v: no pass will read it, so it must be refused, for the route to retry the child",
			inFlight, held, stranded, taken)
	}
}

// An issue the workflow does not record, such as an operator's claim, works the repository Dispatch
// resolves it to (resolved_repo: its own, its nearest ancestor's, or the project's default), read
// from Dispatch at the launch and prepared as the refresher prepares a record's repository: both
// role Apps' installations, GitHub's name and default branch. It launches on X at X's default
// branch as GitHub names it, and the answer is kept for the issue while it holds its workspace, so
// a second launch asks Dispatch nothing; once the workspace is gone the next launch reads Dispatch
// again. An issue that resolves to no repository refuses that one launch with INFRA-4401 A3's
// text, and so do a Dispatch that does not answer, a role App that is not installed on the
// repository and a read that did not answer, each naming its cause; the next launch of another
// issue succeeds, so the daemon is up throughout.
//
// The control is issueRepository's read of the project's default back (dispatch://INFRA-4401
// check a-go, item 5): no issue here names a repository the daemon holds a default for, so each
// launch would refuse.
func TestAnUnrecordedIssuesLaunchWorksItsResolvedRepository(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1", "zeta/tools": "zeta/tools@main", "zeta/gadgets": "zeta/gadgets@main"})
	installs := &installations{missing: map[string]bool{"review zeta/tools": true}}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/gadgets")
	client := &repositoryDispatch{
		issues: map[string]dispatch.Issue{
			"LEGION-9":  {Key: "LEGION-9", Project: "LEGION", ResolvedRepo: "acme/widgets"},
			"LEGION-10": {Key: "LEGION-10", Project: "LEGION"},
			"LEGION-12": {Key: "LEGION-12", Project: "LEGION", ResolvedRepo: "zeta/tools"},
			"LEGION-13": {Key: "LEGION-13", Project: "LEGION", ResolvedRepo: "zeta/gadgets"},
			"LEGION-14": {Key: "LEGION-14", Project: "LEGION", ResolvedRepo: "acme/widgets"},
		},
		failing: map[string]error{"LEGION-11": errors.New("Dispatch answered 503")},
	}
	held := true
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: installs,
		repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(), setRepositories: hearsNothing,
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now),
		workspaceHeld:   func(string, ghrepo.Repository) bool { return held }}

	if repo, branch, err := launchedRepository(ctx, w, "LEGION-9"); err != nil || repo.String() != "Acme/Widgets" || branch != "release/v7.1" {
		t.Fatalf("LEGION-9's launch works %s@%s, %v; want its resolved Acme/Widgets@release/v7.1", repo, branch, err)
	}
	if repo, _, err := launchedRepository(ctx, w, "LEGION-9"); err != nil || repo.String() != "Acme/Widgets" || client.detailReads("LEGION-9") != 1 {
		t.Fatalf("LEGION-9's second launch works %s, %v, after %d reads of Dispatch; want Acme/Widgets kept for the issue, Dispatch read once", repo, err, client.detailReads("LEGION-9"))
	}
	held = false
	if _, _, err := launchedRepository(ctx, w, "LEGION-9"); err != nil || client.detailReads("LEGION-9") != 2 {
		t.Fatalf("LEGION-9's launch once its workspace is gone = %v after %d reads of Dispatch, want Dispatch read again", err, client.detailReads("LEGION-9"))
	}

	for _, tc := range []struct{ key, want string }{
		{"LEGION-10", "LEGION-10 names no repository: set the repository of LEGION-10, or the default repository of project LEGION, in Dispatch"},
		{"LEGION-11", "read the repository of LEGION-11 from Dispatch: Dispatch answered 503"},
		{"LEGION-12", "prepare zeta/tools, the repository Dispatch resolves LEGION-12 to: the review App is not installed on zeta/tools"},
		{"LEGION-13", "prepare zeta/gadgets, the repository Dispatch resolves LEGION-13 to: the implement App's installation on zeta/gadgets could not be read: GitHub answered 502"},
	} {
		if repo, _, err := launchedRepository(ctx, w, tc.key); err == nil || err.Error() != tc.want {
			t.Fatalf("%s's launch works %s, %v; want it refused with %q", tc.key, repo, err, tc.want)
		}
	}
	if repo, _, err := launchedRepository(ctx, w, "LEGION-14"); err != nil || repo.String() != "Acme/Widgets" {
		t.Fatalf("LEGION-14's launch after the refusals works %s, %v; want Acme/Widgets: one launch's refusal stops nothing else", repo, err)
	}
}

// A later launch of an issue the workflow does not record puts the repository kept for the issue
// back in the cache set once a read of the live records has replaced the set (INFRA-4401 D8:
// "unless a later launch names it again"): no record of the issue names the repository there, and
// a repository out of the set is stamped at the sweep and retired after retain_hours while the
// issue's pod still works in it. The launch answers from what is kept for the issue
// (keptRepository), reading neither Dispatch nor GitHub again. Control: with workLaunched deleted
// from unrecordedRepository's kept path, the cache set stays without Acme/Widgets after the second
// launch.
func TestALaterLaunchOfAKeptIssuePutsItsRepositoryBackInTheCacheSet(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	client := &repositoryDispatch{issues: map[string]dispatch.Issue{"LEGION-9": {Key: "LEGION-9", Project: "LEGION", ResolvedRepo: "acme/widgets"}}}
	installs := &installations{}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: installs,
		repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(), setRepositories: hearsNothing,
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now),
		workspaceHeld:   func(string, ghrepo.Repository) bool { return true }}
	cached := func() []string { return repositoryNames(w.cacheRepositories()) }

	if _, _, err := launchedRepository(ctx, w, "LEGION-9"); err != nil || !slices.Equal(cached(), []string{"Acme/Widgets"}) {
		t.Fatalf("LEGION-9's first launch = %v with the cache set %v, want Acme/Widgets in it", err, cached())
	}
	if err := w.refreshLive(ctx); err != nil {
		t.Fatalf("read the live records again: %v", err)
	}
	if got := cached(); len(got) != 0 {
		t.Fatalf("the cache set after the read of the live records = %v, want none: no live record names Acme/Widgets", got)
	}
	repo, branch, err := launchedRepository(ctx, w, "LEGION-9")
	if err != nil || repo.String() != "Acme/Widgets" || branch != "main" {
		t.Fatalf("LEGION-9's second launch works %s@%s, %v; want the kept Acme/Widgets@main", repo, branch, err)
	}
	if reads, asked := client.detailReads("LEGION-9"), installs.reads("implement acme/widgets"); reads != 1 || asked != 1 {
		t.Fatalf("Dispatch was read %d times and GitHub's installation %d times for LEGION-9, want once each: the second launch answers from what is kept", reads, asked)
	}
	if got := cached(); !slices.Equal(got, []string{"Acme/Widgets"}) {
		t.Fatalf("the cache set after LEGION-9's second launch = %v, want Acme/Widgets back in it", got)
	}
}

// A tmux pane adopted after a restart holds its write token for the repository its claim's issue
// works, the one its launch was minted for and whose owner names the directory it writes
// through: its record's, or, for an issue the workflow does not record, the one Dispatch resolves
// it to. A token no claim holds is refused.
func TestAnAdoptedPanesRepositoryIsTheOneItsIssueWorks(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	client := &repositoryDispatch{issues: map[string]dispatch.Issue{"LEGION-9": {Key: "LEGION-9", Project: "LEGION", ResolvedRepo: "acme/widgets"}}}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: &installations{},
		repositories: newRepositories("LEGION", ghrepo.Renames{}), log: quietLogger(), setRepositories: hearsNothing,
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
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
