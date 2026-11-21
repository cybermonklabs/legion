package leakguard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A push the destination already holds everything of adds no commit and so has no record but its
// ref name, which is what a push of an existing commit to a new name is.
func TestAPushThatAddsNoCommitReadsNoCommit(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	head := commitFiles(t, fixture, repo, map[string]string{"README.md": "the file names " + theHost + "\n"}, "a commit naming "+theHost)
	// The destination holds that commit at that ref already, so the push moves nothing and the
	// commit's own text is none of its business.
	destination := destinationAt(t, fixture, repo, head+":refs/heads/main")
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     destination,
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: head}},
	})
	if err != nil {
		t.Fatalf("a push that adds no commit: %v", err)
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
			head := commitFiles(t, fixture, repo, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, "a message of no interest")
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
			wantRefused(t, err, []string{"notes.md: " + theClass})
			if !lacks() {
				t.Errorf("the read of the push fetched %s, the commit the destination's later branch is at, into the clone", short(later))
			}
		})
	}
}

// The read of what the destination holds is made outside the clone, so no configuration of the
// clone's leads it to another repository (Guard.advertised). git hands the hook the URL the
// clone's configuration has already led the push to. Under the clone's url.<base>.pushInsteadOf,
// which git applies to the push URL alone, that URL can be one the clone's url.<base>.insteadOf
// rewrites, and a read made in the clone would list the refs of the stand-in repository the
// rewrite names: the review of cybermonklabs/legion#3946 reproduced that, and its case is the
// first here. The stand-in advertises the commit the push adds, a line the rules refuse, and the
// repository the push writes to holds nothing, so that commit is read and refused. The case holds
// for a push made with `git --git-dir <dir>`, which is how `jj git push` makes one, and whose
// hook git hands GIT_DIR. A URL that is a relative local path is read from the clone's
// directory, where git read it for the push: the repository there holds the pushed commit, so
// the push passes. A destination the clone's own url.<base>.insteadOf alone reaches is read at
// the URL as written, which no transport but file reaches here, so the read fails. Each push
// creates main.
func TestTheReadOfWhatTheDestinationHoldsIsMadeOutsideTheClone(t *testing.T) {
	// pushRewritten is the review's case: origin at a URL that names no repository, which the
	// clone's pushInsteadOf rewrites, for a push alone, to destination.git, which holds nothing,
	// and an insteadOf of the clone's that rewrites destination.git to stand-in.git, which holds
	// head. It answers destination.git, the URL git hands the hook.
	pushRewritten := func(t *testing.T, run func(string, ...string) string, repo, head string) string {
		t.Helper()
		root := filepath.Dir(repo)
		destination, standIn := filepath.Join(root, "destination.git"), filepath.Join(root, "stand-in.git")
		run(root, fixturegit.InitBare(destination, "main")...)
		run(root, fixturegit.InitBare(standIn, "main")...)
		run(repo, "git", "push", "--quiet", standIn, head+":refs/heads/main")
		const fork = "https://example.invalid/fork"
		run(repo, "git", "remote", "add", "origin", fork)
		run(repo, "git", "config", "url."+destination+".pushInsteadOf", fork)
		run(repo, "git", "config", "url."+standIn+".insteadOf", destination)
		if pushed := run(repo, "git", "remote", "get-url", "--push", "origin"); pushed != destination {
			t.Fatalf("origin pushes to %s, want %s: the case would prove nothing", pushed, destination)
		}
		if listed := run(repo, "git", "ls-remote", "--heads", destination); !strings.HasPrefix(listed, head) {
			t.Fatalf("git ls-remote of %s in the clone lists %q, want the stand-in's main at %s: the case would prove nothing", destination, listed, head)
		}
		return destination
	}
	for _, tc := range []struct {
		name string
		// destination makes the repository the push is made to, beside repo, and the clone's
		// configuration the case names, and answers the URL git hands the hook.
		destination func(t *testing.T, run func(dir string, argv ...string) string, repo, head string) string
		// gitDir is whether the hook's environment names the clone's GIT_DIR.
		gitDir bool
		// want is the report, and nil where the push passes or fails; failure is the setup
		// failure, "" where there is none.
		want    []string
		failure string
	}{
		{
			name:        "a push URL the clone's pushInsteadOf names and its insteadOf rewrites",
			destination: pushRewritten,
			want:        []string{"notes.md: " + theClass},
		},
		{
			name:        "the same push made with git --git-dir, as jj makes one",
			destination: pushRewritten,
			gitDir:      true,
			want:        []string{"notes.md: " + theClass},
		},
		{
			name: "a URL that is a relative local path",
			destination: func(_ *testing.T, run func(string, ...string) string, repo, head string) string {
				root := filepath.Dir(repo)
				run(root, fixturegit.InitBare(filepath.Join(root, "destination.git"), "main")...)
				run(repo, "git", "push", "--quiet", "../destination.git", head+":refs/heads/main")
				return "../destination.git"
			},
		},
		{
			name: "a destination the clone's own insteadOf alone reaches",
			destination: func(_ *testing.T, run func(string, ...string) string, repo, head string) string {
				bare := filepath.Join(filepath.Dir(repo), "destination.git")
				run(repo, fixturegit.InitBare(bare, "main")...)
				run(repo, "git", "push", "--quiet", bare, head+":refs/heads/main")
				run(repo, "git", "config", "url.file://"+bare+".insteadOf", theURL)
				return theURL
			},
			failure: "LEAK CHECK SETUP ERROR: the push to origin: the destination's refs could not be read: git ls-remote " + theURL + " exited 128: fatal: transport 'https' not allowed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, head, _ := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
			url := tc.destination(t, fixturegit.Isolated(t, t.TempDir()).Run, repo, head)
			var hook []string
			if tc.gitDir {
				hook = append(hook, "GIT_DIR="+filepath.Join(repo, ".git"))
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, hook...)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     url,
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
			})
			if tc.failure != "" {
				wantSetupFailure(t, err, tc.failure)
				return
			}
			wantRefused(t, err, tc.want)
		})
	}
}

