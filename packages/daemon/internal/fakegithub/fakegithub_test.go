package fakegithub_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fakegithub"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// TestMain takes out of the process every variable that could lead git to a repository, remote or
// credential not the test's own (fixtureenv.MustGuard): every git here is the machine's real one,
// under an environment of the test's own (gitEnv).
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}

// gitEnv is the environment of a real git that reaches the server as GitHub: an isolated home of
// the test's (fixtureenv.Isolated), with https as the one transport, the server's authority as the
// one it trusts, and no prompt for a credential git lacks. https_proxy is Route's, which the
// process holds and Isolated keeps.
func gitEnv(t *testing.T, s *fakegithub.Server) []string {
	t.Helper()
	return append(fixtureenv.Isolated(t.TempDir()), "GIT_ALLOW_PROTOCOL=https", "GIT_SSL_CAINFO="+s.CAFile(), "GIT_TERMINAL_PROMPT=0")
}

// git runs the real git in dir under env and returns what it printed.
func git(dir string, env []string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir, command.Env = dir, env
	out, err := command.CombinedOutput()
	return string(out), err
}

// presenting is git's arguments that have it present token as GitHub asks, through a credential
// helper of its own and no other.
func presenting(token string) []string {
	return []string{"-c", "credential.helper=", "-c",
		"credential.helper=!f() { echo username=" + fakegithub.GitUser + "; echo password=" + token + "; }; f"}
}

// head is what the bare repository's ref names.
func head(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := git(bare, fixtureenv.Isolated(t.TempDir()), "--git-dir="+bare, "rev-parse", ref)
	if err != nil {
		t.Fatalf("read %s of %s: %v: %s", ref, bare, err, out)
	}
	return strings.TrimSpace(out)
}

