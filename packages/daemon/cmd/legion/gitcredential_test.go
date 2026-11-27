package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// gitCredentialBundle writes a gh bundle of a role whose issue is acme's: one directory per
// owner the role's App is installed on, each holding that owner's own write token — acme's,
// sjawhar's and example-org's, the last an owner the App is installed on and no record of the
// project names, since a lease reaches every repository its installation holds and the
// installation is the whole of a role's reach (appauth.GitHubCredential, INFRA-2543). It
// returns the bundle's root and acme's directory, which GH_CONFIG_DIR names.
func gitCredentialBundle(t *testing.T) (root, defaultDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{
		"acme":        ghconfig.Render("ghs_acme_write", "implement", expires),
		"sjawhar":     ghconfig.Render("ghs_sjawhar_write", "implement", expires),
		"example-org": ghconfig.Render("ghs_example_org_write", "implement", expires),
	}}); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, "acme")
}

func runGitCredentialHere(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	previous := gitCredentialStdin
	gitCredentialStdin = strings.NewReader(stdin)
	t.Cleanup(func() { gitCredentialStdin = previous })
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion", "git-credential"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// `legion git-credential get` answers git's credential protocol from the hosts.yml of the
// owner the path git asks about names (credential.useHttpPath), lowercased — a managed user's
// login, which ends in `_<shortcode>`, among them: from that owner's
// directory of LEGION_GH_OWNERS_DIR, whatever the path's second segment is — a repository of
// that owner, and a segment no repository could be named for alike, since the owner's lease
// reaches every repository its installation holds, and since no configured rename of this
// bundle moved one of them (a pair routes by that second segment, which
// TestThePairsRepositoryTakesTheDirectoryOfTheOwnerHoldingItAndItsSiblingsTheirOwn reads) —
// and from nothing else: it answers nothing,
// exit 1, naming why, for any host but github.com over https, a request with no path, a path
// whose owner is no GitHub login, an unset LEGION_GH_OWNERS_DIR, and a hosts.yml with no token.
// A repository of an owner the bundle holds no directory for is read from GitHub with
// GH_CONFIG_DIR's token, the issue owner's write token, which is never the answer for it: here
// GitHub answers that read 404, so the helper answers nothing, exit 1, naming the directory the
// bundle lacks and GitHub's answer (TestGitCredentialAnswersAForksParentToken reads a fork it
// answers for). Every other row makes no read, and none reaches GitHub: the read's curl is a fake
// GitHub that counts its runs. store and erase write nothing and exit 0.
func TestGitCredentialAnswersFromTheOwnersDirectory(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, defaultDir := gitCredentialBundle(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty", ghconfig.HostsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A managed user's directory is written on its own (ghconfig.Write) rather than through
	// WriteBundle, so the row reading it reads this command's own check of the path's owner and
	// not the bundle's.
	if _, err := ghconfig.Write(ghconfig.Dir(root, "mona-cat_octo"), ghconfig.Render("ghs_mona_cat_octo_write", "implement", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	both := map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root}
	const answer = "username=x-access-token\npassword=%s\n"
	github := newFakeGitHub(t, "ghs_acme_write", curlNotFound(t))
	useForkRead(t, github.program, forkReadBound)
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		stdin    string
		code     int
		stdout   string
		stderrIn string
		// forkRead is what stderr names of GitHub's answer to the row's one fork read; every
		// row where it is "" makes no read.
		forkRead string
	}{
		{
			// The path's second segment names nothing this reads: a repository of the owner,
			// and `..`, which no repository is named for, are both answered from the owner's
			// own directory, and neither is a way out of it.
			name:   "a second segment that is no repository name",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/..\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_write", 1),
		},
		{
			name:   "another owner's directory",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=sjawhar/legion-smoke.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_write", 1),
		},
		{
			name:   "an owner named in another case",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=SJawhar/legion-smoke\n",
			stdout: strings.Replace(answer, "%s", "ghs_sjawhar_write", 1),
		},
		{
			// A repository of an owner no record of the project names: the bundle holds that
			// owner's directory because the App is installed on the account, and its token
			// reaches every repository of that installation.
			name:   "a repository of an owner no record of the project names",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=example-org/deployment.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_example_org_write", 1),
		},
		{
			name:   "a managed user's repository",
			env:    both,
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=mona-cat_octo/widgets.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_mona_cat_octo_write", 1),
		},
		{
			name:   "the issue owner's directory",
			env:    map[string]string{"LEGION_GH_OWNERS_DIR": root},
			args:   []string{"get"},
			stdin:  "protocol=https\nhost=github.com\npath=/acme/widgets.git\n\n",
			stdout: strings.Replace(answer, "%s", "ghs_acme_write", 1),
		},
		{
			name:     "an owner the role's App is not installed on",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=Other/Thing\n\n",
			code:     1,
			stderrIn: `the role's GitHub App is not installed on "other": ` + root + " holds no gh directory for it",
			forkRead: "the status 404",
		},
		{
			name:     "a path that is no owner",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=../sjawhar/x\n\n",
			code:     1,
			stderrIn: `"..", which is no GitHub login`,
		},
		{
			name:     "no path (useHttpPath off)",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\n\n",
			code:     1,
			stderrIn: "git sent no path",
		},
		{
			name:     "no owners directory",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: "LEGION_GH_OWNERS_DIR is unset",
		},
		{
			name:     "another host",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=gitlab.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `host "gitlab.com"`,
		},
		{
			name:     "plain http",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=http\nhost=github.com\npath=acme/widgets.git\n\n",
			code:     1,
			stderrIn: `protocol "http"`,
		},
		{
			name:     "a hosts.yml with no token",
			env:      both,
			args:     []string{"get"},
			stdin:    "protocol=https\nhost=github.com\npath=empty/thing.git\n\n",
			code:     1,
			stderrIn: "names no github.com oauth_token",
		},
		{
			name:  "store",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"store"},
			stdin: "protocol=https\nhost=github.com\nusername=x-access-token\npassword=ghs_x\n\n",
		},
		{
			name:  "erase",
			env:   map[string]string{"GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root},
			args:  []string{"erase"},
			stdin: "protocol=https\nhost=github.com\n\n",
		},
		{
			name:     "an unknown operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			args:     []string{"approve"},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
		{
			name:     "no operation",
			env:      map[string]string{"GH_CONFIG_DIR": defaultDir},
			code:     2,
			stderrIn: "usage: legion git-credential get|store|erase",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
				if value, ok := tc.env[name]; ok {
					t.Setenv(name, value)
				} else {
					fixtureenv.Unset(t, name)
				}
			}
			before := len(github.runs(t))
			code, stdout, stderr := runGitCredentialHere(t, tc.stdin, tc.args...)
			if code != tc.code || stdout != tc.stdout || !strings.Contains(stderr, tc.stderrIn) || !strings.Contains(stderr, tc.forkRead) {
				t.Fatalf("legion git-credential %v = exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr containing %q and %q",
					tc.args, code, stdout, stderr, tc.code, tc.stdout, tc.stderrIn, tc.forkRead)
			}
			reads := 0
			if tc.forkRead != "" {
				reads = 1
			}
			if got := len(github.runs(t)) - before; got != reads {
				t.Errorf("the fake GitHub ran %d times, want %d", got, reads)
			}
		})
	}
}

// Each operation the usage line names answers --help, exit 0, with its own usage, and reads no
// request: git-credential reads its own arguments (passThrough), so a help request is its own.
func TestGitCredentialOperationsAnswerHelp(t *testing.T) {
	// Not parallel: it sets the package variable gitCredentialStdin (runGitCredentialHere).
	for _, op := range []string{"get", "store", "erase"} {
		code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\n\n", op, "--help")
		if code != 0 || stdout != "" || !usageOf(stderr, "git-credential", op) {
			t.Errorf("legion git-credential %s --help = exit %d, stdout %q, stderr %q; want exit 0 and its usage", op, code, stdout, stderr)
		}
	}
}

// namedTokens is each lease named for the App, the owner and whether it reads or writes, as the
// daemon's own token source answers one.
type namedTokens struct{}

func (namedTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_write_" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (namedTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_read_" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// namedTokens also answers which owners the App is installed on (appauth.OwnerInstallations),
// which a bundle reads at every build: cybermonklabs alone, the account this
// deployment's App is installed on, so sjawhar's directory is the configured rename's and
// holds the token of the installation its repository moved to.
func (namedTokens) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return []string{"cybermonklabs"}, nil
}

// renamedCredentialBundle writes to disk the bundle appauth.GitHubCredential builds for an
// implementer whose issue's record names sjawhar/legion — the repository as it was recorded
// before the transfer — in a project that serves served, under renames. It returns the bundle's
// root and its default directory, which the pod's GH_CONFIG_DIR names: the directory the record's
// repository routes to (ghconfig.RouteDir), cybermonklabs' wherever the pair resolves.
func renamedCredentialBundle(t *testing.T, served []ghrepo.Repository, renames ghrepo.Renames) (root, defaultDir string) {
	t.Helper()
	credential := appauth.GitHubCredential(namedTokens{}, servesOneOf(served), renames, nil, nil, slog.Default())
	bundle, err := credential(context.Background(), legionclaim.RoleImplementer, ghrepo.MustParse("sjawhar/legion"))
	if err != nil {
		t.Fatalf("build the bundle: %v", err)
	}
	root = filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, bundle); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, bundle.Default)
}