// The read of what the destination holds reads no git configuration but its own command line's
// (Guard.advertised), so no setting of the environment the guard runs under leads it to another
// repository either: not the global file GIT_CONFIG_GLOBAL names, nor the system file
// GIT_CONFIG_SYSTEM names where GIT_CONFIG_NOSYSTEM=0 lets git read one, nor a GIT_CONFIG_COUNT
// block, nor GIT_CONFIG_PARAMETERS, nor the user's own files, ~/.gitconfig and
// $XDG_CONFIG_HOME/git/config, which git reads where no GIT_CONFIG_GLOBAL names another file.
// Each case puts in one of those places a url.<stand-in>.insteadOf that rewrites the
// destination's path to the stand-in's, which a plain git ls-remote of the destination under that
// environment follows. The stand-in holds the commit the push adds, a line the rules refuse, and
// the destination, which the push is made to by its path, holds only the commit before it. A read
// the rewrite led to the stand-in would take that commit as held and pass the push unread; the
// read of the destination reads it, and the push is refused.
func TestTheReadOfWhatTheDestinationHoldsReadsNoConfigurationButItsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		// env is what the case adds to the environment to set key to value: dir is a directory
		// of the case's own, and config writes that one setting to the configuration file at a
		// path and answers the path.
		env func(dir string, config func(file string) string, key, value string) []string
	}{
		{
			name: "the global file GIT_CONFIG_GLOBAL names",
			env: func(dir string, config func(string) string, _, _ string) []string {
				return []string{"GIT_CONFIG_GLOBAL=" + config(filepath.Join(dir, "gitconfig"))}
			},
		},
		{
			name: "the system file GIT_CONFIG_SYSTEM names, under GIT_CONFIG_NOSYSTEM=0",
			env: func(dir string, config func(string) string, _, _ string) []string {
				return []string{"GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM=" + config(filepath.Join(dir, "gitconfig"))}
			},
		},
		{
			name: "a GIT_CONFIG_COUNT block",
			env: func(_ string, _ func(string) string, key, value string) []string {
				return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + key, "GIT_CONFIG_VALUE_0=" + value}
			},
		},
		{
			// git hands the git it starts for `git -c <key>=<value>` GIT_CONFIG_PARAMETERS in this
			// form, each half quoted for sh: measured with git 2.47.3.
			name: "GIT_CONFIG_PARAMETERS",
			env: func(_ string, _ func(string) string, key, value string) []string {
				return []string{"GIT_CONFIG_PARAMETERS=" + shQuote(key) + "=" + shQuote(value)}
			},
		},
		{
			name: "the user's file under HOME",
			env: func(dir string, config func(string) string, _, _ string) []string {
				config(filepath.Join(dir, ".gitconfig"))
				return []string{"HOME=" + dir}
			},
		},
		{
			name: "the user's file under XDG_CONFIG_HOME",
			env: func(dir string, config func(string) string, _, _ string) []string {
				config(filepath.Join(dir, "git", "config"))
				return []string{"XDG_CONFIG_HOME=" + dir}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
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
			// top. The control ls-remote runs under it whole, since Runner.With only adds names.
			env := append(slices.DeleteFunc(fixtureenv.Isolated(t.TempDir()), func(entry string) bool {
				return strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=")
			}), tc.env(t.TempDir(), config, key, destination)...)
			control := fixturegit.Runner{TB: t, Env: env}
			if listed := control.Run("/", "git", "ls-remote", "--heads", destination); !strings.HasPrefix(listed, head) {
				t.Fatalf("git ls-remote of %s under the case's environment lists %q, want the stand-in's main at %s: the case would prove nothing", destination, listed, head)
			}
			guard := New(Options{Check: leakguardtest.Write(t, t.TempDir(), passes), Visibility: answers(Public, nil), Env: env})
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, []string{"notes.md: " + theClass})
		})
	}
}

