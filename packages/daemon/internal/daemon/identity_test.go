package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pane commits as its role's App, minted for the owner of the repository its issue works: the
// launch carries the six git identity variables every worker pane gets and the table of every
// role's identity (runtime.RoleIdentitiesVariable), asked once per App, the claim's own first,
// each for that same repository, nothing else of the Apps, and a launch whose own identity cannot
// be resolved does not happen.
func TestSpawnSpecCarriesTheRoleAppIdentity(t *testing.T) {
	// Not parallel: it unsets environment variables of the whole process (Unset).
	stateDir := t.TempDir()
	token, err := claim.NewToken("s1", "S1-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: token, Issue: "S1-1", Tree: "S1-1", Role: claim.RoleImplementer}
	if err := os.MkdirAll(filepath.Join(stateDir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolePromptPath(stateDir, token), []byte("implement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The six below and the table are the whole of Env only for a daemon holding none of the
	// passthrough variables a launch is told beside them (stallLimitVariables,
	// refusalCooldownVariables): this test's daemon holds none.
	fixtureenv.Unset(t, slices.Concat(stallLimitVariables, refusalCooldownVariables)...)
	bot := runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}
	var asked []string
	s := specs{stateDir: stateDir, project: "s1", appOwners: noAppOwners,
		repository: func(context.Context, string) (launchRepository, error) {
			return launchRepository{Repo: ghrepo.MustParse("zeta/tools"), DefaultBranch: "main"}, nil
		},
		identity: func(_ context.Context, role claim.Role, repository ghrepo.Repository) (runtime.GitIdentity, error) {
			asked = append(asked, string(appOf(role))+" on "+repository.String())
			return bot, nil
		}}
	spec, err := s.SpawnSpec(context.Background(), c)
	if err != nil || spec.Repository != ghrepo.MustParse("zeta/tools") {
		t.Fatalf("SpawnSpec = %q, %v; want zeta/tools, the runtime's to locate the workspace from", spec.Repository, err)
	}
	// The claim's own App first, as before the table, then the other App, whose one identity is
	// all of its roles': the role each is asked with only names its App (workflowRuntime.identity).
	want := []string{string(appauth.Implement) + " on zeta/tools", string(appauth.Review) + " on zeta/tools"}
	if !slices.Equal(asked, want) {
		t.Errorf("identity asked for the Apps %q, want the claim's own App and then the other App, each on the recorded repository alone, %q: the owner its lease is minted for is identity's own to decide", asked, want)
	}
	identity := map[string]string{
		"JJ_USER": bot.Name, "JJ_EMAIL": bot.Email,
		"GIT_AUTHOR_NAME": bot.Name, "GIT_AUTHOR_EMAIL": bot.Email,
		"GIT_COMMITTER_NAME": bot.Name, "GIT_COMMITTER_EMAIL": bot.Email,
		runtime.RoleIdentitiesVariable: runtime.RoleIdentities(map[claim.Role]runtime.GitIdentity{
			claim.RoleArchitect: bot, claim.RolePlanner: bot, claim.RoleImplementer: bot,
			claim.RoleTester: bot, claim.RoleReviewer: bot, claim.RoleMerger: bot,
		}),
	}
	if !maps.Equal(spec.Env, identity) {
		t.Errorf("the launch's Env = %v, want the six git identity variables and the role identities alone", spec.Env)
	}

	s.identity = func(context.Context, claim.Role, ghrepo.Repository) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{}, errors.New("github_app_not_installed")
	}
	if _, err := s.SpawnSpec(context.Background(), c); err == nil || !strings.Contains(err.Error(), "github_app_not_installed") {
		t.Fatalf("SpawnSpec with no identity = %v, want the lease failure", err)
	}
}

// appIdentities is the identity each of the two GitHub Apps commits as, for the launch tests that
// give each App its own: every role of an App commits as that App's one identity
// (appauth.AppRoleFor).
var appIdentities = map[appauth.AppRole]runtime.GitIdentity{
	appauth.Review:    {Name: "legion-review[bot]", Email: "8+legion-review[bot]@users.noreply.github.com"},
	appauth.Implement: {Name: "legion-implement[bot]", Email: "9+legion-implement[bot]@users.noreply.github.com"},
}

// appOf is the App of role, one of Legion's own (appauth.AppRoleFor, with no role of a
// deployment): these tests launch Legion's roles alone.
func appOf(role claim.Role) appauth.AppRole {
	app, _ := appauth.AppRoleFor(role, nil)
	return app
}

// otherApp is the App other than role's own (appOf): the App a launch of role asks for the
// identity of the roles its own App does not cover (specs.roleIdentities).
func otherApp(role claim.Role) appauth.AppRole {
	if appOf(role) == appauth.Implement {
		return appauth.Review
	}
	return appauth.Implement
}

// Every worker's launch carries LEGION_ROLE_IDENTITIES, the identity of each role of claim.Roles
// as its own App mints it (appauth.AppRoleFor: the review App for the architect, the planner, the
// tester and the reviewer, the implement App for the implementer and the merger), asked for the
// repository the launch works, whatever role the launch is: the push step of `legion jj` reads it
// to set each commit's author from its Legion-Role trailer, where the step's rules let it. Each
// App is asked once, the claim's own first, since every role of an App commits as that App's one
// identity. The controller's launch carries none.
func TestEveryLaunchCarriesTheIdentityOfEachRole(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	repository := ghrepo.MustParse("zeta/tools")
	var asked []appauth.AppRole
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
		repository: func(context.Context, string) (launchRepository, error) {
			return launchRepository{Repo: repository, DefaultBranch: "main"}, nil
		},
		identity: func(_ context.Context, role claim.Role, at ghrepo.Repository) (runtime.GitIdentity, error) {
			if at != repository {
				t.Errorf("identity asked for the %s on %s, want the repository the launch works, %s", role, at, repository)
			}
			asked = append(asked, appOf(role))
			return appIdentities[appOf(role)], nil
		}}
	const table = `{"architect":{"name":"legion-review[bot]","email":"8+legion-review[bot]@users.noreply.github.com"},` +
		`"implementer":{"name":"legion-implement[bot]","email":"9+legion-implement[bot]@users.noreply.github.com"},` +
		`"merger":{"name":"legion-implement[bot]","email":"9+legion-implement[bot]@users.noreply.github.com"},` +
		`"planner":{"name":"legion-review[bot]","email":"8+legion-review[bot]@users.noreply.github.com"},` +
		`"reviewer":{"name":"legion-review[bot]","email":"8+legion-review[bot]@users.noreply.github.com"},` +
		`"tester":{"name":"legion-review[bot]","email":"8+legion-review[bot]@users.noreply.github.com"}}`
	for _, role := range claim.Roles {
		token, err := claim.NewToken("s1", "S1-2", role)
		if err != nil {
			t.Fatal(err)
		}
		asked = nil
		spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
		if err != nil {
			t.Fatalf("the %s's SpawnSpec: %v", role, err)
		}
		if want := []appauth.AppRole{appOf(role), otherApp(role)}; !slices.Equal(asked, want) {
			t.Errorf("the %s's launch asked identity for the Apps %q, want %q: its own App, then the other App", role, asked, want)
		}
		if got := spec.Env[runtime.RoleIdentitiesVariable]; got != table {
			t.Errorf("the %s's LEGION_ROLE_IDENTITIES = %q, want every role's identity, %q", role, got, table)
		}
		if own := appIdentities[appOf(role)]; spec.Env["JJ_USER"] != own.Name || spec.Env["JJ_EMAIL"] != own.Email {
			t.Errorf("the %s commits as %q <%s>, want its own App's identity, %q <%s>", role, spec.Env["JJ_USER"], spec.Env["JJ_EMAIL"], own.Name, own.Email)
		}
	}
	controller, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController})
	if err != nil {
		t.Fatalf("the controller's SpawnSpec: %v", err)
	}
	if value, set := controller.Env[runtime.RoleIdentitiesVariable]; set {
		t.Errorf("the controller's launch carries LEGION_ROLE_IDENTITIES=%q, want none: it pushes nothing", value)
	}
}

