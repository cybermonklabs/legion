package ghroutetest

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// The resolver tables ghroute's TargetOf tests read (route_test.go, printed_test.go), every row
// of which `legion gh`'s tests also resolve through its keep of gh's help (cmd/legion's
// ghTargetOfKept): a keep that keeps each help it reads, and then a keep that reads the copies,
// must decide each row as gh's own help does, the refusal text included. A row that reads the
// working directory's git remotes stays beside the resolver: cmd/legion cannot build the
// ghroute.Remote values it needs, whose fields are unexported.

// NoGitRepository is the git remotes of a working directory that is no git repository: none. R is
// ghroute.Remote, which this package cannot name (it imports ghroute not at all), so a caller
// instantiates it: ghroutetest.NoGitRepository[ghroute.Remote].
func NoGitRepository[R any]() ([]R, error) { return nil, nil }

// EditedHelp answers gh's help as saved answers it, with old replaced by new in the help of each
// path edits names (its words joined by spaces, "" for gh's own): the help a gh with other aliases
// or extensions, or a later gh spelling its usages otherwise, prints.
func EditedHelp(saved func([]string) (string, error), edits map[string][2]string) func([]string) (string, error) {
	return func(path []string) (string, error) {
		text, err := saved(path)
		if edit, ok := edits[strings.Join(path, " ")]; ok && err == nil {
			if !strings.Contains(text, edit[0]) {
				return "", fmt.Errorf("the saved help of %q holds no %q to edit", path, edit[0])
			}
			text = strings.Replace(text, edit[0], edit[1], 1)
		}
		return text, err
	}
}

// The edits of the help of a gh with an extension, `hello`, and an alias of its own under pr, `pr
// pv2`, beside gh's default alias co; of a gh whose usage of pr view spells its URL <URL>, as no
// gh this was read from does; and of a gh whose pr view lists an option a later gh might add,
// naming a repository.
var (
	withAnExtensionAndANestedAlias = map[string][2]string{
		"":   {"ALIAS COMMANDS\n", "EXTENSION COMMANDS\n  hello:         Extension hello\n\nALIAS COMMANDS\n"},
		"pr": {"\nFLAGS\n", "\nALIAS COMMANDS\n  pv2:           Alias for \"pr view -R sjawhar/legion\"\n\nFLAGS\n"},
	}
	SpellingItsURLOtherwise = map[string][2]string{
		"pr view": {"<url>", "<URL>"},
	}
	withARepositoryOption = map[string][2]string{
		"pr view": {"\nINHERITED FLAGS\n", "      --mirror string    Read the pull request from this repository instead\n\nINHERITED FLAGS\n"},
	}
)

// TargetCall is one row of a resolver table: a call's argv and environment, the edits of gh's
// saved help it is read under (nil for none), and what TargetOf answers for it: the owner, and the
// repository where the table reads one, or a refusal containing Err.
type TargetCall struct {
	Argv  []string
	Env   map[string]string
	Edits map[string][2]string
	Owner string // "" with no refusal: not named
	Repo  string // "" where the selectors name no one repository
	Err   string // a refusal containing this
}

// Help is the help the call is read under: saved, with the call's Edits where it names any.
func (c TargetCall) Help(saved func([]string) (string, error)) func([]string) (string, error) {
	if c.Edits == nil {
		return saved
	}
	return EditedHelp(saved, c.Edits)
}

// ghRepoAcme is the environment of a call whose GH_REPO names acme/widgets.
var ghRepoAcme = map[string]string{"GH_REPO": "acme/widgets"}

