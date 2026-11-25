package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// reviewFixture is a shared clone standing on an issue's branch at origin, made as the issue's
// workspace provision makes one, and the review clone ProvisionReviewClone is asked to make
// beside it: origin, the commits a commit makes there (base on the default branch, then first on
// the issue's branch), and the request, which gives the reviewer first, the branch's head.
type reviewFixture struct {
	setup       fixturegit.Runner
	git, jj     string
	origin      string
	request     ReviewRequest
	base, first string
}

func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	f := reviewFixture{git: fixturegit.CompiledGit(t), jj: realTool(t, "jj")}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, entry := range fixtureenv.Home(home) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	t.Setenv("JJ_USER", "Legion test")
	t.Setenv("JJ_EMAIL", "legion-test@example.invalid")
	f.setup = fixturegit.Isolated(t, home).With("PATH=" + filepath.Dir(f.git) + ":/usr/bin:/bin")
	repo := ghrepo.MustParse("acme/widgets")
	const issue = "ACME-4"
	f.origin = filepath.Join(root, "origin")
	initOrigin := fixturegit.InitWorkTree(f.origin, "main")
	initOrigin[0] = f.git
	f.setup.Run(root, initOrigin...)
	f.base = f.commit(t, "base\n")
	f.setup.Run(f.origin, f.git, "checkout", "--quiet", "-b", Bookmark(issue))
	f.first = f.commit(t, "base\nfirst\n")
	state := filepath.Join(root, "state")
	located, err := Location(state, repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	f.setup.Run(root, f.git, "clone", "--quiet", "--branch", Bookmark(issue), f.origin, located.Clone)
	review, err := ReviewLocation(state, repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	f.request = ReviewRequest{Clone: located.Clone, Dir: review, Repo: repo, Issue: issue, DefaultBranch: "main", Head: f.first}
	return f
}

// commit commits body as notes.txt on origin's current branch and answers the commit.
func (f reviewFixture) commit(t *testing.T, body string) string {
	t.Helper()
	writeFiles(t, f.origin, map[string]string{"notes.txt": body})
	f.setup.Run(f.origin, f.git, "add", "--all")
	f.setup.Run(f.origin, f.git, "commit", "--quiet", "--message", body)
	return f.setup.Run(f.origin, f.git, "rev-parse", "HEAD")
}

// The review clone holds objects of its own: no file under its object store is a hard link to a
// file of the shared clone's, so no role that writes the shared clone's object files in place
// writes what the reviewer's git reads, and it names no other object store. An
// objects/info/alternates that another role plants in the shared clone, naming a store that role
// writes, does not reach the review clone, whose objects come through git's transport and not as
// a copy of the shared clone's files; and a review clone that holds such a file (on the tmux
// runtime any pane can write one there) is removed and made again, with a log line naming the
// file and the store it names, rather than handed to a reviewer whose git would read objects from
// that store, or left to refuse every start of the reviewer after.
func TestAReviewCloneSharesNoObjectFileWithTheSharedClone(t *testing.T) {
	f := newReviewFixture(t)
	planted := filepath.Join(filepath.Dir(f.origin), "planted")
	initPlanted := fixturegit.InitWorkTree(planted, "main")
	initPlanted[0] = f.git
	f.setup.Run(filepath.Dir(f.origin), initPlanted...)
	plantedObjects := filepath.Join(planted, ".git", "objects")
	writeFiles(t, f.request.Clone, map[string]string{".git/objects/info/alternates": plantedObjects + "\n"})
	run := NewRunner(testTimeout, map[string]string{"git": f.git, "jj": f.jj})
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("ProvisionReviewClone: %v", err)
	}
	alternates := filepath.Join(f.request.Dir, ".git", "objects", "info", "alternates")
	if body, err := os.ReadFile(alternates); !os.IsNotExist(err) {
		t.Errorf("the review clone holds %s = %q (%v), want none: the shared clone's planted store %s reached it", alternates, body, err, plantedObjects)
	}
	files := 0
	err := filepath.WalkDir(filepath.Join(f.request.Dir, ".git", "objects"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		files++
		if links := info.Sys().(*syscall.Stat_t).Nlink; links != 1 {
			t.Errorf("the review clone's object file %s has %d links, want 1: it is shared with another clone", path, links)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("the review clone at %s holds no object file, so this row read nothing", f.request.Dir)
	}

	writeFiles(t, f.request.Dir, map[string]string{".git/objects/info/alternates": plantedObjects + "\n"})
	var logged []string
	f.request.Log = func(line string) { logged = append(logged, line) }
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("a provision of a review clone that holds %s: %v, want the clone made again", alternates, err)
	}
	if body, err := os.ReadFile(alternates); !os.IsNotExist(err) {
		t.Errorf("after the provision the review clone still holds %s = %q (%v), want it made again without one", alternates, body, err)
	}
	if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != f.first {
		t.Errorf("the review clone made again stands on %s, want the head given %s", at, f.first)
	}
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.Contains(line, alternates) && strings.Contains(line, plantedObjects)
	}) {
		t.Errorf("the provision logged %q, want a line naming the planted %s and the store %s it names", logged, alternates, plantedObjects)
	}
}

