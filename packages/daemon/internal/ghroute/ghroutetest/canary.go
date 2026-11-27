package ghroutetest

// Canary is the made-up value the canary rows (CanaryRows) put in every slot of the call they
// make — each argument, each option's value, each selector, each environment value and each
// search query — shaped as the slot reads it (a token in a URL's user info, an owner's place, a
// search word), so that no refusal of legion gh can print any part of it but by quoting a slot's
// value whole.
const Canary = "zqcanary0123"

// CanaryRow is one ghroute.TargetOf call that reaches a refusal path, a call of ghroute.Shown,
// with Canary in its slots: Argv and Env, read under gh's saved help (SavedHelp) with no git
// remotes (NoGitRepository) and every owner's own directory (OwnersOwnDirectories); Names is what
// the refusal must say, the place the path names the value by, which shows the row reached that
// path.
type CanaryRow struct {
	Argv  []string
	Env   map[string]string
	Names string
}

// CanaryRows are the rows of every refusal path of internal/ghroute that ghroute.TargetOf
// reaches, keyed as ShownPaths keys a call of ghroute.Shown in ghroute's own source: the function
// that makes it (a method by its receiver's type) and the source of the place it names the value
// by. ghroute's TestNoRefusalPathPrintsACanary holds this table to its source and prints each
// row's refusal as legion gh prints it (ghroute.PrintRefusal); cmd/legion's test of that name runs
// each row as `legion gh` itself. A path TargetOf does not reach, whose row needs code of ghroute
// to run, has its rows in ghroute's own test (its canaryRuns).
var CanaryRows = map[string][]CanaryRow{
	`TargetOf: "another host than github.com"`: {
		{Argv: []string{"pr", "list", "--search", Canary}, Env: map[string]string{"GH_HOST": "x-access-token:" + Canary + "@ghe.example.com"}, Names: "GH_HOST names another host than github.com"},
		{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_HOST": Canary + ".example.com"}, Names: "GH_HOST names another host than github.com"},
	},
	`TargetOf: "an option given before the command's name"`: {
		{Argv: []string{"--token=" + Canary, "pr", "list", "--search", Canary}, Names: "an option given before the command's name is refused"},
		{Argv: []string{"--" + Canary, "pr", "list"}, Names: "an option given before the command's name is refused"},
	},
	`ghHelpTarget: "an option given to gh help"`: {
		{Argv: []string{"help", "--token=" + Canary, "pr"}, Names: "an option given to gh help is refused"},
	},
	`ghWalk.check: "the command's first word"`: {
		{Argv: []string{Canary, "list", "--body", Canary}, Names: "the command's first word is no command gh's own help lists"},
		{Argv: []string{"x-access-token:" + Canary + "@ghe.example.com", "list"}, Names: "the command's first word is no command gh's own help lists"},
	},
	`ghWalk.named: "a subcommand its help does not list"`: {
		{Argv: []string{"pr", Canary, "--body", Canary}, Names: "`gh pr` and a subcommand its help does not list"},
		{Argv: []string{"issue", "x-access-token:" + Canary, "1"}, Names: "`gh issue` and a subcommand its help does not list"},
	},
	`ghCommand.parse: "an option the command's help does not list"`: {
		{Argv: []string{"secret", "set", "DEPLOY_KEY", "--bdy=" + Canary, "-R", "acme/widgets"}, Names: "an option the command's help does not list is given to `gh secret set`"},
		{Argv: []string{"secret", "set", "DEPLOY_KEY", "--body " + Canary, "-R", "acme/widgets"}, Names: "an option the command's help does not list is given to `gh secret set`"},
		{Argv: []string{"pr", "list", "--token=" + Canary}, Names: "an option the command's help does not list is given to `gh pr list`"},
		{Argv: []string{"pr", "list", "--" + Canary}, Names: "an option the command's help does not list is given to `gh pr list`"},
	},
	`ghCommand.parse: "a short option the command's help does not list"`: {
		{Argv: []string{"secret", "set", "DEPLOY_KEY", "-Z" + Canary, "-R", "acme/widgets"}, Names: "a short option the command's help does not list is given to `gh secret set`"},
		{Argv: []string{"search", "issues", "bug", "-repo:x-access-token:" + Canary}, Names: "a short option the command's help does not list is given to `gh search issues`"},
	},
	`cloneGitFlags: "a git flag after --"`: {
		{Argv: []string{"repo", "clone", "acme/app", "--", "--config=http.https://github.com/.extraheader=AUTHORIZATION: basic " + Canary}, Names: "hands a git flag after -- to git clone"},
		{Argv: []string{"repo", "clone", "acme/app", "--", "--config=http.extraheader=AUTHORIZATION: bearer " + Canary}, Names: "hands a git flag after -- to git clone"},
		{Argv: []string{"repo", "clone", "acme/app", "--", "-chttp.extraheader=AUTHORIZATION: bearer " + Canary}, Names: "hands a git flag after -- to git clone"},
		{Argv: []string{"repo", "clone", "acme/app", "--", "dir-" + Canary, Canary}, Names: "hands a git flag after -- to git clone"},
	},
	`selectors.fromAPI: "the q field"`: {
		{Argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@" + Canary}, Names: "the q field reads the search query from a file"},
	},
	`selectors.fromOptions: "a repository named by its name alone"`: {
		{Argv: []string{"secret", "set", "DEPLOY_KEY", "--user", "--repos", Canary, "--body", Canary}, Names: "a repository named by its name alone is given to --repos, and no --org says whose"},
	},
	`selectors.query: "a word of the search query"`: {
		{Argv: []string{"search", "code", "password: hunter" + Canary}, Names: "a word of the search query is a search qualifier this cannot read"},
		{Argv: []string{"search", "issues", "label:\"" + Canary + "\""}, Names: "a word of the search query is a search qualifier this cannot read"},
	},
	`selectors.query: "a -repo:, -owner:, -user: or -org: qualifier of the search query"`: {
		{Argv: []string{"search", "issues", "--", "-repo:x-access-token:" + Canary + "@ghe.example.com"}, Names: "a -repo:, -owner:, -user: or -org: qualifier of the search query excludes an owner"},
	},
	`selectors.query: "an OR, a NOT or a parenthesis of the search query"`: {
		{Argv: []string{"search", "issues", "repo:acme/widgets", "(" + Canary}, Names: "an OR, a NOT or a parenthesis of the search query joins"},
	},
	`selectors.endpoint: "the endpoint's host"`: {
		{Argv: []string{"api", "https://x-access-token:" + Canary + "@ghe.example.com/repos/acme/app"}, Names: "the endpoint's host is another host than api.github.com"},
	},
	`selectors.endpoint: "the endpoint's repositories/<id> path"`: {
		{Argv: []string{"api", "repositories/1?access_token=" + Canary}, Names: "the endpoint's repositories/<id> path names a repository by its id"},
	},
	`selectors.endpoint: "the endpoint"`: {
		{Argv: []string{"api", "http://x-access-token:" + Canary + "@api.github.com/repos/acme/app"}, Names: "the endpoint is no https URL"},
	},
	`selectors.endpoint: "the endpoint's query string"`: {
		{Argv: []string{"api", "search/issues?q=x&access_token=" + Canary + "%zz"}, Names: "the endpoint's query string is one this cannot read"},
	},
	`repositoryTarget: kind`: {
		{Argv: []string{"pr", "list", "-R", "x-access-token:" + Canary + "@ghe.example.com/acme/widgets"}, Names: "a -R value holds an \"@\", a \":\", a \"?\" or a \"#\""},
		{Argv: []string{"pr", "list", "-R", "ghe.example.com/x-access-token:" + Canary + "@ghe.example.com/acme/app"}, Names: "a -R value holds an \"@\", a \":\", a \"?\" or a \"#\""},
		{Argv: []string{"pr", "list", "-R", "https://admin:hunter'" + Canary + "@ghe.example/acme/app"}, Names: "a -R value holds a user name or a password"},
		{Argv: []string{"pr", "list", "-R", "https://x-access-token:" + Canary + "@github.com/acme/widgets"}, Names: "a -R value holds a user name or a password"},
		{Argv: []string{"pr", "list", "-R", "https://ghe.example.com/" + Canary + "/widgets"}, Names: "a -R value names another host than github.com"},
		{Argv: []string{"pr", "list", "-R", Canary}, Names: "a -R value names no owner"},
		{Argv: []string{"pr", "list", "-R", "acme/" + Canary + "%zz"}, Names: "a -R value names a repository whose name holds a character"},
		{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "x-access-token:" + Canary + "@github.com/acme/widgets"}, Names: "the GH_REPO value holds an \"@\""},
		{Argv: []string{"pr", "list"}, Env: map[string]string{"GH_REPO": "https://ghe.example.com/acme/" + Canary}, Names: "the GH_REPO value names another host than github.com"},
		{Argv: []string{"repo", "view", "https://x-access-token:" + Canary + "@github.com/acme/widgets"}, Names: "a repository argument holds a user name or a password"},
		{Argv: []string{"search", "issues", "--", "repo:" + Canary}, Names: "a repo: qualifier names no owner"},
		{Argv: []string{"search", "issues", "repo:x-access-token:" + Canary + "@ghe.example/acme/app"}, Names: "a repo: qualifier holds an \"@\""},
		{Argv: []string{"api", "repos/x-access-token:" + Canary + "@ghe.example/acme"}, Names: "the endpoint holds an \"@\""},
	},
	`checkedOwner: "an owner that is not a GitHub login"`: {
		{Argv: []string{"repo", "list", "x-access-token:" + Canary}, Names: "an owner argument names an owner that is not a GitHub login"},
		{Argv: []string{"search", "repos", "--owner", "x-access-token:" + Canary}, Names: "a --owner value names an owner that is not a GitHub login"},
		{Argv: []string{"pr", "list", "-R", "https://github.com/x-access-token:" + Canary + "@github.com/acme/app"}, Names: "a -R value names an owner that is not a GitHub login"},
		{Argv: []string{"pr", "list", "-R", "github.com/" + Canary + ".x/app"}, Names: "a -R value names an owner that is not a GitHub login"},
		{Argv: []string{"api", "orgs/" + Canary + ".x/repos"}, Names: "the endpoint names an owner that is not a GitHub login"},
	},
}