// A pod whose issue is recorded on the repository as it was still has to write the repository as
// GitHub holds it now: with the pair configured, `legion git-credential get` for the clone path
// of that record, the one a `jj git push` of the tree presents a token for, answers the write
// token minted for cybermonklabs, the owner the repository moved to, from sjawhar's own
// directory of the bundle appauth.GitHubCredential built. It answers that whether or not the
// project serves another repository of sjawhar under its own name, since the pair is resolved
// for the repository the record names and for nothing else: one more served repository of that
// owner is what turned the live pair off and left 47 trees unable to push. With no pair
// configured the same bundle gives sjawhar its own token, as today, and that is what the same
// request answers.
func TestGitCredentialAnswersARenamedRepositoryWithTheOwnerItMovedTo(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable gitCredentialStdin (runGitCredentialHere).
	pair := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	moved := []ghrepo.Repository{ghrepo.MustParse("cybermonklabs/legion")}
	for _, tc := range []struct {
		name    string
		served  []ghrepo.Repository
		renames ghrepo.Renames
		token   string
	}{
		{name: "the pair configured", served: moved, renames: pair, token: "ghs_implement_write_cybermonklabs"},
		{name: "the pair configured, and the project serves another repository of sjawhar",
			served:  append(slices.Clone(moved), ghrepo.MustParse("sjawhar/oh-my-pi")),
			renames: pair, token: "ghs_implement_write_cybermonklabs"},
		{name: "no pair configured", served: moved, renames: ghrepo.Renames{}, token: "ghs_implement_write_sjawhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, defaultDir := renamedCredentialBundle(t, tc.served, tc.renames)
			t.Setenv("GH_CONFIG_DIR", defaultDir)
			t.Setenv("LEGION_GH_OWNERS_DIR", root)
			code, stdout, stderr := runGitCredentialHere(t, "protocol=https\nhost=github.com\npath=/sjawhar/legion.git\n\n", "get")
			want := "username=x-access-token\npassword=" + tc.token + "\n"
			if code != 0 || stdout != want || stderr != "" {
				t.Fatalf("legion git-credential get for /sjawhar/legion = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, want)
			}
		})
	}
}

// forkRequest is git's credential request for the repository path, as a shared clone sends it
// (credential.useHttpPath).
func forkRequest(path string) string {
	return "protocol=https\nhost=github.com\npath=" + path + "\n\n"
}

// forkReadPane sets the environment the helper's fork read runs in: LEGION_GH_OWNERS_DIR names
// root, GH_CONFIG_DIR names configDir, or nothing where configDir is "", and XDG_STATE_HOME names
// a directory of the test's own. The fork read never reads XDG_STATE_HOME; a row run through
// `legion gh auth git-credential` needs it, since runGh writes its call log there
// (appendGhCallLog).
func forkReadPane(t *testing.T, root, configDir string) {
	t.Helper()
	t.Setenv("LEGION_GH_OWNERS_DIR", root)
	if configDir == "" {
		fixtureenv.Unset(t, "GH_CONFIG_DIR")
	} else {
		t.Setenv("GH_CONFIG_DIR", configDir)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// useForkRead points the helper's fork read at curl and bounds it by bound, which only a test
// does, and puts the binary's own back when the test ends, as runGitCredentialHere puts back the
// helper's stdin.
func useForkRead(t *testing.T, curl string, bound time.Duration) {
	t.Helper()
	previousCurl, previousBound := leakguardCurl, forkReadBound
	leakguardCurl, forkReadBound = curl, bound
	t.Cleanup(func() { leakguardCurl, forkReadBound = previousCurl, previousBound })
}

// forkToken is the write token forkBundle writes for owner, named as gitCredentialBundle names
// its own: ghs_<owner>_write, with the owner's hyphens as underscores.
func forkToken(owner string) string {
	return "ghs_" + strings.ReplaceAll(owner, "-", "_") + "_write"
}

// forkBundle writes a gh bundle holding one directory for each of owners, each with that
// owner's own write token (forkToken), and returns its root and dflt's directory, which
// GH_CONFIG_DIR names. dflt is one of owners.
func forkBundle(t *testing.T, dflt string, owners ...string) (root, defaultDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "gh")
	dirs := make(map[string]ghconfig.Rendered, len(owners))
	for _, owner := range owners {
		dirs[owner] = ghconfig.Render(forkToken(owner), "implement", time.Now().Add(time.Hour))
	}
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: dflt, Dirs: dirs}); err != nil {
		t.Fatal(err)
	}
	return root, ghconfig.Dir(root, dflt)
}