// skipJJNew is a runner that answers every `jj new` as done without running it, as a refresh that
// failed to move the working copy and said nothing would: the clone stays where it was.
type skipJJNew struct{ Runner }

func (r skipJJNew) Run(ctx context.Context, command Command) (Result, error) {
	if len(command.Argv) > 1 && filepath.Base(command.Argv[0]) == "jj" && slices.Contains(command.Argv, "new") {
		return Result{}, nil
	}
	return r.Runner.Run(ctx, command)
}

// A review clone whose checked-out HEAD is not the head the daemon gives the reviewer is refused,
// naming both commits, rather than handed to a reviewer that would read another commit.
func TestAReviewCloneLeftAtAnotherCommitIsRefused(t *testing.T) {
	f := newReviewFixture(t)
	run := NewRunner(testTimeout, map[string]string{"git": f.git, "jj": f.jj})
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("the first provision: %v", err)
	}
	second := f.commit(t, "base\nfirst\nsecond\n")
	f.setup.Run(f.request.Clone, f.git, "fetch", "--quiet", "origin")
	f.request.Head = second
	err := ProvisionReviewClone(context.Background(), skipJJNew{run}, f.request)
	if err == nil || !strings.Contains(err.Error(), f.first) || !strings.Contains(err.Error(), second) {
		t.Fatalf("a provision that left the clone at %s rather than %s = %v, want a refusal naming both", f.first, second, err)
	}
}

// The review clone stands on the head the daemon gives the reviewer (ReviewRequest.Head, the pull
// request's head the daemon records), never on what the shared clone records for the issue's
// branch, which any role that writes the shared clone can move and which goes stale when the
// branch moves at origin without the shared clone's fetch. A head the shared clone holds is
// checked out, fetched by its id where no branch the shared clone records reaches it, whatever the
// shared clone's branch names; with no origin to fetch from (a zero ReviewRequest.Origin), a head
// it does not hold is refused, naming the head and the commit the shared clone records for the
// branch, on a fresh clone and on one already there; and a head that is not a full commit id is
// refused before git reads it.
func TestAReviewCloneStandsOnTheHeadTheDaemonGives(t *testing.T) {
	f := newReviewFixture(t)
	run := NewRunner(testTimeout, map[string]string{"git": f.git, "jj": f.jj})
	second := f.commit(t, "base\nfirst\nsecond\n")

	f.request.Head = second
	if err := ProvisionReviewClone(context.Background(), run, f.request); err == nil || !strings.Contains(err.Error(), second) || !strings.Contains(err.Error(), f.first) {
		t.Fatalf("a first provision at %s, which the shared clone does not hold (its branch at %s) = %v, want a refusal naming both", second, f.first, err)
	}
	if _, err := os.Lstat(f.request.Dir); !os.IsNotExist(err) {
		t.Fatalf("a refused first provision left %s: %v", f.request.Dir, err)
	}

	// Another role moves the shared clone's record of the issue's branch back to the base.
	f.setup.Run(f.request.Clone, f.git, "update-ref", "refs/remotes/origin/"+Bookmark("ACME-4"), f.base)
	f.request.Head = f.first
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("a provision at %s with the shared clone's branch moved to %s: %v", f.first, f.base, err)
	}
	if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != f.first {
		t.Fatalf("with the shared clone's branch moved to %s, the review clone stands on %s, want the head given %s", f.base, at, f.first)
	}

	f.request.Head = second
	if err := ProvisionReviewClone(context.Background(), run, f.request); err == nil || !strings.Contains(err.Error(), second) || !strings.Contains(err.Error(), f.base) {
		t.Fatalf("a refresh at %s, which the shared clone does not hold (its branch at %s) = %v, want a refusal naming both", second, f.base, err)
	}
	if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != f.first {
		t.Fatalf("a refused refresh moved the review clone to %s, want it left on %s", at, f.first)
	}

	// The shared clone fetches the head, then another role moves its record of the branch back to
	// the base again, so no branch the review clone fetches reaches the head.
	f.setup.Run(f.request.Clone, f.git, "fetch", "--quiet", "origin")
	f.setup.Run(f.request.Clone, f.git, "update-ref", "refs/remotes/origin/"+Bookmark("ACME-4"), f.base)
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("a refresh at %s once the shared clone holds it: %v", second, err)
	}
	if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != second {
		t.Fatalf("the review clone stands on %s, want the head given %s", at, second)
	}

	for _, head := range []string{"main", "--upload-pack=touch", second[:12], strings.ToUpper(second)} {
		f.request.Head = head
		if err := ProvisionReviewClone(context.Background(), run, f.request); err == nil || !strings.Contains(err.Error(), "full commit id") {
			t.Errorf("a provision at head %q = %v, want a refusal of a head that is no full commit id", head, err)
		}
	}
}

