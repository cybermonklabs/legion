package daemon

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// recordedIssueRuntime is a workflow runtime over one recorded issue, LEGION-301 on acme/widgets,
// whose project maps acme/widgets and zeta/tools and whose GitHub is github. No default branch is
// read for either repository, so what the runtime can resolve after a preparation
// (ResolveRepository) is what says whether that preparation read one. Its Dispatch counts the
// reads a preparation makes of the issue's detail, which for a recorded issue is none.
func recordedIssueRuntime(t *testing.T, github string) (*workflowRuntime, *detailDispatch) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-301", Tree: "LEGION-301", Project: "LEGION", Title: "existing", Phase: phase.Done,
		Generation: 1, Status: "done", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets")}, {Repo: ghrepo.MustParse("zeta/tools")}}, nil)
	client := &detailDispatch{resolved: map[string]string{"LEGION-301": "zeta/tools"}}
	return &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos,
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
// record already holds it, and reads nothing at all: an event naming that repository is applied as
// it is, and a retained event, which names no repository (ResolvedRepoMissing), takes the record's.
// A resolution Legion serves no installation of reads nothing either, whatever Dispatch has named:
// the workflow keeps the record's repository and says the change waits
// (workflow.Engine.ApplyRepositoryChange), which asks for no default branch. Dispatch is not read
// for any of the three: a recorded issue's repository never comes from its issue detail.
func TestARecordedIssuesEventIsPreparedFromItsRecord(t *testing.T) {
	ctx := context.Background()
	w, client := recordedIssueRuntime(t, refusingGitHub(t))
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
			name:  "an event on a repository Legion serves no installation of",
			event: intake.DispatchIssue{Key: "LEGION-301", Seq: 4, Status: "todo", HandedOver: true, ResolvedRepo: "other/thing"},
			want:  intake.DispatchIssue{Key: "LEGION-301", Seq: 4, Status: "todo", HandedOver: true, ResolvedRepo: "other/thing"},
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
}

// An event that resolves a recorded issue to another repository the project serves is applied as
// it is, and that repository's default branch is read here: the record follows the repository
// Dispatch resolves the issue to (workflow.Engine.ApplyRepositoryChange), and the engine resolves
// it inside the fact's own transaction, which performs no I/O, so a served repository whose
// default branch nobody read is an error there that would fail the fact for ever. What the
// preparation leaves behind is therefore a runtime that resolves the new repository, which is what
// the engine asks it for, and Dispatch is still not read.
func TestARecordedIssuesEventOnAnotherServedRepositoryReadsItsDefaultBranch(t *testing.T) {
	ctx := context.Background()
	w, client := recordedIssueRuntime(t, newNamedGitHub(t, map[string]string{"zeta/tools": "zeta/tools@trunk"}).url)
	if _, err := w.ResolveRepository("zeta/tools"); err == nil {
		t.Fatal("the runtime resolves zeta/tools before any preparation read its default branch; this test proves nothing")
	}

	event := intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}
	got, err := w.prepareRepository(ctx, event)
	if err != nil || !reflect.DeepEqual(got, event) {
		t.Fatalf("prepareRepository of an event resolving elsewhere = %#v, %v; want the event as it is, applied", got, err)
	}
	resolution, err := w.ResolveRepository("zeta/tools")
	if err != nil || resolution.Repo != "zeta/tools" || resolution.DefaultBranch != "trunk" {
		t.Fatalf("after the preparation the runtime resolves zeta/tools to %#v, %v; want zeta/tools at trunk, which is what the engine's own resolution answers from", resolution, err)
	}
	if reads := client.readCount(); reads != 0 {
		t.Fatalf("Dispatch was read %d times for a recorded issue, want none", reads)
	}
}

// A reopen whose edited repository the project serves and GitHub cannot read is refused with that
// read's own error, so the fact is delivered again rather than applied against a repository nobody
// could read. It is the answer prepareUnrecorded gives an admission on such a repository, and for
// the same reason: the engine writes no record on a repository whose default branch no read
// answered.
func TestARecordedIssuesEventIsRefusedWhenGitHubCannotReadItsNewRepository(t *testing.T) {
	ctx := context.Background()
	w, _ := recordedIssueRuntime(t, newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main"}).url)

	reopen := intake.DispatchIssue{Key: "LEGION-301", Seq: 2, Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools"}
	got, err := w.prepareRepository(ctx, reopen)
	if err == nil || !strings.Contains(err.Error(), "zeta/tools") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("prepareRepository of a reopen onto a served repository GitHub answers 404 for = %#v, %v; want the default branch read's own error, for the fact to be delivered again", got, err)
	}
}