// The read of what the destination holds carries the pod's GitHub credential on its own command
// line, and takes nothing of git configuration from its environment (Guard.advertised,
// heldReadCredential): in front of its ls-remote, the helper for https://github.com reset and then
// set to `legion git-credential`, and useHttpPath on for github.com, in that order, the entries an
// issue pod's GIT_CONFIG_COUNT block begins with, so that GitHub answers the read while it asks
// some Git-over-HTTPS reads to authenticate. Its environment carries GIT_CONFIG_NOSYSTEM=1 and
// GIT_CONFIG_GLOBAL=/dev/null (readsNoConfigFile), and no GIT_CONFIG_COUNT block and no
// GIT_CONFIG_PARAMETERS. The guard runs here under an environment that says otherwise of each:
// GIT_CONFIG_NOSYSTEM=0, a global file of its own, and a GIT_CONFIG_COUNT block and
// GIT_CONFIG_PARAMETERS that each name another helper. The stand-in git records each call
// (leakguardtest.RecordsGitCall) and runs the real git; no server of the test's can stand in for
// the credential, whose entries apply to https://github.com alone. The destination holds only the
// commit before the one the push adds, a line the rules refuse, so the push is read and refused.
func TestTheReadOfWhatTheDestinationHoldsCarriesThePodsCredentialOnItsCommandLine(t *testing.T) {
	bin, record := t.TempDir(), filepath.Join(t.TempDir(), "record")
	writeProgram(t, bin, "git", leakguardtest.RecordsGitCall(record)+"exec "+shQuote(fixturegit.CompiledGit(t))+" \"$@\"\n")
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	repo, base, head, destination := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin),
		"GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL="+global,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.https://github.com.helper", "GIT_CONFIG_VALUE_0=!invented git-credential",
		"GIT_CONFIG_PARAMETERS="+shQuote("credential.helper")+"="+shQuote("!invented git-credential"))
	err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
	wantRefused(t, err, []string{"notes.md: " + theClass})
	var reads []map[string]string
	for _, call := range leakguardtest.ReadCalls(t, record) {
		if strings.Contains(" "+call["argv"]+" ", " ls-remote ") {
			reads = append(reads, call)
		}
	}
	if len(reads) != 1 {
		t.Fatalf("git ran ls-remote %d times, want once: %v", len(reads), reads)
	}
	read := reads[0]
	want := strings.Join(gitArgv(
		"-c", "credential.https://github.com.helper=",
		"-c", "credential.https://github.com.helper="+workspace.GitHubCredentialHelper,
		"-c", "credential.https://github.com.useHttpPath=true",
		"ls-remote", "--heads", "--tags", "--", destination)[1:], " ")
	if read["argv"] != want {
		t.Errorf("the read ran as\n  git %s\nwant\n  git %s", read["argv"], want)
	}
	for _, name := range []struct{ key, variable, want string }{
		{"nosystem", "GIT_CONFIG_NOSYSTEM", "1"},
		{"global", "GIT_CONFIG_GLOBAL", "/dev/null"},
		{"count", "GIT_CONFIG_COUNT", "unset"},
		{"key0", "GIT_CONFIG_KEY_0", "unset"},
		{"parameters", "GIT_CONFIG_PARAMETERS", "unset"},
	} {
		if read[name.key] != name.want {
			t.Errorf("the read ran with %s %q, want %q", name.variable, read[name.key], name.want)
		}
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
			repo, base, head, destination := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
			bin := t.TempDir()
			writeProgram(t, bin, "git", `case " $* " in
*" ls-remote "*) printf '%s\trefs/heads/main\n' `+shQuote(tc.field(head))+`; exit 0 ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, []string{"notes.md: " + theClass})
		})
	}
}

// credentialedURL is theURL carrying credentialUser and credentialToken as its userinfo, a URL of
// the shape git can hand a pre-push hook. The userinfo is built apart from the scheme, so no line
// of this file is a URL that carries one, which the shared check would refuse at the push.
const (
	credentialUser, credentialToken = "invented-user", "invented-token"
	credentialUserinfo              = credentialUser + ":" + credentialToken + "@"
)

var credentialedURL = strings.Replace(theURL, "https://", "https://"+credentialUserinfo, 1)

// queriedURL is theURL carrying credentialToken in its query, the other place a URL can carry a
// token, built apart from the URL as credentialedURL's userinfo is.
var queriedURL = theURL + "?access_token=" + credentialToken

// wantNoCredential fails the test where err names credentialedURL's user or token, which
// queriedURL carries too.
func wantNoCredential(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), credentialUser) || strings.Contains(err.Error(), credentialToken) {
		t.Errorf("the failure carries the URL's credential: %v", err)
	}
}

// A read of what the destination holds that fails refuses the push as a setup failure, never a
// push read as one whose destination holds nothing, and the failure names the remote and the URL
// without its credential. The URL git hands a pre-push hook can hold a user and a token
// (https://user:token@host/...) or a token in its query, and the failure names the URL and quotes
// git, which names it too: both are written without the user, the query and the token
// (redact.URLCredentials). A push made to a URL rather than to a remote, `git push
// https://user:token@host/...`, hands the hook that URL as the remote's name too, which every
// failure of the push names (PushName). The pushed commit adds a line the rules refuse, so a read
// that held the commit or read nothing would answer otherwise. The stand-in git here fails every
// call with a URL that holds a user or a query, as a git that cannot reach the host does, and
// runs the real git for every other call.
func TestAFailedReadNamesItsURLWithoutItsCredential(t *testing.T) {
	bin := t.TempDir()
	writeProgram(t, bin, "git", `for arg; do
  case "$arg" in
  *://*@*|*://*[?]*) echo "fatal: unable to access '$arg/': Could not resolve host: github.com" >&2; exit 128 ;;
  esac
done
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
	unreachable := " exited 128: fatal: unable to access '" + theURL + "/': Could not resolve host: github.com"
	for _, tc := range []struct {
		name string
		// remote is the remote's name git hands the hook, and url the URL.
		remote, url string
		want        string
	}{
		{
			name:   "a push to a remote",
			remote: "origin",
			url:    credentialedURL,
			want:   "LEAK CHECK SETUP ERROR: the push to origin: the destination's refs could not be read: git ls-remote " + theURL + unreachable,
		},
		{
			name:   "a push to a URL",
			remote: credentialedURL,
			url:    credentialedURL,
			want:   "LEAK CHECK SETUP ERROR: the push to " + theURL + ": the destination's refs could not be read: git ls-remote " + theURL + unreachable,
		},
		{
			// git's quote of the URL loses the "/" it writes after it with the query, which runs
			// to the quote's end.
			name:   "a push to a URL whose query carries a token",
			remote: queriedURL,
			url:    queriedURL,
			want: "LEAK CHECK SETUP ERROR: the push to " + theURL + ": the destination's refs could not be read: git ls-remote " + theURL +
				" exited 128: fatal: unable to access '" + theURL + "': Could not resolve host: github.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, _ := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
			err := guard.Push(bound(t), mainPush(repo, base, head, tc.remote, tc.url))
			wantSetupFailure(t, err, tc.want)
			wantNoCredential(t, err)
		})
	}
}

// A read of what the destination holds that does not answer ends at its own bound
// (Guard.heldReadBound), although the caller's context has no deadline, as the hook's has none: a
// destination that stops answering refuses the push rather than holding it. The push is refused
// as a setup failure that names the URL, without its credential, and the bound. The stand-in git
// here never answers an ls-remote, and runs the real git for every other call; the bound is 2 s.
func TestAReadOfWhatTheDestinationHoldsThatDoesNotAnswerEndsAtItsBound(t *testing.T) {
	bin := t.TempDir()
	writeProgram(t, bin, "git", `case " $* " in
*" ls-remote "*) exec sleep 30 ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
	repo, base, head, _ := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
	guard.heldReadBound = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := time.Now()
	err := guard.Push(ctx, mainPush(repo, base, head, "origin", credentialedURL))
	answeredWithin10s(t, started)
	wantSetupFailure(t, err, "LEAK CHECK SETUP ERROR: the push to origin: the destination's refs could not be read: git ls-remote "+theURL+": git ran 2 s and was stopped by its bound of 2 s")
	wantNoCredential(t, err)
}

// A failed read of a pushed commit quotes git's own account of it without the user and password
// a URL in it carries. In a partial clone, as an issue pod's fork clones are (--filter=blob:none),
// git fetches an object the clone lacks from the promisor remote the moment a read asks for it,
// and a fetch that fails names the remote's URL, which can carry a token: the list of a commit's
// changes (Guard.git), whose renames are found by the content of its blobs, the read of its
// object, which the push's first cat-file --batch answers (Guard.pushRecords), and the read of its
// files, which the commit's own batch answers (blobBatch), each quote it. The stand-in git here
// fails the one call each case names as such a fetch fails, once the calls of that shape the case
// passes have run, and runs the real git for every other call.
func TestAFailedReadOfACommitQuotesGitWithoutACredential(t *testing.T) {
	const failed = "LEAK CHECK SETUP ERROR: the push to origin: what the push adds could not be read: "
	unreachable := " exited 128: fatal: unable to access '" + theURL + "/': Could not resolve host: github.com"
	for _, tc := range []struct {
		name string
		// call is the git subcommand that fails, as its arguments begin, and passed how many calls
		// of it run the real git before it fails.
		call   string
		passed int
		want   string
	}{
		{name: "the list of a commit's changes", call: "diff-tree", want: failed + "git diff-tree" + unreachable},
		{name: "the read of a commit's object", call: "cat-file --batch", want: failed + "git cat-file" + unreachable},
		{name: "the read of a commit's files", call: "cat-file --batch", passed: 1, want: failed + "git cat-file" + unreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls")
			writeProgram(t, bin, "git", `case " $* " in
*`+shQuote(" "+tc.call+" ")+`*)
  echo >>`+shQuote(calls)+`
  if [ "$(wc -l <`+shQuote(calls)+`)" -gt `+strconv.Itoa(tc.passed)+` ]; then
    echo `+shQuote("fatal: unable to access '"+credentialedURL+"/': Could not resolve host: github.com")+` >&2; exit 128
  fi ;;
esac
exec `+shQuote(fixturegit.CompiledGit(t))+` "$@"
`)
			repo, base, head, destination := pushOf(t, map[string]string{"notes.md": "one\n"}, nil, map[string]string{"notes.md": "one\ntwo\n"}, nil)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, pathWith(bin))
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantSetupFailure(t, err, tc.want)
			wantNoCredential(t, err)
		})
	}
}

