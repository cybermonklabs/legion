package leakguard

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// notesRefused is the file the pushes of these tests add, holding a line the fixture rules
// refuse, and notesReport the report of it.
func notesRefused() map[string]string {
	return map[string]string{"notes.md": "the runner answers at " + leakguardtest.RulesHost + "\n"}
}

var notesReport = []string{"notes.md: " + leakguardtest.RulesHostClass}

// wantRefused fails the test unless err is the rules' refusal whose report is a line of each of
// want (reports), or, where want is nil, unless the push passed.
func wantRefused(t *testing.T, err error, want []string) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("got %v, want the push passed", err)
		}
		return
	}
	reports(t, refusal(t, err).Lines, want...)
}

// The reads of what the destination holds run in the repository the hook runs in, as the push's
// own gits run (Guard.heldRead), so a URL that is a relative local path is read from there, where
// git read it for the push. The destination beside the clone holds the commit the push adds at
// its main: both reads answer it, the negotiation's and the advertised read's, and the push of a
// new main to it, whose commit adds a line the rules refuse, passes, since the destination
// already holds that commit.
func TestTheReadsOfARelativeLocalURLAreMadeAgainstTheRepository(t *testing.T) {
	const url = "../destination.git"
	repo, _, head, _ := pushOf(t, notesRefused())
	root := filepath.Dir(repo)
	fixture := fixturegit.Isolated(t, t.TempDir())
	fixture.Run(root, fixturegit.InitBare(filepath.Join(root, "destination.git"), "main")...)
	fixture.Run(repo, "git", "push", "--quiet", url, head+":refs/heads/main")
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	not, negotiated, err := guard.destinationHolds(bound(t), repo, url, []string{head})
	if err != nil || !negotiated {
		t.Errorf("the reads of %s answered %v, negotiated %t; want both answered", url, err, negotiated)
	}
	if want := "^" + head + "\n^" + head + "\n"; not != want {
		t.Errorf("the destination is taken to hold %q, want the negotiation's and the advertised read's %q", not, want)
	}
	err = guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     url,
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	})
	wantRefused(t, err, nil)
}

