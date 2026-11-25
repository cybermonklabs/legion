package main

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// ghChildEnviron is the environment `legion gh` runs gh with, and the git this command runs to
// route a call (ghroute.ReadGitRemotes), built in one place so the two never read a different
// repository: this package's test hook (ghChildTestEnviron), then the names of environ that gh is
// allowed (ghChildKeeps), then ghChildForced and set, then the git configuration gh's git gets
// (ghChildGitConfig), each later one winning, as exec takes the last value of a name. The hook
// comes first so that a test of the allowlist reads it: a caller's name the allowlist wrongly kept
// would win over the hook's own. It is an allowlist and never the caller's whole environment less
// some names: a trust store the caller names (SSL_CERT_FILE, SSL_CERT_DIR, CURL_CA_BUNDLE,
// GIT_SSL_CAINFO) would let a proxy read gh's token, an editor, browser or pager program runs with
// the call's token directory, a GIT_DIR or GIT_WORK_TREE would point gh's git at another repository
// than the one this command routed by, and a name a later gh learns to read reaches it from nobody
// until it is named here.
func ghChildEnviron(environ []string, set map[string]string) []string {
	forced := maps.Clone(ghChildForced)
	maps.Copy(forced, set)
	out := make([]string, 0, len(forced)+len(ghChildTestEnviron)+32)
	out = append(out, ghChildTestEnviron...)
	for _, pair := range environ {
		if name, _, _ := strings.Cut(pair, "="); forced[name] == "" && ghChildKeeps(name) {
			out = append(out, pair)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(forced)) {
		out = append(out, name+"="+forced[name])
	}
	return append(out, ghChildGitConfig(environ)...)
}

// ghChildForced are the names every gh `legion gh` runs gets, whatever its caller's environment
// holds, beside the GH_CONFIG_DIR, GH_PATH and GH_PAGER runGh sets per call:
//
//   - GH_NO_UPDATE_NOTIFIER and GH_PROMPT_DISABLED, so gh asks no question and checks for no
//     update, which a TTY would otherwise reach;
//   - GH_EDITOR, ghNoEditor, so an editor gh starts fails before it writes anything;
//   - GH_TELEMETRY=0, which gh reads ahead of DO_NOT_TRACK and its config and takes as off
//     (cli/cli v2.104.0 internal/telemetry/telemetry.go:90,104-129);
//   - GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1, so the git gh runs reads neither the
//     caller's ~/.gitconfig nor $XDG_CONFIG_HOME/git/config nor a file GIT_CONFIG_GLOBAL names nor
//     the system's, nor any file one of them includes: each could name an http.proxy and an
//     http.sslCAInfo, and the token gh's credential helper answers would go to that proxy. What
//     git still reads is the repository's own configuration, which runGh judges before gh runs
//     (ghRepoGitConfigRefusal), and the command line ghChildGitConfig and withGitHubUseHTTPPath
//     give it.
var ghChildForced = map[string]string{
	"GH_NO_UPDATE_NOTIFIER": "1",
	"GH_PROMPT_DISABLED":    "1",
	"GH_EDITOR":             ghNoEditor,
	"GH_TELEMETRY":          "0",
	"GIT_CONFIG_GLOBAL":     "/dev/null",
	"GIT_CONFIG_NOSYSTEM":   "1",
}

// ghChildTestEnviron is what this package's tests hand gh on top of its environment, set by their
// TestMain alone (testGhChildEnviron): the test stand-ins' own variables and the capture proxy and
// the CA it signs with, through which a real gh's every request is read (gh_capture_test.go). The
// legion binary holds no code that sets it, so no caller of it can.
var ghChildTestEnviron []string

// ghChildKeeps is whether gh keeps the name of its caller's environment: what a program carrying a
// GitHub token keeps (leakguard.TokenChildKeeps: PATH, HOME, TMPDIR, TERM, TZ, LANG, the LC_ names
// and the proxy variables), and these, each with what reads it (cli/cli v2.104.0 and v2.98.0,
// go-gh v2.17.0 and v2.13.0; the measure is every os.Getenv and os.LookupEnv of their non-test
// code):
//
//   - XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_STATE_HOME and XDG_CACHE_HOME, where gh keeps its state
//     and cache (go-gh pkg/config/config.go:276-327);
//   - GH_REPO, which gh takes a command's own repository from (cli/cli v2.104.0
//     pkg/cmdutil/repo_override.go:77, pkg/cmd/pr/create/create.go:289, pkg/cmd/api/api.go:604)
//     and ghroute.TargetOf routes by, so gh must read the one this command was routed for;
//   - GH_TOKEN and GITHUB_TOKEN, which ghroute.TargetOf refuses for every call but the guard's own
//     visibility read, which runs with the caller's token (ghVisibilityRead);
//   - NO_COLOR, CLICOLOR, CLICOLOR_FORCE, COLORTERM, GLAMOUR_STYLE, GH_MDWIDTH and
//     GH_SPINNER_DISABLED, how gh prints (go-gh pkg/term/env.go:162-173, cli
//     pkg/markdown/markdown.go:20, internal/ghcmd/cmd.go:390);
//   - GIT_TERMINAL_PROMPT, GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME and
//     GIT_COMMITTER_EMAIL, for the git gh runs: no prompt, and the identity a pane's commits carry
//     (runtime's identity environment). Its configuration is ghChildGitConfig's, not the caller's;
//   - LEGION_GH_PATH and LEGION_GH_OWNERS_DIR, which `legion gh auth git-credential`, git's
//     credential helper through GH_PATH, reads, beside the GH_CONFIG_DIR runGh sets, whose token
//     that helper reads a fork with (forkUpstreamDir); PUBLIC_LEAK_CHECK,
//     LEGION_LEAK_CHECKER_MANIFEST_SHA256, LEGION_PRIVATE_NAME_RULES_FILE and WRAPPED_GIT_AT,
//     which the pre-push hook of a push gh's git makes (`gh repo create --source=. --push`) reads,
//     the first through leakguard.Resolve, the second as the manifest digest its guard holds that
//     check to, and the third as the rule file its guard runs: without it, that push would run the
//     shared check alone and none of the role's private-name rules; and LEGION_ROLE and
//     LEGION_ISSUE, which both log.
//
// Everything else stays out: the trust stores, GIT_SSL_CAINFO, GIT_SSL_NO_VERIFY, SSLKEYLOGFILE,
// GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR, GIT_CONFIG_PARAMETERS, GIT_CEILING_DIRECTORIES,
// GIT_DISCOVERY_ACROSS_FILESYSTEM, GH_EDITOR, EDITOR, VISUAL, GIT_EDITOR, GH_BROWSER, BROWSER,
// PAGER, GH_HOST, GH_ENTERPRISE_TOKEN, GITHUB_ENTERPRISE_TOKEN, GH_TELEMETRY_ENDPOINT_URL,
// GH_TELEMETRY, DO_NOT_TRACK, GH_DEBUG, GH_FORCE_TTY, and CLAUDE_CONFIG_DIR and DOCKER_CONFIG, which
// only `gh skills` and `gh attestation verify oci://` read and no Legion caller runs through
// `legion gh`, among them.
func ghChildKeeps(name string) bool {
	return leakguard.TokenChildKeeps(name) || ghChildNames[name]
}

// ghNoEditor is the editor gh is handed (GH_EDITOR, which outranks gh's config `editor`, VISUAL,
// EDITOR and gh's own default, nano; cli/cli v2.104.0 pkg/cmdutil/legacy.go:12,
// pkg/surveyext/editor.go:19-28): a program that exits 1, so an editor gh starts fails and gh
// sends nothing it wrote. gh 2.104.0 starts one with GH_PROMPT_DISABLED set beyond the routes
// refused by name (--editor, `gh gist edit`): `gh discussion create` given an empty -b opens its
// MarkdownEditor whatever prompts allow (pkg/cmd/discussion/create/create.go:75-76,152-153), and
// `gh preview prompter` opens it by design. It sits in a root-owned directory, which no pane can
// write; where it is absent, as on macOS, gh fails to start it, which sends nothing either.
const ghNoEditor = "/bin/false"

// ghChildNames are the whole names ghChildKeeps keeps beside leakguard.TokenChildKeeps' own.
var ghChildNames = map[string]bool{
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true,
	"GH_REPO": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
	"NO_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true, "COLORTERM": true, "GLAMOUR_STYLE": true,
	"GH_MDWIDTH": true, "GH_SPINNER_DISABLED": true,
	"GIT_TERMINAL_PROMPT": true,
	"GIT_AUTHOR_NAME":     true, "GIT_AUTHOR_EMAIL": true, "GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true,
	"LEGION_GH_PATH": true, "LEGION_GH_OWNERS_DIR": true,
	leakguard.CheckVar: true, leakguard.CheckerVar: true, leakguard.RulesFileVar: true, wrappedGitAt: true,
	"LEGION_ROLE": true, "LEGION_ISSUE": true,
}

// ghForcedGitConfig is what the git gh runs gets on its command line after the runtime's pairs,
// which git reads after every file, so each of these wins over the same key a repository's own
// configuration sets:
//
//   - http.sslVerify on, which a repository's generic `http.sslVerify=false` then loses to
//     (measured with git 2.43: through a man-in-the-middle proxy whose CA nothing trusts, the
//     request failed verification and carried nothing). An empty http.sslCAInfo or http.sslCAPath
//     is no part of it: git 2.43 then fails every HTTPS request with "Problem with the SSL CA
//     cert", github.com's own included, so gh's clone and `pr checkout` would break. A URL-scoped
//     http.<url>.sslVerify, sslCAInfo or proxy in the repository's own configuration wins over
//     any entry here, so runGh refuses a repository that sets one (ghRepoGitConfigRefusal).
//   - submodule.recurse, fetch.recurseSubmodules and push.recurseSubmodules off, so the fetch,
//     pull, checkout, merge, reset and push gh runs never recurse into a submodule, whose own
//     configuration (.git/modules/<name>/config) no check of this command reads. git's default
//     fetch.recurseSubmodules is on-demand: measured with git 2.43, a plain `git fetch origin`,
//     and `git pull --ff-only`, whose fetched commit moved a populated submodule's gitlink,
//     fetched that submodule too, and a URL-scoped http.sslVerify=false in the submodule's own
//     configuration handed a man-in-the-middle proxy the credential helper's token. With these
//     three on the command line no fetch recursed, whatever the repository set: submodule.recurse
//     and fetch.recurseSubmodules true in its own configuration, submodule.<name>
//     .fetchRecurseSubmodules true there, or in the .gitmodules of HEAD or of the fetched commit.
//     An explicit recursion, which no configuration turns off, is refused by name: `gh pr
//     checkout --recurse-submodules` and a clone's git flags (ghroute.TargetOf).
var ghForcedGitConfig = [][2]string{
	{"http.sslVerify", "true"},
	{"submodule.recurse", "false"},
	{"fetch.recurseSubmodules", "false"},
	{"push.recurseSubmodules", "no"},
}

// ghChildGitConfig is the GIT_CONFIG_COUNT block the git gh runs reads, as it reads `-c`: of the
// caller's GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> pairs, only those that are one of the pairs the
// runtime itself sets (sandbox.GitConfigPairs: github.com's credential helper reset and set to
// `legion git-credential`, useHttpPath, core.fsmonitor, core.hooksPath and the four pack settings
// of workspace.PackWindowConfig), key and value alike, in the caller's order, then
// ghForcedGitConfig. Any other pair the caller names (an http.proxy, an http.sslCAInfo, an
// http.sslVerify, a url.<base>.insteadOf, an include.path, another credential helper, a
// core.sshCommand or core.gitProxy) is dropped, whatever its case or URL scope. A count that is no
// count, and a pair with a half missing, are dropped too; withGitHubUseHTTPPath then adds its own
// entry after these.
func ghChildGitConfig(environ []string) []string {
	env := map[string]string{}
	for _, pair := range environ {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	var pairs [][2]string
	if count, err := strconv.Atoi(env["GIT_CONFIG_COUNT"]); err == nil {
		// A count past the variables the environment holds names pairs that are not there.
		for i := range min(count, len(environ)) {
			key, hasKey := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
			value, hasValue := env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
			if !hasKey || !hasValue {
				continue
			}
			if slices.ContainsFunc(sandbox.GitConfigPairs(), func(runtime [2]string) bool {
				return gitConfigKey(runtime[0]) == gitConfigKey(key) && runtime[1] == value
			}) {
				pairs = append(pairs, [2]string{key, value})
			}
		}
	}
	pairs = append(pairs, ghForcedGitConfig...)
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, pair := range pairs {
		out = append(out, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+pair[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+pair[1])
	}
	return out
}

// gitConfigKey is a git configuration key as git compares one: its section and its variable name
// lowercased, and the subsection between them, a URL in `credential.<url>.helper`, as written.
func gitConfigKey(key string) string {
	first, last := strings.Index(key, "."), strings.LastIndex(key, ".")
	if first < 0 || first == last {
		return strings.ToLower(key)
	}
	return strings.ToLower(key[:first]) + key[first:last] + strings.ToLower(key[last:])
}

// withGitHubUseHTTPPath adds credential.https://github.com.useHttpPath=true to env's GIT_CONFIG_COUNT
// entries, which git reads as it reads `-c`, so the git gh runs tells its credential helper the
// repository's path, whose owner `legion git-credential` answers for. A GIT_CONFIG_COUNT that is no
// count is refused.
func withGitHubUseHTTPPath(env []string) ([]string, error) {
	count := 0
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "GIT_CONFIG_COUNT="); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("GIT_CONFIG_COUNT is %q, which is no count, so git's configuration cannot be extended; unset it", value)
			}
			count = n
		}
	}
	key, value := fmt.Sprintf("GIT_CONFIG_KEY_%d", count), fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)
	out := make([]string, 0, len(env)+3)
	for _, pair := range env {
		if name, _, _ := strings.Cut(pair, "="); name != "GIT_CONFIG_COUNT" && name != key && name != value {
			out = append(out, pair)
		}
	}
	return append(out, key+"=credential.https://github.com.useHttpPath", value+"=true", fmt.Sprintf("GIT_CONFIG_COUNT=%d", count+1)), nil
}