// A launch's table of every role's identity holds each role the deployment defines too, by the App
// the role acts as (its `github_app`): the checker on the implement App, the critic on the review
// App. The table is one the push step's read takes whole (runtime.ReadRoleIdentities, which takes
// a key of the role grammar since it reads no settings), so a role of the deployment in it turns
// no launch's step off.
func TestALaunchCarriesTheIdentityOfEachRoleOfTheDeployment(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	repository := ghrepo.MustParse("zeta/tools")
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
		roles: []config.Role{
			{Name: "checker", Type: "testing", GitHubApp: config.RoleAppImplement},
			{Name: "critic", Type: "review", GitHubApp: config.RoleAppReview},
		},
		repository: func(context.Context, string) (launchRepository, error) {
			return launchRepository{Repo: repository, DefaultBranch: "main"}, nil
		},
		identity: func(_ context.Context, role claim.Role, _ ghrepo.Repository) (runtime.GitIdentity, error) {
			return appIdentities[appOf(role)], nil
		}}
	every := map[claim.Role]runtime.GitIdentity{
		"checker": appIdentities[appauth.Implement],
		"critic":  appIdentities[appauth.Review],
	}
	for _, role := range claim.Roles {
		every[role] = appIdentities[appOf(role)]
	}
	token, err := claim.NewToken("s1", "S1-2", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer})
	if err != nil {
		t.Fatalf("the implementer's SpawnSpec: %v", err)
	}
	got := spec.Env[runtime.RoleIdentitiesVariable]
	if want := runtime.RoleIdentities(every); got != want {
		t.Errorf("LEGION_ROLE_IDENTITIES = %q, want Legion's roles and the deployment's, each by its App, %q", got, want)
	}
	if read, why := runtime.ReadRoleIdentities(got); why != "" || !maps.Equal(read, every) {
		t.Errorf("the push step reads the launch's table as %v, %q; want every role's identity, %v", read, why, every)
	}
}