// A url.<base>.insteadOf of the push's configuration that rewrites the hook's URL keeps the
// base's bound: the guard makes neither read of what the destination holds
// (readsReachItsRepository), and the push is judged as it was before the reads (R10 of
// dispatch://INFRA-3524's spec, the eleventh case of its check c10). git hands the hook the URL
// its rewrites have already led the push to, and the reads, which run with the push's
// configuration, would apply the rewrite again and answer for the repository it names. Each case
// puts, in one place the push reads its configuration from, a url.<stand-in>.insteadOf whose
// value is the destination's path, and first shows that the rewrite is real, or the case would
// prove nothing: git ls-remote of the destination under that configuration lists the stand-in's
// main. The stand-in holds the commit the push adds, a line the rules refuse, and the destination,
// which the push is made to by its path, holds only the commit before it. Reads the rewrite led to
// the stand-in would take that commit as held and pass the push unread; the guard reads it and
// refuses the push, as the base does.
//
// The places are the repository's own file; the global file GIT_CONFIG_GLOBAL names; the system
// file GIT_CONFIG_SYSTEM names where GIT_CONFIG_NOSYSTEM=0 lets git read one; a GIT_CONFIG_COUNT
// block; GIT_CONFIG_PARAMETERS; the user's own files, ~/.gitconfig and
// $XDG_CONFIG_HOME/git/config, which git reads where no GIT_CONFIG_GLOBAL names another file; and
// the repository's own file reached through a GIT_DIR naming the clone, as git hands one to the
// hook of a push made with `git --git-dir <dir>`, which is how `jj git push` makes one, from a
// directory that is not the clone.
func TestAnInsteadOfThatRewritesTheHooksURLKeepsTheBasesBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		// env is what the case adds to the environment to set key to value: repo is the clone,
		// dir is a directory of the case's own, and config writes that one setting to the
		// configuration file at a path and answers the path.
		env func(repo, dir string, config func(file string) string, key, value string) []string
		// elsewhere is whether the hook runs in a directory that is not the clone.
		elsewhere bool
	}{
		{
			name: "the repository's own file",
			env: func(repo, _ string, config func(string) string, _, _ string) []string {
				config(filepath.Join(repo, ".git", "config"))
				return nil
			},
		},
		{
			name: "the global file GIT_CONFIG_GLOBAL names",
			env: func(_, dir string, config func(string) string, _, _ string) []string {
				return []string{"GIT_CONFIG_GLOBAL=" + config(filepath.Join(dir, "gitconfig"))}
			},
		},
		{
			name: "the system file GIT_CONFIG_SYSTEM names, under GIT_CONFIG_NOSYSTEM=0",
			env: func(_, dir string, config func(string) string, _, _ string) []string {
				return []string{"GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM=" + config(filepath.Join(dir, "gitconfig"))}
			},
		},
		{
			name: "a GIT_CONFIG_COUNT block",
			env: func(_, _ string, _ func(string) string, key, value string) []string {
				return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + key, "GIT_CONFIG_VALUE_0=" + value}
			},
		},
		{
			// git hands the git it starts for `git -c <key>=<value>` GIT_CONFIG_PARAMETERS in this
			// form, each half quoted for sh: measured with git 2.47.3.
			name: "GIT_CONFIG_PARAMETERS",
			env: func(_, _ string, _ func(string) string, key, value string) []string {
				return []string{"GIT_CONFIG_PARAMETERS=" + shQuote(key) + "=" + shQuote(value)}
			},
		},
		{
			name: "the user's file under HOME",
			env: func(_, dir string, config func(string) string, _, _ string) []string {
				config(filepath.Join(dir, ".gitconfig"))
				return []string{"HOME=" + dir}
			},
		},
		{
			name: "the user's file under XDG_CONFIG_HOME",
			env: func(_, dir string, config func(string) string, _, _ string) []string {
				config(filepath.Join(dir, "git", "config"))
				return []string{"XDG_CONFIG_HOME=" + dir}
			},
		},
		{
			name: "the repository's own file through a GIT_DIR naming the clone, as a push of jj's hands it",
			env: func(repo, _ string, config func(string) string, _, _ string) []string {
				config(filepath.Join(repo, ".git", "config"))
				return []string{"GIT_DIR=" + filepath.Join(repo, ".git")}
			},
			elsewhere: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, notesRefused())
			fixture := fixturegit.Isolated(t, t.TempDir())
			standIn := filepath.Join(t.TempDir(), "stand-in.git")
			fixture.Run(repo, fixturegit.InitBare(standIn, "main")...)
			fixture.Run(repo, "git", "push", "--quiet", standIn, head+":refs/heads/main")
			key := "url." + standIn + ".insteadOf"
			config := func(file string) string {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				fixture.Run(filepath.Dir(file), "git", "config", "--file", file, key, destination)
				return file
			}
			// The environment is fixtureenv.Isolated's without its GIT_CONFIG_GLOBAL=/dev/null, as
			// a host's is, so that git reads the user's files there, and the case's own names on
			// top. The control ls-remote runs under it whole, in the directory the hook runs in.
			env := append(slices.DeleteFunc(fixtureenv.Isolated(t.TempDir()), func(entry string) bool {
				return strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=")
			}), tc.env(repo, t.TempDir(), config, key, destination)...)
			hook := repo
			if tc.elsewhere {
				hook = t.TempDir()
			}
			control := fixturegit.Runner{TB: t, Env: env}
			if listed := control.Out(hook, "git", "ls-remote", "--heads", destination); !strings.HasPrefix(listed, head) {
				t.Fatalf("git ls-remote of %s under the case's configuration lists %q, want the stand-in's main at %s: the case would prove nothing", destination, listed, head)
			}
			guard := New(Options{Check: leakguardtest.Write(t, t.TempDir(), passes), Visibility: answers(Public, nil), Env: withRules(t, env)})
			push := mainPush(repo, base, head, "origin", destination)
			push.Dir = hook
			wantRefused(t, guard.Push(bound(t), push), notesReport)
		})
	}
}

