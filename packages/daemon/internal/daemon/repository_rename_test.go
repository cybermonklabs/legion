package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// renamedWidgets is the pair a deployment configures on the day its key is deployed, before the
// transfer: acme/widgets, which GitHub still holds and the records name, and
// cybermonklabs/widgets, the name the repository moves to, which GitHub does not have yet.
var renamedWidgets = ghrepo.MustRename("acme/widgets", "cybermonklabs/widgets")

// A rename configured before the transfer changes nothing the daemon records on: the refresher
// reads both names' installations (readPairs), GitHub answers that neither App is installed on
// cybermonklabs/widgets, which it does not have yet, and the rename rule serves a record on
// acme/widgets under its own name. What it does change is what GitHub intake hears: the consumer's
// filter subjects carry both names, so an event arrives whichever name GitHub sends it under on
// the day the repository moves, and an issue Dispatch resolves to the name it moved to is recorded
// on the one the daemon read, under that one name. With no pair configured none of it happens:
// nothing is heard under the second name until an issue is recorded on it.
func TestARenameConfiguredBeforeTheTransferIsHeardWhileTheServedSetIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		// heard is what GitHub intake hears, which is also the GitHub consumer's filter subjects.
		heard []string
	}{
		{name: "the pair configured", renames: ghrepo.MustRenames(renamedWidgets), heard: []string{"acme/widgets", "cybermonklabs/widgets", "zeta/tools"}},
		{name: "no pair configured", renames: ghrepo.Renames{}, heard: []string{"acme/widgets", "zeta/tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs := &installations{missing: map[string]bool{"implement cybermonklabs/widgets": true, "review cybermonklabs/widgets": true}}
			w, _, _, stream := repositoryRigWith(t, installs, func(w *workflowRuntime) {
				w.repositories = newRepositories("LEGION", tc.renames)
			})
			if to := ghrepo.MustParse("cybermonklabs/widgets"); !recordsOn(w, "acme/widgets") || w.repositories.servesRepository(to) {
				t.Fatalf("recorded on acme/widgets %v, the rule serving cybermonklabs/widgets %v; want acme/widgets alone: a configured pair widens nothing",
					recordsOn(w, "acme/widgets"), w.repositories.servesRepository(to))
			}
			if got := w.repositories.serving(ghrepo.MustParse("acme/widgets")); got.String() != "acme/widgets" {
				t.Fatalf("a record on acme/widgets is served as %s, want its own name: GitHub still holds it", got)
			}
			heard, err := w.heardAtBoot(context.Background())
			if err != nil {
				t.Fatalf("heardAtBoot: %v", err)
			}
			if got := repositoryNames(heard); !slices.Equal(got, tc.heard) {
				t.Fatalf("heard %v, want %v", got, tc.heard)
			}
			want := make([]string, 0, len(tc.heard))
			for _, repo := range tc.heard {
				want = append(want, "notifications.github."+strings.ReplaceAll(repo, "/", ".")+".>")
			}
			slices.Sort(want)
			got := githubFilterSubjects(t, stream)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("the GitHub consumer's filter subjects = %v, want %v", got, want)
			}
			resolved, err := w.ResolveRepository("cybermonklabs/widgets")
			if err != nil {
				t.Fatalf("ResolveRepository(cybermonklabs/widgets): %v", err)
			}
			switch {
			case tc.renames.IsZero():
				if !resolved.Unread || resolved.Refusal != "GitHub has not been read for cybermonklabs/widgets yet" {
					t.Fatalf("ResolveRepository(cybermonklabs/widgets) = %#v, want it unread: no pair joins it to a repository the daemon has read", resolved)
				}
			case resolved.Repo != "acme/widgets" || resolved.GitHubRepo != "acme/widgets" || resolved.DefaultBranch != "main":
				t.Fatalf("ResolveRepository(cybermonklabs/widgets) = %#v, want acme/widgets@main under one name", resolved)
			}
		})
	}
}