// forkReadFixture is the absolute path of testdata/fork-read/<name>: one of GitHub's own answers
// to `GET /repos/<owner>/<name>`, as `curl --include` prints it, with the outside names replaced
// (that directory's README names each capture).
func forkReadFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "fork-read", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// derivedForkRead is the path of a file of the test's own holding the capture name with one
// textual change, the first old replaced by with: an answer of a shape no capture has. A capture
// that holds no old fails the test, so a change that changed nothing serves no capture unchanged.
func derivedForkRead(t *testing.T, name, old, with string) string {
	t.Helper()
	data, err := os.ReadFile(forkReadFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s holds no %q to change", name, old)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), old, with, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// printsAnswer is the shell text of a curl that heard GitHub answer with the file at path: it
// prints the file, as `curl --include` prints an answer, and exits status, which is 0 for an
// answer whatever its HTTP status, as curl exits.
func printsAnswer(path string, status int) string {
	return "cat " + shellWord(path) + "\nexit " + strconv.Itoa(status) + "\n"
}

// shellWord is s as one word of a shell script, single-quoted.
func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// forkReadSecrets are the names of the caller's environment that no curl carrying a token is
// handed (leakguard.TokenChildKeeps keeps none of them): the token variables, which would put
// another credential than GH_CONFIG_DIR's beside the read, the trust stores, by which a proxy
// could read inside curl's TLS tunnel, and the TLS key log, which OpenSSL writes the
// connection's keys to.
var forkReadSecrets = []string{"GH_TOKEN", "GITHUB_TOKEN", "SSL_CERT_FILE", "CURL_CA_BUNDLE", "SSLKEYLOGFILE"}

// forkReadRoute are the names of the caller's environment that say how the fork read reaches
// GitHub, which the fake GitHub records beside forkReadSecrets: the proxy variables curl reads for
// an https URL, and the NO_PROXY lists it reads them against.
var forkReadRoute = []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"}

// forkReadRecorded are the names each run of the fake GitHub records, in the order it records them.
var forkReadRecorded = slices.Concat(forkReadSecrets, forkReadRoute)

// fakeGitHub is a stand-in curl for the helper's fork read (leakguardCurl), which says what it
// was asked and with which credential: each run appends one line to record (fakeGitHub.runs reads
// it) and writes its standard input, where the request's Authorization header comes, to stdin.
type fakeGitHub struct {
	program, record, stdin string
}

// newFakeGitHub writes a fakeGitHub. Where token is set, it answers as GitHub answers a request
// with any other credential (bad-credentials.http, 401) unless its standard input is the one
// header line `Authorization: Bearer <token>`; otherwise, and wherever token is "", it does what
// then says.
func newFakeGitHub(t *testing.T, token, then string) fakeGitHub {
	t.Helper()
	dir := t.TempDir()
	github := fakeGitHub{program: filepath.Join(dir, "curl"), record: filepath.Join(dir, "record"), stdin: filepath.Join(dir, "stdin")}
	format, values := make([]string, 0, len(forkReadRecorded)), make([]string, 0, len(forkReadRecorded))
	for _, name := range forkReadRecorded {
		format = append(format, name+"=%s")
		values = append(values, `"${`+name+`-unset}"`)
	}
	script := "#!/bin/sh\n" +
		"{ printf '" + strings.Join(format, `\t`) + "' " + strings.Join(values, " ") + `; printf '\t%s' "$@"; echo; } >> ` + shellWord(github.record) + "\n" +
		"cat > " + shellWord(github.stdin) + "\n"
	if token != "" {
		script += `if [ "$(cat ` + shellWord(github.stdin) + `)" != ` + shellWord("Authorization: Bearer "+token) + " ]; then\n" +
			printsAnswer(forkReadFixture(t, "bad-credentials.http"), 0) + "fi\n"
	}
	if err := execfile.Write(github.program, []byte(script+then), 0o700); err != nil {
		t.Fatal(err)
	}
	return github
}

// forkReadRun is one run of a fakeGitHub: its arguments after the program's own path, and the
// value it was given of each name of forkReadRecorded, `unset` for one it was not given.
type forkReadRun struct {
	argv []string
	env  map[string]string
}

// runs is every run of the fake GitHub so far, in order.
func (g fakeGitHub) runs(t *testing.T) []forkReadRun {
	t.Helper()
	data := readRecord(t, g.record)
	var runs []forkReadRun
	for line := range strings.Lines(data) {
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) < len(forkReadRecorded) {
			t.Fatalf("the fake GitHub recorded the line %q, which names no environment", line)
		}
		recorded := forkReadRun{argv: fields[len(forkReadRecorded):], env: make(map[string]string, len(forkReadRecorded))}
		for _, field := range fields[:len(forkReadRecorded)] {
			name, value, _ := strings.Cut(field, "=")
			recorded.env[name] = value
		}
		runs = append(runs, recorded)
	}
	return runs
}

// runGhGitCredentialHere runs `legion gh auth git-credential get` in process, as gh's own git
// asks it, with stdin as git's request. LEGION_GH_PATH names a real gh that records a run of it,
// and the test fails where it ran: `legion gh` answers this request itself (runGh), with the
// helper's own function.
func runGhGitCredentialHere(t *testing.T, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	ran := filepath.Join(t.TempDir(), "ran")
	gh := filepath.Join(t.TempDir(), "gh")
	if err := execfile.Write(gh, []byte("#!/bin/sh\necho ran >> "+shellWord(ran)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_GH_PATH", gh)
	previous := gitCredentialStdin
	gitCredentialStdin = strings.NewReader(stdin)
	t.Cleanup(func() { gitCredentialStdin = previous })
	var out, errb bytes.Buffer
	code = run(context.Background(), []string{"legion", "gh", "auth", "git-credential", "get"}, &out, &errb)
	if _, err := os.Stat(ran); err == nil {
		t.Errorf("legion gh auth git-credential get ran the real gh %s", gh)
	}
	return code, out.String(), errb.String()
}

// answersNothingForTheFork fails the test unless the helper, asked for the repository slug of an
// owner the bundle at root holds no directory for, answered nothing, exit 1, on one stderr that
// says so as ghConfigDirFor says it and names slug and each of says, and that holds no token.
// Every token these tests write begins `ghs_`; the one that holds a line break ends `break_write`
// after it.
func answersNothingForTheFork(t *testing.T, code int, stdout, stderr, root, slug string, says ...string) {
	t.Helper()
	owner, _, _ := strings.Cut(slug, "/")
	if code != 1 || stdout != "" {
		t.Errorf("legion git-credential get for %s = exit %d, stdout %q, stderr %q; want exit 1 and no answer", slug, code, stdout, stderr)
	}
	unserved := `the role's GitHub App is not installed on "` + strings.ToLower(owner) + `": ` + root + " holds no gh directory for it"
	for _, want := range append([]string{unserved, slug}, says...) {
		if !strings.Contains(stderr, want) {
			t.Errorf("legion git-credential get for %s printed %q on stderr, which does not name %q", slug, stderr, want)
		}
	}
	if strings.Contains(stderr, "ghs_") || strings.Contains(stderr, "break_write") {
		t.Errorf("legion git-credential get for %s printed %q on stderr, which holds a token", slug, stderr)
	}
}

// R2: a repository whose owner the bundle holds no directory for, which is a fork, is read from
// GitHub (GET /repos/<owner>/<name> through the curl leakguardCurl names), and `legion
// git-credential get` answers the token it would answer for the fork's parent where the bundle
// holds a directory for the parent's owner. A push to an outside author's fork of a served
// repository, the head of a pull request to it, then carries the token the role holds for the
// served repository, and not nothing, which git ended every such push on. The owner and the name
// are asked in another case and with `.git`, as git may send them, and the answer is the same;
// the parent is routed as every request is (ghConfigDirFor), so a parent that a configured rename
// moved is answered from the directory of the owner it moved to; and `legion gh auth
// git-credential get`, which gh's own git asks, answers the same through the same function. Each
// answer comes from one read.
func TestGitCredentialAnswersAForksParentToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGhGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	// The bundle appauth.GitHubCredential builds with the configured rename sjawhar/legion ->
	// cybermonklabs/legion and the App installed on both owners of it (pairRoutedBundle):
	// sjawhar's directory holds sjawhar's own lease, so only the pair's routing answers
	// cybermonklabs', and the bundle holds no directory for acme, the source's owner.
	renamed := pairRoutedBundle(t)
	forkOfAcme := forkReadFixture(t, "fork-of-a-served-repository.http")
	// fork-of-a-served-repository.http with its parent's full_name changed to sjawhar/legion: of
	// its two `"full_name": "acme/sample-project"` lines, the first is the parent's and the second
	// the source's.
	movedParent := derivedForkRead(t, "fork-of-a-served-repository.http", `"full_name": "acme/sample-project"`, `"full_name": "sjawhar/legion"`)
	for _, tc := range []struct {
		name string
		// root is the bundle, configDir the directory of it GH_CONFIG_DIR names, and readToken
		// that directory's token, the one the fake GitHub takes.
		root, configDir, readToken string
		path                       string // the path git asks for
		answer                     string // the file the fake GitHub prints
		viaGh                      bool   // asked through `legion gh auth git-credential get`
		token                      string // the token answered
	}{
		{
			name: "a fork of a served repository", root: root, configDir: acme, readToken: "ghs_acme_write",
			path: "outside-author/sample-project", answer: forkOfAcme, token: "ghs_acme_write",
		},
		{
			name: "the owner and name asked in another case", root: root, configDir: acme, readToken: "ghs_acme_write",
			path: "Outside-Author/Sample-Project", answer: forkOfAcme, token: "ghs_acme_write",
		},
		{
			name: "a .git suffix on the path", root: root, configDir: acme, readToken: "ghs_acme_write",
			path: "outside-author/sample-project.git", answer: forkOfAcme, token: "ghs_acme_write",
		},
		{
			name: "a parent that a configured rename moved", root: renamed, configDir: ghconfig.Dir(renamed, "cybermonklabs"),
			readToken: "ghs_implement_write_cybermonklabs",
			path:      "outside-author/sample-project.git", answer: movedParent, token: "ghs_implement_write_cybermonklabs",
		},
		{
			name: "through legion gh auth git-credential get", root: root, configDir: acme, readToken: "ghs_acme_write",
			path: "outside-author/sample-project.git", answer: forkOfAcme, viaGh: true, token: "ghs_acme_write",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHub(t, tc.readToken, printsAnswer(tc.answer, 0))
			useForkRead(t, github.program, forkReadBound)
			forkReadPane(t, tc.root, tc.configDir)
			var code int
			var stdout, stderr string
			if tc.viaGh {
				code, stdout, stderr = runGhGitCredentialHere(t, forkRequest(tc.path))
			} else {
				code, stdout, stderr = runGitCredentialHere(t, forkRequest(tc.path), "get")
			}
			if want := "username=x-access-token\npassword=" + tc.token + "\n"; code != 0 || stdout != want {
				t.Fatalf("legion git-credential get for %s = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.path, code, stdout, stderr, want)
			}
			if runs := github.runs(t); len(runs) != 1 {
				t.Errorf("the fake GitHub ran %d times, want one read", len(runs))
			}
		})
	}
}

// R2: a fork whose parent's owner the bundle holds no directory for is answered with the token
// of its source, the repository at the root of its fork network, where the bundle holds the
// source owner's directory. fork-of-a-fork.http is such a fork: its parent is
// outside-org/sample-project, itself a fork, and its source acme/sample-project.
func TestGitCredentialAnswersAForksSourceTokenWhenItsParentIsNotServed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	github := newFakeGitHub(t, "ghs_acme_write", printsAnswer(forkReadFixture(t, "fork-of-a-fork.http"), 0))
	useForkRead(t, github.program, forkReadBound)
	forkReadPane(t, root, acme)
	code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project.git"), "get")
	if want := "username=x-access-token\npassword=ghs_acme_write\n"; code != 0 || stdout != want {
		t.Fatalf("legion git-credential get for a fork of a fork = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, want)
	}
	if runs := github.runs(t); len(runs) != 1 {
		t.Errorf("the fake GitHub ran %d times, want one read", len(runs))
	}
}

// R3: a fork whose parent's and source's owners the bundle both holds a directory for is
// answered with the parent's token, not the source's: the parent is the repository the fork was
// made from, and so the nearer of the two. GH_CONFIG_DIR is a third owner's directory, so the
// token the read carries is neither answer.
func TestGitCredentialPrefersTheParentWhenBothOwnersAreServed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, sjawhar := forkBundle(t, "sjawhar", "sjawhar", "outside-org", "acme")
	github := newFakeGitHub(t, forkToken("sjawhar"), printsAnswer(forkReadFixture(t, "fork-of-a-fork.http"), 0))
	useForkRead(t, github.program, forkReadBound)
	forkReadPane(t, root, sjawhar)
	code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project.git"), "get")
	if want := "username=x-access-token\npassword=" + forkToken("outside-org") + "\n"; code != 0 || stdout != want {
		t.Fatalf("legion git-credential get for a fork of a fork = exit %d, stdout %q, stderr %q; want exit 0 and the parent's %q", code, stdout, stderr, want)
	}
	if runs := github.runs(t); len(runs) != 1 {
		t.Errorf("the fake GitHub ran %d times, want one read", len(runs))
	}
}

// R2: the fork read carries the token of GH_CONFIG_DIR's directory, whichever directory of the
// bundle that is, and no other credential. The token goes to curl on its standard input, as the
// one header line `Authorization: Bearer <token>`, and never in its argv, which any process of
// the machine can read; and curl is handed none of the caller's token variables, trust stores or
// TLS key log (forkReadSecrets), which the test sets in its own environment to see whether they
// reach it. The fake GitHub answers 401 to any other header, so the answer, acme's token from
// either directory, proves which token went. curl's last argument is the repository git named at
// https://api.github.com, and the value after its --max-time is seconds of one decimal inside the
// read's bound (programbound.Of of forkReadBound: the product's 10 s, and in this binary what is
// left of its deadline when the read starts, read here before the read so it is no less than the
// read's), and no less than that bound less the time the call took, curlMargin and the tenth the
// guard rounds down to; leakguard's own tests read the guard's other curl options (Guard.CurlGet).
func TestGitCredentialReadsTheForkWithGHConfigDirsToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	forkOfAcme := forkReadFixture(t, "fork-of-a-served-repository.http")
	for _, tc := range []struct{ name, configDir, token string }{
		{name: "GH_CONFIG_DIR is the issue owner's directory", configDir: acme, token: "ghs_acme_write"},
		{name: "GH_CONFIG_DIR is another directory of the bundle", configDir: ghconfig.Dir(root, "sjawhar"), token: "ghs_sjawhar_write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range forkReadSecrets {
				t.Setenv(name, "the-callers-"+name)
			}
			github := newFakeGitHub(t, tc.token, printsAnswer(forkOfAcme, 0))
			useForkRead(t, github.program, forkReadBound)
			forkReadPane(t, root, tc.configDir)
			readAt := time.Now()
			bound := programbound.Of(&forkReadBound)
			code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project.git"), "get")
			took := time.Since(readAt)
			if want := "username=x-access-token\npassword=ghs_acme_write\n"; code != 0 || stdout != want {
				t.Fatalf("legion git-credential get for outside-author/sample-project = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, want)
			}
			runs := github.runs(t)
			if len(runs) != 1 {
				t.Fatalf("the fake GitHub ran %d times, want one read", len(runs))
			}
			read := runs[0]
			if header, err := os.ReadFile(github.stdin); err != nil || string(header) != "Authorization: Bearer "+tc.token+"\n" {
				t.Errorf("curl read %q on its standard input (%v), want the one header line of %s's token", header, err, tc.configDir)
			}
			for _, name := range forkReadSecrets {
				if value := read.env[name]; value != "unset" {
					t.Errorf("curl was handed %s=%q, want no %s", name, value, name)
				}
			}
			for _, arg := range read.argv {
				if strings.Contains(arg, tc.token) {
					t.Errorf("curl's argv %q holds the token", read.argv)
				}
			}
			// What the fork read hands the guard's curl read (leakguard's Guard.CurlGet): the
			// repository git named, and a --max-time inside the read's bound, bound. The guard's
			// own tests read its other options.
			const wantURL = "https://api.github.com/repos/outside-author/sample-project"
			if len(read.argv) == 0 || read.argv[len(read.argv)-1] != wantURL {
				t.Errorf("curl ran with %q, want %s as its last argument", read.argv, wantURL)
			}
			maxTime := ""
			if at := slices.Index(read.argv, "--max-time"); at >= 0 && at+1 < len(read.argv) {
				maxTime = read.argv[at+1]
			}
			seconds, err := strconv.ParseFloat(maxTime, 64)
			least := bound - took - forkCurlMargin - 100*time.Millisecond
			if _, decimals, _ := strings.Cut(maxTime, "."); err != nil || len(decimals) != 1 || seconds < least.Seconds() || seconds > bound.Seconds() {
				t.Errorf("curl ran with %q, whose --max-time is %q; want seconds of one decimal, at least %s (the read's bound less the call's %s, curlMargin and a tenth) and at most the read's bound of %s",
					read.argv, maxTime, least.Round(100*time.Millisecond), took.Round(time.Millisecond), bound.Round(100*time.Millisecond))
			}
		})
	}
}

