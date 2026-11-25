package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The backfill keeps a repository's spelling where a shared clone already spells it: an upgrade of
// a tmux daemon fills an issue it runs with the spelling of the shared clone whose owner and name
// match Dispatch's resolution case-insensitively, so the issue's workspace, which that clone's
// spelling named, is the one it keeps working in, its unpushed work there. With no clone, as under
// Kubernetes, whose pods provision their own, the record takes GitHub's spelling. The default
// branch is GitHub's either way.
func TestTheBackfillKeepsTheSpellingOfAnExistingClone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		clone bool
		want  string
	}{
		{"a tmux daemon with the issue's clone and workspace", true, "acme/widgets"},
		{"a daemon with no clone", false, "Acme/Widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-301', 'LEGION-301', 'LEGION', 'existing', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			before, err := workspace.Location(state, ghrepo.MustParse("acme/widgets"), "LEGION-301")
			if err != nil {
				t.Fatal(err)
			}
			if tc.clone {
				for _, dir := range []string{filepath.Join(before.Clone, ".git"), before.Dir} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(before.Dir, "retained-work"), []byte("unpushed work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1"})
			w := &workflowRuntime{pool: pool, records: records, defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
			client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-301", ResolvedRepo: "acme/widgets"}}}
			if _, err := backfillRepositories(ctx, pool, records, client, "LEGION", state, w.defaultBranches, ghrepo.Renames{}, productionRepositoryReads(), quietLogger()); err != nil {
				t.Fatal(err)
			}
			repo, branch, err := w.issueRepository(ctx, "LEGION-301")
			if err != nil || repo.String() != tc.want || branch != "release/v7.1" {
				t.Fatalf("LEGION-301 is recorded on %s@%s, %v; want %s@release/v7.1", repo, branch, err, tc.want)
			}
			if !tc.clone {
				return
			}
			after, err := workspace.Location(state, repo, "LEGION-301")
			if err != nil {
				t.Fatal(err)
			}
			if after.Dir != before.Dir {
				t.Fatalf("the workspace after the backfill is %s, want the one the clone's spelling named, %s", after.Dir, before.Dir)
			}
			if work, err := os.ReadFile(filepath.Join(after.Dir, "retained-work")); err != nil || string(work) != "unpushed work" {
				t.Fatalf("the retained work after the backfill = %q, %v; want it where the issue keeps working", work, err)
			}
		})
	}
}

// leftAs is the log line the backfill writes for key's record when it leaves it as it is
// (leaveUnfilled): the issue, the repository the backfill read for it, and the cause, which names
// the step that settles it, as the text handler quotes them.
func leftAs(key, repo, cause string) string {
	return "issue=" + key + " repo=" + repo + " cause=" + strconv.Quote(cause)
}