// FailsClosedCalls are the rows of ghroute's TestGhTargetOfFailsClosed that read no git remote:
// TargetOf reads the owner a gh command targets from every selector gh's own help names, and fails
// closed, an argument it cannot place refused, naming it, and a command naming two owners too.
var FailsClosedCalls = []TargetCall{
	// Round 4's: an option or another repository naming one owner while gh takes the command's
	// own repository from the working directory or GH_REPO (deep 1, quality B, final B2).
	{Argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, Env: ghRepoAcme, Err: "more than one owner, acme (the GH_REPO value naming acme/widgets) and sjawhar"},
	// Round 4's minor findings: options mentioning URLs or a team, and project link and unlink,
	// which read GH_REPO with -R a plain string (deep 3, deep 4).
	{Argv: []string{"project", "link", "1", "--owner", "acme"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"project", "unlink", "1", "--owner", "acme", "--team", "sjawhar/core"}, Err: "more than one owner"},
	{Argv: []string{"project", "link", "1", "--team", "core", "-R", "acme/widgets"}, Err: `a --team value names no owner`},
	// Round 5's: gh takes a passthrough leaf's repository from the first argument even after
	// `--` (deep 1); project link and unlink read no repository of their own with --team
	// (quality 2).
	{Argv: []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"repo", "fork", "--", "sjawhar/legion", "--depth=1"}, Owner: "sjawhar"},
	// What a clone leaf hands git clone after its repository is git's, not gh's: a value naming
	// another owner's repository there routes nothing. Only the git clone flags ghroute's
	// cloneGitFlagTakes holds pass, so a -R there, which git clone has no use for, is refused.
	{Argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "--branch", "acme/w"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "-R", "acme/w"}, Err: "hands a git flag after -- to git clone"},
	{Argv: []string{"project", "unlink", "1", "--team", "sjawhar/core"}, Env: ghRepoAcme, Owner: "sjawhar"},
	// --version is gh's alone; before anything, gh reads the next word as a command, an
	// alias's or an extension's among them.
	{Argv: []string{"--version"}},
	{Argv: []string{"--version", "hello"}, Edits: withAnExtensionAndANestedAlias, Err: `"--version" comes before the command's name`},
	{Argv: []string{"--version", "co"}, Err: `"--version" comes before the command's name`},
	{Argv: []string{"-R", "sjawhar/legion", "--version", "hello"}, Edits: withAnExtensionAndANestedAlias, Err: `"--version" comes before the command's name`},
	{Argv: []string{"--help", "--version"}, Err: `"--version" comes before the command's name`},
	// A first word gh's own help does not list, whose help gh may answer by installing an
	// extension, and the commands that run another program on gh's token.
	{Argv: []string{"stack", "view"}, Err: "the command's first word is no command gh's own help lists"},
	{Argv: []string{"ext", "exec", "hello"}, Err: "the command's first word is no command gh's own help lists"},
	{Argv: []string{"extension", "exec", "hello"}, Err: "`gh extension exec` runs another program"},
	{Argv: []string{"help", "extension", "exec"}, Err: "`gh extension exec` runs another program"},
	// Options name a repository as arguments do: by ghOptionKinds, an issue by URL among them,
	// and one a later gh adds refused; a URL in an option naming none is text.
	{Argv: []string{"issue", "edit", "1", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/2", "-R", "acme/widgets"}, Err: "more than one owner"},
	{Argv: []string{"issue", "edit", "1", "--add-sub-issue", "2", "-R", "acme/widgets"}, Owner: "acme"},
	{Argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/sjawhar/legion/issues/1"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "sync", "acme/widgets", "--source", "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2", "-R", "acme/widgets"}, Owner: "acme"},
	{Argv: []string{"pr", "comment", "1", "--body", "https://example.com/sjawhar/legion", "-R", "acme/widgets"}, Owner: "acme"},
	{Argv: []string{"repo", "edit", "acme/widgets", "--homepage", "https://github.com/sjawhar/legion"}, Owner: "acme"},
	{Argv: []string{"pr", "view", "1", "--mirror", "sjawhar/legion", "-R", "acme/widgets"}, Edits: withARepositoryOption, Err: "names a repository, an owner, a team or a URL in a way this does not know"},
	// gh api's placeholders as gh reads them, the colon form too, in a typed field's value.
	{Argv: []string{"api", "graphql", "-F", "o=:owner", "-F", "r=:repo", "-f", "query=q"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"api", "repos/:owner/:repo/pulls"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"api", "graphql", "-f", "o=:owner", "-f", "query=q"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}},
	{Argv: []string{"api", "repos/{owner}/{repo}/commits/{branch}"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Owner: "sjawhar"},
	// A pr command reads OWNER/REPO#N as a branch of its own repository. `gh agent-task` sends
	// the token to the Copilot API host GitHub names, so it is refused whatever it names, an
	// issue URL and a pull request's alike; and `--editor` runs an editor after the guard has
	// read the call. Each is refused before any repository is read. (`gh gist edit` and `gh auth
	// login`, `logout`, `refresh` and `switch`, whose help these saved files do not hold, are
	// refused in cmd/legion's TestRealGhAndGitSendTheTargetOwnersToken, with the real gh.)
	{Argv: []string{"agent-task", "view", "sjawhar/legion#1"}, Err: "`gh agent-task` sends the token to the Copilot API host"},
	{Argv: []string{"issue", "comment", "1", "-R", "acme/widgets", "-e"}, Err: `"--editor" runs an editor program`},
	{Argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--editor"}, Err: `"--editor" runs an editor program`},
	// -e is refused where it is --editor, and taken where the leaf's help gives it another
	// option: `gh secret set`'s -e is --env.
	{Argv: []string{"secret", "set", "TOKEN", "-R", "acme/widgets", "-e", "production", "--body", "x"}, Owner: "acme"},
	{Argv: []string{"pr", "view", "sjawhar/legion#1", "-R", "acme/widgets"}, Owner: "acme"},
	// The first re-review's reproductions.
	{Argv: []string{"-R", "sjawhar/legion", "pr", "view", "1", "--json", "title"}, Owner: "sjawhar"},
	{Argv: []string{"--repo=sjawhar/legion", "pr", "view", "1", "--json", "title"}, Owner: "sjawhar"},
	{Argv: []string{"api", "--", "repos/sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "--json", "title", "--", "https://github.com/sjawhar/legion/pull/1"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "deploy-key", "add", "key.pub", "-R", "sjawhar/legion", "--title", "-Racme/widgets"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "--json", "number", "view", "https://github.com/sjawhar/legion/pull/1"}, Err: "an option given before the command's name is refused"},
	// The first review's reproductions.
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: ghRepoAcme, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, Env: ghRepoAcme, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, Err: "more than one owner, acme (a -R value naming acme/widgets) and sjawhar (a URL argument naming sjawhar/legion)"},
	{Argv: []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, Env: ghRepoAcme, Owner: "sjawhar"},
	// The second re-review's reproductions. --help is a boolean, the last of its values
	// deciding, before the command's name or after; only true is help.
	{Argv: []string{"api", "repos/sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "--help", "view", "1", "-R", "sjawhar/legion", "--help=0"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--help"}},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "-h"}},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=maybe"}, Err: `"--help" has a value that is no boolean`},
	{Argv: []string{"--help=false", "api", "repos/sjawhar/legion"}, Owner: "sjawhar"},
	// A name in --repos is a repository of the --org.
	{Argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion", "--body", "dummy", "-R", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"secret", "set", "TOKEN", "-o", "sjawhar", "-r", "legion,smoke", "--body", "dummy"}, Owner: "sjawhar"},
	{Argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion,acme/widgets", "--body", "dummy"}, Err: "more than one owner"},
	{Argv: []string{"secret", "set", "TOKEN", "--user", "--repos", "legion", "--body", "dummy"}, Err: "no --org says whose"},
	// pflag's optional value after a shorthand: -c takes its default, and the rest of the
	// word is more shorthands.
	{Argv: []string{"browse", "-c", "-R", "sjawhar/legion", "--no-browser"}, Owner: "sjawhar"},
	{Argv: []string{"browse", "-cnR", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"browse", "-c=abc", "-R", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"browse", "-c", "abc"}},
	// A gh alias, which may carry a repository of its own, and an extension, which reads gh's
	// token itself, are refused before their help is read, at any depth, and so is the Copilot
	// CLI gh runs.
	{Argv: []string{"co", "https://github.com/sjawhar/legion/pull/1"}, Err: `"co" is a gh alias`},
	{Argv: []string{"pr", "pv2", "1"}, Edits: withAnExtensionAndANestedAlias, Err: `"pv2" is a gh alias`},
	{Argv: []string{"hello", "-R", "sjawhar/legion"}, Edits: withAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, Edits: withAnExtensionAndANestedAlias, Owner: "sjawhar"},
	{Argv: []string{"pr", "ls", "-R", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"copilot"}, Err: "`gh copilot` runs another program"},
	// A search query names its owner in plain qualifiers, one to an argument, or not at all.
	{Argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, Owner: "sjawhar"},
	{Argv: []string{"search", "issues", "bug OR crash"}, Env: ghRepoAcme},
	{Argv: []string{"search", "issues", "(repo:sjawhar/legion)"}, Err: "a word of the search query is a search qualifier this cannot read for certain"},
	{Argv: []string{"search", "issues", "repo:sjawhar/legion is:open"}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"search", "issues", `label:"help wanted"`}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"search", "issues", "repo:sjawhar/legion", "OR", "bug"}, Err: "an OR, a NOT or a parenthesis of the search query joins the query's"},
	{Argv: []string{"search", "issues", "(", "repo:sjawhar/legion", ")"}, Err: "an OR, a NOT or a parenthesis of the search query joins the query's"},
	{Argv: []string{"search", "issues", "--", "bug", "-repo:acme/widgets"}, Err: "a -repo:, -owner:, -user: or -org: qualifier of the search query excludes an owner"},
	{Argv: []string{"search", "issues", "bug", "-repo:acme/widgets"}, Err: "a short option the command's help does not list is given to `gh search issues`"},
	{Argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, Owner: "sjawhar"},
	{Argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=repo:sjawhar/legion is:open"}, Owner: "sjawhar"},
	{Argv: []string{"api", "search/issues?q=%28repo:sjawhar/legion%29"}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@query.txt"}, Err: "reads the search query from a file"},
	// gh sends its token only to GH_HOST, and GH_TOKEN or GITHUB_TOKEN in place of any file's.
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: map[string]string{"GH_HOST": "ghe.example.com"}, Err: "GH_HOST names another host than github.com"},
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: map[string]string{"GH_HOST": "GitHub.com"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_TOKEN": "ghp_x"}, Err: "GH_TOKEN is set"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}, Err: "GITHUB_TOKEN is set"},
	// An argument the leaf's usage names in a way this does not know, or has no place for.
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1"}, Edits: SpellingItsURLOtherwise, Err: "<URL>"},
	{Argv: []string{"pr", "view", "1"}, Edits: SpellingItsURLOtherwise, Err: "<URL>"},
	{Argv: []string{"pr", "view", "1", "2"}, Err: "the 2nd argument has no place in `gh pr view`'s usage"},
	{Argv: []string{"pr", "list", "sjawhar/legion"}, Err: "the 1st argument has no place in `gh pr list`'s usage"},
	{Argv: []string{"api", "repos/acme/widgets", "repos/sjawhar/legion"}, Err: "the 2nd argument has no place in `gh api`'s usage"},
	// The third re-review's: gh's help of an extension runs the extension, so the target of
	// `gh help` is read as a command's path, a gh alias or an extension refused before anything
	// runs; gh's own help and its topics are not. And a plural slot, `{<numbers> | <urls>}`,
	// takes one or more arguments, every one a selector.
	{Argv: []string{"help"}},
	{Argv: []string{"help", "pr", "view"}},
	{Argv: []string{"help", "environment"}},
	{Argv: []string{"help", "co"}, Err: `"co" is a gh alias`},
	{Argv: []string{"help", "hello"}, Edits: withAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"help", "pr", "pv2"}, Edits: withAnExtensionAndANestedAlias, Err: `"pv2" is a gh alias`},
	{Argv: []string{"-R", "acme/widgets", "help", "hello"}, Edits: withAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"help", "--bogus", "hello"}, Err: "an option given to gh help is refused: `gh help` takes only the names of commands"},
	{Argv: []string{"issue", "edit", "1", "2", "--title", "x", "-R", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/sjawhar/legion/issues/2", "--title", "x"}, Owner: "sjawhar"},
	{Argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/acme/widgets/issues/2", "--title", "x"}, Err: "more than one owner"},
	// A URL is one in any case, as gh parses it.
	{Argv: []string{"pr", "view", "HTTPS://GITHUB.COM/sjawhar/legion/pull/1"}, Owner: "sjawhar"},
	{Argv: []string{"api", "HTTPS://API.GITHUB.COM/repos/sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"api", "HTTPS://UPLOADS.GITHUB.COM/repos/sjawhar/legion/releases/1/assets?name=x"}, Owner: "sjawhar"},
	{Argv: []string{"api", "REPOS/sjawhar/legion"}, Owner: "sjawhar"},
	// A command naming no repository, and GH_REPO where the leaf reads it.
	{Argv: []string{"pr", "list"}},
	{Argv: []string{"pr", "list"}, Env: ghRepoAcme, Owner: "acme"},
	{Argv: []string{"api", "graphql", "-f", "query=q", "-F", "owner=Sjawhar", "-F", "name=legion"}, Env: ghRepoAcme, Owner: "sjawhar"},
	{Argv: []string{"api", "graphql", "-f", "query=q", "-F", "o=sjawhar", "-F", "id=PRRT_x"}},
	{Argv: []string{"api", "user"}, Env: ghRepoAcme},
	{Argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}, Env: ghRepoAcme},
	{Argv: []string{"pr"}},
	{Argv: nil},
	{Argv: []string{"--help"}},
	{Argv: []string{"pr", "view", "1", "--help"}},
	{Argv: []string{"pr", "view", "1", "-R", "acme/w", "-h"}},
	{Argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}},
	{Argv: []string{"api", "graphql", "-F", "owner={owner}", "-f", "query=q"}, Env: ghRepoAcme, Owner: "acme"},
	{Argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
	{Argv: []string{"api", "repos/{owner}/{repo}/pulls"}, Env: ghRepoAcme, Owner: "acme"},
	{Argv: []string{"auth", "status"}, Env: ghRepoAcme},
	{Argv: []string{"auth", "status", "-h", "github.com"}},
	{Argv: []string{"browse", "--commit"}},
	{Argv: []string{"search", "issues", "is:open"}, Env: ghRepoAcme},
	// Explicit selectors.
	{Argv: []string{"api", "-X", "GET", "https://api.github.com/orgs/Acme/teams"}, Owner: "acme"},
	{Argv: []string{"pr", "-R", "sjawhar/legion", "view", "1"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "create", "--fill", "-R", "github.com/Acme/widgets"}, Owner: "acme"},
	{Argv: []string{"repo", "clone", "sjawhar/legion", "--", "--depth=1", "--branch", "acme/w"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "create", "sjawhar/new", "--private"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "create", "newrepo", "--private"}},
	{Argv: []string{"repo", "list", "Sjawhar"}, Owner: "sjawhar"},
	{Argv: []string{"search", "issues", "--owner", "acme", "bug"}, Owner: "acme"},
	{Argv: []string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/other"}, Owner: "sjawhar"},
	{Argv: []string{"secret", "set", "TOKEN", "--org", "acme"}, Owner: "acme"},
	// A managed user's login ends in `_<shortcode>`, which is a GitHub login, and is keyed
	// lowercased as every owner is.
	{Argv: []string{"pr", "view", "1", "-R", "Mona-Cat_Octo/widgets"}, Owner: "mona-cat_octo"},
	// Refusals.
	{Argv: []string{"pr", "view", "1", "--bogus"}, Err: "an option the command's help does not list is given to `gh pr view`"},
	{Argv: []string{"pr", "view", "1", "-Z"}, Err: "a short option the command's help does not list is given to `gh pr view`"},
	{Argv: []string{"pr", "nonexist"}, Err: "gh has no command `gh pr` and a subcommand its help does not list"},
	{Argv: []string{"nonexist"}, Err: "the command's first word is no command gh's own help lists"},
	{Argv: []string{"--bogus", "pr", "list"}, Err: "an option given before the command's name is refused"},
	{Argv: []string{"api", "repositories/12345"}, Err: "by its id"},
	{Argv: []string{"api", "https://ghe.example.com/api/v3/repos/acme/w"}, Err: "another host"},
	{Argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/w"}, Err: "another host"},
	// An absolute endpoint is GitHub's own https://api.github.com/… or https://uploads.github.com/…
	// or nothing: gh sends the bundle's token to whatever host and scheme it names, so an http://
	// one hands the token over in the clear, GitHub's 301 to https coming only after it has it.
	{Argv: []string{"api", "http://api.github.com/repos/acme/w"}, Err: "is no https URL"},
	{Argv: []string{"api", "HTTP://API.GITHUB.COM/repos/acme/w"}, Err: "is no https URL"},
	{Argv: []string{"api", "ftp://api.github.com/repos/acme/w"}, Err: "is no https URL"},
	{Argv: []string{"api", "-X", "POST", "http://api.github.com/repos/acme/w/issues/1/comments"}, Err: "is no https URL"},
	// gh api's whole URL is placed only over https and on api.github.com or uploads.github.com,
	// spelled in ASCII: another GitHub host, http, and `ſ` (U+017F), which strings.EqualFold
	// folds onto `s`, are refused.
	{Argv: []string{"api", "https://github.com/repos/acme/widgets"}, Err: "another host"},
	{Argv: []string{"api", "https://gist.github.com/repos/acme/widgets"}, Err: "another host"},
	{Argv: []string{"api", "https://upload\u017f.github.com/repos/acme/widgets/releases/1/assets?name=x"}, Err: "the endpoint's host is another host than api.github.com or uploads.github.com"},
	{Argv: []string{"api", "http://uploads.github.com/repos/acme/widgets/releases/1/assets?name=x"}, Err: "is no https URL"},
	// A host is compared in ASCII alone: `İ` (U+0130), which strings.ToLower lowers to `i`,
	// makes no github.com of `gİthub.com`, in -R, a URL argument or --hostname.
	{Argv: []string{"pr", "view", "1", "-R", "g\u0130thub.com/acme/widgets"}, Err: "names another host than github.com"},
	{Argv: []string{"pr", "view", "https://g\u0130thub.com/acme/widgets/pull/1"}, Err: "names another host than github.com"},
	{Argv: []string{"api", "--hostname", "g\u0130thub.com", "repos/acme/w"}, Err: "names another host than github.com"},
	{Argv: []string{"api", "https://api.github.com/repos/Acme/w"}, Owner: "acme"},
	{Argv: []string{"api", "repos/../x/y"}, Err: "the endpoint names an owner that is not a GitHub login"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "../x/y"}, Err: `names another host`},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "../y"}, Err: "the GH_REPO value names an owner that is not a GitHub login"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "widgets"}, Err: "names no owner"},
	{Argv: []string{"repo", "view", "widgets"}, Err: "names no owner"},
	{Argv: []string{"pr", "view", "1", "-R"}, Err: "has no value"},
	{Argv: []string{"pr", "view", "1", "-R="}, Err: "a -R value names no owner"},
	{Argv: []string{"search", "issues", "--owner", "acme", "repo:sjawhar/x"}, Err: "more than one owner"},
	{Argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"label", "clone", "acme/widgets", "-R", "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"secret", "set", "TOKEN", "--org", "acme", "-R", "sjawhar/x"}, Err: "more than one owner"},
	{Argv: []string{"auth", "status", "-h", "ghe.example.com"}, Err: "another host"},
}

// WalkedCalls is a resolver table whose rows read gh's help of every command as the gh release
// Version prints it, saved whole (cmd/legion's testdata/gh-help-walk/<Version>, which each
// package's savedGhHelpOf answers from), in place of the help SavedHelp answers from
// testdata/gh-help. Each row is read under that help with its own Edits where it names any.
type WalkedCalls struct {
	Version string
	Calls   []TargetCall
}

// hBeforeTheCommand is the refusal of a -h before the command's name.
const hBeforeTheCommand = `"-h" comes before the command's name, and only -R/--repo and --help may (gh can take the word after such a -h as its value`

// HBeforeTheCommandCalls are the rows of ghroute's TestGhTargetOfFailsClosed with a -h before
// the command's name: each is refused but as the whole argv (the walk's -h case says why); `gh pr
// --help` is the way to ask. The rows read gh 2.98.0's help of every command, saved whole, which
// holds `gh status`, so the words of a -h row are gh's commands and the row is refused for its -h
// alone.
var HBeforeTheCommandCalls = WalkedCalls{Version: "2.98.0", Calls: []TargetCall{
	{Argv: []string{"-h", "status", "config", "set", "-h", "github.com", "editor", "x"}, Err: hBeforeTheCommand},
	{Argv: []string{"-h", "status", "auth", "logout", "-h", "github.com"}, Err: hBeforeTheCommand},
	{Argv: []string{"-h", "status", "repo", "create", "acme/x", "--public"}, Err: hBeforeTheCommand},
	{Argv: []string{"auth", "-h", "status", "logout", "-h", "github.com"}, Err: hBeforeTheCommand},
	{Argv: []string{"repo", "-h", "view", "create", "acme/x", "--public"}, Err: hBeforeTheCommand},
	{Argv: []string{"-R", "acme/widgets", "-h", "pr", "view", "1"}, Err: hBeforeTheCommand},
	{Argv: []string{"-h", "pr", "view", "1", "-R", "acme/w"}, Err: hBeforeTheCommand},
	{Argv: []string{"-h", "status"}, Err: hBeforeTheCommand},
	{Argv: []string{"-R", "acme/widgets", "-h"}, Err: hBeforeTheCommand},
	{Argv: []string{"pr", "-h"}, Err: hBeforeTheCommand},
	{Argv: []string{"-h"}},
}}

// OneRepositoryCalls are the rows of ghroute's TestGhTargetOfNamesTheOneRepositoryEverySelectorNames
// that read no git remote: beside the owner, TargetOf reads the one repository every selector of
// the command names, and none where they name two of the owner's or a selector names the owner
// alone.
var OneRepositoryCalls = []TargetCall{
	// One repository, named by each selector that names one.
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"--repo=sjawhar/Legion", "pr", "view", "1"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"repo", "clone", "sjawhar/legion"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"repo", "clone", "github.com/sjawhar/legion"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/2/files"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"api", "repos/sjawhar/legion/pulls/1/files"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"api", "REPOS/sjawhar/Legion"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"api", "https://uploads.github.com/repos/acme/widgets/releases/1/assets?name=x"}, Owner: "acme", Repo: "widgets"},
	{Argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "sjawhar/Legion"}, Owner: "sjawhar", Repo: "legion"},
	{Argv: []string{"api", "repos/{owner}/{repo}/pulls"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Owner: "sjawhar", Repo: "legion"},
	// The same repository named twice is still the one: an option's URL beside -R.
	{Argv: []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9", "-R", "acme/widgets"}, Owner: "acme", Repo: "widgets"},
	// Two spellings of the one repository are one repository, not two: the pull request's URL
	// spells it as GitHub shows it and -R as the bundle's directory is named.
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/Legion-Smoke/pull/7", "-R", "sjawhar/legion-smoke"}, Owner: "sjawhar", Repo: "legion-smoke"},
	// Two repositories of the one owner: the command reaches both, so it runs as the owner.
	{Argv: []string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/other"}, Owner: "sjawhar"},
	{Argv: []string{"secret", "set", "TOKEN", "-o", "sjawhar", "-r", "legion,smoke", "--body", "dummy"}, Owner: "sjawhar"},
	// A selector naming the owner alone: whatever else of the owner's gh resolves is in
	// reach, so the one repository another selector names is not the command's alone.
	{Argv: []string{"repo", "list", "Sjawhar"}, Owner: "sjawhar"},
	{Argv: []string{"api", "orgs/acme/teams"}, Owner: "acme"},
	{Argv: []string{"api", "users/sjawhar/repos"}, Owner: "sjawhar"},
	{Argv: []string{"search", "issues", "--owner", "acme", "bug"}, Owner: "acme"},
	{Argv: []string{"search", "issues", "org:sjawhar"}, Owner: "sjawhar"},
	{Argv: []string{"project", "link", "1", "--team", "sjawhar/core"}, Owner: "sjawhar"},
	{Argv: []string{"secret", "set", "TOKEN", "--org", "acme"}, Owner: "acme"},
	{Argv: []string{"api", "graphql", "-f", "query=q", "-F", "owner=Sjawhar", "-F", "name=legion"}, Owner: "sjawhar"},
	{Argv: []string{"api", "repos/sjawhar/{repo}/pulls"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Owner: "sjawhar"},
	// A command naming no owner at all names no repository either.
	{Argv: []string{"pr", "list"}},
	{Argv: []string{"api", "user"}},
}

// RefusalAdvice are ghroute's TestGhTargetOfRefusalsSayHowToNameTheRepository's calls, each a
// command line split on spaces, and what its refusal must say: how to name the repository
// instead - -R, or, for gh api, which has none, the endpoint; for a gh alias, the command it
// stands for.
var RefusalAdvice = map[string]string{
	"pr view 1 --bogus":              "pass -R <owner>/<repo>",
	"api repositories/12345":         "name it in the endpoint, repos/<owner>/<repo>/…",
	"pr --json n view 1":             "pass -R <owner>/<repo>",
	"co 1":                           "run the gh command it stands for, with -R <owner>/<repo>",
	"copilot":                        "run gh's own commands, with -R <owner>/<repo>",
	"search issues (repo:acme/w)":    "pass -R <owner>/<repo>",
	"api search/issues?q=(repo:a/b)": "name it in the endpoint",
}

// sentBy is the leakguard.GhSent that steps build, each in turn, on the zero value: calls of
// its exported methods Names and UsesStdin (sentNames, sentUsesStdin), the two ghroute's
// sentFiles builds its answer with. Its fields of standard input are leakguard's own and no
// literal here can set them, so an answer to compare whole is built so.
func sentBy(steps ...func(*leakguard.GhSent)) *leakguard.GhSent {
	sent := &leakguard.GhSent{}
	for _, step := range steps {
		step(sent)
	}
	return sent
}

// sentNames is the step of sentBy that names path as option's value (leakguard.GhSent.Names).
func sentNames(option, path, field string, use leakguard.StdinUse) func(*leakguard.GhSent) {
	return func(sent *leakguard.GhSent) { sent.Names(option, path, field, use) }
}

// sentUsesStdin is the step of sentBy that names standard input with use alone
// (leakguard.GhSent.UsesStdin).
func sentUsesStdin(use leakguard.StdinUse) func(*leakguard.GhSent) {
	return func(sent *leakguard.GhSent) { sent.UsesStdin(use) }
}

// SentFileCall is one row of ghroute's TestGhTargetOfReadsTheFilesTheCallWouldSend: a call and
// what TargetOf reads it would send to GitHub beside its argv (ghroute.Target.Sent), the parse's
// whole answer, built by the calls sentFiles makes for it (sentBy).
type SentFileCall struct {
	Name string
	Argv []string
	Want *leakguard.GhSent
}

// SentFileCalls are the sent table's rows: a short cluster gives its last letter the next
// argument, an attached value may carry an `=` of its own, and a file a field names carries the
// field's name. A value of `-`, an `@-`, a field given a literal and a raw field name no file to
// open; a `-` and an `@-` are standard input instead, which the parse answers is sent, with the
// use the option that named it makes of it, or none where two options with different uses did,
// and with the name of each field whose value it is. `gh workflow run --json`, at a last value of
// true, names it as a request body; `--json` of any other command takes the fields to print and
// names none.
var SentFileCalls = []SentFileCall{
	{
		Name: "a short cluster whose last letter is the option",
		Argv: []string{"pr", "create", "-R", "acme/widgets", "-dF", "body.md"},
		Want: sentBy(sentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "a short option with its value attached by an equals sign",
		Argv: []string{"pr", "create", "-R", "acme/widgets", "-F=body.md"},
		Want: sentBy(sentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "a separate --body-file",
		Argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "body.md"},
		Want: sentBy(sentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "gh api's field read from a file",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "--method", "PATCH", "-F", "body=@body.md"},
		Want: sentBy(sentNames("--field", "body.md", "body", leakguard.StdinField)),
	},
	{
		Name: "gh api's request body",
		Argv: []string{"api", "repos/acme/widgets/pulls", "--input", "request.json"},
		Want: sentBy(sentNames("--input", "request.json", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "a raw field, which sends its value literally",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-f", "body=@body.md"},
		Want: sentBy(),
	},
	{
		Name: "a field given a literal value",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=text"},
		Want: sentBy(),
	},
	{
		Name: "a body option whose value is standard input",
		Argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "-"},
		Want: sentBy(sentNames("--body-file", "-", "", leakguard.StdinText)),
	},
	{
		Name: "a short option whose value is standard input",
		Argv: []string{"pr", "create", "-R", "acme/widgets", "-F", "-"},
		Want: sentBy(sentNames("--body-file", "-", "", leakguard.StdinText)),
	},
	{
		Name: "a field read from standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@-"},
		Want: sentBy(sentNames("--field", "-", "body", leakguard.StdinField)),
	},
	{
		Name: "a GraphQL query read from standard input",
		Argv: []string{"api", "graphql", "-F", "query=@-"},
		Want: sentBy(sentNames("--field", "-", "query", leakguard.StdinField)),
	},
	{
		// The options are read in the order their names sort, so the field before --input.
		Name: "a GraphQL query read from standard input beside a request body on it",
		Argv: []string{"api", "graphql", "--input", "-", "-F", "query=@-"},
		Want: sentBy(sentNames("--field", "-", "query", leakguard.StdinField),
			sentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's field read from standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "-F", "notes=@-"},
		Want: sentBy(sentNames("--field", "-", "notes", leakguard.StdinField)),
	},
	{
		Name: "gh api's request body on standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls", "--input", "-"},
		Want: sentBy(sentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's inputs as JSON on standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json"},
		Want: sentBy(sentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's --json=true, which sends standard input as its inputs",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json=true"},
		Want: sentBy(sentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's --json whose last value is false",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json", "--json=false"},
		Want: sentBy(),
	},
	{
		Name: "gh workflow run's --json beside a field read from standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json", "-F", "notes=@-"},
		Want: sentBy(sentNames("--field", "-", "notes", leakguard.StdinField),
			sentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "another command's --json, which names the fields to print",
		Argv: []string{"pr", "list", "-R", "acme/widgets", "--json", "number"},
		Want: sentBy(),
	},
	{
		Name: "two fields read from standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "title=@-", "-F", "body=@-"},
		Want: sentBy(sentNames("--field", "-", "title", leakguard.StdinField),
			sentNames("--field", "-", "body", leakguard.StdinField)),
	},
	{
		Name: "two different options naming standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "--input", "-", "-F", "body=@-"},
		Want: sentBy(sentNames("--field", "-", "body", leakguard.StdinField),
			sentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		// Named once, by the option whose name sorts first.
		Name: "the same file named twice",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@body.md", "--input", "body.md"},
		Want: sentBy(sentNames("--field", "body.md", "body", leakguard.StdinField)),
	},
}

// OctocatNotServed is the refusal of a call routed to octocat, an owner neither
// WriteRoutedBundle's bundle nor ServesAcmeAndSjawhar holds a directory for.
const OctocatNotServed = `the role's GitHub App is not installed on "octocat"`

// WriteRoutedBundle writes a bundle under a directory of t's as the daemon writes one
// (ghconfig.WriteBundle), for a call routed over it by ghroute.DirFor, and answers its root:
// directories for acme and sjawhar and none for any other owner, which DirFor answers with a bare
// OwnerNotServedError; and two configured renames, octocat/moved -> sjawhar/moved, whose `to`
// owner has a directory, and sjawhar/gone -> nobody/gone, whose `to` owner has none, which DirFor
// refuses naming the pair around that error.
func WriteRoutedBundle(t testing.TB) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{
		Default: "acme",
		Dirs: map[string]ghconfig.Rendered{
			"acme":    ghconfig.Render("ghs_acme_write", "implement", expires),
			"sjawhar": ghconfig.Render("ghs_sjawhar_write", "implement", expires),
		},
		Pairs: []ghrepo.Rename{ghrepo.MustRename("octocat/moved", "sjawhar/moved"), ghrepo.MustRename("sjawhar/gone", "nobody/gone")},
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// RoutedPane is the pane of the issue owner acme over the bundle at root (WriteRoutedBundle).
func RoutedPane(root string) map[string]string {
	return map[string]string{"GH_CONFIG_DIR": ghconfig.Dir(root, "acme"), "LEGION_GH_OWNERS_DIR": root}
}

// RoutedCall is one row of ghroute's TestGhTargetOfFailsClosed over the bundle WriteRoutedBundle
// writes, read in a working directory that is no git repository: a call and its environment, and
// the directory it runs with, or a refusal containing Err.
type RoutedCall struct {
	Argv []string
	Env  map[string]string
	Dir  string // the directory the command runs with, where it is not refused
	Err  string // a refusal containing this
}

// RoutedCalls are the bundle table's rows that read no git remote, over the bundle at root: reads
// naming only owners the bundle holds no directory for, which run with GH_CONFIG_DIR's directory,
// and the calls that get the routing and the refusal they get with no fallback.
func RoutedCalls(root string) []RoutedCall {
	pane := RoutedPane(root)
	return []RoutedCall{
		// Reads naming two owners with no directory, by a search query's qualifiers and by a
		// GraphQL query's variables.
		{Argv: []string{"search", "issues", "owner:octocat", "org:hubot"}, Env: pane, Dir: pane["GH_CONFIG_DIR"]},
		{Argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "org=hubot", "-f", "query=query($owner:String!,$org:String!){a:repositoryOwner(login:$owner){login} b:organization(login:$org){login}}"}, Env: pane, Dir: pane["GH_CONFIG_DIR"]},
		// A read where no GH_CONFIG_DIR names the directory a command naming no owner runs with.
		{Argv: []string{"repo", "view", "octocat/hello", "--json", "name"}, Err: OctocatNotServed},
		// A write naming an owner with no directory, and a GraphQL mutation naming one.
		{Argv: []string{"issue", "create", "-R", "octocat/hello", "--title", "t", "--body", "b"}, Env: pane, Err: OctocatNotServed},
		{Argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", `query=mutation{addStar(input:{starrableId:"x"}){clientMutationId}}`}, Env: pane, Err: OctocatNotServed},
		// A read naming an owner with no directory beside one the bundle serves.
		{Argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "org=sjawhar", "-f", "query={viewer{login}}"}, Env: pane, Err: OctocatNotServed},
		// The repository a configured rename pair moves: refused naming the pair where its `to`
		// owner has no directory, though its `from` owner has one, and served from the `to`
		// owner's directory where that owner has one, though its `from` owner has none.
		{Argv: []string{"repo", "view", "sjawhar/gone", "--json", "name"}, Env: pane, Err: "the configured rename sjawhar/gone -> nobody/gone"},
		{Argv: []string{"repo", "view", "octocat/moved", "--json", "name"}, Env: pane, Dir: ghconfig.Dir(root, "sjawhar")},
	}
}

// SelectorKindPane is the pane of the issue owner acme whose bundle under /owners serves acme and
// sjawhar and holds no directory for octocat (ServesAcmeAndSjawhar): the environment every
// SelectorKindRead is read in.
var SelectorKindPane = map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}

// ServesAcmeAndSjawhar is the routing of SelectorKindPane's bundle: acme's and sjawhar's
// directories under /owners, and for any other owner what ghroute.DirFor answers an owner it
// holds no directory for, a bare OwnerNotServedError, wrapping nothing.
func ServesAcmeAndSjawhar(owner, _ string) (string, error) {
	if owner == "acme" || owner == "sjawhar" {
		return ghconfig.Dir("/owners", owner), nil
	}
	return "", &leakguard.OwnerNotServedError{Owner: owner, OwnersDir: "/owners"}
}

// SelectorKindReleases are the gh releases every SelectorKindRead is read under, by the help of
// every command each prints, saved whole (cmd/legion's testdata/gh-help-walk/<release>).
var SelectorKindReleases = []string{"2.98.0", "2.104.0"}

// SelectorKindRead is one row of ghroute's TestGhTargetOfRoutesEverySelectorKindOfARead:
// octocat's read, with the leaf whose path it is typed with and the kind it reads octocat through:
// an argument's or an option's as the help's kinds are named, or, for a member no help lists, what
// it is. GH_REPO, where the row sets it, joins SelectorKindPane's environment (Of). Refused marks a
// member the selector reader counts that is no read writesNothing admits.
type SelectorKindRead struct {
	Leaf, Kind string
	Argv       []string
	Env        map[string]string
	Refused    bool
}

// Of is the row's read with owner in octocat's place: its argv, and SelectorKindPane's
// environment with the row's own joined to it.
func (r SelectorKindRead) Of(owner string) (argv []string, env map[string]string) {
	argv = make([]string, len(r.Argv))
	for i, word := range r.Argv {
		argv[i] = strings.ReplaceAll(word, "octocat", owner)
	}
	env = maps.Clone(SelectorKindPane)
	for key, value := range r.Env {
		env[key] = strings.ReplaceAll(value, "octocat", owner)
	}
	return argv, env
}

// SelectorKindReads are the selector-kind table's rows: octocat's read through each kind of
// argument and option a read leaf's help gives, then through the members no help lists: gh api's
// endpoint forms, the GraphQL variables naming an owner, GH_REPO, the two members that are no
// read, and a search query's qualifiers.
var SelectorKindReads = func() []SelectorKindRead {
	const viaRepo = "option --repo (repository)"
	reads := []SelectorKindRead{
		{Leaf: "pr view", Kind: "argument url", Argv: []string{"pr", "view", "https://github.com/octocat/hello/pull/1", "--json", "number"}},
		{Leaf: "pr view", Kind: viaRepo, Argv: []string{"pr", "view", "1", "-R", "octocat/hello", "--json", "number"}},
		{Leaf: "pr list", Kind: viaRepo, Argv: []string{"pr", "list", "-R", "octocat/hello"}},
		{Leaf: "pr diff", Kind: "argument url", Argv: []string{"pr", "diff", "https://github.com/octocat/hello/pull/1"}},
		{Leaf: "pr diff", Kind: viaRepo, Argv: []string{"pr", "diff", "1", "-R", "octocat/hello"}},
		{Leaf: "pr checks", Kind: "argument url", Argv: []string{"pr", "checks", "https://github.com/octocat/hello/pull/1"}},
		{Leaf: "pr checks", Kind: viaRepo, Argv: []string{"pr", "checks", "1", "-R", "octocat/hello"}},
		{Leaf: "pr status", Kind: viaRepo, Argv: []string{"pr", "status", "-R", "octocat/hello"}},
		{Leaf: "run view", Kind: viaRepo, Argv: []string{"run", "view", "1", "-R", "octocat/hello"}},
		{Leaf: "run list", Kind: viaRepo, Argv: []string{"run", "list", "-R", "octocat/hello"}},
		{Leaf: "run watch", Kind: viaRepo, Argv: []string{"run", "watch", "1", "-R", "octocat/hello"}},
		{Leaf: "issue view", Kind: "argument url", Argv: []string{"issue", "view", "https://github.com/octocat/hello/issues/1", "--json", "number"}},
		{Leaf: "issue view", Kind: viaRepo, Argv: []string{"issue", "view", "1", "-R", "octocat/hello"}},
		{Leaf: "issue list", Kind: viaRepo, Argv: []string{"issue", "list", "-R", "octocat/hello"}},
		{Leaf: "issue status", Kind: viaRepo, Argv: []string{"issue", "status", "-R", "octocat/hello"}},
		{Leaf: "repo view", Kind: "argument repository", Argv: []string{"repo", "view", "octocat/hello", "--json", "name"}},
		{Leaf: "repo list", Kind: "argument owner", Argv: []string{"repo", "list", "octocat", "--json", "name"}},
		{Leaf: "release view", Kind: viaRepo, Argv: []string{"release", "view", "v1", "-R", "octocat/hello"}},
		{Leaf: "release list", Kind: viaRepo, Argv: []string{"release", "list", "-R", "octocat/hello"}},
		{Leaf: "workflow view", Kind: viaRepo, Argv: []string{"workflow", "view", "ci.yml", "-R", "octocat/hello"}},
		{Leaf: "workflow list", Kind: viaRepo, Argv: []string{"workflow", "list", "-R", "octocat/hello"}},
		{Leaf: "label list", Kind: viaRepo, Argv: []string{"label", "list", "-R", "octocat/hello"}},
		{Leaf: "cache list", Kind: viaRepo, Argv: []string{"cache", "list", "-R", "octocat/hello"}},
		{Leaf: "search code", Kind: viaRepo, Argv: []string{"search", "code", "hello", "--repo", "octocat/hello"}},
		{Leaf: "search code", Kind: "option --owner (owner)", Argv: []string{"search", "code", "hello", "--owner", "octocat"}},
		{Leaf: "search commits", Kind: viaRepo, Argv: []string{"search", "commits", "fix", "--repo", "octocat/hello"}},
		{Leaf: "search commits", Kind: "option --owner (owner)", Argv: []string{"search", "commits", "fix", "--owner", "octocat"}},
		{Leaf: "search issues", Kind: viaRepo, Argv: []string{"search", "issues", "bug", "--repo", "octocat/hello"}},
		{Leaf: "search issues", Kind: "option --owner (owner)", Argv: []string{"search", "issues", "bug", "--owner", "octocat"}},
		{Leaf: "search issues", Kind: "option --project (owner/name)", Argv: []string{"search", "issues", "--project", "octocat/1", "--json", "number"}},
		{Leaf: "search prs", Kind: viaRepo, Argv: []string{"search", "prs", "fix", "--repo", "octocat/hello"}},
		{Leaf: "search prs", Kind: "option --owner (owner)", Argv: []string{"search", "prs", "fix", "--owner", "octocat"}},
		{Leaf: "search prs", Kind: "option --project (owner/name)", Argv: []string{"search", "prs", "--project", "octocat/1", "--json", "number"}},
		{Leaf: "search repos", Kind: "option --owner (owner)", Argv: []string{"search", "repos", "--owner", "octocat", "--json", "name"}},
		// gh api's endpoint forms, the GraphQL variables naming an owner and GH_REPO, which no
		// help lists.
		{Leaf: "api", Kind: "endpoint repos/<owner>/<repo>/…", Argv: []string{"api", "repos/octocat/hello/pulls"}},
		{Leaf: "api", Kind: "endpoint repos/<owner>/<repo>", Argv: []string{"api", "repos/octocat/hello"}},
		{Leaf: "api", Kind: "endpoint orgs/<owner>", Argv: []string{"api", "orgs/octocat"}},
		{Leaf: "api", Kind: "endpoint users/<owner>", Argv: []string{"api", "users/octocat"}},
		{Leaf: "api", Kind: "GraphQL variable owner", Argv: []string{"api", "graphql", "-f", "owner=octocat", "-f", "query=query($owner: String!) { repositoryOwner(login: $owner) { login } }"}},
		{Leaf: "api", Kind: "GraphQL variable org", Argv: []string{"api", "graphql", "-f", "org=octocat", "-f", "query=query($org: String!) { organization(login: $org) { login } }"}},
		{Leaf: "api", Kind: "GraphQL variable organization", Argv: []string{"api", "graphql", "-f", "organization=octocat", "-f", "query=query($organization: String!) { organization(login: $organization) { login } }"}},
		{Leaf: "api", Kind: "GraphQL variable login", Argv: []string{"api", "graphql", "-f", "login=octocat", "-f", "query=query($login: String!) { user(login: $login) { login } }"}},
		{Leaf: "pr list", Kind: "GH_REPO", Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "octocat/hello"}},
		// The two members that are no read: octocat's gets the OwnerNotServedError refusal.
		{Leaf: "api", Kind: "endpoint https://api.github.com/…", Argv: []string{"api", "https://api.github.com/repos/octocat/hello"}, Refused: true},
		{Leaf: "api", Kind: "a search endpoint's q field", Argv: []string{"api", "search/issues", "-f", "q=repo:octocat/hello"}, Refused: true},
	}
	// A search query's qualifiers naming an owner, as the query words of every search leaf, and
	// as the q of gh api's search endpoint.
	for _, qualifier := range []string{"repo:octocat/hello", "owner:octocat", "user:octocat", "org:octocat"} {
		for _, leaf := range []string{"search code", "search commits", "search issues", "search prs", "search repos"} {
			reads = append(reads, SelectorKindRead{Leaf: leaf, Kind: "argument query", Argv: append(strings.Fields(leaf), qualifier)})
		}
		reads = append(reads, SelectorKindRead{Leaf: "api", Kind: "endpoint search/<kind>?q=…", Argv: []string{"api", "search/issues?q=" + qualifier}})
	}
	return reads
}()

// LeafCommandCall is one row of ghroute's TestTargetOfNamesTheLeafCommand: a call, and the leaf
// TargetOf names it runs (ghroute.Target.Command), nil for none.
type LeafCommandCall struct {
	Argv, Command []string
}

// LeafCommandCalls are the leaf table's rows: a cobra alias, read as its command, and gh api, each
// naming its leaf; and a leaf's --help, a group alone, gh's version and `gh help` of a leaf, which
// run none.
var LeafCommandCalls = []LeafCommandCall{
	{[]string{"pr", "ls", "-R", "sjawhar/legion"}, []string{"pr", "list"}},
	{[]string{"-R", "sjawhar/legion", "pr", "view", "1", "--help"}, nil},
	{[]string{"api", "repos/sjawhar/legion"}, []string{"api"}},
	{[]string{"pr"}, nil},
	{[]string{"--version"}, nil},
	{[]string{"help", "pr", "view"}, nil},
}

// MadeUpToken is the made-up token PrintedSelectorCalls write where a person or a CI checkout
// could, of which no refusal `legion gh` prints of them may hold any part.
const MadeUpToken = "zqtoken0123"

// PrintedSelectorCall is one row of ghroute's TestLegionGhPrintsNoTokenOfASelectorOrAMarker that
// reads no git remote: a call in acme's pane whose -R or GH_REPO value holds MadeUpToken, and the
// words the refusal `legion gh` prints of it must hold (Names).
type PrintedSelectorCall struct {
	Argv  []string
	Env   map[string]string
	Names string
}

// PrintedSelectorCalls are the printed table's rows that read no git remote, keyed by each row's
// name: a URL with a user name or a password, on github.com or another host, and on another host
// with a token in its path, each named by its kind alone; and each value with no scheme, which
// the URL rule cannot read, as -R and as GH_REPO.
var PrintedSelectorCalls = func() map[string]PrintedSelectorCall {
	at := "@"
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	with := func(extra map[string]string) map[string]string {
		env := maps.Clone(pane)
		maps.Copy(env, extra)
		return env
	}
	tokenURL := func(host string) string {
		return "https://x-access-token:" + MadeUpToken + at + host + "/acme/widgets"
	}
	const kindWords = "holds an \"@\", a \":\", a \"?\" or a \"#\" where a GitHub repository's name holds none"
	schemeless := map[string]string{
		"a user name and a token":             "x-access-token:" + MadeUpToken + at + "ghe.example.com/acme/widgets",
		"a token as the user":                 MadeUpToken + at + "ghe.example.com/acme/widgets",
		"a token in the query":                "ghe.example.com/acme/widgets?t=" + MadeUpToken,
		"a token in the fragment":             "ghe.example.com/acme/widgets#" + MadeUpToken,
		"a user and password on www.github":   "x-access-token:" + MadeUpToken + at + "www.github.com/acme/widgets",
		"a user and password on github.com":   "x-access-token:" + MadeUpToken + at + "github.com/acme/widgets",
		"an scp-like token":                   MadeUpToken + at + "github.com:acme/widgets",
		"a space before the @, with a scheme": "https://x-access-token:zq tok0123" + at + "github.com/acme/widgets",
	}
	calls := map[string]PrintedSelectorCall{
		"GH_REPO with user info":      {[]string{"pr", "list"}, with(map[string]string{"GH_REPO": tokenURL("ghe.example.com")}), "the GH_REPO value holds a user name or a password"},
		"-R with user info":           {[]string{"pr", "list", "-R", tokenURL("ghe.example.com")}, with(nil), "a -R value holds a user name or a password"},
		"-R with user info on github": {[]string{"pr", "list", "-R", tokenURL("github.com")}, with(nil), "a -R value holds a user name or a password"},
		"-R on another host":          {[]string{"pr", "list", "-R", "https://ghe.example.com/" + MadeUpToken + "/widgets"}, with(nil), "a -R value names another host than github.com"},
		"GH_REPO on another host":     {[]string{"pr", "list"}, with(map[string]string{"GH_REPO": "ghe.example.com/" + MadeUpToken + "/widgets"}), "the GH_REPO value names another host than github.com"},
	}
	for shape, value := range schemeless {
		calls["-R, "+shape] = PrintedSelectorCall{[]string{"pr", "list", "-R", value}, with(nil), "a -R value " + kindWords}
		calls["GH_REPO, "+shape] = PrintedSelectorCall{[]string{"pr", "list"}, with(map[string]string{"GH_REPO": value}), "the GH_REPO value " + kindWords}
	}
	return calls
}()