// Where the shared clone does not hold the head the daemon gives, the provision fetches that
// commit from origin — the URL the request names, the issue's recorded repository — straight into
// a review clone it has just made, with the credential the request names: the tmux daemon's read
// lease, asked only when that fetch runs and removed after it, or a pod reviewer's own GitHub
// credential helper. A clone already there is made again for it, with a log line, so the fetch
// reads only a configuration the provision has just made, and the reviewer starts on the head. The
// fetch reads nothing of the shared clone: its url.<base>.insteadOf (for GitHub, and for this
// test's local origin), its http.proxy and its remote.origin.url, each another role's plant,
// change nothing, and the shared clone is not written. Only a head origin does not hold either is
// refused, naming it and the commit the shared clone records for the issue's branch; and an
// origin URL that is neither GitHub over https nor a local repository is never fetched from.
func TestAReviewCloneFetchesAHeadTheSharedCloneLacksFromOrigin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lease bool
	}{{"the daemon's read lease", true}, {"the reviewer's own credential helper", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			run := NewRunner(testTimeout, map[string]string{"git": f.git, "jj": f.jj})
			credentials := t.TempDir()
			minted := 0
			f.request.Origin = OriginWithHelper(f.origin)
			if tc.lease {
				f.request.Origin = OriginWithToken(f.origin, func() (string, error) { minted++; return "read-lease", nil }, credentials)
			}
			var logged []string
			f.request.Log = func(line string) { logged = append(logged, line) }
			if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
				t.Fatalf("a provision at %s, which the shared clone holds: %v", f.first, err)
			}
			if minted != 0 {
				t.Errorf("a provision at a head the shared clone holds asked for the read lease %d times, want none", minted)
			}
			holds := func(commit string) bool {
				_, err := f.setup.Try(f.request.Clone, f.git, "cat-file", "-e", commit+"^{commit}")
				return err == nil
			}

			// Another role plants configuration in the shared clone that would send a fetch, and
			// the credential it carries, elsewhere: GitHub, and this test's origin, rewritten to a
			// port nothing serves, a proxy there, and its origin pointed there too.
			for _, setting := range [][2]string{
				{"url.https://127.0.0.1:1/.insteadOf", "https://github.com/"},
				{"url.https://127.0.0.1:1/origin.insteadOf", f.origin},
				{"http.proxy", "http://127.0.0.1:1"},
			} {
				f.setup.Run(f.request.Clone, f.git, "config", "--add", setting[0], setting[1])
			}
			f.setup.Run(f.request.Clone, f.git, "remote", "set-url", "origin", "https://127.0.0.1:1/planted")

			// The branch moves at origin, and the shared clone does not fetch it.
			second := f.commit(t, "base\nfirst\nsecond\n")
			f.request.Head = second
			if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
				t.Fatalf("a refresh at %s, which the shared clone lacks and origin holds: %v", second, err)
			}
			if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != second {
				t.Fatalf("the review clone stands on %s, want the head given %s", at, second)
			}
			if holds(second) {
				t.Errorf("the shared clone holds %s, want it untouched: the fetch from origin goes into the review clone", second)
			}
			if !slices.ContainsFunc(logged, func(line string) bool {
				return strings.Contains(line, "made again") && strings.Contains(line, f.origin)
			}) {
				t.Errorf("the provision logged %q, want a line saying the clone is made again to fetch from %s", logged, f.origin)
			}
			if tc.lease && minted != 1 {
				t.Errorf("the fetch from origin asked for the read lease %d times, want once", minted)
			}
			if left, err := os.ReadDir(credentials); err != nil || len(left) != 0 {
				t.Errorf("the credential directory holds %v (%v) after the fetch, want nothing", left, err)
			}

			// A fresh clone, at a head the shared clone lacks again.
			if err := RemoveReviewClone(f.request.Dir); err != nil {
				t.Fatal(err)
			}
			third := f.commit(t, "base\nfirst\nsecond\nthird\n")
			f.request.Head = third
			if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
				t.Fatalf("a first provision at %s, which the shared clone lacks and origin holds: %v", third, err)
			}
			if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != third {
				t.Fatalf("the fresh review clone stands on %s, want the head given %s", at, third)
			}

			nowhere := strings.Repeat("ab", 20)
			f.request.Head = nowhere
			if err := ProvisionReviewClone(context.Background(), run, f.request); err == nil || !strings.Contains(err.Error(), nowhere) || !strings.Contains(err.Error(), f.first) {
				t.Fatalf("a provision at %s, which origin does not hold either (the shared clone's branch at %s) = %v, want a refusal naming both", nowhere, f.first, err)
			}

			// An origin URL that names a command.
			marker := filepath.Join(t.TempDir(), "ran")
			command := "ext::sh -c touch% " + marker
			fourth := f.commit(t, "base\nfirst\nsecond\nthird\nfourth\n")
			named := f.request
			named.Head = fourth
			named.Origin = OriginWithHelper(command)
			if err := ProvisionReviewClone(context.Background(), run, named); err == nil || !strings.Contains(err.Error(), fourth) || !strings.Contains(err.Error(), "ext::") {
				t.Fatalf("a provision at %s from origin %q = %v, want a refusal naming the head and that origin", fourth, command, err)
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Fatalf("the origin's command ran: %s exists (%v)", marker, err)
			}
		})
	}
}