// commitIn writes a file into the clone at dir and commits it.
func commitIn(t *testing.T, dir string, env []string, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "--quiet", "-m", "add " + name}} {
		if out, err := git(dir, env, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// A real git clones a private repository over TLS from the server as https://github.com, through
// the proxy and the trust Route gives it, presenting a token that may write; a commit it pushes
// back is the bare repository's main, and so is a branch it pushes. Every request reached the
// server under the name github.com.
func TestARealGitClonesAndPushesOverTLSWithATokenThatMayWrite(t *testing.T) {
	s := fakegithub.Start(t, fakegithub.Options{})
	repo := s.NewRepository(t, "acme", "widgets", false)
	s.Grant("ghs_writer", true)
	s.Route(t)
	env := gitEnv(t, s)
	clone := filepath.Join(t.TempDir(), "widgets")

	if out, err := git(t.TempDir(), env, append(presenting("ghs_writer"), "clone", "--quiet", repo.URL(), clone)...); err != nil {
		t.Fatalf("git clone %s: %v: %s", repo.URL(), err, out)
	}
	if readme, err := os.ReadFile(filepath.Join(clone, "README.md")); err != nil || string(readme) != "# widgets\n" {
		t.Fatalf("the clone's README.md is %q (%v), want the seed's", readme, err)
	}
	commitIn(t, clone, env, "pushed.txt")
	for _, refspec := range []string{"HEAD:main", "HEAD:refs/heads/feature"} {
		if out, err := git(clone, env, append(presenting("ghs_writer"), "push", "--quiet", "origin", refspec)...); err != nil {
			t.Fatalf("git push %s: %v: %s", refspec, err, out)
		}
	}
	local := head(t, filepath.Join(clone, ".git"), "HEAD")
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature"} {
		if got := head(t, repo.Bare, ref); got != local {
			t.Errorf("the bare repository's %s is %s, want the pushed %s", ref, got, local)
		}
	}
	requests := s.Requests()
	if !slices.ContainsFunc(requests, func(line string) bool {
		return strings.HasPrefix(line, "GET github.com/acme/widgets/info/refs?service=git-receive-pack -> 200")
	}) ||
		slices.ContainsFunc(requests, func(line string) bool { return !strings.Contains(line, " github.com/") }) {
		t.Errorf("the server answered %q, want every request under github.com and the push's advertisement served", requests)
	}
}

// A request of git's smart HTTP that carries no credential, or one the server does not take,
// gets GitHub's 401 with its Basic challenge, and a real git so refused clones nothing. A public
// repository is read with none, and still refuses a push that carries none.
func TestAGitRequestWithNoCredentialIsRefused401WithGitHubsChallenge(t *testing.T) {
	s := fakegithub.Start(t, fakegithub.Options{})
	private := s.NewRepository(t, "acme", "widgets", false)
	public := s.NewRepository(t, "acme", "gadgets", true)
	s.Grant("ghs_reader", false)
	s.Route(t)
	client := s.Client()

	for _, tc := range []struct {
		name, user, token string
	}{
		{name: "no credential"},
		{name: "a token the server does not take", user: fakegithub.GitUser, token: "ghs_unknown"},
		{name: "a taken token under another user", user: "someone", token: "ghs_reader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, private.URL()+"/info/refs?service=git-upload-pack", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.user != "" {
				request.SetBasicAuth(tc.user, tc.token)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") != `Basic realm="GitHub"` {
				t.Errorf("answered %d with WWW-Authenticate %q, want 401 with GitHub's Basic challenge",
					response.StatusCode, response.Header.Get("WWW-Authenticate"))
			}
		})
	}

	env := gitEnv(t, s)
	if out, err := git(t.TempDir(), env, "-c", "credential.helper=", "clone", "--quiet", private.URL(), "widgets"); err == nil ||
		!strings.Contains(out, "could not read Username") {
		t.Errorf("git clone with no credential = %v: %s, want git refused for one it cannot read", err, out)
	}
	if out, err := git(t.TempDir(), env, append(presenting("ghs_unknown"), "clone", "--quiet", private.URL(), "widgets")...); err == nil ||
		!strings.Contains(out, "Authentication failed") {
		t.Errorf("git clone with a token the server does not take = %v: %s, want git's authentication failure", err, out)
	}

	clone := filepath.Join(t.TempDir(), "gadgets")
	if out, err := git(t.TempDir(), env, "-c", "credential.helper=", "clone", "--quiet", public.URL(), clone); err != nil {
		t.Fatalf("git clone of a public repository with no credential: %v: %s", err, out)
	}
	commitIn(t, clone, env, "anonymous.txt")
	before := head(t, public.Bare, "refs/heads/main")
	if out, err := git(clone, env, "-c", "credential.helper=", "push", "--quiet", "origin", "HEAD:main"); err == nil ||
		!strings.Contains(out, "could not read Username") {
		t.Errorf("git push with no credential to a public repository = %v: %s, want it refused for a credential", err, out)
	}
	if after := head(t, public.Bare, "refs/heads/main"); after != before {
		t.Errorf("the public repository's main moved from %s to %s on a push with no credential", before, after)
	}
}

// A token that only reads clones, and its push is refused 403, on the advertisement git asks for
// before it sends anything; the bare repository is as it was.
func TestAPushWithAReadOnlyTokenIsRefused403(t *testing.T) {
	s := fakegithub.Start(t, fakegithub.Options{})
	repo := s.NewRepository(t, "acme", "widgets", false)
	s.Grant("ghs_reader", false)
	s.Route(t)
	env := gitEnv(t, s)
	clone := filepath.Join(t.TempDir(), "widgets")
	if out, err := git(t.TempDir(), env, append(presenting("ghs_reader"), "clone", "--quiet", repo.URL(), clone)...); err != nil {
		t.Fatalf("git clone with a token that reads: %v: %s", err, out)
	}
	commitIn(t, clone, env, "refused.txt")
	before := head(t, repo.Bare, "refs/heads/main")
	out, err := git(clone, env, append(presenting("ghs_reader"), "push", "--quiet", "origin", "HEAD:main")...)
	if err == nil || !strings.Contains(out, "403") {
		t.Fatalf("git push with a token that only reads = %v: %s, want GitHub's 403", err, out)
	}
	t.Logf("git push with a token that only reads: %s", strings.TrimSpace(out))
	if after := head(t, repo.Bare, "refs/heads/main"); after != before {
		t.Errorf("main moved from %s to %s on a refused push", before, after)
	}

	request, err := http.NewRequest(http.MethodGet, repo.URL()+"/info/refs?service=git-receive-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(fakegithub.GitUser, "ghs_reader")
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Permission to acme/widgets.git denied") {
		t.Errorf("the push advertisement answered %d %q, want 403 naming the repository", response.StatusCode, body)
	}
}