// backfillLogged runs the backfill of project LEGION with a logger it answers the lines of, and
// whether a record is left.
func backfillLogged(t *testing.T, pool *store.Pool, records record.Store, client dispatch.Client, state string, named *defaultBranches, renames ghrepo.Renames) (bool, string) {
	t.Helper()
	var logs bytes.Buffer
	left, err := backfillRepositories(context.Background(), pool, records, client, "LEGION", state, named, renames, productionRepositoryReads(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("backfill = %v, want no error: one record's repository never stops the boot", err)
	}
	return left, logs.String()
}

// Two shared clones that spell one repository in two ways leave the backfill no spelling to keep,
// so it leaves the record as it is and says why, naming both, rather than move an issue's
// workspace onto either by guess; the boot goes on (INFRA-4401 D11).
func TestTheBackfillLeavesARecordTwoClonesOfOneRepositorySpell(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-301', 'LEGION-301', 'LEGION', 'existing', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	var clones []string
	for _, spelling := range []string{"acme/widgets", "Acme/Widgets"} {
		located, err := workspace.Location(state, ghrepo.MustParse(spelling), "LEGION-301")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(located.Clone, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		clones = append(clones, located.Clone)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-301", ResolvedRepo: "acme/widgets"}}}
	left, logged := backfillLogged(t, pool, record.NewStore(), client, state,
		newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), ghrepo.Renames{})
	want := leftAs("LEGION-301", "acme/widgets", "two shared clones spell acme/widgets ("+clones[1]+", "+clones[0]+"): remove the one no issue works in")
	if !left || !strings.Contains(logged, want) {
		t.Fatalf("backfill left %t, logging:\n%s\nwant the record left, logged as %s", left, logged, want)
	}
	if got := recordedIssue(t, &workflowRuntime{pool: pool, records: record.NewStore()}, "LEGION-301"); got.Repo != "" {
		t.Fatalf("LEGION-301 = %s after the backfill left it, want it unfilled", got.Repo)
	}
}

// A record written before the record carried its repository may already have a pull request,
// which names the repository the issue worked in. The fill takes Dispatch's resolution only when
// that pull request is on it, in any case: one on another repository (the issue's own repository
// or an ancestor's, set in Dispatch since, outranking the project's default) leaves the record as
// it is and says why, naming the issue, both repositories and the fix, since a record on the wrong
// repository would create branches there and disarm a stranger's pull request; the boot goes on
// (INFRA-4401 D11).
func TestTheBackfillLeavesARecordWhosePullRequestIsOnAnotherRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/widgets"}}}
	for _, tc := range []struct{ name, prRepo, want string }{
		{"on another repository", "zeta/tools", "Dispatch resolves it to acme/widgets, but its pull request #7 is on zeta/tools: set the issue's repository in Dispatch to zeta/tools (dispatch issue-update --issue LEGION-1 --repo zeta/tools)"},
		{"on the resolved repository in another case", "ACME/Widgets", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
				values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, '', '')`); err != nil {
				t.Fatal(err)
			}
			records := record.NewStore()
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: "LEGION-1", Repo: tc.prRepo, Number: 7, Branch: "legion/LEGION-1",
					HeadSHA: "head", State: record.PullRequestOpen})
			}); err != nil {
				t.Fatal(err)
			}
			w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
			left, logged := backfillLogged(t, pool, records, client, t.TempDir(),
				newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), ghrepo.Renames{})
			got := recordedIssue(t, w, "LEGION-1")
			if tc.want != "" {
				if want := leftAs("LEGION-1", "acme/widgets", tc.want); !left || !strings.Contains(logged, want) {
					t.Fatalf("backfill left %t, logging:\n%s\nwant the record left, logged as %s", left, logged, want)
				}
				if got.Repo != "" {
					t.Fatalf("LEGION-1 = %s after the backfill left it, want it unfilled", got.Repo)
				}
				return
			}
			if left {
				t.Fatalf("backfill left a record, logging:\n%s\nwant LEGION-1 filled", logged)
			}
			if got.Repo != "acme/widgets" || got.GitHubRepo != "acme/widgets" {
				t.Fatalf("LEGION-1 = %s (GitHub's %s), want acme/widgets", got.Repo, got.GitHubRepo)
			}
		})
	}
}

// A record written before the record carried its repository may have no pull request yet but a
// workspace a tmux daemon provisioned, which names the repository the issue works in. The fill
// leaves a record whose resolution is elsewhere as it is and says why, naming the workspace and
// the fix, rather than provision the issue anew in another repository and strand its unpushed
// work; a workspace under the resolved repository, in any case, fills. The boot goes on
// (INFRA-4401 D11).
func TestTheBackfillLeavesARecordWhoseWorkspaceIsInAnotherRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/widgets"}}}
	for _, tc := range []struct{ name, workspaceIn, want string }{
		{"in another repository", "zeta/tools", "Dispatch resolves it to acme/widgets, but its workspace %s is in zeta/tools: set the issue's repository in Dispatch to zeta/tools (dispatch issue-update --issue LEGION-1 --repo zeta/tools)"},
		{"in the resolved repository in another case", "ACME/Widgets", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
				values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, '', '')`); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			location, err := workspace.Location(state, ghrepo.MustParse(tc.workspaceIn), "LEGION-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(location.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			records := record.NewStore()
			w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
			left, logged := backfillLogged(t, pool, records, client, state,
				newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), ghrepo.Renames{})
			got := recordedIssue(t, w, "LEGION-1")
			if tc.want != "" {
				if want := leftAs("LEGION-1", "acme/widgets", fmt.Sprintf(tc.want, location.Dir)); !left || !strings.Contains(logged, want) {
					t.Fatalf("backfill left %t, logging:\n%s\nwant the record left, logged as %s", left, logged, want)
				}
				if got.Repo != "" {
					t.Fatalf("LEGION-1 = %s after the backfill left it, want it unfilled", got.Repo)
				}
				return
			}
			if left || got.Repo != "acme/widgets" {
				t.Fatalf("backfill left %t, LEGION-1 = %s; want it filled with acme/widgets", left, got.Repo)
			}
		})
	}
}

