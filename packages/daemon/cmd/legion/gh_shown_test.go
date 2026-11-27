package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghShownCanary is the made-up value the canary rows put in every slot of the call they make —
// each argument, each option's value, each selector, each environment value and each search query
// — shaped as the slot reads it (a token in a URL's user info, an owner's place, a search word), so
// that no refusal of legion gh can print any part of it but by quoting a slot's value whole.
const ghShownCanary = "zqcanary0123"

// ghCanaryRow is one call that reaches a refusal path (ghShownPaths) with ghShownCanary in its
// slots: argv and env for ghroute.TargetOf, under savedGhHelp unless help is set and with no git
// remotes unless remotes is set, or run for a path ghroute.TargetOf does not reach; names is what
// the refusal must say, the place the path names the value by, which shows the row reached that
// path.
type ghCanaryRow struct {
	argv    []string
	env     map[string]string
	help    func(path []string) (string, error)
	remotes func() ([]ghroute.Remote, error)
	run     func() error
	names   string
}

// ghCanaryRows are the rows of every refusal path, keyed as ghShownPaths keys a call of
// ghroute.Shown: the function that makes it, internal/ghroute's under `ghroute.`, and the source
// of the place it names the value by.
var ghCanaryRows = map[string][]ghCanaryRow{
	`ghroute.TargetOf: "another host than github.com"`: {
		{argv: []string{"pr", "list", "--search", ghShownCanary}, env: map[string]string{"GH_HOST": "x-access-token:" + ghShownCanary + "@ghe.example.com"}, names: "GH_HOST names another host than github.com"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_HOST": ghShownCanary + ".example.com"}, names: "GH_HOST names another host than github.com"},
	},
	`ghroute.TargetOf: "an option given before the command's name"`: {
		{argv: []string{"--token=" + ghShownCanary, "pr", "list", "--search", ghShownCanary}, names: "an option given before the command's name is refused"},
		{argv: []string{"--" + ghShownCanary, "pr", "list"}, names: "an option given before the command's name is refused"},
	},
	`ghroute.ghHelpTarget: "an option given to gh help"`: {
		{argv: []string{"help", "--token=" + ghShownCanary, "pr"}, names: "an option given to gh help is refused"},
	},
	`ghroute.ghWalk.check: "the command's first word"`: {
		{argv: []string{ghShownCanary, "list", "--body", ghShownCanary}, names: "the command's first word is no command gh's own help lists"},
		{argv: []string{"x-access-token:" + ghShownCanary + "@ghe.example.com", "list"}, names: "the command's first word is no command gh's own help lists"},
	},
	`ghroute.ghWalk.named: "a subcommand its help does not list"`: {
		{argv: []string{"pr", ghShownCanary, "--body", ghShownCanary}, names: "`gh pr` and a subcommand its help does not list"},
		{argv: []string{"issue", "x-access-token:" + ghShownCanary, "1"}, names: "`gh issue` and a subcommand its help does not list"},
	},
	`ghroute.ghCommand.parse: "an option the command's help does not list"`: {
		{argv: []string{"secret", "set", "DEPLOY_KEY", "--bdy=" + ghShownCanary, "-R", "acme/widgets"}, names: "an option the command's help does not list is given to `gh secret set`"},
		{argv: []string{"secret", "set", "DEPLOY_KEY", "--body " + ghShownCanary, "-R", "acme/widgets"}, names: "an option the command's help does not list is given to `gh secret set`"},
		{argv: []string{"pr", "list", "--token=" + ghShownCanary}, names: "an option the command's help does not list is given to `gh pr list`"},
		{argv: []string{"pr", "list", "--" + ghShownCanary}, names: "an option the command's help does not list is given to `gh pr list`"},
	},
	`ghroute.ghCommand.parse: "a short option the command's help does not list"`: {
		{argv: []string{"secret", "set", "DEPLOY_KEY", "-Z" + ghShownCanary, "-R", "acme/widgets"}, names: "a short option the command's help does not list is given to `gh secret set`"},
		{argv: []string{"search", "issues", "bug", "-repo:x-access-token:" + ghShownCanary}, names: "a short option the command's help does not list is given to `gh search issues`"},
	},
	`ghroute.cloneGitFlags: "a git flag after --"`: {
		{argv: []string{"repo", "clone", "acme/app", "--", "--config=http.https://github.com/.extraheader=AUTHORIZATION: basic " + ghShownCanary}, names: "hands a git flag after -- to git clone"},
		{argv: []string{"repo", "clone", "acme/app", "--", "--config=http.extraheader=AUTHORIZATION: bearer " + ghShownCanary}, names: "hands a git flag after -- to git clone"},
		{argv: []string{"repo", "clone", "acme/app", "--", "-chttp.extraheader=AUTHORIZATION: bearer " + ghShownCanary}, names: "hands a git flag after -- to git clone"},
		{argv: []string{"repo", "clone", "acme/app", "--", "dir-" + ghShownCanary, ghShownCanary}, names: "hands a git flag after -- to git clone"},
	},
	`ghroute.selectors.fromAPI: "the q field"`: {
		{argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@" + ghShownCanary}, names: "the q field reads the search query from a file"},
	},
	`ghroute.selectors.fromOptions: "a repository named by its name alone"`: {
		{argv: []string{"secret", "set", "DEPLOY_KEY", "--user", "--repos", ghShownCanary, "--body", ghShownCanary}, names: "a repository named by its name alone is given to --repos, and no --org says whose"},
	},
	`ghroute.selectors.query: "a word of the search query"`: {
		{argv: []string{"search", "code", "password: hunter" + ghShownCanary}, names: "a word of the search query is a search qualifier this cannot read"},
		{argv: []string{"search", "issues", "label:\"" + ghShownCanary + "\""}, names: "a word of the search query is a search qualifier this cannot read"},
	},
	`ghroute.selectors.query: "a -repo:, -owner:, -user: or -org: qualifier of the search query"`: {
		{argv: []string{"search", "issues", "--", "-repo:x-access-token:" + ghShownCanary + "@ghe.example.com"}, names: "a -repo:, -owner:, -user: or -org: qualifier of the search query excludes an owner"},
	},
	`ghroute.selectors.query: "an OR, a NOT or a parenthesis of the search query"`: {
		{argv: []string{"search", "issues", "repo:acme/widgets", "(" + ghShownCanary}, names: "an OR, a NOT or a parenthesis of the search query joins"},
	},
	`ghroute.selectors.endpoint: "the endpoint's host"`: {
		{argv: []string{"api", "https://x-access-token:" + ghShownCanary + "@ghe.example.com/repos/acme/app"}, names: "the endpoint's host is another host than api.github.com"},
	},
	`ghroute.selectors.endpoint: "the endpoint's repositories/<id> path"`: {
		{argv: []string{"api", "repositories/1?access_token=" + ghShownCanary}, names: "the endpoint's repositories/<id> path names a repository by its id"},
	},
	`ghroute.selectors.endpoint: "the endpoint"`: {
		{argv: []string{"api", "http://x-access-token:" + ghShownCanary + "@api.github.com/repos/acme/app"}, names: "the endpoint is no https URL"},
	},
	`ghroute.selectors.endpoint: "the endpoint's query string"`: {
		{argv: []string{"api", "search/issues?q=x&access_token=" + ghShownCanary + "%zz"}, names: "the endpoint's query string is one this cannot read"},
	},
	`ghroute.repositoryTarget: kind`: {
		{argv: []string{"pr", "list", "-R", "x-access-token:" + ghShownCanary + "@ghe.example.com/acme/widgets"}, names: "a -R value holds an \"@\", a \":\", a \"?\" or a \"#\""},
		{argv: []string{"pr", "list", "-R", "ghe.example.com/x-access-token:" + ghShownCanary + "@ghe.example.com/acme/app"}, names: "a -R value holds an \"@\", a \":\", a \"?\" or a \"#\""},
		{argv: []string{"pr", "list", "-R", "https://admin:hunter'" + ghShownCanary + "@ghe.example/acme/app"}, names: "a -R value holds a user name or a password"},
		{argv: []string{"pr", "list", "-R", "https://x-access-token:" + ghShownCanary + "@github.com/acme/widgets"}, names: "a -R value holds a user name or a password"},
		{argv: []string{"pr", "list", "-R", "https://ghe.example.com/" + ghShownCanary + "/widgets"}, names: "a -R value names another host than github.com"},
		{argv: []string{"pr", "list", "-R", ghShownCanary}, names: "a -R value names no owner"},
		{argv: []string{"pr", "list", "-R", "acme/" + ghShownCanary + "%zz"}, names: "a -R value names a repository whose name holds a character"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "x-access-token:" + ghShownCanary + "@github.com/acme/widgets"}, names: "the GH_REPO value holds an \"@\""},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "https://ghe.example.com/acme/" + ghShownCanary}, names: "the GH_REPO value names another host than github.com"},
		{argv: []string{"repo", "view", "https://x-access-token:" + ghShownCanary + "@github.com/acme/widgets"}, names: "a repository argument holds a user name or a password"},
		{argv: []string{"search", "issues", "--", "repo:" + ghShownCanary}, names: "a repo: qualifier names no owner"},
		{argv: []string{"search", "issues", "repo:x-access-token:" + ghShownCanary + "@ghe.example/acme/app"}, names: "a repo: qualifier holds an \"@\""},
		{argv: []string{"api", "repos/x-access-token:" + ghShownCanary + "@ghe.example/acme"}, names: "the endpoint holds an \"@\""},
	},
	`ghroute.checkedOwner: "an owner that is not a GitHub login"`: {
		{argv: []string{"repo", "list", "x-access-token:" + ghShownCanary}, names: "an owner argument names an owner that is not a GitHub login"},
		{argv: []string{"search", "repos", "--owner", "x-access-token:" + ghShownCanary}, names: "a --owner value names an owner that is not a GitHub login"},
		{argv: []string{"pr", "list", "-R", "https://github.com/x-access-token:" + ghShownCanary + "@github.com/acme/app"}, names: "a -R value names an owner that is not a GitHub login"},
		{argv: []string{"pr", "list", "-R", "github.com/" + ghShownCanary + ".x/app"}, names: "a -R value names an owner that is not a GitHub login"},
		{argv: []string{"api", "orgs/" + ghShownCanary + ".x/repos"}, names: "the endpoint names an owner that is not a GitHub login"},
	},
	`ghroute.ForkDir: "the repository the request names"`: {
		{run: func() error {
			repo, err := ghrepo.Parse("the canary's repository", ghShownCanary+"/app")
			if err != nil {
				return err
			}
			_, _, err = ghroute.ForkDir(os.TempDir(), repo, "", "")
			return err
		}, names: "the repository the request names is a fork"},
	},
	`withGitHubUseHTTPPath: "a value that is not printed"`: {
		{run: func() error {
			_, err := withGitHubUseHTTPPath([]string{"GIT_CONFIG_COUNT=" + ghShownCanary})
			return err
		}, names: "GIT_CONFIG_COUNT holds a value that is not printed, which is no count"},
	},
}