// Inside `agent-secrets NAME -- git push`, both HTTPS_PROXY and https_proxy name the secrets
// broker's airlock proxy, whose URL's user is cipheraddr.AirlockProxyUser. The fork read carries
// GH_CONFIG_DIR's directory's token (leakguard.DirectoryToken), so curl keeps neither of them and
// no ALL_PROXY or all_proxy, and connects to GitHub directly; it keeps the caller's NO_PROXY and
// no_proxy as they are.
func TestGitCredentialsForkReadLeavesTheAirlocksProxyOut(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	airlock := "http://" + cipheraddr.AirlockProxyUser + ":placeholder@broker.internal.example:13381"
	want := map[string]string{
		"HTTPS_PROXY": "unset", "https_proxy": "unset", "ALL_PROXY": "unset", "all_proxy": "unset",
		"NO_PROXY": "localhost,.internal", "no_proxy": "localhost,.svc",
	}
	t.Setenv("HTTPS_PROXY", airlock)
	t.Setenv("https_proxy", airlock)
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("all_proxy", "http://127.0.0.1:2")
	t.Setenv("NO_PROXY", want["NO_PROXY"])
	t.Setenv("no_proxy", want["no_proxy"])
	github := newFakeGitHub(t, "ghs_acme_write", printsAnswer(forkReadFixture(t, "fork-of-a-served-repository.http"), 0))
	useForkRead(t, github.program, forkReadBound)
	forkReadPane(t, root, acme)
	if code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project.git"), "get"); code != 0 || stdout != "username=x-access-token\npassword=ghs_acme_write\n" {
		t.Fatalf("legion git-credential get for outside-author/sample-project = exit %d, stdout %q, stderr %q; want exit 0 and acme's token", code, stdout, stderr)
	}
	runs := github.runs(t)
	if len(runs) != 1 {
		t.Fatalf("the fake GitHub ran %d times, want one read", len(runs))
	}
	for _, name := range forkReadRoute {
		if got := runs[0].env[name]; got != want[name] {
			t.Errorf("curl was handed %s=%q, want %q", name, got, want[name])
		}
	}
}