// A pod reviewer's clone fetches a head the shared clone lacks from GitHub with the reviewer's own
// credential helper (OriginWithHelper): git asks `legion git-credential`, here a stand-in first on
// PATH that answers as the reviewer's gh bundle does, for https://github.com and the repository's
// path, and presents the token it answers. The stand-in GitHub takes no request without that
// token, so a fetch that does not ask the helper, or asks it for no path, cannot have the head, and
// the provision is refused.
func TestAPodReviewersCloneFetchesAHeadTheSharedCloneLacksWithItsOwnCredentialHelper(t *testing.T) {
	f := newReviewFixture(t)
	const token = "reviewer-helper-token"
	github := newGitHubStandIn(t, false, token)
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "legion"), `#!/bin/sh
[ "$1 $2" = "git-credential get" ] || exit 0
request=$(cat)
printf '%s\n' "$request" | grep -qx 'host=github.com' || exit 0
printf '%s\n' "$request" | grep -qx 'path=acme/widgets' || exit 0
printf 'username=x-access-token\npassword=%s\n' '`+token+`'
`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	// The branch moves at GitHub, and the shared clone does not fetch it.
	second := f.commit(t, "base\nfirst\nsecond\n")
	f.setup.Run(f.origin, f.git, "push", "--quiet", github.remote, "HEAD:refs/heads/"+Bookmark("ACME-4"))
	if _, err := f.setup.Try(f.request.Clone, f.git, "cat-file", "-e", second+"^{commit}"); err == nil {
		t.Fatalf("the shared clone holds %s, so this test would fetch nothing from GitHub", second)
	}
	f.request.Head = second
	f.request.Origin = OriginWithHelper(GitHubURL(f.request.Repo))
	run := NewRunner(testTimeout, map[string]string{"git": f.git, "jj": f.jj})
	if err := ProvisionReviewClone(context.Background(), run, f.request); err != nil {
		t.Fatalf("a provision at %s, which the shared clone lacks and GitHub holds: %v", second, err)
	}
	if at := f.setup.Run(f.request.Dir, f.git, "rev-parse", "HEAD"); at != second {
		t.Fatalf("the review clone stands on %s, want the head given %s", at, second)
	}
	github.tokensPresented(t, token)
}

