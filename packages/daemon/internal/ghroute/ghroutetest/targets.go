package ghroutetest

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// The resolver tables ghroute's TargetOf tests read (route_test.go), every row of which `legion
// gh`'s tests also resolve through its keep of gh's help (cmd/legion's ghTargetOfKept), as main's
// #4394 did: a keep that keeps each help it reads, and then a keep that reads the copies, must
// decide each row as gh's own help does, the refusal text included. A row that needs the working
// directory's git remotes stays beside the resolver, since the keep never reads them.

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
	WithAnExtensionAndANestedAlias = map[string][2]string{
		"":   {"ALIAS COMMANDS\n", "EXTENSION COMMANDS\n  hello:         Extension hello\n\nALIAS COMMANDS\n"},
		"pr": {"\nFLAGS\n", "\nALIAS COMMANDS\n  pv2:           Alias for \"pr view -R sjawhar/legion\"\n\nFLAGS\n"},
	}
	SpellingItsURLOtherwise = map[string][2]string{
		"pr view": {"<url>", "<URL>"},
	}
	WithARepositoryOption = map[string][2]string{
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
	{Argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, Env: ghRepoAcme, Err: "more than one owner, acme (GH_REPO acme/widgets) and sjawhar"},
	// Round 4's minor findings: options mentioning URLs or a team, and project link and unlink,
	// which read GH_REPO with -R a plain string (deep 3, deep 4).
	{Argv: []string{"project", "link", "1", "--owner", "acme"}, Env: map[string]string{"GH_REPO": "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"project", "unlink", "1", "--owner", "acme", "--team", "sjawhar/core"}, Err: "more than one owner"},
	{Argv: []string{"project", "link", "1", "--team", "core", "-R", "acme/widgets"}, Err: `"core" names no owner`},
	// Round 5's: gh takes a passthrough leaf's repository from the first argument even after
	// `--` (deep 1); project link and unlink read no repository of their own with --team
	// (quality 2).
	{Argv: []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, Err: "more than one owner"},
	{Argv: []string{"repo", "fork", "--", "sjawhar/legion", "--depth=1"}, Owner: "sjawhar"},
	// What a clone leaf hands git clone after its repository is git's, not gh's: a value naming
	// another owner's repository there routes nothing. Only the git clone flags ghroute's
	// cloneGitFlagTakes holds pass, so a -R there, which git clone has no use for, is refused.
	{Argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "--branch", "acme/w"}, Owner: "sjawhar"},
	{Argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "-R", "acme/w"}, Err: `hands "-R" to git clone`},
	{Argv: []string{"project", "unlink", "1", "--team", "sjawhar/core"}, Env: ghRepoAcme, Owner: "sjawhar"},
	// --version is gh's alone; before anything, gh reads the next word as a command, an
	// alias's or an extension's among them.
	{Argv: []string{"--version"}},
	{Argv: []string{"--version", "hello"}, Edits: WithAnExtensionAndANestedAlias, Err: `"--version" comes before the command's name`},
	{Argv: []string{"--version", "co"}, Err: `"--version" comes before the command's name`},
	{Argv: []string{"-R", "sjawhar/legion", "--version", "hello"}, Edits: WithAnExtensionAndANestedAlias, Err: `"--version" comes before the command's name`},
	{Argv: []string{"--help", "--version"}, Err: `"--version" comes before the command's name`},
	// A first word gh's own help does not list, whose help gh may answer by installing an
	// extension, and the commands that run another program on gh's token.
	{Argv: []string{"stack", "view"}, Err: `"stack" is no command gh's own help lists`},
	{Argv: []string{"ext", "exec", "hello"}, Err: `"ext" is no command gh's own help lists`},
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
	{Argv: []string{"pr", "view", "1", "--mirror", "sjawhar/legion", "-R", "acme/widgets"}, Edits: WithARepositoryOption, Err: "names a repository, an owner, a team or a URL in a way this does not know"},
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
	{Argv: []string{"issue", "comment", "1", "-R", "acme/widgets", "-e"}, Err: `"-e" runs an editor program`},
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
	{Argv: []string{"pr", "--json", "number", "view", "https://github.com/sjawhar/legion/pull/1"}, Err: `"--json" comes before the command's name`},
	// The first review's reproductions.
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: ghRepoAcme, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, Env: ghRepoAcme, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, Err: "more than one owner, acme (--repo acme/widgets) and sjawhar"},
	{Argv: []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, Env: ghRepoAcme, Owner: "sjawhar"},
	// The second re-review's reproductions. --help is a boolean, the last of its values
	// deciding, before the command's name or after; only true is help.
	{Argv: []string{"api", "repos/sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "--help", "view", "1", "-R", "sjawhar/legion", "--help=0"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--help"}},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "-h"}},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=maybe"}, Err: `"--help=maybe" gives --help a value that is no boolean`},
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
	{Argv: []string{"pr", "pv2", "1"}, Edits: WithAnExtensionAndANestedAlias, Err: `"pv2" is a gh alias`},
	{Argv: []string{"hello", "-R", "sjawhar/legion"}, Edits: WithAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, Edits: WithAnExtensionAndANestedAlias, Owner: "sjawhar"},
	{Argv: []string{"pr", "ls", "-R", "sjawhar/legion"}, Owner: "sjawhar"},
	{Argv: []string{"copilot"}, Err: "`gh copilot` runs another program"},
	// A search query names its owner in plain qualifiers, one to an argument, or not at all.
	{Argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, Owner: "sjawhar"},
	{Argv: []string{"search", "issues", "bug OR crash"}, Env: ghRepoAcme},
	{Argv: []string{"search", "issues", "(repo:sjawhar/legion)"}, Err: `"(repo:sjawhar/legion)" is a search qualifier this cannot read for certain`},
	{Argv: []string{"search", "issues", "repo:sjawhar/legion is:open"}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"search", "issues", `label:"help wanted"`}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"search", "issues", "repo:sjawhar/legion", "OR", "bug"}, Err: `"OR" joins the query's`},
	{Argv: []string{"search", "issues", "(", "repo:sjawhar/legion", ")"}, Err: `"(" joins the query's`},
	{Argv: []string{"search", "issues", "--", "bug", "-repo:acme/widgets"}, Err: `"-repo:acme/widgets" excludes an owner`},
	{Argv: []string{"search", "issues", "bug", "-repo:acme/widgets"}, Err: `"-repo:acme/widgets" names -r`},
	{Argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, Owner: "sjawhar"},
	{Argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=repo:sjawhar/legion is:open"}, Owner: "sjawhar"},
	{Argv: []string{"api", "search/issues?q=%28repo:sjawhar/legion%29"}, Err: "is a search qualifier this cannot read for certain"},
	{Argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@query.txt"}, Err: "reads the search query from a file"},
	// gh sends its token only to GH_HOST, and GH_TOKEN or GITHUB_TOKEN in place of any file's.
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: map[string]string{"GH_HOST": "ghe.example.com"}, Err: `GH_HOST is "ghe.example.com"`},
	{Argv: []string{"api", "repos/sjawhar/legion"}, Env: map[string]string{"GH_HOST": "GitHub.com"}, Owner: "sjawhar"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_TOKEN": "ghp_x"}, Err: "GH_TOKEN is set"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}, Err: "GITHUB_TOKEN is set"},
	// An argument the leaf's usage names in a way this does not know, or has no place for.
	{Argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1"}, Edits: SpellingItsURLOtherwise, Err: "<URL>"},
	{Argv: []string{"pr", "view", "1"}, Edits: SpellingItsURLOtherwise, Err: "<URL>"},
	{Argv: []string{"pr", "view", "1", "2"}, Err: `"2" is an argument`},
	{Argv: []string{"pr", "list", "sjawhar/legion"}, Err: `"sjawhar/legion" is an argument`},
	{Argv: []string{"api", "repos/acme/widgets", "repos/sjawhar/legion"}, Err: `"repos/sjawhar/legion" is an argument`},
	// The third re-review's: gh's help of an extension runs the extension, so the target of
	// `gh help` is read as a command's path, a gh alias or an extension refused before anything
	// runs; gh's own help and its topics are not. And a plural slot, `{<numbers> | <urls>}`,
	// takes one or more arguments, every one a selector.
	{Argv: []string{"help"}},
	{Argv: []string{"help", "pr", "view"}},
	{Argv: []string{"help", "environment"}},
	{Argv: []string{"help", "co"}, Err: `"co" is a gh alias`},
	{Argv: []string{"help", "hello"}, Edits: WithAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"help", "pr", "pv2"}, Edits: WithAnExtensionAndANestedAlias, Err: `"pv2" is a gh alias`},
	{Argv: []string{"-R", "acme/widgets", "help", "hello"}, Edits: WithAnExtensionAndANestedAlias, Err: `"hello" is a gh extension`},
	{Argv: []string{"help", "--bogus", "hello"}, Err: "\"--bogus\" is an option of `gh help`"},
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
	{Argv: []string{"pr", "view", "1", "--bogus"}, Err: `"--bogus" is no option`},
	{Argv: []string{"pr", "view", "1", "-Z"}, Err: "names -Z"},
	{Argv: []string{"pr", "nonexist"}, Err: "gh has no command `gh pr nonexist`"},
	{Argv: []string{"nonexist"}, Err: `"nonexist" is no command gh's own help lists`},
	{Argv: []string{"--bogus", "pr", "list"}, Err: `"--bogus" comes before the command's name`},
	{Argv: []string{"api", "repositories/12345"}, Err: "by its id"},
	{Argv: []string{"api", "https://ghe.example.com/api/v3/repos/acme/w"}, Err: "another host"},
	{Argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/w"}, Err: "another host"},
	// An absolute endpoint is GitHub's own https://api.github.com/… or https://uploads.github.com/…
	// or nothing: gh sends the bundle's token to whatever host and scheme it names, so an http://
	// one hands the token over in the clear, GitHub's 301 to https coming only after it has it.
	{Argv: []string{"api", "http://api.github.com/repos/acme/w"}, Err: "is not https"},
	{Argv: []string{"api", "HTTP://API.GITHUB.COM/repos/acme/w"}, Err: "is not https"},
	{Argv: []string{"api", "ftp://api.github.com/repos/acme/w"}, Err: "is not https"},
	{Argv: []string{"api", "-X", "POST", "http://api.github.com/repos/acme/w/issues/1/comments"}, Err: "is not https"},
	// gh api's whole URL is placed only over https and on api.github.com or uploads.github.com,
	// spelled in ASCII: another GitHub host, http, and `ſ` (U+017F), which strings.EqualFold
	// folds onto `s`, are refused.
	{Argv: []string{"api", "https://github.com/repos/acme/widgets"}, Err: "another host"},
	{Argv: []string{"api", "https://gist.github.com/repos/acme/widgets"}, Err: "another host"},
	{Argv: []string{"api", "https://upload\u017f.github.com/repos/acme/widgets/releases/1/assets?name=x"}, Err: "names another host than api.github.com or uploads.github.com"},
	{Argv: []string{"api", "http://uploads.github.com/repos/acme/widgets/releases/1/assets?name=x"}, Err: "is not https"},
	// A host is compared in ASCII alone: `İ` (U+0130), which strings.ToLower lowers to `i`,
	// makes no github.com of `gİthub.com`, in -R, a URL argument or --hostname.
	{Argv: []string{"pr", "view", "1", "-R", "g\u0130thub.com/acme/widgets"}, Err: "names another host than github.com"},
	{Argv: []string{"pr", "view", "https://g\u0130thub.com/acme/widgets/pull/1"}, Err: "names another host than github.com"},
	{Argv: []string{"api", "--hostname", "g\u0130thub.com", "repos/acme/w"}, Err: "names another host than github.com"},
	{Argv: []string{"api", "https://api.github.com/repos/Acme/w"}, Owner: "acme"},
	{Argv: []string{"api", "repos/../x/y"}, Err: `"..", which is no GitHub login`},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "../x/y"}, Err: `names another host`},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "../y"}, Err: "no GitHub login"},
	{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "widgets"}, Err: "names no owner"},
	{Argv: []string{"repo", "view", "widgets"}, Err: "names no owner"},
	{Argv: []string{"pr", "view", "1", "-R"}, Err: "has no value"},
	{Argv: []string{"pr", "view", "1", "-R="}, Err: `"=" names no owner`},
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
// the command's name, main's #4472: each is refused but as the whole argv (the walk's -h case
// says why); `gh pr --help` is the way to ask. The rows read gh 2.98.0's help of every command,
// saved whole, which holds `gh status`, so the words of a -h row are gh's commands and the row is
// refused for its -h alone.
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