// A launch whose other App's identity cannot be minted still happens: a claim of either App
// commits as its own App's identity, and its table holds its own App's roles alone. The other
// App's roles are left out, which the push step reads as roles with no identity, so a commit
// whose trailer names one keeps its author. The other App is asked once rather than once per
// role, and the daemon logs one warning naming the claim, that App and the mint's error.
func TestALaunchWhoseOtherAppCannotBeMintedCarriesItsOwnAppsRoles(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	refused := errors.New("the App lease could not be minted")
	for _, role := range claim.Roles {
		own, other := appOf(role), otherApp(role)
		t.Run(string(role), func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", role)
			if err != nil {
				t.Fatal(err)
			}
			var asked []claim.Role
			var logged bytes.Buffer
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
				log: slog.New(slog.NewJSONHandler(&logged, nil)),
				identity: func(_ context.Context, asking claim.Role, _ ghrepo.Repository) (runtime.GitIdentity, error) {
					asked = append(asked, asking)
					if appOf(asking) == other {
						return runtime.GitIdentity{}, refused
					}
					return appIdentities[own], nil
				}}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
			if err != nil {
				t.Fatalf("SpawnSpec = %v, want the launch: a failed mint of the %s App fails no launch of the %s", err, other, role)
			}
			for name, value := range appIdentities[own].Env() {
				if spec.Env[name] != value {
					t.Errorf("the launch's %s = %q, want %q: the %s commits as its own App's identity", name, spec.Env[name], value, role)
				}
			}
			ownRoles := map[claim.Role]runtime.GitIdentity{}
			for _, r := range claim.Roles {
				if appOf(r) == own {
					ownRoles[r] = appIdentities[own]
				}
			}
			if got, want := spec.Env[runtime.RoleIdentitiesVariable], runtime.RoleIdentities(ownRoles); got != want {
				t.Errorf("LEGION_ROLE_IDENTITIES = %q, want the %s App's roles alone, %q", got, own, want)
			}
			if len(asked) != 2 || asked[0] != role || appOf(asked[1]) != other {
				t.Errorf("identity asked for %q, want the %s and then one role of the %s App, once", asked, role, other)
			}
			var warnings []map[string]any
			for line := range strings.Lines(logged.String()) {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("the launch logged %q, which is no JSON record: %v", line, err)
				}
				warnings = append(warnings, record)
			}
			if len(warnings) != 1 {
				t.Fatalf("the launch logged %d records, want one warning:\n%s", len(warnings), logged.String())
			}
			if w := warnings[0]; w["level"] != "WARN" || w["claim"] != string(token) || w["app"] != string(other) || w["error"] != refused.Error() {
				t.Errorf("the launch logged %v, want one warning naming the claim %s, the %s App and the mint's error %q", w, token, other, refused)
			}
		})
	}
}