// forkCurlMargin is leakguard's curlMargin: the time the guard keeps back from a curl read's
// --max-time, so curl stops itself before the read's deadline kills it.
const forkCurlMargin = 500 * time.Millisecond

// R2: a repository of an owner the bundle holds no directory for that GitHub answers, but that
// is no fork of a repository the bundle serves, gets nothing, exit 1, after one read: a
// repository that is no fork, named as no fork (not-a-fork.http, whose full_name differs from
// the name asked in case alone, which is the same repository); a fork whose parent's and
// source's owners both lack a directory, named with the parent and the source tried; and an
// answer for another repository than the one asked, named with both names, since no token for
// the repository asked can be read from it.
func TestGitCredentialAnswersNothingForAnOutsideRepositoryThatIsNoServedFork(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	neither, sjawhar := forkBundle(t, "sjawhar", "sjawhar", "example-org")
	// fork-of-a-served-repository.http with its own full_name, the first full_name of the file,
	// changed to outside-author/other-repository.
	another := derivedForkRead(t, "fork-of-a-served-repository.http", `"full_name": "outside-author/sample-project"`, `"full_name": "outside-author/other-repository"`)
	for _, tc := range []struct {
		name                       string
		root, configDir, readToken string
		slug                       string // the repository asked
		answer                     string // the file the fake GitHub prints
		says                       []string
	}{
		{
			name: "no fork", root: root, configDir: acme, readToken: "ghs_acme_write",
			slug: "outside-org/sample-library", answer: forkReadFixture(t, "not-a-fork.http"),
			says: []string{"is no fork"},
		},
		{
			name: "a fork whose parent's and source's owners are outside the bundle", root: neither, configDir: sjawhar, readToken: forkToken("sjawhar"),
			slug: "outside-author/sample-project", answer: forkReadFixture(t, "fork-of-a-fork.http"),
			says: []string{"outside-org/sample-project", "acme/sample-project"},
		},
		{
			name: "a full_name of another repository", root: root, configDir: acme, readToken: "ghs_acme_write",
			slug: "outside-author/sample-project", answer: another,
			says: []string{"outside-author/other-repository"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHub(t, tc.readToken, printsAnswer(tc.answer, 0))
			useForkRead(t, github.program, forkReadBound)
			forkReadPane(t, tc.root, tc.configDir)
			code, stdout, stderr := runGitCredentialHere(t, forkRequest(tc.slug), "get")
			answersNothingForTheFork(t, code, stdout, stderr, tc.root, tc.slug, tc.says...)
			if runs := github.runs(t); len(runs) != 1 {
				t.Errorf("the fake GitHub ran %d times, want one read", len(runs))
			}
		})
	}
}