// A push whose URL is also the name of a remote of the repository keeps the base's bound: the
// guard makes neither read of what the destination holds (readsReachItsRepository,
// urlNamesARemote), and the push is judged as it was before the reads. git ls-remote and git
// fetch read their repository argument as a remote's name first, so reads of the hook's URL would
// read the remote of that name, which need not be the repository the push writes to. Each case's
// destination, which the push is made to by its URL, holds only the commit before the one the push
// adds, and a stand-in holds that commit, which adds a line the rules refuse. A remote named the
// URL leads a read of it to the stand-in, and the case first shows that the read really goes
// there, or it would prove nothing: git ls-remote of the URL in the clone lists the stand-in's
// main. Reads that went there would take the pushed commit as held and pass the push, which would
// land it in the destination; the guard reads it and refuses the push, as the base does.
//
// The remotes: one named the URL file://<destination>, with a url of the stand-in's; one of the
// same name with no url and an uploadpack, `git-upload-pack '<stand-in>' #`, which git runs for a
// read of it in place of the destination's; and, for the URL destination.git, a bare repository's
// relative path in the clone's directory and so a name with no `/`, a legacy
// remotes/destination.git file and a legacy branches/destination.git file, each naming the
// stand-in.
func TestAPushWhoseURLIsTheNameOfARemoteKeepsTheBasesBound(t *testing.T) {
	// relative is the destination of the legacy files' cases: a bare repository in the clone's
	// directory that holds base, named by its relative path, which holds no `/`.
	relative := func(t *testing.T, fixture fixturegit.Runner, repo, base string) string {
		t.Helper()
		const url = "destination.git"
		fixture.Run(repo, fixturegit.InitBare(filepath.Join(repo, url), "main")...)
		fixture.Run(repo, "git", "push", "--quiet", url, base+":refs/heads/main")
		return url
	}
	// legacy writes content to the clone's legacy file of kind, remotes or branches, for name.
	legacy := func(t *testing.T, repo, kind, name, content string) {
		t.Helper()
		dir := filepath.Join(repo, ".git", kind)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// remote makes the case's remote named the URL, which leads a read of it to standIn, and
		// answers the URL: destination is a bare repository that holds base, which the case may
		// use.
		remote func(t *testing.T, fixture fixturegit.Runner, repo, base, destination, standIn string) string
	}{
		{
			name: "a remote named the URL, with a url",
			remote: func(_ *testing.T, fixture fixturegit.Runner, repo, _, destination, standIn string) string {
				url := "file://" + destination
				fixture.Run(repo, "git", "config", "remote."+url+".url", standIn)
				return url
			},
		},
		{
			name: "a remote named the URL, with an uploadpack",
			remote: func(_ *testing.T, fixture fixturegit.Runner, repo, _, destination, standIn string) string {
				url := "file://" + destination
				fixture.Run(repo, "git", "config", "remote."+url+".uploadpack", "git-upload-pack "+shQuote(standIn)+" #")
				return url
			},
		},
		{
			name: "a legacy remotes file named the URL",
			remote: func(t *testing.T, fixture fixturegit.Runner, repo, base, _, standIn string) string {
				url := relative(t, fixture, repo, base)
				legacy(t, repo, "remotes", url, "URL: "+standIn+"\n")
				return url
			},
		},
		{
			name: "a legacy branches file named the URL",
			remote: func(t *testing.T, fixture fixturegit.Runner, repo, base, _, standIn string) string {
				url := relative(t, fixture, repo, base)
				legacy(t, repo, "branches", url, standIn+"\n")
				return url
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, notesRefused())
			fixture := fixturegit.Isolated(t, t.TempDir())
			standIn := filepath.Join(t.TempDir(), "stand-in.git")
			fixture.Run(repo, fixturegit.InitBare(standIn, "main")...)
			fixture.Run(repo, "git", "push", "--quiet", standIn, head+":refs/heads/main")
			url := tc.remote(t, fixture, repo, base, destination, standIn)
			if listed, want := fixture.Out(repo, "git", "ls-remote", "--heads", "--", url), head+"\trefs/heads/main\n"; listed != want {
				t.Fatalf("git ls-remote of %s in the clone lists %q, want the stand-in's main %q: the case would prove nothing", url, listed, want)
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", url))
			if err == nil {
				t.Fatalf("the push to %s passed, which would land the commit the rules refuse there: the guard read the remote named %s, the stand-in, and took that commit as held", url, url)
			}
			reports(t, refusal(t, err).Lines, notesReport...)
		})
	}
}