// The commit an update's remote side names holds the commits it reaches, where the clone has it,
// even where the destination advertises nothing else: the update reads only the commits after it
// (Guard.commits). A remote side the clone does not have holds nothing, since rev-list cannot walk
// from it (Guard.holds), and the push is read whole. The commit the remote side names here adds a
// line the rules refuse, the pushed commit on it adds none, and the destination advertises no ref
// at all. The commit the clone does not have is one of another repository's.
func TestAnUpdatesRemoteSideHoldsWhatTheCloneHas(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo, other := filepath.Join(root, "repo"), filepath.Join(root, "other")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(other, "main")...)
	base := commitFiles(t, fixture, repo, map[string]string{"held.md": "the runner answers at " + theHost + "\n"}, "the commit the destination's main is at")
	head := commitFiles(t, fixture, repo, map[string]string{"notes.md": "a line of ordinary prose\n"}, "a message of no interest")
	unknown := commitFiles(t, fixture, other, map[string]string{"README.md": "one\n"}, "a commit the clone has never had")
	destination := destinationAt(t, fixture, repo)
	for _, tc := range []struct {
		name      string
		remoteSHA string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{name: "a remote side the clone has", remoteSHA: base},
		{name: "a remote side the clone does not have", remoteSHA: unknown, want: []string{"held.md: " + theClass}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), mainPush(repo, tc.remoteSHA, head, "origin", destination))
			wantRefused(t, err, tc.want)
		})
	}
}