// The request a pod reviewer's role-init makes fetches a head the shared clone lacks from GitHub
// at the repository the layout names, the issue's recorded repository, with the reviewer's own
// credential helper, and never from a URL the shared clone's configuration names.
func TestAPodReviewersRequestFetchesFromTheRecordedRepository(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	review, err := ReviewLocation("/legion", repo, "ACME-3")
	if err != nil {
		t.Fatal(err)
	}
	request, err := ReviewRequestFor(review, "ACME-3", "main", strings.Repeat("ab", 20))
	if err != nil {
		t.Fatal(err)
	}
	if request.Origin.IsZero() || request.Origin.URL != GitHubURL(repo) {
		t.Fatalf("the pod reviewer's request fetches from %q (zero: %v), want %s", request.Origin.URL, request.Origin.IsZero(), GitHubURL(repo))
	}
}

// A review clone that is already there is brought to the head at every provision, from the shared
// clone's objects and with no network, as a fresh clone would stand: a reviewer started on it
// reviews the head of now, not the one its clone was first made at. The request gives no head, as
// a daemon that records no pull request head for the issue gives none, so the head is the one the
// shared clone records. What
// the reviewer wrote in it before stays in a change of its own, and the working copy holds the new
// head's tree; a lock a reviewer's git killed mid-write left in the clone does not stop it, and
// a temporary clone a killed attempt left beside it is gone with the next one. Once origin no
// longer holds the issue's branch and the shared clone has pruned it, the clone stands on the
// default branch instead, never on the branch's stale head; and with neither branch at origin,
// the provision is refused, naming both.
func TestAReviewCloneIsBroughtToTheCurrentHeadAtEachProvision(t *testing.T) {
	git := fixturegit.CompiledGit(t)
	jj := realTool(t, "jj")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, entry := range fixtureenv.Home(home) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	t.Setenv("JJ_USER", "Legion test")
	t.Setenv("JJ_EMAIL", "legion-test@example.invalid")
	setup := fixturegit.Isolated(t, home).With("PATH=" + filepath.Dir(git) + ":/usr/bin:/bin")
	repo := ghrepo.MustParse("acme/widgets")
	const issue = "ACME-2"
	state := filepath.Join(root, "state")
	origin := filepath.Join(root, "origin")
	initOrigin := fixturegit.InitWorkTree(origin, "main")
	initOrigin[0] = git
	setup.Run(root, initOrigin...)
	commit := func(body, message string) string {
		t.Helper()
		writeFiles(t, origin, map[string]string{"notes.txt": body})
		setup.Run(origin, git, "add", "--all")
		setup.Run(origin, git, "commit", "--quiet", "--message", message)
		return setup.Run(origin, git, "rev-parse", "HEAD")
	}
	base := commit("base\n", "base")
	setup.Run(origin, git, "checkout", "--quiet", "-b", Bookmark(issue))
	first := commit("base\nfirst\n", "first head")
	located, err := Location(state, repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	setup.Run(root, git, "clone", "--quiet", "--branch", Bookmark(issue), origin, located.Clone)
	review, err := ReviewLocation(state, repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := review + ".review-killed"
	writeFiles(t, abandoned, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	run := NewRunner(testTimeout, map[string]string{"git": git, "jj": jj})
	request := ReviewRequest{Clone: located.Clone, Dir: review, Repo: repo, Issue: issue, DefaultBranch: "main"}
	if err := ProvisionReviewClone(context.Background(), run, request); err != nil {
		t.Fatalf("the first provision: %v", err)
	}
	if _, err := os.Lstat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("the temporary clone a killed attempt left at %s is still there: %v", abandoned, err)
	}
	if at := setup.Run(review, git, "rev-parse", "HEAD"); at != first {
		t.Fatalf("the review clone stands on %s, want the first head %s", at, first)
	}

	// The branch moves at origin, the shared clone fetches it, the reviewer has written a file of
	// its own in its clone since, and its git was killed holding the lock of the branch's ref.
	second := commit("base\nfirst\nsecond\n", "second head")
	setup.Run(located.Clone, git, "fetch", "--quiet", "origin")
	writeFiles(t, review, map[string]string{
		"reviewer-scratch.txt": "notes\n",
		".git/refs/remotes/origin/" + Bookmark(issue) + ".lock": "",
	})
	if err := ProvisionReviewClone(context.Background(), run, request); err != nil {
		t.Fatalf("the second provision: %v", err)
	}
	if at := setup.Run(review, git, "rev-parse", "HEAD"); at != second {
		t.Fatalf("after the shared clone moved, the review clone stands on %s, want the second head %s", at, second)
	}
	if body, err := os.ReadFile(filepath.Join(review, "notes.txt")); err != nil || string(body) != "base\nfirst\nsecond\n" {
		t.Fatalf("the review clone's working copy holds notes.txt = %q (%v), want the second head's", body, err)
	}
	if empty := setup.Run(review, jj, "log", "-R", review, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "empty"); empty != "true" {
		t.Fatalf("the review clone's working copy is not an empty change on the head (empty=%s)", empty)
	}
	if kept := setup.Run(review, jj, "log", "-R", review, "--ignore-working-copy", "-r", `files("reviewer-scratch.txt") & ~@`, "--no-graph", "-T", `"kept"`); kept != "kept" {
		t.Fatalf("the reviewer's own file is in no change of its own after the move: %q", kept)
	}

	// The issue's branch goes from origin and from the shared clone's record of it.
	setup.Run(origin, git, "checkout", "--quiet", "main")
	setup.Run(origin, git, "branch", "--quiet", "--delete", "--force", Bookmark(issue))
	setup.Run(located.Clone, git, "fetch", "--quiet", "--prune", "origin")
	if err := ProvisionReviewClone(context.Background(), run, request); err != nil {
		t.Fatalf("the provision after the branch went: %v", err)
	}
	if at := setup.Run(review, git, "rev-parse", "HEAD"); at != base {
		t.Fatalf("after the issue's branch went, the review clone stands on %s, want the default branch's %s", at, base)
	}
	// The clone's trunk() is the default branch, which its jj resolves with the issue's branch
	// gone: jj names trunk() after origin's HEAD at `jj git init`, and an alias naming the issue's
	// branch fails every jj command once that branch goes (jj 0.46 resolves it for the short
	// prefixes of each commit it prints, so the provision's own `jj new` fails too).
	if trunk := setup.Run(review, jj, "log", "-R", review, "--ignore-working-copy", "-r", "trunk()", "--no-graph", "-T", "commit_id"); trunk != base {
		t.Fatalf("the review clone's trunk() is %q after the issue's branch went, want the default branch's %s", trunk, base)
	}
	request.DefaultBranch = "release/v7"
	if err := ProvisionReviewClone(context.Background(), run, request); err == nil || !strings.Contains(err.Error(), Bookmark(issue)) || !strings.Contains(err.Error(), "release/v7") {
		t.Fatalf("a provision with neither branch at origin = %v, want a refusal naming both", err)
	}
}

// IssueWorkspace answers the issue's own workspace for a reviewer's clone, and any other directory
// as it is: the launcher's sweep and restore and the shim's bundle work the issue's workspace
// whichever role's LEGION_WORKSPACE they read.
func TestIssueWorkspaceIsTheWorkspaceOfAReviewClonesIssue(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	located, err := Location("/legion", repo, "ACME-3")
	if err != nil {
		t.Fatal(err)
	}
	review, err := ReviewLocation("/legion", repo, "ACME-3")
	if err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		review:                                located.Dir,
		located.Dir:                           located.Dir,
		"":                                    "",
		"/legion/reviews/acme/widgets":        "/legion/reviews/acme/widgets",
		"/elsewhere/reviews/acme/w/acme-3":    "/elsewhere/workspaces/acme/w/acme-3",
		"/legion/reviews/acme/widgets/ACME-3": "/legion/reviews/acme/widgets/ACME-3",
	} {
		if got := IssueWorkspace(dir); got != want {
			t.Errorf("IssueWorkspace(%q) = %q, want %q", dir, got, want)
		}
	}
}