// A line the destination's ls-remote prints whose first field is no object id as git prints one,
// 40 or 64 lower-case hex digits, holds nothing (Guard.advertised, isObjectID). The batch check
// that asks which of those shas the clone has as commits runs in the clone, and cat-file reads a
// revision name or expression there as the clone's own, and an abbreviated or upper-case sha too:
// each of these fields names the commit the push adds, which a read that asked of it would take
// as one the destination holds, passing the push unread. The stand-in git here answers the
// ls-remote with one line, the case's field at refs/heads/main, and runs the real git for every
// other call. The commit adds a line the rules refuse, so the push is read and refused.
func TestALineOfTheDestinationsRefsWhoseFieldIsNoObjectIDHoldsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// field is the line's first field, of head, the commit the push adds.
		field func(head string) string
	}{
		{name: "a revision name", field: func(string) string { return "HEAD" }},
		{name: "a revision expression", field: func(string) string { return "main~0" }},
		{name: "an abbreviated sha", field: func(head string) string { return head[:12] }},
		{name: "an upper-case sha", field: strings.ToUpper},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, notesRefused())
			bin := t.TempDir()
			writeProgram(t, bin, "git", `case " $* " in
*" ls-remote "*) printf '%s\trefs/heads/main\n' `+shQuote(tc.field(head))+`; exit 0 ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, notesReport)
		})
	}
}

// The read of what the destination holds fetches nothing. In a partial clone, as an issue pod's
// fork clones are (--filter=blob:none), git fetches an object the clone lacks from the promisor
// remote the moment anything asks for it. The read of the destination's refs asks for every
// commit they are at (Guard.advertised), and the read of the commits an update adds asks for the
// one its remote side names (Guard.holds): a commit the clone lacks is answered missing instead,
// and holds nothing. The promisor here has a commit the clone never fetched, which the destination
// holds at its branch later; once the push is read, the clone still lacks it, and the push's own
// commit is read and refused. The push moves main, whose remote side is a commit the clone has,
// or later itself, whose remote side is the commit the clone lacks.
func TestTheReadOfWhatTheDestinationHoldsFetchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// ref is the ref the push moves, and atLater is whether its remote side is the commit the
		// clone lacks rather than the base.
		ref     string
		atLater bool
	}{
		{name: "a branch of the destination's", ref: "refs/heads/main"},
		{name: "the commit the update's remote side names", ref: "refs/heads/later", atLater: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			promisor, destination := filepath.Join(root, "promisor.git"), filepath.Join(root, "destination.git")
			seed, repo := filepath.Join(root, "seed"), filepath.Join(root, "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(root, fixturegit.InitBare(promisor, "main")...)
			fixture.Run(promisor, "git", "config", "uploadpack.allowFilter", "true")
			fixture.Run(promisor, "git", "config", "uploadpack.allowAnySHA1InWant", "true")
			fixture.Run(root, fixturegit.InitBare(destination, "main")...)
			fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
			base := commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
			fixture.Run(seed, "git", "push", "--quiet", promisor, "main")
			fixture.Run(root, "git", "clone", "--quiet", "--filter=blob:none", "file://"+promisor, repo)
			later := commitFiles(t, fixture, seed, map[string]string{"README.md": "one\ntwo\n"}, "a commit the clone never fetched")
			fixture.Run(seed, "git", "push", "--quiet", promisor, later+":refs/heads/later")
			fixture.Run(seed, "git", "push", "--quiet", destination, base+":refs/heads/main", later+":refs/heads/later")
			// lacks is whether the clone lacks the later commit, asked so that the asking fetches
			// nothing.
			lacks := func() bool {
				_, err := fixture.With("GIT_NO_LAZY_FETCH=1").Try(repo, "git", "cat-file", "-e", later)
				return err != nil
			}
			if !lacks() {
				t.Fatalf("the clone has %s before the push is read, so this test proves nothing", short(later))
			}
			head := commitFiles(t, fixture, repo, notesRefused(), "a message of no interest")
			remoteSHA := base
			if tc.atLater {
				remoteSHA = later
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     destination,
				Updates: []Update{{LocalRef: tc.ref, LocalSHA: head, RemoteRef: tc.ref, RemoteSHA: remoteSHA}},
			})
			wantRefused(t, err, notesReport)
			if !lacks() {
				t.Errorf("the read of the push fetched %s, the commit the destination's later branch is at, into the clone", short(later))
			}
		})
	}
}

// credentialedURL is theURL carrying an invented user and token as its userinfo, a URL of the
// shape git can hand a pre-push hook. The userinfo is built apart from the scheme, so no line of
// this file is a URL that carries one, which the shared check would refuse at the push.
const (
	credentialUser, credentialToken = "invented-user", "invented-token"
	credentialUserinfo              = credentialUser + ":" + credentialToken + "@"
)

var credentialedURL = strings.Replace(theURL, "https://", "https://"+credentialUserinfo, 1)

// wantNoCredential fails the test where err names credentialedURL's user or token.
func wantNoCredential(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), credentialUser) || strings.Contains(err.Error(), credentialToken)) {
		t.Errorf("the answer carries the URL's credential: %v", err)
	}
}

// A read of what the destination holds that does not answer ends at its own bound
// (heldReadBound), although the caller's context has no deadline, as the hook's has none: a
// destination that stops answering does not hold the push. A read past its bound counts as
// failed, which refuses nothing: the push is read as it was before the reads, here from the
// commit its remote side names, which the clone has, and its own commit, which adds a line the
// rules refuse, is refused, the URL's credential in no part of the answer. The push answers no
// sooner than the read's bound of 2 s, which the read waited out. The stand-in git here never
// answers an ls-remote, and runs the real git for every other call; the fetch negotiation with the
// URL fails at once, since the test's GIT_ALLOW_PROTOCOL lets no https through.
func TestAReadOfWhatTheDestinationHoldsThatDoesNotAnswerEndsAtItsBound(t *testing.T) {
	shorten(t, &heldReadBound, 2*time.Second)
	bin := t.TempDir()
	writeProgram(t, bin, "git", `case " $* " in
*" ls-remote "*) exec sleep 86400 ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
	repo, base, head, _ := pushOf(t, notesRefused())
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var err error
	began := time.Now()
	answered(t, "the push whose read of the destination's refs never answers", func() {
		err = guard.Push(ctx, mainPush(repo, base, head, "origin", credentialedURL))
	})
	if took := time.Since(began); took < heldReadBound {
		t.Errorf("the push answered after %s, before the read's bound of %s: the read did not wait for its bound", took, heldReadBound)
	}
	wantRefused(t, err, notesReport)
	wantNoCredential(t, err)
}