// A repository GitHub renamed (acme/old, which GitHub answers with a 301 to acme/new) while
// Dispatch still resolves an issue to its old name: with no shared clone the record takes GitHub's
// new name; a shared clone under the old name, which the workspace would keep, leaves the record
// as it is, the log naming what fixes it; and a record already on the old name with no GitHub
// spelling, which no later boot can rename, is left as it is, the log naming why. Neither stops
// the boot (INFRA-4401 D11).
func TestTheBackfillOfARepositoryGitHubRenamedNamesTheFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/old": "301 acme/new", "acme/new": "acme/new@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/old"}}}
	seed := func(t *testing.T, repo, branch string) (*workflowRuntime, string) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
			values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, $1, $2)`, repo, branch); err != nil {
			t.Fatal(err)
		}
		return &workflowRuntime{pool: pool, records: record.NewStore(), log: quietLogger(),
			defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}, t.TempDir()
	}
	backfill := func(t *testing.T, w *workflowRuntime, state string) (bool, string) {
		t.Helper()
		return backfillLogged(t, w.pool, w.records, client, state, w.defaultBranches, ghrepo.Renames{})
	}

	t.Run("no shared clone", func(t *testing.T) {
		w, state := seed(t, "", "")
		if left, logged := backfill(t, w, state); left {
			t.Fatalf("backfill left a record, logging:\n%s\nwant LEGION-1 filled", logged)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "acme/new" || got.DefaultBranch != "main" || got.GitHubRepo != "acme/new" {
			t.Fatalf("LEGION-1 = %s@%s (GitHub's %s), want acme/new@main (GitHub's acme/new)", got.Repo, got.DefaultBranch, got.GitHubRepo)
		}
	})

	t.Run("a shared clone under the old name", func(t *testing.T) {
		w, state := seed(t, "", "")
		clone, err := workspace.Location(state, ghrepo.MustParse("acme/old"), "LEGION-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(clone.Clone, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		want := leftAs("LEGION-1", "acme/old", "GitHub renamed acme/old to acme/new, and the shared clone "+clone.Clone+
			" still has the old name: set the repository of LEGION-1 in Dispatch to acme/new, or remove that clone if no issue works in it")
		if left, logged := backfill(t, w, state); !left || !strings.Contains(logged, want) {
			t.Fatalf("backfill left %t, logging:\n%s\nwant the record left, logged as %s", left, logged, want)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "" || got.GitHubRepo != "" {
			t.Fatalf("LEGION-1 = %s (GitHub's %s) after the backfill left it, want it unfilled", got.Repo, got.GitHubRepo)
		}
	})

	t.Run("a record on the old name with no GitHub spelling", func(t *testing.T) {
		w, state := seed(t, "acme/old", "main")
		want := leftAs("LEGION-1", "acme/old", "GitHub renamed acme/old to acme/new, and a record keeps its repository name unless a pair of repository_renames names both; "+
			"only a pre-release build of this daemon leaves a record with no GitHub spelling, so recreate that build's database")
		if left, logged := backfill(t, w, state); !left || !strings.Contains(logged, want) {
			t.Fatalf("backfill left %t, logging:\n%s\nwant the record left, logged as %s", left, logged, want)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "acme/old" || got.GitHubRepo != "" {
			t.Fatalf("LEGION-1 = %s (GitHub's %s) after the backfill left it, want it unchanged", got.Repo, got.GitHubRepo)
		}
	})
}

// servedOwnerTokens is roleTokens that mints for one owner alone, the owner whose installation
// holds the repository now, and refuses every other: that is what a transfer costs, since a
// GitHub App is installed on an owner and the installation covering a repository after a
// transfer is the new owner's. GitHub answers the old address itself — a REST read of it
// redirects — so the token is the half no redirect makes up for, and a token source that
// answered whatever owner it was asked for would hide the mint the backfill makes.
type servedOwnerTokens struct {
	roleTokens
	owner string
}

func (t servedOwnerTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	if !strings.EqualFold(owner, t.owner) {
		return appauth.Lease{}, fmt.Errorf("no installation of the %s App on %s covers the repository", role, owner)
	}
	return t.roleTokens.Token(ctx, role, owner)
}

// A transfer the deployment configures as a pair (`repository_renames`) leaves no record: GitHub
// holds the repository under cybermonklabs/legion, which the rename rule serves, while Dispatch still
// resolves sjawhar/legion and the record keeps the name it was recorded under until the rename pass,
// so the backfill writes the pair's spelling of that same name as GitHub's spelling of it. A
// record's two repository columns must name one repository (record.Store.FillIssueRepository,
// NameIssueRepository), which the name it moved to would not.
//
// Each backfill reads GitHub for the repository the rename rule serves the recorded one as
// (defaultBranches, through repositories.serving), because only cybermonklabs' installation
// covers it now: a read minted for sjawhar is refused, as this token source refuses it, and that
// refusal would leave the record. Both backfills do it: the record written before the record
// carried its repository, whose shared clone keeps the old name, and the record on the old name
// with no GitHub spelling. The repository is then heard under both names, so the events GitHub
// publishes under the name it moved to reach the record all the same.
func TestTheBackfillOfAConfiguredRenameFillsTheRecordsOwnName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"sjawhar/legion": "301 cybermonklabs/legion", "cybermonklabs/legion": "cybermonklabs/legion@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "sjawhar/legion"}}}
	seed := func(t *testing.T, repo, branch string) (*workflowRuntime, string) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
			values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, $1, $2)`, repo, branch); err != nil {
			t.Fatal(err)
		}
		// Both role Apps answered installed on the name the repository moved to, which is what
		// the rename rule serves and what makes cybermonklabs the owner whose installation holds it.
		repos := newRepositories("LEGION", transferredLegion)
		repos.fill(ghrepo.MustParse("cybermonklabs/legion"))
		return &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, log: quietLogger(),
			repositories:    repos,
			defaultBranches: newDefaultBranches(servedOwnerTokens{owner: "cybermonklabs"}, repos, github.url, defaultBranchTTL, time.Now)}, t.TempDir()
	}
	backfill := func(w *workflowRuntime, state string) error {
		_, err := backfillRepositories(ctx, w.pool, w.records, client, "LEGION", state, w.defaultBranches, transferredLegion, productionRepositoryReads(), quietLogger())
		return err
	}

	t.Run("a record with no repository whose shared clone keeps the old name", func(t *testing.T) {
		w, state := seed(t, "", "")
		clone, err := workspace.Location(state, ghrepo.MustParse("sjawhar/legion"), "LEGION-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(clone.Clone, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := backfill(w, state); err != nil {
			t.Fatalf("backfill = %v, want the boot to go on: the pair names one repository, read on the owner that holds it", err)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "sjawhar/legion" || got.DefaultBranch != "main" || got.GitHubRepo != "sjawhar/legion" {
			t.Fatalf("LEGION-1 = %s@%s (GitHub's %s), want sjawhar/legion@main (GitHub's sjawhar/legion)", got.Repo, got.DefaultBranch, got.GitHubRepo)
		}
	})

	t.Run("a record on the old name with no GitHub spelling", func(t *testing.T) {
		w, state := seed(t, "sjawhar/legion", "main")
		if err := backfill(w, state); err != nil {
			t.Fatalf("backfill = %v, want the boot to go on: the pair names one repository, read on the owner that holds it", err)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "sjawhar/legion" || got.GitHubRepo != "sjawhar/legion" {
			t.Fatalf("LEGION-1 = %s (GitHub's %s), want sjawhar/legion under both names", got.Repo, got.GitHubRepo)
		}
		heard, err := w.heardAtBoot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := repositoryNames(heard); !slices.Equal(got, []string{"cybermonklabs/legion", "sjawhar/legion"}) {
			t.Fatalf("heard %v, want both names of the pair: the events now arrive under cybermonklabs/legion", got)
		}
	})
}