// A forward merge, the default branch merged onto an older tip of a branch the destination
// holds, reads no commit the destination's branches reach. The default branch's commit here adds
// a line the rules refuse, and the destination's main holds it, so the push of the merge does not
// publish it, whichever ref the push moves. A commit that adds the same line on top of the merge
// is on no branch of the destination, so it is read and refused, and it is the whole of the
// report.
func TestAForwardMergeReadsNoCommitTheDestinationHolds(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the first commit")
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
	tip := commitFiles(t, fixture, repo, map[string]string{"work.md": "a line of the branch\n"}, "the branch's own commit")
	fixture.Run(repo, "git", "checkout", "--quiet", "main")
	commitFiles(t, fixture, repo, map[string]string{"main.md": "the runner answers at " + theHost + "\n"}, "a commit of the default branch")
	// The destination holds both branches: its work is the old tip the push moves, and its main
	// holds the default branch's commit.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main", "work")
	fixture.Run(repo, "git", "checkout", "--quiet", "work")
	fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "-m", "merge main into work", "main")
	merge := fixture.Run(repo, "git", "rev-parse", "HEAD")
	if parents := fixture.Run(repo, "git", "rev-list", "--parents", "-n", "1", merge); len(strings.Fields(parents)) != 3 {
		t.Fatalf("the fixture's merge is %q, want a commit of two parents", parents)
	}
	onTop := commitFiles(t, fixture, repo, map[string]string{"copy.md": "the runner answers at " + theHost + "\n"}, "a commit the destination does not hold")

	for _, tc := range []struct {
		name string
		head string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{name: "the merge alone", head: merge},
		{
			name: "a commit of the same line on top of the merge",
			head: onTop,
			want: []string{"copy.md: " + theClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     remote,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: tc.head, RemoteRef: "refs/heads/work", RemoteSHA: tip}},
			})
			wantRefused(t, err, tc.want)
		})
	}
}