// ghSourceCall is one call in the non-test files ghSources names as ghSourceCalls hands it on: the
// function that holds it (owner, a method by its receiver's type, internal/ghroute's under
// `ghroute.`), the file and line it stands at, the call, and text, which is a node's source.
type ghSourceCall struct {
	owner, at string
	call      *ast.CallExpr
	text      func(ast.Node) string
}

// ghSources are the non-test files of legion gh the walk reads, each glob with the prefix its
// functions are named under: this package's gh*.go, and internal/ghroute's, the routing every
// call of legion gh runs through (ghroute.TargetOf), whose refusals legion gh prints.
var ghSources = []struct{ glob, prefix string }{
	{"gh*.go", ""},
	{filepath.Join("..", "..", "internal", "ghroute", "*.go"), "ghroute."},
}

// ghSourceCalls is every call in the non-test files ghSources names, in each function's body. It
// is the one walk ghShownPaths and ghFormatSites both read, so a file or a receiver form one of
// them reaches the other reaches too. A source glob matching no non-test file is refused: the
// walk would have read nothing of it.
func ghSourceCalls(t *testing.T) []ghSourceCall {
	t.Helper()
	var calls []ghSourceCall
	fset := token.NewFileSet()
	for _, src := range ghSources {
		files, err := filepath.Glob(src.glob)
		if err != nil {
			t.Fatal(err)
		}
		read := 0
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			read++
			source, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(fset, name, source, 0)
			if err != nil {
				t.Fatal(err)
			}
			text := func(node ast.Node) string {
				return string(source[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				owner := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) == 1 {
					recv := fn.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					switch generic := recv.(type) {
					case *ast.IndexExpr:
						recv = generic.X
					case *ast.IndexListExpr:
						recv = generic.X
					}
					if ident, ok := recv.(*ast.Ident); ok {
						owner = ident.Name + "." + owner
					} else {
						t.Fatalf("%s: a receiver of a form the walk does not read: %s", fset.Position(fn.Pos()), text(recv))
					}
				}
				owner = src.prefix + owner
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok {
						calls = append(calls, ghSourceCall{owner: owner, at: fset.Position(call.Pos()).String(), call: call, text: text})
					}
					return true
				})
			}
		}
		if read == 0 {
			t.Fatalf("UNKNOWN: no non-test file of %s was read", src.glob)
		}
	}
	return calls
}