// SentBy is the leakguard.GhSent that steps build, each in turn, on the zero value: calls of
// its exported methods Names and UsesStdin (SentNames, SentUsesStdin), the two ghroute's
// sentFiles builds its answer with. Its fields of standard input are leakguard's own and no
// literal here can set them, so an answer to compare whole is built so.
func SentBy(steps ...func(*leakguard.GhSent)) *leakguard.GhSent {
	sent := &leakguard.GhSent{}
	for _, step := range steps {
		step(sent)
	}
	return sent
}

// SentNames is the step of SentBy that names path as option's value (leakguard.GhSent.Names).
func SentNames(option, path, field string, use leakguard.StdinUse) func(*leakguard.GhSent) {
	return func(sent *leakguard.GhSent) { sent.Names(option, path, field, use) }
}

// SentUsesStdin is the step of SentBy that names standard input with use alone
// (leakguard.GhSent.UsesStdin).
func SentUsesStdin(use leakguard.StdinUse) func(*leakguard.GhSent) {
	return func(sent *leakguard.GhSent) { sent.UsesStdin(use) }
}

// SentFileCall is one row of ghroute's TestGhTargetOfReadsTheFilesTheCallWouldSend: a call and
// what TargetOf reads it would send to GitHub beside its argv (ghroute.Target.Sent), the parse's
// whole answer, built by the calls sentFiles makes for it (SentBy).
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
		Want: SentBy(SentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "a short option with its value attached by an equals sign",
		Argv: []string{"pr", "create", "-R", "acme/widgets", "-F=body.md"},
		Want: SentBy(SentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "a separate --body-file",
		Argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "body.md"},
		Want: SentBy(SentNames("--body-file", "body.md", "", leakguard.StdinText)),
	},
	{
		Name: "gh api's field read from a file",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "--method", "PATCH", "-F", "body=@body.md"},
		Want: SentBy(SentNames("--field", "body.md", "body", leakguard.StdinField)),
	},
	{
		Name: "gh api's request body",
		Argv: []string{"api", "repos/acme/widgets/pulls", "--input", "request.json"},
		Want: SentBy(SentNames("--input", "request.json", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "a raw field, which sends its value literally",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-f", "body=@body.md"},
		Want: SentBy(),
	},
	{
		Name: "a field given a literal value",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=text"},
		Want: SentBy(),
	},
	{
		Name: "a body option whose value is standard input",
		Argv: []string{"pr", "comment", "1", "-R", "acme/widgets", "--body-file", "-"},
		Want: SentBy(SentNames("--body-file", "-", "", leakguard.StdinText)),
	},
	{
		Name: "a short option whose value is standard input",
		Argv: []string{"pr", "create", "-R", "acme/widgets", "-F", "-"},
		Want: SentBy(SentNames("--body-file", "-", "", leakguard.StdinText)),
	},
	{
		Name: "a field read from standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@-"},
		Want: SentBy(SentNames("--field", "-", "body", leakguard.StdinField)),
	},
	{
		Name: "a GraphQL query read from standard input",
		Argv: []string{"api", "graphql", "-F", "query=@-"},
		Want: SentBy(SentNames("--field", "-", "query", leakguard.StdinField)),
	},
	{
		// The options are read in the order their names sort, so the field before --input.
		Name: "a GraphQL query read from standard input beside a request body on it",
		Argv: []string{"api", "graphql", "--input", "-", "-F", "query=@-"},
		Want: SentBy(SentNames("--field", "-", "query", leakguard.StdinField),
			SentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's field read from standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "-F", "notes=@-"},
		Want: SentBy(SentNames("--field", "-", "notes", leakguard.StdinField)),
	},
	{
		Name: "gh api's request body on standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls", "--input", "-"},
		Want: SentBy(SentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's inputs as JSON on standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json"},
		Want: SentBy(SentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's --json=true, which sends standard input as its inputs",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json=true"},
		Want: SentBy(SentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "gh workflow run's --json whose last value is false",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json", "--json=false"},
		Want: SentBy(),
	},
	{
		Name: "gh workflow run's --json beside a field read from standard input",
		Argv: []string{"workflow", "run", "ci.yaml", "-R", "acme/widgets", "--json", "-F", "notes=@-"},
		Want: SentBy(SentNames("--field", "-", "notes", leakguard.StdinField),
			SentUsesStdin(leakguard.StdinRequestBody)),
	},
	{
		Name: "another command's --json, which names the fields to print",
		Argv: []string{"pr", "list", "-R", "acme/widgets", "--json", "number"},
		Want: SentBy(),
	},
	{
		Name: "two fields read from standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "title=@-", "-F", "body=@-"},
		Want: SentBy(SentNames("--field", "-", "title", leakguard.StdinField),
			SentNames("--field", "-", "body", leakguard.StdinField)),
	},
	{
		Name: "two different options naming standard input",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "--input", "-", "-F", "body=@-"},
		Want: SentBy(SentNames("--field", "-", "body", leakguard.StdinField),
			SentNames("--input", "-", "", leakguard.StdinRequestBody)),
	},
	{
		// Named once, by the option whose name sorts first.
		Name: "the same file named twice",
		Argv: []string{"api", "repos/acme/widgets/pulls/1", "-F", "body=@body.md", "--input", "body.md"},
		Want: SentBy(SentNames("--field", "body.md", "body", leakguard.StdinField)),
	},
}