// R2: every other answer of the fork read, and every read that cannot be made, is nothing, exit
// 1, naming the repository and the cause, with no second read: GitHub's 404, 401 and 301 (a
// fork that moved, whose redirect curl does not follow), each named by its status and the 301 by
// where it points; a spent core limit, named with its reset in UTC, and a secondary limit, named
// with the wait its Retry-After asks for; a curl that could not reach GitHub; a body that is not
// JSON; a 200 with no fork field; a fork whose parent is a repository of an owner the bundle
// holds a directory for, which a configured rename moved to an owner it holds none for, so that a
// request for the parent answers nothing (ghConfigDirFor) and so does the fork, naming the parent
// and the pair, with its source, acme/sample-project, not tried though the bundle holds acme's
// directory; and no curl at the path. No read is made without a token: where GH_CONFIG_DIR is
// unset, is no directory of the bundle, holds a hosts.yml with no token, or a token with a line
// break, which would put a header of its own on curl's request, the helper names which and the
// fake GitHub never runs. No stderr holds a token.
func TestGitCredentialAnswersNothingWhenTheForkReadFails(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, acme := gitCredentialBundle(t)
	// root's one configured rename, sjawhar/legion -> cybermonklabs/legion, moves a repository of
	// sjawhar, an owner root holds a directory for, to cybermonklabs, which it holds none for. Only
	// the row whose fork's parent is sjawhar/legion routes through it.
	pairs := ghconfig.RenderPairs([]ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")})
	if err := os.WriteFile(filepath.Join(root, ghconfig.PairsFile), []byte(pairs), 0o600); err != nil {
		t.Fatal(err)
	}
	// Three directories whose token the read cannot carry: one of the bundle whose hosts.yml is
	// empty, one of the bundle whose token holds a line break, and one outside the bundle with a
	// hosts.yml of its own, named for an owner of it.
	empty := ghconfig.Dir(root, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, ghconfig.HostsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const lineBreakToken = "ghs_line\nbreak_write"
	lineBreak, rendered := ghconfig.Dir(root, "line-break"), ghconfig.Render(lineBreakToken, "implement", time.Now().Add(time.Hour))
	if token, err := ghconfig.TokenFromHosts([]byte(rendered.Hosts)); err != nil || token != lineBreakToken {
		t.Fatalf("the hosts.yml rendered for %q reads back as %q, %v", lineBreakToken, token, err)
	}
	if _, err := ghconfig.Write(lineBreak, rendered); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "acme")
	if _, err := ghconfig.Write(outside, ghconfig.Render("ghs_outside_the_bundle_write", "implement", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	// fork-of-a-served-repository.http with its body's opening brace replaced by `<`, as a
	// proxy's HTML page would begin; the blank line before it ends the headers.
	notJSON := derivedForkRead(t, "fork-of-a-served-repository.http", "\r\n\r\n{", "\r\n\r\n<")
	// fork-of-a-served-repository.http without its own `"fork": true,` line; the parent's and
	// the source's say `"fork": false,`.
	noFork := derivedForkRead(t, "fork-of-a-served-repository.http", "  \"fork\": true,\n", "")
	// fork-of-a-served-repository.http with its parent's full_name changed to sjawhar/legion, the
	// repository root's pair moves: of its two `"full_name": "acme/sample-project"` lines, the first
	// is the parent's and the second the source's, which stays acme's.
	movedParent := derivedForkRead(t, "fork-of-a-served-repository.http", `"full_name": "acme/sample-project"`, `"full_name": "sjawhar/legion"`)
	forkOfAcme := printsAnswer(forkReadFixture(t, "fork-of-a-served-repository.http"), 0)
	for _, tc := range []struct {
		name string
		// configDir is GH_CONFIG_DIR, none where it is "", and token its token, the one the
		// fake GitHub takes, which takes any where it is "".
		configDir, token string
		answer           string // what the fake GitHub does: the shell text of its answer
		noCurl           bool   // leakguardCurl names a path with no file
		reads            int    // how many times the fake GitHub runs
		// says is what stderr names beside the repository, <GH_CONFIG_DIR> and <curl> standing
		// for GH_CONFIG_DIR and leakguardCurl.
		says []string
	}{
		{
			name: "404", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: curlNotFound(t), says: []string{"the status 404"},
		},
		{
			// bad-credentials.http, whatever the token.
			name: "401", configDir: acme, reads: 1,
			answer: printsAnswer(forkReadFixture(t, "bad-credentials.http"), 0), says: []string{"the status 401"},
		},
		{
			name: "301 for a fork that moved", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: printsAnswer(forkReadFixture(t, "moved.http"), 0),
			says:   []string{"the status 301", "https://api.github.com/repositories/1146927990"},
		},
		{
			name: "a spent core limit", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: printsAnswer(forkReadFixture(t, "core-limit-spent.http"), 0),
			says:   []string{"rate limit is spent until 2026-11-14T06:00:00Z UTC"},
		},
		{
			name: "a secondary limit", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: printsAnswer(forkReadFixture(t, "secondary-limit.http"), 0),
			says:   []string{"secondary rate limit", "60 s"},
		},
		{
			// curl's own words for a connection refused, exit 7, with nothing on stdout.
			name: "curl's transport failure", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: "printf '%s\\n' 'curl: (7) Failed to connect to api.github.com port 443' >&2\nexit 7\n",
			says:   []string{"curl could not reach GitHub"},
		},
		{
			name: "a body that is not JSON", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: printsAnswer(notJSON, 0), says: []string{"not JSON"},
		},
		{
			name: "a 200 with no fork field", configDir: acme, token: "ghs_acme_write", reads: 1,
			answer: printsAnswer(noFork, 0), says: []string{`no "fork" field`},
		},
		{
			// A request for sjawhar/legion answers nothing, so the fork answers nothing too, and not
			// acme's token, its source's, though the bundle holds acme's directory.
			name:      "a fork of a served owner's repository a configured rename moved to an owner with no directory",
			configDir: acme, token: "ghs_acme_write", reads: 1, answer: printsAnswer(movedParent, 0),
			says: []string{
				"a fork of sjawhar/legion",
				"the configured rename sjawhar/legion -> cybermonklabs/legion serves sjawhar/legion from cybermonklabs's gh directory",
				"its source is not tried",
			},
		},
		{
			name: "no curl at the path", configDir: acme, noCurl: true,
			says: []string{"there is no curl at", "<curl>"},
		},
		{
			name: "GH_CONFIG_DIR unset", answer: forkOfAcme,
			says: []string{"GH_CONFIG_DIR is unset"},
		},
		{
			name: "GH_CONFIG_DIR outside the bundle", configDir: outside, answer: forkOfAcme,
			says: []string{"GH_CONFIG_DIR", "<GH_CONFIG_DIR>", "is no directory of the bundle"},
		},
		{
			name: "GH_CONFIG_DIR's hosts.yml holds no token", configDir: empty, answer: forkOfAcme,
			says: []string{"names no github.com oauth_token"},
		},
		{
			name: "a token with a line break", configDir: lineBreak, answer: forkOfAcme,
			says: []string{"holds a line break"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHub(t, tc.token, tc.answer)
			curl := github.program
			if tc.noCurl {
				curl = filepath.Join(t.TempDir(), "curl")
			}
			useForkRead(t, curl, forkReadBound)
			forkReadPane(t, root, tc.configDir)
			code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project"), "get")
			paths := strings.NewReplacer("<GH_CONFIG_DIR>", tc.configDir, "<curl>", curl)
			says := make([]string, 0, len(tc.says))
			for _, said := range tc.says {
				says = append(says, paths.Replace(said))
			}
			answersNothingForTheFork(t, code, stdout, stderr, root, "outside-author/sample-project", says...)
			if runs := github.runs(t); len(runs) != tc.reads {
				t.Errorf("the fake GitHub ran %d times, want %d", len(runs), tc.reads)
			}
		})
	}
}

// R2: where the directory the fork read routed to holds a hosts.yml that names no token, the
// helper answers nothing, exit 1, on a line that names the fork, that directory and the parent
// that routed there: the token read's own error names the directory alone, which tells a person
// reading git's refusal neither which fork was read nor why that directory was asked. The bundle
// holds sjawhar's directory, which GH_CONFIG_DIR names and whose token the read carries, and
// acme's, the parent's owner's, with an empty hosts.yml. No stderr holds a token.
func TestGitCredentialNamesTheForksRouteWhenItsParentHoldsNoToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, sjawhar := forkBundle(t, "sjawhar", "sjawhar")
	acme := ghconfig.Dir(root, "acme")
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acme, ghconfig.HostsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	github := newFakeGitHub(t, forkToken("sjawhar"), printsAnswer(forkReadFixture(t, "fork-of-a-served-repository.http"), 0))
	useForkRead(t, github.program, forkReadBound)
	forkReadPane(t, root, sjawhar)
	code, stdout, stderr := runGitCredentialHere(t, forkRequest("outside-author/sample-project.git"), "get")
	if code != 1 || stdout != "" {
		t.Errorf("legion git-credential get for outside-author/sample-project = exit %d, stdout %q, stderr %q; want exit 1 and no answer", code, stdout, stderr)
	}
	for _, want := range []string{
		"legion git-credential: the fork outside-author/sample-project routes to " + acme + " through its parent acme/sample-project, and ",
		"names no github.com oauth_token; answering nothing",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("legion git-credential get for outside-author/sample-project printed %q on stderr, which does not name %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "ghs_") {
		t.Errorf("legion git-credential get for outside-author/sample-project printed %q on stderr, which holds a token", stderr)
	}
	if runs := github.runs(t); len(runs) != 1 {
		t.Errorf("the fake GitHub ran %d times, want one read", len(runs))
	}
}

// R2: a fork read that GitHub never answers is stopped, and with it what the curl started: curl
// runs in a process group of its own (leakguard's Guard.run), and the read's end stops that whole
// group. A stop that killed curl alone would leave what curl started running, reparented and
// waited for by nobody.
//
// The row reads that in two reads, as TestAHelpReadThatTimesOutStopsWhatGhStarted does, so that
// no verdict of it is a race with the clock. In the first, the read's bound is 2 s and the fake
// GitHub never answers: the helper answers nothing, exit 1, naming the bound. In the second, the
// fake GitHub starts a sleep of its own, detached from its output so nothing holds the read's
// pipes, records that sleep's PID and never answers, and the helper's context ends once the sleep
// is recorded: the helper answers nothing, naming the cancel, and the sleep is gone too. Guard.run
// gives curl the one stop procgroup makes whichever end of the read's context comes, its bound's or
// this one. In one read under the bound of 2 s, a loaded machine could let the bound stop curl
// before curl had recorded its sleep. Each wait, the helper's answer included, has the test
// binary's deadline as its only bound, and each stand-in sleeps about a day, so a stop that never
// comes fails the row by name and no sleep ends by itself before it.
func TestGitCredentialStopsAForkReadThatNeverAnswers(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (startGitCredentialGet) and leakguardCurl (useForkRead).
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this test reads from it whether the fake GitHub's sleep is still running", err)
	}
	t.Run("a read GitHub never answers is stopped at its bound", func(t *testing.T) {
		root, acme := gitCredentialBundle(t)
		curlPid := filepath.Join(t.TempDir(), "curl.pid")
		const curlSeconds = "86388"
		github := newFakeGitHub(t, "ghs_acme_write", holdsStandIn(curlPid, curlSeconds))
		useForkRead(t, github.program, 2*time.Second)
		forkReadPane(t, root, acme)
		killRecordedHelper(t, curlPid, curlSeconds)
		answered, _ := startGitCredentialGet(t, forkRequest("outside-author/sample-project"))
		got := testwait.Receive(t, "legion git-credential get to answer once the fork read's bound of 2 s passed", answered)
		answersNothingForTheFork(t, got.code, got.stdout, got.stderr, root, "outside-author/sample-project")
		// The guard gives curl what is left of the read's 2 s when curl starts, and names it in
		// whole seconds (leakguard's Guard.run), so a loaded machine that starts curl late names
		// less; one that leaves curl too little refuses the read before curl starts (curlTime).
		if said := forkReadStopped.FindStringSubmatch(got.stderr); said != nil {
			ran, _ := strconv.Atoi(said[1])
			bound, _ := strconv.Atoi(said[2])
			if bound <= 0 || bound > 2 || ran < bound {
				t.Errorf("the helper printed %q: curl ran %d s under a bound of %d s, want a bound more than 0 and no more than the read's 2 s, and a run no shorter than it", got.stderr, ran, bound)
			}
		} else if strings.Contains(got.stderr, forkReadNoTime) {
			t.Logf("the read's 2 s left curl too little before it started: %q", got.stderr)
		} else {
			t.Errorf("the helper printed %q, want curl stopped by its bound, or the read refused for too little time left", got.stderr)
		}
	})
	t.Run("the stop reaches the sleep curl started", func(t *testing.T) {
		root, acme := gitCredentialBundle(t)
		dir := t.TempDir()
		curlPid, sleepPid := filepath.Join(dir, "curl.pid"), filepath.Join(dir, "sleep.pid")
		const curlSeconds, sleepSeconds = "86390", "86389"
		github := newFakeGitHub(t, "ghs_acme_write",
			"sleep "+sleepSeconds+" </dev/null >/dev/null 2>&1 &\necho $! > "+shellWord(sleepPid)+"\n"+holdsStandIn(curlPid, curlSeconds))
		useForkRead(t, github.program, forkReadBound)
		forkReadPane(t, root, acme)
		killRecordedHelper(t, sleepPid, sleepSeconds)
		killRecordedHelper(t, curlPid, curlSeconds)
		answered, stop := startGitCredentialGet(t, forkRequest("outside-author/sample-project"))
		var pid int
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll, "the fake GitHub to record the sleep it starts", func() bool {
			var running bool
			pid, running = recordedHelper(sleepPid, sleepSeconds)
			return running
		})
		stop()
		got := testwait.Receive(t, "legion git-credential get to answer once its context ended", answered)
		answersNothingForTheFork(t, got.code, got.stdout, got.stderr, root, "outside-author/sample-project", "stopped by a cancel")
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, testBoundMargin), testwait.Poll,
			fmt.Sprintf("the fake GitHub's sleep, PID %d, to end once the helper answered: want the read's end to stop curl's whole process group", pid),
			func() bool { return !helperRunning(pid, sleepSeconds) })
	})
}