// ghShownCallees are how a call of ghroute.Shown reads in the source: by its own name inside
// internal/ghroute, and by its package's name here.
var ghShownCallees = []string{"Shown", "ghroute.Shown"}

// ghShownPaths is every call of ghroute.Shown in the non-test files ghSources names, the refusal
// paths of legion gh that put a person's value into a refusal, each keyed by the function that
// makes it (a method by its receiver's type) and the source of its place argument, with the file
// and line it stands at. It is read from the source, so a call added to the code is a path here
// without anyone listing it. A place two calls of one function share would key both as one, and
// is refused.
func ghShownPaths(t *testing.T) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for _, c := range ghSourceCalls(t) {
		if !slices.Contains(ghShownCallees, c.text(c.call.Fun)) {
			continue
		}
		if len(c.call.Args) != 2 {
			t.Fatalf("%s: ghroute.Shown takes a value and a place, given %d arguments", c.at, len(c.call.Args))
		}
		key := c.owner + ": " + c.text(c.call.Args[1])
		if prior, seen := paths[key]; seen {
			t.Fatalf("%s and %s both call ghroute.Shown as %s: give each path a place of its own", prior, c.at, key)
		}
		paths[key] = c.at
	}
	return paths
}

// Every refusal path of legion gh that puts a value a person gave it into a refusal — every call
// of ghroute.Shown the source holds — has rows, and every row's path is one the source holds: a
// new path fails this until it joins the table. Each row puts ghShownCanary in every slot of its
// call and reaches its path, which names the value by its place (the row's names), and the canary
// reaches neither stdout nor stderr: ghroute.TargetOf's refusal printed as legion gh prints it
// (ghroute.PrintRefusal), and, for a row that needs no help, remotes or code of its own, `legion
// gh` itself, run as a pane runs it, with the saved help's stand-in gh, whose own output the help
// read never prints. A source holding no path at all is refused: the table would test nothing.
func TestNoRefusalPathPrintsACanary(t *testing.T) {
	paths := ghShownPaths(t)
	if len(paths) == 0 {
		t.Fatal("UNKNOWN: the source holds no call of ghroute.Shown, so this reads no refusal path")
	}
	for _, key := range slices.Sorted(maps.Keys(paths)) {
		if len(ghCanaryRows[key]) == 0 {
			t.Errorf("the refusal path %s (%s) has no row in ghCanaryRows: a new path joins the table", key, paths[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(ghCanaryRows)) {
		if _, ok := paths[key]; !ok {
			t.Errorf("ghCanaryRows names the refusal path %s, which no call of ghroute.Shown in the source has", key)
		}
	}
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf 'gh ran %s\\n' \"$*\"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pane := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PATH": ghWrapperDir(t) + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
	}
	rows, ran := 0, 0
	for _, key := range slices.Sorted(maps.Keys(ghCanaryRows)) {
		for i, row := range ghCanaryRows[key] {
			rows++
			t.Run(key+"/"+strconv.Itoa(i), func(t *testing.T) {
				err := ghCanaryRefusal(row)
				if err == nil {
					t.Fatalf("%v with %v = nil error, want a refusal by the path %s", row.argv, row.env, key)
				}
				var stderr bytes.Buffer
				ghroute.PrintRefusal(&stderr, err)
				if strings.Contains(stderr.String(), ghShownCanary) {
					t.Errorf("the path %s: legion gh printed %q, which holds the canary", key, stderr.String())
				}
				if !strings.Contains(stderr.String(), row.names) {
					t.Errorf("the path %s: legion gh printed %q, want it to name %q", key, stderr.String(), row.names)
				}
				if row.help != nil || row.remotes != nil || row.run != nil {
					return
				}
				ran++
				env := maps.Clone(pane)
				maps.Copy(env, row.env)
				code, stdout, stderrOf := legionGh(t, env, row.argv...)
				if code != 2 || strings.Contains(stdout, ghShownCanary) || strings.Contains(stderrOf, ghShownCanary) || !strings.Contains(stderrOf, row.names) {
					t.Errorf("the path %s: legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 naming %q, the canary in neither", key, row.argv, code, stdout, stderrOf, row.names)
				}
			})
		}
	}
	t.Logf("%d refusal paths, %d rows, %d of them run as legion gh too", len(paths), rows, ran)
}

// ghCanaryRefusal is the refusal row makes: its own run's, or ghroute.TargetOf's of its argv and
// env.
func ghCanaryRefusal(row ghCanaryRow) error {
	if row.run != nil {
		return row.run()
	}
	help, remotes := row.help, row.remotes
	if help == nil {
		help = savedGhHelp
	}
	if remotes == nil {
		remotes = noGitRepository
	}
	_, err := ghroute.TargetOf(row.argv, row.env, help, remotes, ghroutetest.OwnersOwnDirectories)
	return err
}

// ghFormattedValues are the places in the non-test files ghSources names that quote or format a
// value themselves, without ghroute.Shown, each keyed as ghFormatSites keys it and given the
// reason it is no value a person typed in a slot a refusal names: a name gh's own help or this
// command's tables gave, a position, a kind built from literals, a configured path, a value of
// GitHub's answer. A new such place fails TestNoRefusalFormatsAPersonsValueItself until it joins
// this table, which is where a reviewer reads it.
var ghFormattedValues = map[string]string{
	"ghConfigShape: %w err":                                         "a read error of a file of the gh directory GH_CONFIG_DIR names, a configured path",
	"ghWrapperPath: %w err":                                         "a read error of the wrapper's own path on PATH",
	"runGhChild: %w cause":                                          "the cause of a stop: a signal or a deadline",
	"runGhChild: %w err (2 places)":                                 "exec's error of starting LEGION_GH_PATH, a configured path",
	"ghCallLogValue: strconv.Quote(value)":                          "the call log's fields (role, issue, a placed repository, gh's command path, a visibility), no refusal",
	"appendGhCallLog: %s ghCallLogValue(commandPath)":               "the call log's field, quoted by ghCallLogValue",
	"appendGhCallLog: %s ghCallLogValue(env[\"LEGION_ISSUE\"])":     "the call log's field, quoted by ghCallLogValue",
	"appendGhCallLog: %s ghCallLogValue(env[\"LEGION_ROLE\"])":      "the call log's field, quoted by ghCallLogValue",
	"appendGhCallLog: %s ghCallLogValue(repo)":                      "the call log's field, quoted by ghCallLogValue",
	"appendGhCallLog: %s ghCallLogValue(visibility)":                "the call log's field, quoted by ghCallLogValue",
	"appendGhCallLog: %s time.Now().UTC().Format(time.RFC3339Nano)": "a clock reading",
	"ghConfigShape: %s key":                                         "a key of ghRoutingKeys, a table",
	"ghConfigShape: %s path (8 places)":                             "a file of the gh directory GH_CONFIG_DIR names, a configured path",
	"ghRepoGitConfigRefusal: %s key (8 places)":                     "a git config key, which ghroute.PrintRefusal passes through the URL rule",
	"ghRepoGitConfigRefusal: %s origin (4 places)":                  "the config file git names for the key, a path",
	"ghRepoGitConfigRefusal: %s procgroup.WaitDelayNow()":           "the WaitDelay procgroup gives git's children, a duration this program sets",
	"ghRepoGitConfigRefusal: %s strings.TrimSpace(stderr.String())": "git's own error output, which ghroute.PrintRefusal passes through the URL rule",
	"ghRepoGitConfigRefusal: %v err":                                "git's exit status",
	"ghWrapperPath: %s wrapper (2 places)":                          "the wrapper's own path on PATH",
	"ghWrapperPath: %v err":                                         "the error of resolving that path",
	"refuseLegionGhAsRealGh: %q gh":                                 "LEGION_GH_PATH, a configured path, no argument of the call",
	"refuseLegionGhAsRealGh: %s gh":                                 "LEGION_GH_PATH, a configured path, no argument of the call",
	"runGh: %s ghUsage":                                             "this command's usage, a literal",
	"runGh: %s name":                                                "the name of an environment variable",
	"runGhChild: %s gh (3 places)":                                  "LEGION_GH_PATH, a configured path",

	"ghroute.DirFor: %w err (2 places)":                                                              "a read error of the bundle's configured renames file, a configured path, or ownerDir's leakguard.OwnerNotServedError of the configured pair's `to` owner and the bundle's path",
	"ghroute.DirFor: %s pair.From":                                                                   "a configured rename's `from` repository, from the bundle's pairs file the daemon writes, which holds the repository the call named as configured",
	"ghroute.DirFor: %s pair.To":                                                                     "a configured rename's `to` repository, from the bundle's pairs file the daemon writes",
	"ghroute.DirFor: %s dir":                                                                         "the configured pair's `to` owner, lowercased (ghconfig.RouteDir)",
	"ghroute.ForkDir: %s dir":                                                                        "a directory of the bundle, its configured path and an owner GitHub's answer names",
	"ghroute.ForkDir: %s named (3 places)":                                                           "Shown's words for the repository the request names, never the repository",
	"ghroute.ForkDir: %s strings.Join(tried, \"; \")":                                                "why each upstream GitHub's answer names did not route, built from literals, GitHub's answer and the errors this table holds",
	"ghroute.ForkDir: %s upstream.fullName (5 places)":                                               "a repository's full_name in GitHub's answer for the fork, no value of the call",
	"ghroute.ForkDir: %s upstream.role (3 places)":                                                   "parent or source, a literal of this function",
	"ghroute.ForkDir: %v err (3 places)":                                                             "why an upstream GitHub's answer names did not route: ownerDir's, ghrepo.Parse's of GitHub's full_name, or DirFor's refusal, each of GitHub's answer or the configuration",
	"ghroute.PrintRefusal: %s redact.URLCredentials(err.Error())":                                    "the refusal through the URL rule",
	"ghroute.ReadGitRemotes: %q name":                                                                "a git remote's name, the text before the tab of git remote -v's line",
	"ghroute.ReadHelp: %s bound (2 places)":                                                          "a duration",
	"ghroute.ReadHelp: %v context.Cause(ctx) (2 places)":                                             "the cause of a stop: a signal or a deadline",
	"ghroute.ReadHelp: %v err":                                                                       "gh's exit status",
	"ghroute.ReentryRefusal: %s real":                                                                "LEGION_GH_PATH, a configured path",
	"ghroute.Remote.what: %q r.name (2 places)":                                                      "a git remote's name, which git config holds and the refusals of remotes name",
	"ghroute.Remote.what: %s markerShown(r.resolved)":                                                "a gh-resolved marker's value, printed only as base or a checked OWNER/REPO (markerShown)",
	"ghroute.Remote.what: %s r.name":                                                                 "a git remote's name, which git config holds and the refusals of remotes name",
	"ghroute.Remote.what: %s redact.URLNotPrinted":                                                   "the words that stand for a URL not printed",
	"ghroute.Remote.what: %s remoteShown(r.url)":                                                     "the URL of a line repository read as a github.com OWNER/REPO, through remoteShown, the URL rule's",
	"ghroute.TargetOf: %s name (2 places)":                                                           "the name of the token variable set, GH_TOKEN or GITHUB_TOKEN",
	"ghroute.cannotPlace: %s advice":                                                                 "this command's own advice, a literal",
	"ghroute.cannotPlace: %s name":                                                                   "its callers' name, each held in this table or a ghroute.Shown call or a literal",
	"ghroute.cannotPlace: %s why":                                                                    "its callers' reason, built from literals and help-listed names",
	"ghroute.checkedOwner: cannotPlace(kind)":                                                        "a slot's kind, built from literals and a help-listed option name",
	"ghroute.cloneGitFlags: %q name":                                                                 "a git clone flag of cloneGitFlagTakes, by the table's spelling",
	"ghroute.cloneGitFlags: %s cloneGitFlagList()":                                                   "the flags cloneGitFlagTakes holds",
	"ghroute.cloneGitFlags: %s leaf (2 places)":                                                      "the leaf's command path, as gh's help spelled it",
	"ghroute.ghArgs.set: %q \"--\"+name":                                                             "an option the leaf's help lists, by the help's name",
	"ghroute.ghArgs.set: %s why":                                                                     "a reason of refusedOptions, a table",
	"ghroute.ghCommand.addFlag: %q strings.TrimSpace(line) (5 places)":                               "a line of gh's own help, which gh printed for no argument of the call",
	"ghroute.ghCommand.addFlag: %s match[1]":                                                         "a short option of gh's own help",
	"ghroute.ghCommand.addFlag: %s match[2]":                                                         "an option of gh's own help",
	"ghroute.ghCommand.addFlag: %s prior":                                                            "an option of gh's own help",
	"ghroute.ghCommand.mayRead: %q word (2 places)":                                                  "a gh alias or extension gh's own help lists",
	"ghroute.ghCommand.parse: cannotPlace why leaf (2 places)":                                       "the leaf's command path, as gh's help spelled it",
	"ghroute.ghCommand.parse: cannotPlace(strconv.Quote(\"--\"+name)) (4 places)":                    "an option the leaf's help lists, by the help's name",
	"ghroute.ghCommand.parse: strconv.Quote(\"--\"+name) (4 places)":                                 "an option the leaf's help lists, by the help's name",
	"ghroute.ghWalk.check: %s path":                                                                  "a command path of ghRunsAnotherProgram, a table",
	"ghroute.ghWalk.named: strconv.Quote(word)":                                                      "a subcommand gh's own help lists, quoted only when it does",
	"ghroute.gitLines: %s procgroup.WaitDelayNow()":                                                  "the WaitDelay procgroup gives git's children, a duration this program sets",
	"ghroute.gitLines: %s strings.Join(args, \" \") (2 places)":                                      "this command's own git arguments",
	"ghroute.gitLines: %s strings.TrimSpace(stderr.String())":                                        "git's own error output, which ghroute.PrintRefusal passes through the URL rule",
	"ghroute.gitLines: %v err":                                                                       "git's exit status",
	"ghroute.ordinalArgument: %s suffix":                                                             "an ordinal's suffix",
	"ghroute.parseGhHelp: %q entry (2 places)":                                                       "a line of gh's own help",
	"ghroute.parseGhHelp: %q placeholder":                                                            "a usage placeholder of gh's own help",
	"ghroute.parseGhHelp: %q strings.TrimSpace(line)":                                                "a line of gh's own help",
	"ghroute.parseGhHelp: %s section (3 places)":                                                     "a section heading of gh's own help",
	"ghroute.parseGhHelp: %s strings.Join(cmd.path, \" \")":                                          "the command path gh's help names",
	"ghroute.parseGhHelp: %s strings.Join(slices.Sorted(maps.Keys(ghPassedOnPlaceholders)), \", \")": "a table's placeholders",
	"ghroute.parseGhHelp: %s what (9 places)":                                                        "the command as ghWalk.named names it, help-listed words or places",
	"ghroute.parseGhHelp: %w err":                                                                    "addFlag's error, which quotes a line of gh's own help",
	"ghroute.readGhCommand: %s what":                                                                 "the command as ghWalk.named names it, help-listed words or places",
	"ghroute.readGhCommand: %w err":                                                                  "ReadHelp's error, this command's own words with gh's exit, gh's output not printed",
	"ghroute.refusedCommand: %s refused":                                                             "a command path of a table of refused commands",
	"ghroute.refusedCommand: %s why":                                                                 "a reason of that table",
	"ghroute.repositoryTarget: cannotPlace(shown) (5 places)":                                        "Shown's words for the value: its slot's kind, never the value",
	"ghroute.selectors.dir: %s filepath.Base(dir)":                                                   "an owner's directory, a login the bundle holds",
	"ghroute.selectors.dir: %s strings.Join(listed, \" and \")":                                      "each owner and the slots naming it (ghNamed.shown), whose owner, name and kind selectors.add's callers check",
	"ghroute.selectors.dir: %s strings.Join(routed[dir], \", \")":                                    "each slot naming that directory (ghNamed.shown), checked as selectors.add says",
	"ghroute.selectors.endpoint: %s hosts (3 places)":                                                "the hosts of ghAPIHosts, a table",
	"ghroute.selectors.fromAPI: cannotPlace(ordinalArgument(2))":                                     "a position",
	"ghroute.selectors.fromCommand: %s leaf":                                                         "the leaf's command path, as gh's help spelled it",
	"ghroute.selectors.fromCommand: %s unknown":                                                      "a usage placeholder of gh's own help",
	"ghroute.selectors.fromCommand: cannotPlace why leaf":                                            "the leaf's command path, as gh's help spelled it",
	"ghroute.selectors.fromCommand: cannotPlace(ordinalArgument(i+1))":                               "a position",
	"ghroute.selectors.fromCommand: cannotPlace(ordinalArgument(len(cmd.slots)+1))":                  "a position",
	"ghroute.selectors.fromOptions: cannotPlace why name":                                            "an option the leaf's help lists, by the help's name",
	"ghroute.selectors.fromOptions: cannotPlace(kind)":                                               "a slot's kind, built from literals and a help-listed option name",
	"ghroute.selectors.fromOptions: cannotPlace(strconv.Quote(\"--\"+name))":                         "an option the leaf's help lists, by the help's name",
	"ghroute.selectors.fromOptions: strconv.Quote(\"--\"+name)":                                      "an option the leaf's help lists, by the help's name",
	"ghroute.selectors.fromWorkingDirectory: %s advice (2 places)":                                   "this command's own advice, a literal",
	"ghroute.selectors.fromWorkingDirectory: %s remote.what()":                                       "a remote as Remote.what names it",
	"ghroute.selectors.fromWorkingDirectory: %s why":                                                 "a reason built from literals",
	"ghroute.selectors.fromWorkingDirectory: %v err":                                                 "a git read's error, which ghroute.PrintRefusal passes through the URL rule",
}

// ghFormatSites is every place in the non-test files ghSources names that quotes or formats a
// value without ghroute.Shown, fn the function that holds it, a method by its receiver's type:
//   - each strconv.Quote call (`fn: strconv.Quote(<arg>)`);
//   - each errors.New call whose text is no literal (`fn: errors.New(<arg>)`);
//   - each fmt call whose format is no literal (`fn: <the call's function> format <format>`), whose
//     arguments no reading can hold to a verb;
//   - each argument a %s, %v, %q, %x, %X or %w verb of an fmt call's literal format formats (`fn:
//     %q <arg>`) but a literal, a ghroute.Shown call or a position (ordinalArgument);
//   - each cannotPlace call whose name is neither a ghroute.Shown call nor a literal (`fn:
//     cannotPlace(<name>)`);
//   - and each piece of a cannotPlace call's why, read as the "+" of its pieces, that is neither a
//     literal, a ghroute.Shown call nor an fmt.Sprintf call, whose format and arguments the fmt
//     case reads on its own (`fn: cannotPlace why <piece>`): an fmt.Sprint piece, whose arguments
//     no format holds to a verb, is reported whole.
//
// A format or a text built only of literals joined with "+" is a literal. A place that stands
// more than once in one function is keyed with its count (`(5 places)`).
func ghFormatSites(t *testing.T) map[string]string {
	t.Helper()
	sites, found := map[string]string{}, map[string][]string{}
	for _, c := range ghSourceCalls(t) {
		call, owner, text := c.call, c.owner, c.text
		add := func(key string) { found[owner+": "+key] = append(found[owner+": "+key], c.at) }
		calls := func(expr ast.Expr, names ...string) bool {
			inner, ok := expr.(*ast.CallExpr)
			return ok && slices.Contains(names, text(inner.Fun))
		}
		switch callee := text(call.Fun); {
		case callee == "strconv.Quote" && len(call.Args) == 1:
			add("strconv.Quote(" + text(call.Args[0]) + ")")
		case callee == "errors.New" && len(call.Args) == 1:
			if _, literal := literalString(call.Args[0]); !literal {
				add("errors.New(" + text(call.Args[0]) + ")")
			}
		case callee == "cannotPlace" && len(call.Args) > 1:
			if _, literal := literalString(call.Args[0]); !literal && !calls(call.Args[0], ghShownCallees...) {
				add("cannotPlace(" + text(call.Args[0]) + ")")
			}
			for _, piece := range sumPieces(call.Args[1]) {
				if _, literal := literalString(piece); literal || calls(piece, append(slices.Clone(ghShownCallees), "fmt.Sprintf")...) {
					continue
				}
				add("cannotPlace why " + text(piece))
			}
		case strings.HasPrefix(callee, "fmt.") && len(call.Args) > 0:
			formatAt := 0
			if callee == "fmt.Fprintf" {
				formatAt = 1
			}
			if !strings.HasSuffix(callee, "f") || formatAt >= len(call.Args) {
				break
			}
			format, literal := literalString(call.Args[formatAt])
			if !literal {
				add(callee + " format " + text(call.Args[formatAt]))
				break
			}
			for i, verb := range formatVerbs(format) {
				arg := formatAt + 1 + i
				if arg >= len(call.Args) || !strings.ContainsRune("sqvxXw", verb) {
					continue
				}
				if _, literal := literalString(call.Args[arg]); literal || calls(call.Args[arg], append(slices.Clone(ghShownCallees), "ordinalArgument")...) {
					continue
				}
				add("%" + string(verb) + " " + text(call.Args[arg]))
			}
		}
	}
	for key, at := range found {
		if len(at) > 1 {
			key += " (" + strconv.Itoa(len(at)) + " places)"
		}
		sites[key] = strings.Join(at, ", ")
	}
	return sites
}

// literalString is expr's value when expr is a string literal or literals joined with "+", and
// whether it is.
func literalString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		return value, err == nil
	case *ast.ParenExpr:
		return literalString(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := literalString(e.X)
		if !ok {
			return "", false
		}
		right, ok := literalString(e.Y)
		return left + right, ok
	}
	return "", false
}

// sumPieces is expr read as the "+" of its pieces, each piece that is no "+" itself.
func sumPieces(expr ast.Expr) []ast.Expr {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return sumPieces(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(sumPieces(e.X), sumPieces(e.Y)...)
		}
	}
	return []ast.Expr{expr}
}