// githubAPI points the daemon's REST client at s, as the process's default client
// (githubrest reaches GitHub through http.DefaultClient), until the test ends.
func githubAPI(t *testing.T, s *fakegithub.Server) {
	t.Helper()
	held := http.DefaultClient.Transport
	http.DefaultClient.Transport = s.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = held })
}

// call sends one request to the server's API under token ("" for none) and decodes its JSON answer
// into into, returning the status.
func call(t *testing.T, s *fakegithub.Server, method, path, token string, body any, into any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, fakegithub.API+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answered, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if into != nil && len(answered) > 0 {
		if err := json.Unmarshal(answered, into); err != nil {
			t.Fatalf("%s %s answered %d %q: %v", method, path, response.StatusCode, answered, err)
		}
	}
	return response.StatusCode
}

// Each REST and GraphQL answer is read by the daemon's own code that reads it from GitHub: an App's
// key mints its installation's write and read tokens through internal/appauth, the tokens a push
// is held to; token mode's reads of a token; ghbranch's read of a repository and its create of a
// branch; githubrest's pull request, review, permission and activity reads; and the leak guard's
// visibility reads and its check's tarball.
func TestTheDaemonsOwnClientsReadEachRESTAndGraphQLAnswer(t *testing.T) {
	s := fakegithub.Start(t, fakegithub.Options{})
	repo := s.NewRepository(t, "acme", "widgets", false)
	public := s.NewRepository(t, "acme", "gadgets", true)
	s.Grant("ghs_writer", true)
	s.Grant("ghs_reader", false)
	s.Route(t)
	githubAPI(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	widgets := ghrepo.MustParse(repo.Slug())
	writer := githubrest.Client{Token: "ghs_writer", API: githubrest.RepositoryAPI(fakegithub.API, widgets)}
	reader := githubrest.Client{Token: "ghs_reader", API: githubrest.RepositoryAPI(fakegithub.API, widgets)}
	main := head(t, repo.Bare, "refs/heads/main")

	t.Run("an App's key mints its installation's write and read tokens", func(t *testing.T) {
		app := s.AddApp(t, "legion-test-implement", "acme")
		manager := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: strconv.FormatInt(app.ID, 10), PrivateKey: app.PrivateKey}},
			appauth.Options{BaseURL: fakegithub.API, HTTPClient: s.Client()})
		t.Cleanup(manager.Close)
		owners, err := manager.InstalledOwners(ctx, appauth.Implement)
		if err != nil || !slices.Equal(owners, []string{"acme"}) {
			t.Fatalf("InstalledOwners = %q, %v; want acme", owners, err)
		}
		if installed, err := manager.InstalledOn(ctx, appauth.Implement, widgets); err != nil || !installed {
			t.Fatalf("InstalledOn(acme/widgets) = %v, %v; want installed", installed, err)
		}
		write, err := manager.Token(ctx, appauth.Implement, "acme")
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		bot := "legion-test-implement[bot]"
		if write.Identity.Name != bot || !strings.HasSuffix(write.Identity.Email, "+"+bot+"@users.noreply.github.com") || time.Until(write.ExpiresAt) < 50*time.Minute {
			t.Errorf("the write lease is %+v, want the App's bot identity and an hour", write)
		}
		read, err := manager.ReadToken(ctx, appauth.Implement, "acme")
		if err != nil {
			t.Fatalf("ReadToken: %v", err)
		}
		if _, err := manager.Token(ctx, appauth.Implement, "elsewhere"); !errors.Is(err, appauth.ErrNotInstalled) {
			t.Errorf("Token on an owner the App is not installed on = %v, want ErrNotInstalled", err)
		}

		env := gitEnv(t, s)
		clone := filepath.Join(t.TempDir(), "widgets")
		if out, err := git(t.TempDir(), env, append(presenting(read.Token), "clone", "--quiet", repo.URL(), clone)...); err != nil {
			t.Fatalf("git clone with the read token: %v: %s", err, out)
		}
		commitIn(t, clone, env, "by-the-app.txt")
		if out, err := git(clone, env, append(presenting(read.Token), "push", "--quiet", "origin", "HEAD:refs/heads/app-read")...); err == nil || !strings.Contains(out, "403") {
			t.Errorf("git push with the read token = %v: %s, want 403", err, out)
		}
		if out, err := git(clone, env, append(presenting(write.Token), "push", "--quiet", "origin", "HEAD:refs/heads/app-write")...); err != nil {
			t.Errorf("git push with the write token: %v: %s", err, out)
		}

		// Token mode's reads of a token: its bot through GraphQL, the App by its slug, and the
		// repositories its installation holds.
		var viewer struct {
			Data struct {
				Viewer struct {
					Login string `json:"login"`
				} `json:"viewer"`
			} `json:"data"`
		}
		if status := call(t, s, http.MethodPost, "/graphql", write.Token, map[string]string{"query": "{viewer{login}}"}, &viewer); status != http.StatusOK || viewer.Data.Viewer.Login != bot {
			t.Errorf("GraphQL viewer = %d %q, want %s", status, viewer.Data.Viewer.Login, bot)
		}
		var named struct {
			ID int64 `json:"id"`
		}
		if status := call(t, s, http.MethodGet, "/apps/legion-test-implement", "", nil, &named); status != http.StatusOK || named.ID != app.ID {
			t.Errorf("GET /apps/<slug> = %d id %d, want the App's %d", status, named.ID, app.ID)
		}
		var listed struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if status := call(t, s, http.MethodGet, "/installation/repositories?per_page=100&page=1", write.Token, nil, &listed); status != http.StatusOK ||
			len(listed.Repositories) != 2 || listed.Repositories[0].FullName != "acme/gadgets" || listed.Repositories[1].FullName != "acme/widgets" {
			t.Errorf("GET /installation/repositories = %d %+v, want acme's two", status, listed)
		}
	})

	t.Run("a repository's name, default branch and visibility", func(t *testing.T) {
		named, err := ghbranch.Read(ctx, reader, widgets)
		if err != nil || named.Name.String() != "acme/widgets" || named.DefaultBranch != "main" {
			t.Fatalf("ghbranch.Read = %+v, %v; want acme/widgets on main", named, err)
		}
		var shown struct {
			Visibility string `json:"visibility"`
			Private    bool   `json:"private"`
		}
		if err := reader.Get(ctx, "", &shown); err != nil || shown.Visibility != "private" || !shown.Private {
			t.Errorf("the repository reads %+v, %v; want private", shown, err)
		}
		if status := call(t, s, http.MethodGet, "/repos/acme/widgets", "", nil, nil); status != http.StatusNotFound {
			t.Errorf("a private repository read with no token answered %d, want GitHub's 404", status)
		}
		if status := call(t, s, http.MethodGet, "/repos/acme/gadgets", "", nil, &shown); status != http.StatusOK || shown.Visibility != "public" {
			t.Errorf("a public repository read with no token answered %d %+v, want it public", status, shown)
		}
		if status := call(t, s, http.MethodGet, "/repos/acme/widgets", "ghs_unknown", nil, nil); status != http.StatusUnauthorized {
			t.Errorf("a read with a token the server does not take answered %d, want 401", status)
		}
	})

	t.Run("a branch is created by a token that may write, and refused to one that only reads", func(t *testing.T) {
		if err := ghbranch.Create(ctx, writer, widgets, "legion/feature", "main"); err != nil {
			t.Fatalf("ghbranch.Create: %v", err)
		}
		if got := head(t, repo.Bare, "refs/heads/legion/feature"); got != main {
			t.Errorf("the created branch is at %s, want main's %s", got, main)
		}
		if err := ghbranch.Create(ctx, writer, widgets, "legion/feature", "main"); err != nil {
			t.Errorf("ghbranch.Create of a branch that is there: %v, want GitHub's 422 read as done", err)
		}
		err := ghbranch.Create(ctx, reader, widgets, "legion/other", "main")
		var refused *githubrest.Answer
		if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
			t.Errorf("ghbranch.Create with a token that only reads = %v, want GitHub's 403", err)
		}
	})

	t.Run("pull requests, their reviews, a collaborator's permission and a branch's activity", func(t *testing.T) {
		mergeable := true
		open := s.AddPullRequest(t, repo.Slug(), fakegithub.PullRequest{Head: "legion/feature", Base: "main", Body: "the change", Mergeable: &mergeable})
		merged := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		s.AddPullRequest(t, repo.Slug(), fakegithub.PullRequest{Head: "legion/done", Base: "main", State: "closed", Merged: true, MergedAt: &merged})
		found, ok, err := reader.OpenPullRequestOn(ctx, "acme", "legion/feature")
		if err != nil || !ok || found.Number != open.Number || found.Head.SHA != main || !found.HeadIn("acme/widgets", ghrepo.Renames{}) {
			t.Fatalf("OpenPullRequestOn(legion/feature) = %+v, %v, %v; want #%d at %s", found, ok, err, open.Number, main)
		}
		if _, ok, err := reader.OpenPullRequestOn(ctx, "acme", "legion/done"); err != nil || ok {
			t.Errorf("OpenPullRequestOn(legion/done) = %v, %v; want none open", ok, err)
		}
		var full githubrest.PullRequest
		if err := reader.Get(ctx, fmt.Sprintf("/pulls/%d", open.Number), &full); err != nil || full.State != "open" ||
			full.Base.Ref != "main" || full.Body != "the change" || full.Mergeable == nil || !*full.Mergeable {
			t.Errorf("GET /pulls/%d = %+v, %v; want it open on main, mergeable", open.Number, full, err)
		}
		var done githubrest.PullRequest
		if err := reader.Get(ctx, "/pulls/2", &done); err != nil || !done.IsMerged() {
			t.Errorf("GET /pulls/2 = %+v, %v; want it merged", done, err)
		}

		review := s.AddReview(t, repo.Slug(), open.Number, fakegithub.Review{User: "octo-reviewer", State: "APPROVED", Body: "ship it", CommitID: main,
			SubmittedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)})
		type githubReview struct {
			ID   int64 `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
		}
		reviews, err := githubrest.GetPages[githubReview](ctx, reader, fmt.Sprintf("/pulls/%d/reviews", open.Number))
		if err != nil || len(reviews) != 1 || reviews[0].ID != review.ID || reviews[0].User.Login != "octo-reviewer" || reviews[0].State != "APPROVED" || reviews[0].CommitID != main {
			t.Errorf("the reviews read %+v, %v; want the one approval", reviews, err)
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := reader.Get(ctx, fmt.Sprintf("/pulls/%d/reviews/%d", open.Number, review.ID), &body); err != nil || body.Body != "ship it" {
			t.Errorf("the review's body reads %q, %v", body.Body, err)
		}

		s.SetPermission(t, repo.Slug(), "octo-reviewer", "write")
		var permission struct {
			Permission string `json:"permission"`
			User       struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := reader.Get(ctx, "/collaborators/octo-reviewer/permission", &permission); err != nil || permission.Permission != "write" || permission.User.Login != "octo-reviewer" {
			t.Errorf("octo-reviewer's permission reads %+v, %v; want write", permission, err)
		}
		var refused *githubrest.Answer
		if err := reader.Get(ctx, "/collaborators/stranger/permission", &permission); !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
			t.Errorf("a stranger's permission = %v, want GitHub's 404", err)
		}

		pushed := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
		s.AddActivity(t, repo.Slug(), "main", fakegithub.Activity{Type: "push", Before: strings.Repeat("0", 40), After: main,
			Actor: "legion-test-implement[bot]", ActorType: "Bot", Timestamp: pushed})
		activity, err := reader.BranchActivity(ctx, "main")
		if err != nil || len(activity) != 1 || activity[0].Type != "push" || activity[0].After != main ||
			activity[0].Actor.Login != "legion-test-implement[bot]" || !activity[0].Timestamp.Equal(pushed) {
			t.Errorf("main's activity reads %+v, %v; want the one push", activity, err)
		}
	})

	t.Run("the leak guard's GraphQL visibility read", func(t *testing.T) {
		args := leakguard.VisibilityGraphQLArgs("acme/widgets")
		query := ""
		for _, arg := range args {
			if value, ok := strings.CutPrefix(arg, "query="); ok {
				query = value
			}
		}
		type answer struct {
			Data struct {
				Repository *struct {
					NameWithOwner string `json:"nameWithOwner"`
					Visibility    string `json:"visibility"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Type string `json:"type"`
			} `json:"errors"`
		}
		for _, tc := range []struct{ name, want string }{{"widgets", "PRIVATE"}, {"gadgets", "PUBLIC"}} {
			var got answer
			status := call(t, s, http.MethodPost, "/graphql", "ghs_reader",
				map[string]any{"query": query, "variables": map[string]string{"owner": "acme", "name": tc.name}}, &got)
			if status != http.StatusOK || got.Data.Repository == nil || got.Data.Repository.NameWithOwner != "acme/"+tc.name || got.Data.Repository.Visibility != tc.want {
				t.Errorf("acme/%s's GraphQL visibility = %d %+v, want %s", tc.name, status, got, tc.want)
			}
		}
		var missing answer
		if status := call(t, s, http.MethodPost, "/graphql", "ghs_reader",
			map[string]any{"query": query, "variables": map[string]string{"owner": "acme", "name": "nothing"}}, &missing); status != http.StatusOK ||
			missing.Data.Repository != nil || len(missing.Errors) != 1 || missing.Errors[0].Type != "NOT_FOUND" {
			t.Errorf("a repository the server does not serve read %d %+v, want GraphQL's NOT_FOUND", status, missing)
		}
		if status := call(t, s, http.MethodPost, "/graphql", "", map[string]string{"query": query}, nil); status != http.StatusUnauthorized {
			t.Errorf("GraphQL with no token answered %d, want 401", status)
		}
		var shown struct {
			Visibility string `json:"visibility"`
		}
		if status := call(t, s, http.MethodGet, "/repos/"+public.Slug(), "ghs_reader", nil, &shown); status != http.StatusOK || shown.Visibility != "public" {
			t.Errorf("the REST visibility read of %s = %d %q, want public", public.Slug(), status, shown.Visibility)
		}
	})

	t.Run("the leak check's tarball", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, githubrest.RepositoryAPI(fakegithub.API, widgets)+"/tarball/main", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer ghs_reader")
		response, err := s.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("the tarball answered %d", response.StatusCode)
		}
		unzipped, err := gzip.NewReader(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		archive := tar.NewReader(unzipped)
		var names []string
		for {
			header, err := archive.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, header.Name)
		}
		want := "acme-widgets-" + main[:7] + "/README.md"
		if !slices.Contains(names, want) {
			t.Errorf("the tarball holds %q, want %s", names, want)
		}
	})

	if status := call(t, s, http.MethodGet, "/repos/acme/widgets/no-such-thing", "ghs_reader", nil, nil); status != http.StatusNotFound {
		t.Errorf("a call the fake does not answer got %d, want 404", status)
	}
}
