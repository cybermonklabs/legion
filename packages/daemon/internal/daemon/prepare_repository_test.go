package daemon

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// recordedIssueRuntime is a workflow runtime over one recorded issue, LEGION-301 on acme/widgets,
// whose repository state holds a read of acme/widgets and of zeta/tools at main, as earlier
// preparations would have kept them (repositories.fill), and whose GitHub is github and whose
// installations are installs. Its Dispatch counts the reads a preparation makes of the issue's
// detail, which for a recorded issue is none.
func recordedIssueRuntime(t *testing.T, github string, installs *installations) (*workflowRuntime, *detailDispatch) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-301", Tree: "LEGION-301", Project: "LEGION", Title: "existing", Phase: phase.Done,
		Generation: 1, Status: "done", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("zeta/tools"))
	client := &detailDispatch{resolved: map[string]string{"LEGION-301": "zeta/tools"}}
	return &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, installs: installs, log: quietLogger(),
		defaultBranches: newDefaultBranches(roleTokens{}, repos, github, defaultBranchTTL, time.Now)}, client
}

// refusingGitHub fails the test on every request: what a preparation that must read GitHub nothing
// is run against, since the read itself, and not what it answers, is what these cases are about.
func refusingGitHub(t *testing.T) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub was asked %s %s; an event prepared from the record alone reads GitHub nothing", r.Method, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}})
}

// A recorded issue's event is prepared from its record while Dispatch resolves the issue where the
// record already holds it, and reads nothing at all: an event naming that repository, in any
// spelling, is applied as it is, and a retained event, which names no repository
// (ResolvedRepoMissing), takes the record's. Neither GitHub nor Dispatch is read for any of them:
// a recorded issue's repository never comes from its issue detail, and a resolution that is the
// record's own repository is no change for a correction to read GitHub for.
func TestARecordedIssuesEventIsPreparedFromItsRecord(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	w, client := recordedIssueRuntime(t, refusingGitHub(t), installs)
	for _, tc := range []struct {
		name  string
		event intake.DispatchIssue
		want  intake.DispatchIssue
	}{
		{
			name:  "an event on the repository the record holds",
			event: intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets"},
			want:  intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets"},
		},
		{
			name:  "a retained event, which names no repository",
			event: intake.DispatchIssue{Key: "LEGION-301", Seq: 3, Status: "todo", HandedOver: true, ResolvedRepoMissing: true},
			want:  intake.DispatchIssue{Key: "LEGION-301", Seq: 3, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets"},
		},
		{
			name:  "an event on the record's repository in another spelling",
			event: intake.DispatchIssue{Key: "LEGION-301", Seq: 4, Status: "todo", HandedOver: true, ResolvedRepo: "Acme/Widgets"},
			want:  intake.DispatchIssue{Key: "LEGION-301", Seq: 4, Status: "todo", HandedOver: true, ResolvedRepo: "Acme/Widgets"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := w.prepareRepository(ctx, tc.event)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("prepareRepository of %#v = %#v, %v; want %#v applied", tc.event, got, err, tc.want)
			}
		})
	}
	if reads := client.readCount(); reads != 0 {
		t.Fatalf("Dispatch was read %d times for a recorded issue, want none", reads)
	}
	if asked := installs.asks(); len(asked) != 0 {
		t.Fatalf("GitHub's installations were read %v, want none: nothing changed", asked)
	}
}

// An event that resolves a recorded issue to another repository carries the read its correction
// is written from (intake.DispatchIssue.RepositoryRead): both role Apps' installations on it and
// GitHub's name and default branch of it, read at this preparation. The record follows the
// repository Dispatch resolves the issue to (workflow.Engine.ApplyRepositoryChange), and the
// engine resolves it inside the fact's own transaction, which performs no I/O, so the read is
// made here. The repository state kept a read of zeta/tools at main, and GitHub answers trunk
// now: the event carries trunk, never the kept read (dispatch://INFRA-3136's rule that a
// correction never writes from a kept read). Dispatch is still not read.
func TestARecordedIssuesEventOnAnotherRepositoryCarriesGitHubsReadOfIt(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	w, client := recordedIssueRuntime(t, newNamedGitHub(t, map[string]string{"zeta/tools": "Zeta/Tools@trunk"}).url, installs)
	if kept, held := w.repositories.lookup("zeta/tools"); !held || kept.DefaultBranch != "main" {
		t.Fatalf("the repository state keeps %+v for zeta/tools (%t), want its read at main; this test proves nothing", kept, held)
	}

	event := intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}
	got, err := w.prepareRepository(ctx, event)
	if err != nil {
		t.Fatalf("prepareRepository of an event resolving elsewhere = %v, want it prepared", err)
	}
	want := ghbranch.Repository{Name: ghrepo.MustParse("Zeta/Tools"), DefaultBranch: "trunk"}
	if got.RepositoryRead == nil || *got.RepositoryRead != want || got.RepositoryRefusal != "" {
		t.Fatalf("the prepared event carries the read %+v and the refusal %q, want GitHub's answer %+v and no refusal", got.RepositoryRead, got.RepositoryRefusal, want)
	}
	for _, role := range []string{"implement zeta/tools", "review zeta/tools"} {
		if reads := installs.reads(role); reads != 1 {
			t.Fatalf("the preparation made %d reads of %q, want one: a correction reads the installations at its preparation", reads, role)
		}
	}
	if reads := client.readCount(); reads != 0 {
		t.Fatalf("Dispatch was read %d times for a recorded issue, want none", reads)
	}
}

// A recorded issue's event onto a repository a role App is not installed on carries GitHub's
// refusal and its kind, which the change waits on the record with; one onto a repository GitHub
// cannot read carries neither, so its correction writes nothing and says nothing, and the issue's
// next event or the next boot reads it again. Neither fails the fact: the event is applied.
func TestARecordedIssuesEventCarriesWhatGitHubAnsweredForItsNewRepository(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		missing  map[string]bool
		refusal  string
		kind     intake.RepositoryRefusalKind
		branches map[string]string
	}{
		{name: "a role App not installed", missing: map[string]bool{"review zeta/tools": true},
			refusal: "the review App is not installed on zeta/tools", kind: intake.RefusedByGitHub,
			branches: map[string]string{"zeta/tools": "zeta/tools@trunk"}},
		{name: "a name GitHub answers 404 for", branches: map[string]string{"acme/widgets": "acme/widgets@main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := recordedIssueRuntime(t, newNamedGitHub(t, tc.branches).url, &installations{missing: tc.missing})
			event := intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}
			got, err := w.prepareRepository(ctx, event)
			if err != nil {
				t.Fatalf("prepareRepository = %v, want the event applied", err)
			}
			if got.RepositoryRead != nil || got.RepositoryRefusal != tc.refusal || got.RepositoryRefusalKind != tc.kind {
				t.Fatalf("the prepared event carries the read %+v and the refusal %q (%v), want no read and %q (%v)",
					got.RepositoryRead, got.RepositoryRefusal, got.RepositoryRefusalKind, tc.refusal, tc.kind)
			}
		})
	}
}