// formatVerbs is the verb of each argument format formats, in order; %% takes none.
func formatVerbs(format string) []rune {
	var verbs []rune
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' {
			continue
		}
		i++
		for i < len(runes) && strings.ContainsRune("+-# 0123456789.[]*", runes[i]) {
			i++
		}
		if i < len(runes) && runes[i] != '%' {
			verbs = append(verbs, runes[i])
		}
	}
	return verbs
}

// No refusal of legion gh formats a person's value itself: every place in the source that quotes
// or formats a value without ghroute.Shown (ghFormatSites) is one ghFormattedValues holds, with the
// reason its value is none a person typed, and every place that table holds is still in the
// source. A place added to the code, such as a cannotPlace(strconv.Quote(word), …) of a search
// word, fails here naming its function and its argument. A source holding no such place at all
// is refused: the walk would have read nothing.
func TestNoRefusalFormatsAPersonsValueItself(t *testing.T) {
	sites := ghFormatSites(t)
	if len(sites) == 0 {
		t.Fatal("UNKNOWN: the source holds no strconv.Quote call, no formatted value and no cannotPlace name, so this read nothing")
	}
	for _, key := range slices.Sorted(maps.Keys(sites)) {
		if _, ok := ghFormattedValues[key]; !ok {
			t.Errorf("%s formats a value itself, at %s, which ghFormattedValues does not hold: name a person's value by its place through ghroute.Shown, or name why this one is none", key, sites[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(ghFormattedValues)) {
		if _, ok := sites[key]; !ok {
			t.Errorf("ghFormattedValues holds %s, which the source no longer has", key)
		}
	}
	t.Logf("%d places format a value without ghroute.Shown, each reviewed", len(sites))
}