// What GitHub intake hears is spelled as GitHub spells it, whatever case the pair is configured
// in: NATS matches a subject case-sensitively and Envoy publishes under GitHub's own spelling, so
// a pair spelled CyberMonkLabs/legion against GitHub's cybermonklabs/legion would subscribe to
// subjects nothing publishes. A partner is therefore heard only where nothing else names it: a
// record keeps the spelling GitHub gave it (record.Issue.GitHubRepo), and the pair's own spelling
// is used for the side no record names, which is how a pair configured before the transfer is
// heard.
func TestHeardRepositoriesKeepGitHubsSpellingOverThePairs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The pair is written in the deployment's own case, which is neither name as GitHub spells it.
	pair := ghrepo.MustRenames(ghrepo.MustRename("SJAWHAR/Legion", "CyberMonkLabs/legion"))
	for _, tc := range []struct {
		name string
		// recorded is GitHub's spelling each record of the project keeps.
		recorded []string
		heard    []string
	}{
		{name: "both names are recorded", recorded: []string{"sjawhar/legion", "cybermonklabs/legion"},
			heard: []string{"cybermonklabs/legion", "sjawhar/legion"}},
		{name: "the partner no record names is heard as the pair spells it", recorded: []string{"cybermonklabs/legion"},
			heard: []string{"cybermonklabs/legion", "SJAWHAR/Legion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			for index, repo := range tc.recorded {
				if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch, github_repo)
					values ($1, $1, 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, $2, 'main', $2)`, fmt.Sprintf("LEGION-%d", index+1), repo); err != nil {
					t.Fatal(err)
				}
			}
			w := &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"},
				repositories: newRepositories("LEGION", pair), log: quietLogger()}
			heard, err := w.heardAtBoot(ctx)
			if err != nil {
				t.Fatalf("heardAtBoot: %v", err)
			}
			if got := repositoryNames(heard); !slices.Equal(got, tc.heard) {
				t.Fatalf("heard %v, want %v", got, tc.heard)
			}
		})
	}
}

// The twelve cases of the rename rule (repositories.serving): the six states GitHub's answers of
// both role Apps' installations can leave over one configured pair, read from the entries the
// preparations keep, each in the two owner-set states, with what the rule answers for a record
// naming the repository the pair moved from and for one naming the repository it moved to. State
// B is one more repository of the owner the pair moved the repository away from, both Apps
// installed on it, which is the one thing production changed on 2026-11-06: it must change no
// answer here.
//
// Four rules that are not this one pass most of this table, and each case below that one of them
// fails is named where it sits:
//
//   - The owner rule this replaced — resolve while the project holds the pair's to owner and not
//     its from owner — answers I1, I2 and I0 wrong in state B alone, where it leaves a record on
//     sjawhar/legion unresolved. That is the production failure: one more repository of sjawhar
//     made sjawhar an owner of the project, the pair switched off, and 47 trees could not push for
//     42 minutes.
//   - "Resolve whenever the pair's to is served" fails I4's first row, where the two names are two
//     repositories and each must keep its own.
//   - A symmetric rule, which answers from either side as ghrepo.Renames.Partner does, fails I3's
//     second row, where it would address the repository the transfer has not yet moved the name
//     away from.
//   - An unanswered read counted as not served fails I0's first row, where the pair is served as
//     configured until GitHub answers (INFRA-4401 D2).
func TestTheRenameRuleResolvesARecordedRepositoryOverTheSixInstallationStates(t *testing.T) {
	t.Parallel()
	const recordedOld, recordedNew = renameFrom, renameTo
	pair := transferredLegion
	for _, tc := range []struct {
		name string
		// answers is GitHub's answer of both Apps' installations on each name it answered for:
		// installed or not. A name it holds no answer for was never read, or its reads failed.
		answers map[string]bool
		// old and new are what the rule answers for a record naming sjawhar/legion and for one
		// naming cybermonklabs/legion.
		old, new string
	}{
		{
			// The boot before the refresher's first job, or one whose reads of both names failed:
			// the rule serves the pair as the operator configured it. Counting the unanswered
			// reads as not served fails the first answer here, which is the outage of 2026-11-06.
			name: "I0: GitHub has answered no read of either name",
			old:  recordedNew, new: recordedNew,
		},
		{
			name:    "I1: both Apps answered installed on the name the pair moved to, the old name unanswered",
			answers: map[string]bool{recordedNew: true},
			old:     recordedNew, new: recordedNew,
		},
		{
			// The state right after the transfer: no installation of the owner the repository
			// left covers it.
			name:    "I2: both Apps installed on the new name and neither on the old one",
			answers: map[string]bool{recordedNew: true, recordedOld: false},
			old:     recordedNew, new: recordedNew,
		},
		{
			// The pair configured before the transfer: GitHub still holds sjawhar/legion, so a
			// record of it is addressed there. A symmetric rule fails the second answer.
			name:    "I3: both Apps installed on the old name and neither on the new one",
			answers: map[string]bool{recordedOld: true, recordedNew: false},
			old:     recordedOld, new: recordedNew,
		},
		{
			// Somebody made a repository at the old location, which deletes GitHub's redirect: the
			// two names are two repositories, and each record keeps its own. A rule that resolved
			// whenever the pair's to is served fails the first answer here.
			name:    "I4: both Apps installed on both names",
			answers: map[string]bool{recordedOld: true, recordedNew: true},
			old:     recordedOld, new: recordedNew,
		},
		{
			name:    "I5: neither App installed on either name",
			answers: map[string]bool{recordedOld: false, recordedNew: false},
			old:     recordedOld, new: recordedNew,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []struct {
				name  string
				other bool
			}{
				{name: "state A: no other repository of the owner the pair moved from is answered for"},
				{name: "state B: both Apps answered installed on sjawhar/oh-my-pi beside them", other: true},
			} {
				t.Run(state.name, func(t *testing.T) {
					repos := newRepositories("LEGION", pair)
					for name, installed := range tc.answers {
						answerInstalled(repos, name, installed)
					}
					if state.other {
						answerInstalled(repos, "sjawhar/oh-my-pi", true)
					}
					for recorded, want := range map[string]string{recordedOld: tc.old, recordedNew: tc.new} {
						if got := repos.serving(ghrepo.MustParse(recorded)); got.String() != want {
							t.Errorf("a record naming %s is served as %s over %v, want %s", recorded, got, tc.answers, want)
						}
					}
				})
			}
		})
	}
}

// mintingTokens is servedOwnerTokens keeping the owner of every token it is asked for: it refuses
// each owner but the one whose installation covers the repository, as a transfer leaves GitHub: an
// App is installed on an owner, so the installation holding a moved repository is the owner it
// moved to, and a mint for the owner it left reaches nothing, however the address redirects. A
// source that answered whatever owner it was asked for would hide which owner a read of GitHub's
// spelling or a role's token is minted for.
type mintingTokens struct {
	servedOwnerTokens

	mu     sync.Mutex
	minted []string
}

func (t *mintingTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	t.mu.Lock()
	t.minted = append(t.minted, strings.ToLower(owner))
	t.mu.Unlock()
	return t.servedOwnerTokens.Token(ctx, role, owner)
}

// owners is every owner a token was minted for, lowercased, sorted and once each.
func (t *mintingTokens) owners() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	owners := slices.Clone(t.minted)
	slices.Sort(owners)
	return slices.Compact(owners)
}

// transferredRuntime is a workflow runtime over a fresh database whose project records a live
// issue on sjawhar/legion, the `from` of transferredLegion, read by installs and minted by tokens
// only for cybermonklabs, with GitHub after the transfer: the old address answered with a redirect
// to cybermonklabs/legion, at main.
func transferredRuntime(t *testing.T, installs *installations, tokens *mintingTokens) *workflowRuntime {
	t.Helper()
	github := newNamedGitHub(t, map[string]string{renameFrom: "301 " + renameTo, renameTo: renameTo + "@main"})
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "LEGION"}, dispatch: &repositoryDispatch{},
		dispatchProject: "LEGION", installs: installs, tokens: tokens, repositories: newRepositories("LEGION", transferredLegion), log: quietLogger()}
	w.defaultBranches = newDefaultBranches(tokens, w.repositories, github.url, defaultBranchTTL, time.Now)
	putOutboxIssue(t, w.pool, w.records, anchorOn("LEGION-1", renameFrom))
	return w
}

// A live record on the old name of a configured pair, with both role Apps installed on the pair's
// `to` and on no repository of the old owner — the state a transfer leaves — is worked as the new
// owner's: the refresher's first job reads both names (readPairs) and prepares the record's
// repository under the name the rename rule serves it as (prepareLive), so the mint of a role's
// token, the read of its default branch and the gh bundle's owner alias (bundleDirs'
// renames.OwnerAliases) are each the new owner's. Control (dispatch://INFRA-4401 check a-go, item
// 6): with served meaning a repository that a record names, sjawhar/legion is served under its own
// name and each of the three goes to sjawhar, whose installation covers nothing.
func TestARecordOnAPairsOldNameGetsTheNewOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const old, moved = renameFrom, renameTo
	tokens := &mintingTokens{servedOwnerTokens: servedOwnerTokens{owner: "cybermonklabs"}}
	w := transferredRuntime(t, &installations{missing: map[string]bool{"implement " + old: true, "review " + old: true}}, tokens)
	w.prepareAtStart(ctx)
	if got := w.repositories.serving(ghrepo.MustParse(old)); got.String() != moved {
		t.Fatalf("a record on %s is served as %s, want %s", old, got, moved)
	}
	if kept, ok := w.defaultBranches.Kept(old); !ok || kept.Name.String() != moved || kept.DefaultBranch != "main" {
		t.Fatalf("the default branch read for %s = %+v, %v; want GitHub's %s@main, read on the new owner's token", old, kept, ok, moved)
	}
	if _, err := w.identity(ctx, claim.RoleImplementer, ghrepo.MustParse(old)); err != nil {
		t.Fatalf("the implementer's identity for a record on %s: %v, want it minted for the new owner", old, err)
	}
	if got := tokens.owners(); !slices.Equal(got, []string{"cybermonklabs"}) {
		t.Fatalf("tokens were minted for %v, want the new owner alone", got)
	}
	if aliases := w.repositories.renames.OwnerAliases(w.repositories.servesRepository); !maps.Equal(aliases, map[string]string{"sjawhar": "cybermonklabs"}) {
		t.Fatalf("the gh bundle's owner aliases = %v, want sjawhar's directory to hold cybermonklabs' lease", aliases)
	}
}

// An issue Dispatch resolves to a configured pair's old name, with nothing read for either name
// yet, is recorded on the pair's new name (INFRA-4401 D1): GitHub answers that neither role App is
// installed on the old name, which a transfer leaves, so the preparation prepares the pair's other
// name too and both Apps answer installed there (prepareRead). The record takes GitHub's name and
// default branch of the new name, and the read is kept under both names, so the issue's apply
// finds it under the name it resolves to. Control: with the read of the pair's other name removed,
// the issue is refused for the Apps the old name lacks.
func TestAnIssueResolvingToAPairsOldNameIsRecordedOnItsNewName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const old, moved = renameFrom, renameTo
	installs := &installations{missing: map[string]bool{"implement " + old: true, "review " + old: true}}
	github := newNamedGitHub(t, map[string]string{old: "301 " + moved, moved: moved + "@main"})
	repos := newRepositories("LEGION", transferredLegion)
	w := &workflowRuntime{dispatch: &repositoryDispatch{}, dispatchProject: "LEGION", installs: installs, repositories: repos, log: quietLogger(),
		defaultBranches: newDefaultBranches(roleTokens{}, repos, github.url, defaultBranchTTL, time.Now)}
	prepared, err := w.prepareUnrecorded(ctx, intake.DispatchIssue{Key: "LEGION-7", Seq: 1, Status: "todo", Title: "on the old name", Rank: "A", HandedOver: true, ResolvedRepo: old}, false)
	if err != nil || prepared.RepositoryRefusal != "" {
		t.Fatalf("prepare an issue on %s = refusal %q, %v; want it prepared on %s", old, prepared.RepositoryRefusal, err, moved)
	}
	resolved, err := w.ResolveRepository(old)
	if err != nil || resolved.Unread || resolved.Repo != moved || resolved.GitHubRepo != moved || resolved.DefaultBranch != "main" {
		t.Fatalf("ResolveRepository(%s) = %#v, %v; want the issue recorded on %s@main", old, resolved, err, moved)
	}
	if asked, want := installs.asks(), []string{"implement " + moved, "implement " + old, "review " + moved, "review " + old}; !slices.Equal(asked, want) {
		t.Fatalf("installation reads = %v, want %v: the old name, then the pair's new one", asked, want)
	}
}

// A boot whose installation read of a configured pair's `to` does not answer still serves the
// pair as configured (INFRA-4401 D2): until GitHub has answered both role Apps' reads of the `to`,
// a record on the `from` is served under the `to`, so the mint of its role's token is the new
// owner's, never the old owner's, whose installation no longer covers the repository (the outage
// of 2026-11-06, api/handoff_rename_test.go). Control: with an unanswered read counted as not
// served, the record is served under its own name and the mint goes to sjawhar, which refuses it.
func TestAnUnansweredReadOfAPairsNewNameStillServesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const old, moved = renameFrom, renameTo
	tokens := &mintingTokens{servedOwnerTokens: servedOwnerTokens{owner: "cybermonklabs"}}
	installs := &installations{missing: map[string]bool{"implement " + old: true, "review " + old: true}}
	installs.setFailing(&appauth.TransientError{Err: errors.New("GitHub answered 502")}, "implement "+moved)
	w := transferredRuntime(t, installs, tokens)
	w.prepareAtStart(ctx)
	if entry := w.repositories.entryOf(moved); entry.answered() || entry.readError == "" {
		t.Fatalf("%s's entry = %+v, want its implement App's read unanswered", moved, entry)
	}
	if got := w.repositories.serving(ghrepo.MustParse(old)); got.String() != moved {
		t.Fatalf("a record on %s is served as %s while %s's read is unanswered, want %s", old, got, moved, moved)
	}
	if _, err := w.identity(ctx, claim.RoleImplementer, ghrepo.MustParse(old)); err != nil {
		t.Fatalf("the implementer's identity for a record on %s: %v, want it minted for the new owner", old, err)
	}
	if got := tokens.owners(); !slices.Equal(got, []string{"cybermonklabs"}) {
		t.Fatalf("tokens were minted for %v, want the new owner alone", got)
	}
}