// A commit a rehearsal holds is still one the real push publishes. Before the real push of a
// release branch, a role rehearses it on a local bare repository: through a remote of its own, or
// by pointing the pushed remote's push URL at the bare for one run. Either way git writes a
// remote-tracking ref under the pushed remote's own refs/remotes/origin/ — for a remote named
// origin/rehearsal at refs/remotes/origin/rehearsal/*, and for origin's own push URL at
// refs/remotes/origin/* — and that ref says nothing of what the destination holds. The commit
// here adds a line the rules refuse, and the destination, whose main is at the commit before it,
// does not hold it, so the push to origin reads it and is refused. A push to the rehearsal itself
// reads none of what the rehearsal holds.
func TestACommitARehearsalHoldsIsReadAtThePushToTheDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rehearse pushes HEAD to rehearsal's refs/heads/rehearsal, and answers the remote that
		// push named.
		rehearse func(run func(string, ...string) string, repo, rehearsal string) string
		// tracked is the remote-tracking ref the rehearsal's push wrote.
		tracked string
	}{
		{
			// git 2.56 refuses `git remote add` of a name that is another remote's and a slash
			// ("is a subset of existing remote"); a configuration written by hand still holds one.
			name: "a remote named origin/rehearsal",
			rehearse: func(run func(string, ...string) string, repo, rehearsal string) string {
				run(repo, "git", "config", "remote.origin/rehearsal.url", rehearsal)
				run(repo, "git", "config", "remote.origin/rehearsal.fetch", "+refs/heads/*:refs/remotes/origin/rehearsal/*")
				run(repo, "git", "push", "--quiet", "origin/rehearsal", "HEAD:refs/heads/rehearsal")
				return "origin/rehearsal"
			},
			tracked: "refs/remotes/origin/rehearsal/rehearsal",
		},
		{
			name: "origin's push URL pointed at the rehearsal for one push",
			rehearse: func(run func(string, ...string) string, repo, rehearsal string) string {
				run(repo, "git", "remote", "set-url", "--push", "origin", rehearsal)
				run(repo, "git", "push", "--quiet", "origin", "HEAD:refs/heads/rehearsal")
				run(repo, "git", "config", "--unset", "remote.origin.pushurl")
				return "origin"
			},
			tracked: "refs/remotes/origin/rehearsal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			remote, rehearsal, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "rehearsal.git"), filepath.Join(root, "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(root, fixturegit.InitBare(remote, "main")...)
			fixture.Run(root, fixturegit.InitBare(rehearsal, "main")...)
			fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
			base := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
			fixture.Run(repo, "git", "remote", "add", "origin", remote)
			fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
			head := commitFiles(t, fixture, repo, map[string]string{"README.md": "one\nthe runner answers at " + theHost + "\n"}, "a message of no interest")
			rehearsed := tc.rehearse(fixture.Run, repo, rehearsal)
			if held := fixture.Run(repo, "git", "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/"); held != "refs/remotes/origin/main "+base+"\n"+tc.tracked+" "+head {
				t.Fatalf("the fixture's remote-tracking refs are %q, want origin/main at the base and %s at the commit", held, tc.tracked)
			}
			for _, push := range []struct {
				name string
				// remote and url are what git hands the hook; remoteSHA is the commit the push's
				// destination holds at refs/heads/main.
				remote, url, remoteSHA string
				// want is the report, and nil where the push passes.
				want []string
			}{
				{name: "the push to origin", remote: "origin", url: remote, remoteSHA: base, want: []string{"README.md: " + theClass}},
				{name: "the push to the rehearsal", remote: rehearsed, url: rehearsal, remoteSHA: noSHA},
			} {
				t.Run(push.name, func(t *testing.T) {
					guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
					err := guard.Push(bound(t), PushRequest{
						Dir:     repo,
						Remote:  push.remote,
						URL:     push.url,
						Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: push.remoteSHA}},
					})
					wantRefused(t, err, push.want)
				})
			}
		})
	}
}