// The fetch negotiation with the destination fetches nothing and writes no ref (Guard.negotiated),
// although it runs in the clone: the destination's main moved on to a commit the clone does not
// hold, which a fetch would bring, and once the negotiation has answered the commit beneath it
// that the clone holds, the clone's refs, its FETCH_HEAD and its count of objects are what they
// were before, and it still lacks the later commit.
func TestTheNegotiationFetchesNothingAndWritesNoRef(t *testing.T) {
	repo, base, head, destination := pushOf(t, map[string]string{"notes.md": "a line of the push's own\n"})
	fixture := fixturegit.Isolated(t, t.TempDir())
	seed := filepath.Join(t.TempDir(), "seed")
	fixture.Run(filepath.Dir(seed), "git", "clone", "--quiet", destination, seed)
	later := commitFiles(t, fixture, seed, map[string]string{"later.md": "a later line\n"}, "a commit the clone does not hold")
	fixture.Run(seed, "git", "push", "--quiet", "origin", "main")
	// held is what the clone holds: its refs, its FETCH_HEAD, none where there is none, and its
	// count of objects.
	held := func() string {
		fetchHead, err := os.ReadFile(filepath.Join(repo, ".git", "FETCH_HEAD"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return fixture.Raw(repo, "git", "for-each-ref") + "FETCH_HEAD: " + string(fetchHead) + "\n" + fixture.Raw(repo, "git", "count-objects", "-v")
	}
	before := held()
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	common, err := guard.negotiated(bound(t), repo, destination, []string{head})
	if err != nil {
		t.Fatalf("the negotiation: %v", err)
	}
	if want := []string{base}; !slices.Equal(common, want) {
		t.Errorf("the negotiation answered %q, want the commit the clone and the destination hold, %q", common, want)
	}
	if after := held(); after != before {
		t.Errorf("the clone held\n%s\nbefore the negotiation and\n%s\nafter it", before, after)
	}
	if _, err := fixture.Try(repo, "git", "cat-file", "-e", later); err == nil {
		t.Errorf("the clone holds %s, the destination's later commit, after the negotiation", short(later))
	}
}

// In a shallow clone the fetch negotiation reads the clone's shallow file, as every git run in the
// clone does (Guard.negotiated), so its walk stops at the clone's shallow commit: it answers the
// commit the destination holds, says nothing on its standard error, and leaves the shallow file as
// it was. The clone holds the destination's main one commit deep. A negotiation that does not read
// the shallow file, one in a bare repository whose alternate is the clone's object directory, says
// that it could not read the shallow commit's parent, which the clone does not hold, so the
// negotiation's silence is the walk that stopped there. The stand-in git here writes what the
// negotiation says on its standard error to a file, and runs the real git.
func TestTheNegotiationInAShallowCloneReadsItsShallowFile(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	destination, seed, clone := filepath.Join(root, "destination.git"), filepath.Join(root, "seed"), filepath.Join(root, "clone")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(destination, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "the destination's first commit")
	tip := commitFiles(t, fixture, seed, map[string]string{"README.md": "one\ntwo\n"}, "the destination's main")
	fixture.Run(seed, "git", "push", "--quiet", destination, "main")
	fixture.Run(root, "git", "clone", "--quiet", "--depth=1", "file://"+destination, clone)
	own := commitFiles(t, fixture, clone, map[string]string{"notes.md": "a line of the push's own\n"}, "the push's own commit")
	shallowFile := filepath.Join(clone, ".git", "shallow")
	shallow, err := os.ReadFile(shallowFile)
	if err != nil || strings.TrimSpace(string(shallow)) != tip {
		t.Fatalf("the clone's shallow file holds %q (%v), want the destination's main %s: the case would prove nothing", shallow, err, tip)
	}
	alternate := filepath.Join(t.TempDir(), "alternate.git")
	fixture.Run(root, fixturegit.InitBare(alternate, "main")...)
	said, _ := fixture.With("GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(clone, ".git", "objects")).Try(alternate,
		"git", "fetch", "--negotiate-only", "--negotiation-tip="+own, destination)
	if !strings.Contains(said, "Could not read") {
		t.Fatalf("the negotiation without the shallow file said %q, want that it could not read the shallow commit's parent: the case would prove nothing", said)
	}
	bin, stderr := t.TempDir(), filepath.Join(t.TempDir(), "stderr")
	git := shQuote(fixturegit.CompiledGit(t))
	writeProgram(t, bin, "git", `case " $* " in
*" --negotiate-only "*) exec `+git+` "$@" 2>>`+shQuote(stderr)+` ;;
esac
exec `+git+` "$@"
`)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
	common, err := guard.negotiated(bound(t), clone, destination, []string{own})
	if err != nil {
		t.Fatalf("the negotiation: %v", err)
	}
	if want := []string{tip}; !slices.Equal(common, want) {
		t.Errorf("the negotiation answered %q, want the destination's main %q", common, want)
	}
	switch written, err := os.ReadFile(stderr); {
	case err != nil:
		t.Errorf("the negotiation's standard error was not written: %v; want the stand-in to have run it", err)
	case len(written) != 0:
		t.Errorf("the negotiation said %q on its standard error, want nothing", written)
	}
	if after, err := os.ReadFile(shallowFile); err != nil || string(after) != string(shallow) {
		t.Errorf("the clone's shallow file holds %q (%v) after the negotiation, want %q as before", after, err, shallow)
	}
}

// A fetch negotiation that does not answer ends at its own bound (heldReadBound), although the
// caller's context has no deadline, as the hook's has none, and counts as failed: what the
// destination is taken to hold is the advertised read's answer alone, and the negotiation is
// answered as one that did not answer, which leaves the remote-tracking refs to bound the push
// (destinationHolds). The answer comes no sooner than the bound of 2 s, which the negotiation
// waited out. The stand-in git here never answers a `fetch --negotiate-only`, and runs the real
// git for every other call.
func TestANegotiationThatDoesNotAnswerEndsAtItsBound(t *testing.T) {
	shorten(t, &heldReadBound, 2*time.Second)
	bin := t.TempDir()
	writeProgram(t, bin, "git", `case " $* " in
*" --negotiate-only "*) exec sleep 86400 ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
	repo, base, head, destination := pushOf(t, notesRefused())
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var not string
	var negotiated bool
	var err error
	began := time.Now()
	answered(t, "the reads whose negotiation never answers", func() {
		not, negotiated, err = guard.destinationHolds(ctx, repo, destination, []string{head})
	})
	if took := time.Since(began); took < heldReadBound {
		t.Errorf("the reads answered after %s, before the negotiation's bound of %s: the negotiation did not wait for its bound", took, heldReadBound)
	}
	if err != nil {
		t.Fatalf("the reads: %v, want a negotiation past its bound to count as failed", err)
	}
	if negotiated {
		t.Error("the negotiation counted as answered, want it failed")
	}
	if want := "^" + base + "\n"; not != want {
		t.Errorf("the destination is taken to hold %q, want the advertised read's %q alone", not, want)
	}
}

// Neither read of what the destination holds uses a credential or the REST quota
// (Guard.heldRead). The guard runs under a GIT_CONFIG_COUNT block that names a credential helper
// which logs each call, as an issue pod's names `legion git-credential`, which makes a REST read
// for an owner the bundle does not serve; the gh and curl the guard would run are stand-ins that
// log each call. Both reads of a local destination answer, and no log holds a line. A destination
// that asks for a credential, a server of the test's own on 127.0.0.1 answering every request 401
// with a challenge, reached under the row's GIT_ALLOW_PROTOCOL=http, fails both reads: the server
// is asked, and no request of either read carries an Authorization header, the helper still
// unused, although the clone's own configuration names it too, as credential.helper and as
// credential.<the URL>.helper (the twelfth case of R10's check c10). A git ls-remote of the URL in
// the clone under the guard's environment first runs the helper once for each of the three, so
// the case proves something. The push to it is judged as it was before the reads, bounded by the
// clone's remote-tracking ref of it, which holds a commit that adds a line the rules refuse: the
// push of a commit of the branch's own on it passes, and no log holds a line.
func TestTheReadsOfWhatTheDestinationHoldsUseNoCredentialAndNoRESTRead(t *testing.T) {
	bin, logs := t.TempDir(), t.TempDir()
	logged := map[string]string{}
	for _, program := range []string{"credential-helper", "gh", "curl"} {
		logged[program] = filepath.Join(logs, program)
		writeProgram(t, bin, program, `echo "$*" >> `+shQuote(logged[program])+"\nexit 1\n")
	}
	credential := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=" + filepath.Join(bin, "credential-helper")}
	unlogged := func(t *testing.T) {
		t.Helper()
		for program, log := range logged {
			if n := countLines(t, log); n != 0 {
				t.Errorf("the %s stand-in ran %d times, want none", program, n)
			}
		}
	}
	t.Run("a local destination", func(t *testing.T) {
		repo, base, head, destination := pushOf(t, notesRefused())
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, append([]string{pathWith(bin)}, credential...)...)
		guard.curl = filepath.Join(bin, "curl")
		not, negotiated, err := guard.destinationHolds(bound(t), repo, destination, []string{head})
		if err != nil || !negotiated {
			t.Errorf("the reads answered %v, negotiated %t; want both answered", err, negotiated)
		}
		if want := "^" + base + "\n^" + base + "\n"; not != want {
			t.Errorf("the destination is taken to hold %q, want the negotiation's and the advertised read's base, %q", not, want)
		}
		unlogged(t)
	})
	t.Run("a destination that asks for a credential", func(t *testing.T) {
		var mu sync.Mutex
		var requests, authorized int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests++
			if r.Header.Get("Authorization") != "" {
				authorized++
			}
			mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		url := server.URL + "/" + theRepo + ".git"
		home, root := t.TempDir(), t.TempDir()
		repo := filepath.Join(root, "repo")
		fixture := fixturegit.Isolated(t, home)
		fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
		held := commitFiles(t, fixture, repo, notesRefused(), "a commit the destination holds")
		fixture.Run(repo, "git", "remote", "add", "origin", url)
		fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", held)
		fixture.Run(repo, "git", "config", "credential.helper", filepath.Join(bin, "credential-helper"))
		fixture.Run(repo, "git", "config", "credential."+url+".helper", filepath.Join(bin, "credential-helper"))
		fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
		own := commitFiles(t, fixture, repo, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
		env := withRules(t, loopbackEnv(t.TempDir(), pathWith(bin), credential...))
		if _, err := (fixturegit.Runner{TB: t, Env: env}).Try(repo, "git", "ls-remote", url); err == nil {
			t.Fatalf("git ls-remote of %s answered, want the server's 401: the case would prove nothing", url)
		}
		if n := countLines(t, logged["credential-helper"]); n != 3 {
			t.Fatalf("git ls-remote of %s in the clone ran the credential helper %d times, want once for each of the three settings that name it: the case would prove nothing", url, n)
		}
		if err := os.Remove(logged["credential-helper"]); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requests, authorized = 0, 0
		mu.Unlock()
		guard := New(Options{
			Check: leakguardtest.Write(t, t.TempDir(), passes),
			Curl:  filepath.Join(bin, "curl"),
			Env:   env,
		})
		not, negotiated, err := guard.destinationHolds(bound(t), repo, url, []string{own})
		if err != nil || negotiated || not != "" {
			t.Errorf("the reads answered %q, negotiated %t, %v; want both failed and nothing held", not, negotiated, err)
		}
		err = guard.Push(bound(t), PushRequest{
			Dir:     repo,
			Remote:  "origin",
			URL:     url,
			Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
		})
		wantRefused(t, err, nil)
		mu.Lock()
		defer mu.Unlock()
		if requests == 0 {
			t.Error("the reads sent the server no request")
		}
		if authorized != 0 {
			t.Errorf("%d of the reads' %d requests carried an Authorization header", authorized, requests)
		}
		unlogged(t)
	})
}

// The reads of what the destination holds carry the transport settings of the push's own
// configuration (Guard.heldRead): an http.extraHeader that the push's configuration names reaches
// the destination on both reads, set once in the repository's own file and once in
// GIT_CONFIG_PARAMETERS, as `git -c` hands it the hook. No skip of the guard's names it
// (readsReachItsRepository). The destination is a server of the test's own on 127.0.0.1 that
// serves git's smart HTTP from a real bare repository (gitHTTP) and records each request: both
// reads answer, the negotiation included, every request of both carries the header, and the
// server saw the ls-remote's request for the refs and the negotiation's fetch command.
func TestTheReadsOfWhatTheDestinationHoldsCarryThePushsTransportSettings(t *testing.T) {
	const header, value = "X-Legion-Test", "carried"
	for _, tc := range []struct {
		name string
		// set puts http.extraHeader in the case's place for the clone at repo, and answers what
		// the guard's environment holds beyond loopbackEnv's.
		set func(t *testing.T, fixture fixturegit.Runner, repo string) []string
	}{
		{
			name: "the repository's own file",
			set: func(_ *testing.T, fixture fixturegit.Runner, repo string) []string {
				fixture.Run(repo, "git", "config", "http.extraHeader", header+": "+value)
				return nil
			},
		},
		{
			name: "GIT_CONFIG_PARAMETERS",
			set: func(*testing.T, fixturegit.Runner, string) []string {
				return []string{"GIT_CONFIG_PARAMETERS=" + shQuote("http.extraHeader") + "=" + shQuote(header+": "+value)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, notesRefused())
			var mu sync.Mutex
			var requests, bare int
			commands := map[string]bool{}
			backend := gitHTTP(t, filepath.Dir(destination))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				read := body
				if r.Header.Get("Content-Encoding") == "gzip" {
					if unzipped, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
						read, _ = io.ReadAll(unzipped)
					}
				}
				mu.Lock()
				requests++
				if r.Header.Get(header) != value {
					bare++
				}
				for _, command := range []string{"ls-refs", "fetch"} {
					if bytes.Contains(read, []byte("command="+command)) {
						commands[command] = true
					}
				}
				mu.Unlock()
				backend.ServeHTTP(w, r)
			}))
			defer server.Close()
			url := server.URL + "/" + filepath.Base(destination)
			extra := tc.set(t, fixturegit.Isolated(t, t.TempDir()), repo)
			guard := New(Options{
				Check: leakguardtest.Write(t, t.TempDir(), passes),
				Env:   withRules(t, loopbackEnv(t.TempDir(), "PATH="+os.Getenv("PATH"), extra...)),
			})
			not, negotiated, err := guard.destinationHolds(bound(t), repo, url, []string{head})
			if err != nil || !negotiated {
				t.Errorf("the reads of %s answered %v, negotiated %t; want both answered", url, err, negotiated)
			}
			if want := "^" + base + "\n^" + base + "\n"; not != want {
				t.Errorf("the destination is taken to hold %q, want the negotiation's and the advertised read's base, %q", not, want)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests == 0 || bare != 0 {
				t.Errorf("%d of the reads' %d requests carried no %s: %s, want every one to carry it", bare, requests, header, value)
			}
			if !commands["ls-refs"] || !commands["fetch"] {
				t.Errorf("the server saw the commands %v, want the ls-remote's ls-refs and the negotiation's fetch", commands)
			}
		})
	}
}