// A launch whose context ends while the other App is being minted fails with that mint's error,
// naming the claim, as one whose context ends during its own mint does, and logs no warning: an
// ended context is no failure of the other App, so it is never read as one (roleIdentities).
func TestALaunchWhoseContextEndsDuringTheOtherAppsMintFails(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	for _, ending := range []struct {
		name string
		want error
		// launch is the launch's context, and what the other App's mint does with it before it
		// answers that context's error, as a mint the context ended does.
		launch func() (ctx context.Context, during func(), stop func())
	}{
		{name: "cancelled during the mint", want: context.Canceled, launch: func() (context.Context, func(), func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel, cancel
		}},
		{name: "past its deadline during the mint", want: context.DeadlineExceeded, launch: func() (context.Context, func(), func()) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			return ctx, func() { <-ctx.Done() }, cancel
		}},
	} {
		for _, role := range claim.Roles {
			own := appOf(role)
			t.Run(ending.name+"/"+string(role), func(t *testing.T) {
				token, err := claim.NewToken("s1", "S1-2", role)
				if err != nil {
					t.Fatal(err)
				}
				ctx, during, stop := ending.launch()
				defer stop()
				var logged bytes.Buffer
				s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
					log: slog.New(slog.NewJSONHandler(&logged, nil)),
					identity: func(ctx context.Context, asking claim.Role, _ ghrepo.Repository) (runtime.GitIdentity, error) {
						if app := appOf(asking); app != own {
							during()
							return runtime.GitIdentity{}, fmt.Errorf("mint the %s App lease for %s: %w", app, asking, ctx.Err())
						}
						return appIdentities[own], nil
					}}
				spec, err := s.SpawnSpec(ctx, supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
				if err == nil {
					t.Fatalf("SpawnSpec = %v, want a failure: the launch's context ended during the other App's mint", spec.Env)
				}
				if !errors.Is(err, ending.want) || !strings.Contains(err.Error(), string(token)) {
					t.Errorf("SpawnSpec = %v, want the mint's error, %v, naming the claim %s", err, ending.want, token)
				}
				if logged.Len() != 0 {
					t.Errorf("the launch logged %s, want nothing: an ended context is no failure of the other App", logged.String())
				}
			})
		}
	}
}

// launchTokens is a token source whose every write lease carries its App's identity (identities),
// and which records each App it mints for, in order. Like every other test's token source, it
// answers no known-identity read.
type launchTokens struct {
	identities map[appauth.AppRole]runtime.GitIdentity
	minted     []appauth.AppRole
}

func (l *launchTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	l.minted = append(l.minted, role)
	return appauth.Lease{Token: string(role) + "-app-token-for-" + owner, Identity: l.identities[role]}, nil
}

func (l *launchTokens) ReadToken(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	return appauth.Lease{}, fmt.Errorf("a launch asks the %s App for no read token", role)
}

// knownLaunchTokens is launchTokens that also answers appauth.KnownIdentities for the Apps known
// names, as appauth.Manager does once a lease of the App fetched its identity, and records each
// App it is asked about, in order.
type knownLaunchTokens struct {
	*launchTokens
	known map[appauth.AppRole]bool
	read  []appauth.AppRole
}

func (k *knownLaunchTokens) KnownIdentity(role appauth.AppRole) (runtime.GitIdentity, bool) {
	k.read = append(k.read, role)
	if !k.known[role] {
		return runtime.GitIdentity{}, false
	}
	return k.identities[role], true
}