// What holds a commit for the rules is a branch or a tag of the destination, as the destination
// reports them at the push, an annotated tag by the commit it peels to (Guard.advertised). No ref
// of the repository's own holds one, whatever remote it is under: a ref written by hand under
// origin's remote-tracking refs, and the one git writes for a push to origin made while origin
// named a local bare repository and then pointed at the destination with `git remote set-url`,
// say nothing of what the destination holds (Guard.Push). The commit here adds a line the rules
// refuse, and the destination's main is at the commit before it.
func TestOnlyWhatTheDestinationAdvertisesHoldsACommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold makes a ref of the repository's, or of the destination's, reach head, destination
		// being the destination's path, the URL the push is made to.
		hold func(t *testing.T, run func(dir string, argv ...string) string, repo, destination, head string)
		// tracked is the remote-tracking ref hold writes at head, "" where it writes none.
		tracked string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{
			name: "a ref written by hand under origin's remote-tracking refs",
			hold: func(_ *testing.T, run func(string, ...string) string, repo, destination, head string) {
				run(repo, "git", "remote", "add", "origin", destination)
				run(repo, "git", "update-ref", "refs/remotes/origin/x", head)
			},
			tracked: "refs/remotes/origin/x",
			want:    []string{"notes.md: " + theClass},
		},
		{
			name: "origin made at a local bare, pushed to, and pointed at the destination",
			hold: func(t *testing.T, run func(string, ...string) string, repo, destination, _ string) {
				rehearsal := filepath.Join(t.TempDir(), "rehearsal.git")
				run(repo, fixturegit.InitBare(rehearsal, "main")...)
				run(repo, "git", "remote", "add", "origin", rehearsal)
				run(repo, "git", "push", "--quiet", "origin", "HEAD:refs/heads/main")
				run(repo, "git", "remote", "set-url", "origin", destination)
			},
			tracked: "refs/remotes/origin/main",
			want:    []string{"notes.md: " + theClass},
		},
		{
			name: "an annotated tag of the destination's",
			hold: func(_ *testing.T, run func(string, ...string) string, repo, destination, head string) {
				run(repo, "git", "tag", "--annotate", "--message", "a release", "v1", head)
				run(repo, "git", "push", "--quiet", destination, "refs/tags/v1")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, nil, nil, map[string]string{"notes.md": "the runner answers at " + theHost + "\n"}, nil)
			fixture := fixturegit.Isolated(t, t.TempDir())
			tc.hold(t, fixture.Run, repo, destination, head)
			if tc.tracked != "" {
				if at := fixture.Run(repo, "git", "rev-parse", tc.tracked); at != head {
					t.Fatalf("%s is at %s, want the commit %s: the case would prove nothing", tc.tracked, at, head)
				}
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, tc.want)
		})
	}
}