// forkReadStopped is how the helper says a fork read its bound stopped: the whole seconds curl ran,
// and the bound the guard gave it (leakguard's stoppedError).
var forkReadStopped = regexp.MustCompile(`curl ran (\d+) s and was stopped by its bound of (\d+) s`)

// forkReadNoTime is how the helper says a fork read whose bound left curl too little time before
// curl started (leakguard's curlTime).
const forkReadNoTime = "too little of the caller's bound is left for the read through curl"

// gitCredentialAnswer is what one `legion git-credential get` answered: its exit status and what
// it printed.
type gitCredentialAnswer struct {
	code           int
	stdout, stderr string
}

// startGitCredentialGet starts `legion git-credential get` in process, as runGitCredentialHere
// runs it, with stdin as git's request and under a context of its own, and answers the channel its
// answer comes on and that context's cancel. The end of t cancels the context and waits for the
// call before the cleanups registered earlier run, so a call the row gave up on is stopped, and
// never reads the helper's stdin or the fork read's curl and bound while a cleanup puts them back.
func startGitCredentialGet(t *testing.T, stdin string) (<-chan gitCredentialAnswer, context.CancelFunc) {
	t.Helper()
	previous := gitCredentialStdin
	gitCredentialStdin = strings.NewReader(stdin)
	t.Cleanup(func() { gitCredentialStdin = previous })
	ctx, cancel := context.WithCancel(context.Background())
	answered, ended := make(chan gitCredentialAnswer, 1), make(chan struct{})
	go func() {
		defer close(ended)
		var out, errb bytes.Buffer
		code := run(ctx, []string{"legion", "git-credential", "get"}, &out, &errb)
		answered <- gitCredentialAnswer{code: code, stdout: out.String(), stderr: errb.String()}
	}()
	t.Cleanup(func() {
		cancel()
		<-ended
	})
	return answered, cancel
}