// A launch takes the other App's identity the daemon already holds (appauth.KnownIdentities, read
// through workflowRuntime.knownIdentity) and mints that App no lease: the table holds all six
// roles, each its own App's identity, the claim's own App is minted for the claim's own identity
// even though its identity is known too, and the other App is read once and never minted. Where
// the source does not know the other App's identity yet, or answers no such read at all, as every
// other fake does, the other App is minted once, after the claim's own.
func TestALaunchTakesTheOtherAppsKnownIdentityWithNoMint(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	every := map[claim.Role]runtime.GitIdentity{}
	for _, role := range claim.Roles {
		every[role] = appIdentities[appOf(role)]
	}
	table := runtime.RoleIdentities(every)
	for _, role := range claim.Roles {
		own, other := appOf(role), otherApp(role)
		t.Run(string(role), func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", role)
			if err != nil {
				t.Fatal(err)
			}
			launch := func(name string, tokens appauth.Tokens) {
				t.Helper()
				w := &workflowRuntime{tokens: tokens}
				s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
					repository: func(context.Context, string) (launchRepository, error) {
						return launchRepository{Repo: ghrepo.MustParse("zeta/tools"), DefaultBranch: "main"}, nil
					},
					identity: w.identity, knownIdentity: w.knownIdentity}
				spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
				if err != nil {
					t.Fatalf("%s: SpawnSpec: %v", name, err)
				}
				if got := spec.Env[runtime.RoleIdentitiesVariable]; got != table {
					t.Errorf("%s: LEGION_ROLE_IDENTITIES = %q, want all six roles, %q", name, got, table)
				}
				if spec.Env["JJ_USER"] != appIdentities[own].Name || spec.Env["JJ_EMAIL"] != appIdentities[own].Email {
					t.Errorf("%s: the %s commits as %q <%s>, want its own App's identity, %+v", name, role, spec.Env["JJ_USER"], spec.Env["JJ_EMAIL"], appIdentities[own])
				}
			}

			known := &knownLaunchTokens{launchTokens: &launchTokens{identities: appIdentities}, known: map[appauth.AppRole]bool{own: true, other: true}}
			launch("both Apps known", known)
			if want := []appauth.AppRole{own}; !slices.Equal(known.minted, want) {
				t.Errorf("both Apps known: minted %q, want %q: the claim's own identity is a mint's, and the other App's is known", known.minted, want)
			}
			if want := []appauth.AppRole{other}; !slices.Equal(known.read, want) {
				t.Errorf("both Apps known: read the known identity of %q, want %q: the other App once, and the claim's own App's roles take its own identity", known.read, want)
			}

			unknown := &knownLaunchTokens{launchTokens: &launchTokens{identities: appIdentities}, known: map[appauth.AppRole]bool{own: true}}
			launch("the other App not yet known", unknown)
			if want := []appauth.AppRole{own, other}; !slices.Equal(unknown.minted, want) {
				t.Errorf("the other App not yet known: minted %q, want %q", unknown.minted, want)
			}

			plain := &launchTokens{identities: appIdentities}
			launch("a source with no known-identity read", plain)
			if want := []appauth.AppRole{own, other}; !slices.Equal(plain.minted, want) {
				t.Errorf("a source with no known-identity read: minted %q, want %q", plain.minted, want)
			}
		})
	}
}

// A launch whose own App's lease cannot be minted does not happen, whatever the daemon knows of
// either App's identity: the claim's own identity is a mint's (identity), never the known read,
// and its failure fails the launch, carrying the mint's error and naming the claim, before the
// other App is asked anything.
func TestALaunchWhoseOwnAppCannotBeMintedFails(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	refused := errors.New("the App lease could not be minted")
	for _, role := range claim.Roles {
		own := appOf(role)
		t.Run(string(role), func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", role)
			if err != nil {
				t.Fatal(err)
			}
			var asked []claim.Role
			var read []claim.Role
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
				identity: func(_ context.Context, asking claim.Role, _ ghrepo.Repository) (runtime.GitIdentity, error) {
					asked = append(asked, asking)
					if appOf(asking) == own {
						return runtime.GitIdentity{}, refused
					}
					return runtime.GitIdentity{Name: "legion-" + string(asking) + "[bot]", Email: string(asking) + "@users.noreply.github.com"}, nil
				},
				knownIdentity: func(reading claim.Role) (runtime.GitIdentity, bool) {
					read = append(read, reading)
					return runtime.GitIdentity{Name: "legion-" + string(reading) + "[bot]", Email: string(reading) + "@users.noreply.github.com"}, true
				}}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
			if err == nil {
				t.Fatalf("SpawnSpec = %v, want a failure: the %s's own App could not be minted", spec.Env, role)
			}
			if !errors.Is(err, refused) || !strings.Contains(err.Error(), string(token)) {
				t.Errorf("SpawnSpec = %v, want the mint's error, naming the claim %s", err, token)
			}
			if want := []claim.Role{role}; !slices.Equal(asked, want) || len(read) != 0 {
				t.Errorf("identity asked for %q and the known identity read for %q, want %q and nothing: the claim's own mint comes first and its failure ends the launch", asked, read, want)
			}
		})
	}
}