// R2: a request for a repository of an owner the bundle holds a directory for is answered as it
// was before the fork read, and no read is made: a served owner's repository gets that owner's
// token; one a configured rename moved to an owner with no directory keeps ghConfigDirFor's
// refusal naming the pair, since the path's own owner is served; and a path naming only an
// outside owner, or an outside owner and a name GitHub serves no repository under (ghrepo.Parse
// refuses `..`), answers nothing, as before, with no repository to read. So does an outside
// owner's repository in a bundle whose pairs file ghconfig.ReadPairs refuses, naming that file's
// error, with no read: neither the fork's parent nor its source could be routed by that file. The
// fake GitHub answers any read with a fork of acme's repository, so a read would hand out acme's
// token.
func TestGitCredentialMakesNoReadForAServedOwner(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (forkReadPane); it sets the package variables forkReadBound (useForkRead), gitCredentialStdin (runGitCredentialHere) and leakguardCurl (useForkRead).
	root, _ := gitCredentialBundle(t)
	// A bundle whose configured rename moved sjawhar/legion to cybermonklabs, which the bundle
	// holds no directory for.
	moved := filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(moved, ghconfig.Bundle{
		Default: "acme",
		Dirs: map[string]ghconfig.Rendered{
			"acme":    ghconfig.Render(forkToken("acme"), "implement", expires),
			"sjawhar": ghconfig.Render(forkToken("sjawhar"), "implement", expires),
		},
		Pairs: []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")},
	}); err != nil {
		t.Fatal(err)
	}
	// A bundle whose pairs file holds a line that is no pair.
	broken, _ := gitCredentialBundle(t)
	if err := os.WriteFile(filepath.Join(broken, ghconfig.PairsFile), []byte("not-a-pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, pairsErr := ghconfig.ReadPairs(broken)
	if pairsErr == nil {
		t.Fatalf("ghconfig.ReadPairs read the pairs file of %s, want a refusal", broken)
	}
	unserved := `the role's GitHub App is not installed on "outside-author": ` + root + " holds no gh directory for it"
	for _, tc := range []struct {
		name, root, path string
		code             int
		stdout, stderrIn string
	}{
		{
			name: "a served owner's repository", root: root, path: "acme/widgets.git",
			stdout: "username=x-access-token\npassword=ghs_acme_write\n",
		},
		{
			name: "a served owner's repository a configured rename moved to an owner with no directory", root: moved, path: "sjawhar/legion.git",
			code: 1, stderrIn: `the configured rename sjawhar/legion -> cybermonklabs/legion serves sjawhar/legion from cybermonklabs's gh directory, and the role's GitHub App is not installed on "cybermonklabs"`,
		},
		{name: "a path naming only an outside owner", root: root, path: "outside-author", code: 1, stderrIn: unserved},
		{name: "a path whose name is no GitHub name", root: root, path: "outside-author/..", code: 1, stderrIn: unserved},
		{
			name: "an outside owner's repository under a pairs file that cannot be read", root: broken, path: "outside-author/sample-project.git",
			code: 1, stderrIn: "read the gh bundle's configured renames: " + pairsErr.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newFakeGitHub(t, "", printsAnswer(forkReadFixture(t, "fork-of-a-served-repository.http"), 0))
			useForkRead(t, github.program, forkReadBound)
			forkReadPane(t, tc.root, ghconfig.Dir(tc.root, "acme"))
			code, stdout, stderr := runGitCredentialHere(t, forkRequest(tc.path), "get")
			if code != tc.code || stdout != tc.stdout || !strings.Contains(stderr, tc.stderrIn) {
				t.Errorf("legion git-credential get for %s = exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr containing %q",
					tc.path, code, stdout, stderr, tc.code, tc.stdout, tc.stderrIn)
			}
			if runs := github.runs(t); len(runs) != 0 {
				t.Errorf("the fake GitHub ran %d times, want no read", len(runs))
			}
		})
	}
}
