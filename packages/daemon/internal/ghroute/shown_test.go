package ghroute

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
)

// canaryRun is one row of a refusal path as TestNoRefusalPathPrintsACanary runs it: run makes the
// refusal, with ghroutetest.Canary in every slot of its call, and names is what the refusal must
// say, the place the path names the value by, which shows the row reached that path.
type canaryRun struct {
	run   func() error
	names string
}

// canaryRuns are the rows of this package's refusal paths that TargetOf does not reach, which need
// code of this package to run, keyed as ghroutetest.ShownPaths keys a call of Shown: the function
// that makes it and the source of the place it names the value by. Every other path's rows are
// ghroutetest.CanaryRows, which `legion gh`'s test of this name also runs as `legion gh` itself.
var canaryRuns = map[string][]canaryRun{
	`ForkDir: "the repository the request names"`: {
		{run: func() error {
			repo, err := ghrepo.Parse("the canary's repository", ghroutetest.Canary+"/app")
			if err != nil {
				return err
			}
			_, _, err = ForkDir(os.TempDir(), repo, "", "")
			return err
		}, names: "the repository the request names is a fork"},
	},
}

// Every refusal path of this package that puts a value a person gave into a refusal — every call
// of Shown its non-test files hold (ghroutetest.ShownPaths) — has rows, in ghroutetest.CanaryRows
// or canaryRuns, and every row's path is one the source holds: a new path fails this until it
// joins a table, and so does a function holding one that is renamed, under its new name. Each row
// puts ghroutetest.Canary in every slot of its call and reaches its path, which names the value by
// its place (the row's names), and the refusal printed as `legion gh` prints it (PrintRefusal)
// holds no part of the canary: TargetOf's of a CanaryRows row, read under savedGhHelp with no git
// remotes, or a canaryRuns row's own. A source holding no path at all is refused: the tables would
// test nothing. cmd/legion's test of this name runs every CanaryRows row as `legion gh` itself.
func TestNoRefusalPathPrintsACanary(t *testing.T) {
	t.Parallel()
	paths := ghroutetest.ShownPaths(t, "*.go")
	rows := map[string][]canaryRun{}
	for key, calls := range ghroutetest.CanaryRows {
		for _, call := range calls {
			rows[key] = append(rows[key], canaryRun{run: func() error {
				_, err := TargetOf(call.Argv, call.Env, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
				return err
			}, names: call.Names})
		}
	}
	for key, runs := range canaryRuns {
		rows[key] = append(rows[key], runs...)
	}
	for _, key := range slices.Sorted(maps.Keys(paths)) {
		if len(rows[key]) == 0 {
			t.Errorf("the refusal path %s (%s) has no row in ghroutetest.CanaryRows or canaryRuns: a new path joins one of them", key, paths[key])
		}
	}
	count := 0
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		if _, ok := paths[key]; !ok {
			t.Errorf("ghroutetest.CanaryRows or canaryRuns names the refusal path %s, which no call of Shown in the source has", key)
		}
		for i, row := range rows[key] {
			count++
			t.Run(key+"/"+strconv.Itoa(i), func(t *testing.T) {
				err := row.run()
				if err == nil {
					t.Fatalf("the row naming %q = nil error, want a refusal by the path %s", row.names, key)
				}
				printed := ghPrinted(err)
				if strings.Contains(printed, ghroutetest.Canary) {
					t.Errorf("the path %s: legion gh printed %q, which holds the canary", key, printed)
				}
				if !strings.Contains(printed, row.names) {
					t.Errorf("the path %s: legion gh printed %q, want it to name %q", key, printed, row.names)
				}
			})
		}
	}
	t.Logf("%d refusal paths, %d rows", len(paths), count)
}

// formattedValues are the places in this package's non-test files that quote or format a value
// themselves, without Shown, each keyed as ghroutetest.FormatSites keys it and given the reason
// it is no value a person typed in a slot a refusal names: a name gh's own help or this package's
// tables gave, a position, a kind built from literals, a configured path, a value of GitHub's
// answer. A new such place fails TestNoRefusalFormatsAPersonsValueItself until it joins this
// table, which is where a reviewer reads it; a function renamed fails it too, under its new name.
// cmd/legion's gh*.go files are held to its own table (its ghFormattedValues).
var formattedValues = map[string]string{
	"DirFor: %w err (2 places)":                                           "a read error of the bundle's configured renames file, a configured path, or ownerDir's leakguard.OwnerNotServedError of the configured pair's `to` owner and the bundle's path",
	"DirFor: %s pair.From":                                                "a configured rename's `from` repository, from the bundle's pairs file the daemon writes, which holds the repository the call named as configured",
	"DirFor: %s pair.To":                                                  "a configured rename's `to` repository, from the bundle's pairs file the daemon writes",
	"DirFor: %s dir":                                                      "the configured pair's `to` owner, lowercased (ghconfig.RouteDir)",
	"ForkDir: %s dir":                                                     "a directory of the bundle, its configured path and an owner GitHub's answer names",
	"ForkDir: %s named (3 places)":                                        "Shown's words for the repository the request names, never the repository",
	"ForkDir: %s strings.Join(tried, \"; \")":                             "why each upstream GitHub's answer names did not route, built from literals, GitHub's answer and the errors this table holds",
	"ForkDir: %s upstream.fullName (5 places)":                            "a repository's full_name in GitHub's answer for the fork, no value of the call",
	"ForkDir: %s upstream.role (3 places)":                                "parent or source, a literal of this function",
	"ForkDir: %v err (3 places)":                                          "why an upstream GitHub's answer names did not route: ownerDir's, ghrepo.Parse's of GitHub's full_name, or DirFor's refusal, each of GitHub's answer or the configuration",
	"PrintRefusal: %s redact.URLCredentials(err.Error())":                 "the refusal through the URL rule",
	"ReadGitRemotes: %q name":                                             "a git remote's name, the text before the tab of git remote -v's line",
	"ReadHelp: %s bound (2 places)":                                       "a duration",
	"ReadHelp: %v context.Cause(ctx) (2 places)":                          "the cause of a stop: a signal or a deadline",
	"ReadHelp: %v err":                                                    "gh's exit status",
	"ReentryRefusal: %s real":                                             "LEGION_GH_PATH, a configured path",
	"Remote.what: %q r.name (2 places)":                                   "a git remote's name, which git config holds and the refusals of remotes name",
	"Remote.what: %s markerShown(r.resolved)":                             "a gh-resolved marker's value, printed only as base or a checked OWNER/REPO (markerShown)",
	"Remote.what: %s r.name":                                              "a git remote's name, which git config holds and the refusals of remotes name",
	"Remote.what: %s redact.URLNotPrinted":                                "the words that stand for a URL not printed",
	"Remote.what: %s remoteShown(r.url)":                                  "the URL of a line repository read as a github.com OWNER/REPO, through remoteShown, the URL rule's",
	"TargetOf: %s name (2 places)":                                        "the name of the token variable set, GH_TOKEN or GITHUB_TOKEN",
	"cannotPlace: %s advice":                                              "this command's own advice, a literal",
	"cannotPlace: %s name":                                                "its callers' name, each held in this table or a Shown call or a literal",
	"cannotPlace: %s why":                                                 "its callers' reason, built from literals and help-listed names",
	"checkedOwner: cannotPlace(kind)":                                     "a slot's kind, built from literals and a help-listed option name",
	"cloneGitFlags: %q name":                                              "a git clone flag of cloneGitFlagTakes, by the table's spelling",
	"cloneGitFlags: %s cloneGitFlagList()":                                "the flags cloneGitFlagTakes holds",
	"cloneGitFlags: %s leaf (2 places)":                                   "the leaf's command path, as gh's help spelled it",
	"ghArgs.set: %q \"--\"+name":                                          "an option the leaf's help lists, by the help's name",
	"ghArgs.set: %s why":                                                  "a reason of refusedOptions, a table",
	"ghCommand.addFlag: %q strings.TrimSpace(line) (5 places)":            "a line of gh's own help, which gh printed for no argument of the call",
	"ghCommand.addFlag: %s match[1]":                                      "a short option of gh's own help",
	"ghCommand.addFlag: %s match[2]":                                      "an option of gh's own help",
	"ghCommand.addFlag: %s prior":                                         "an option of gh's own help",
	"ghCommand.mayRead: %q word (2 places)":                               "a gh alias or extension gh's own help lists",
	"ghCommand.parse: cannotPlace why leaf (2 places)":                    "the leaf's command path, as gh's help spelled it",
	"ghCommand.parse: cannotPlace(strconv.Quote(\"--\"+name)) (4 places)": "an option the leaf's help lists, by the help's name",
	"ghCommand.parse: strconv.Quote(\"--\"+name) (4 places)":              "an option the leaf's help lists, by the help's name",
	"ghWalk.check: %s path":                                               "a command path of ghRunsAnotherProgram, a table",
	"ghWalk.named: strconv.Quote(word)":                                   "a subcommand gh's own help lists, quoted only when it does",
	"gitLines: %s procgroup.WaitDelayNow()":                               "the WaitDelay procgroup gives git's children, a duration this program sets",
	"gitLines: %s strings.Join(args, \" \") (2 places)":                   "this command's own git arguments",
	"gitLines: %s strings.TrimSpace(stderr.String())":                     "git's own error output, which PrintRefusal passes through the URL rule",
	"gitLines: %v err":                                                    "git's exit status",
	"ordinalArgument: %s suffix":                                          "an ordinal's suffix",
	"parseGhHelp: %q entry (2 places)":                                    "a line of gh's own help",
	"parseGhHelp: %q placeholder":                                         "a usage placeholder of gh's own help",
	"parseGhHelp: %q strings.TrimSpace(line)":                             "a line of gh's own help",
	"parseGhHelp: %s section (3 places)":                                  "a section heading of gh's own help",
	"parseGhHelp: %s strings.Join(cmd.path, \" \")":                       "the command path gh's help names",
	"parseGhHelp: %s strings.Join(slices.Sorted(maps.Keys(ghPassedOnPlaceholders)), \", \")": "a table's placeholders",
	"parseGhHelp: %s what (9 places)":                                       "the command as ghWalk.named names it, help-listed words or places",
	"parseGhHelp: %w err":                                                   "addFlag's error, which quotes a line of gh's own help",
	"readGhCommand: %s what":                                                "the command as ghWalk.named names it, help-listed words or places",
	"readGhCommand: %w err":                                                 "ReadHelp's error, this command's own words with gh's exit, gh's output not printed",
	"refusedCommand: %s refused":                                            "a command path of a table of refused commands",
	"refusedCommand: %s why":                                                "a reason of that table",
	"repositoryTarget: cannotPlace(shown) (5 places)":                       "Shown's words for the value: its slot's kind, never the value",
	"selectors.dir: %s filepath.Base(dir)":                                  "an owner's directory, a login the bundle holds",
	"selectors.dir: %s strings.Join(listed, \" and \")":                     "each owner and the slots naming it (ghNamed.shown), whose owner, name and kind selectors.add's callers check",
	"selectors.dir: %s strings.Join(routed[dir], \", \")":                   "each slot naming that directory (ghNamed.shown), checked as selectors.add says",
	"selectors.endpoint: %s hosts":                                          "the hosts of ghAPIHosts, a table",
	"selectors.endpoint: %s refused":                                        "Shown's words for the endpoint, its host or itself, and why it is refused, built from literals and the hosts of ghAPIHosts, a table",
	"selectors.fromAPI: cannotPlace(ordinalArgument(2))":                    "a position",
	"selectors.fromCommand: %s leaf":                                        "the leaf's command path, as gh's help spelled it",
	"selectors.fromCommand: %s unknown":                                     "a usage placeholder of gh's own help",
	"selectors.fromCommand: cannotPlace why leaf":                           "the leaf's command path, as gh's help spelled it",
	"selectors.fromCommand: cannotPlace(ordinalArgument(i+1))":              "a position",
	"selectors.fromCommand: cannotPlace(ordinalArgument(len(cmd.slots)+1))": "a position",
	"selectors.fromOptions: cannotPlace why name":                           "an option the leaf's help lists, by the help's name",
	"selectors.fromOptions: cannotPlace(kind)":                              "a slot's kind, built from literals and a help-listed option name",
	"selectors.fromOptions: cannotPlace(strconv.Quote(\"--\"+name))":        "an option the leaf's help lists, by the help's name",
	"selectors.fromOptions: strconv.Quote(\"--\"+name)":                     "an option the leaf's help lists, by the help's name",
	"selectors.fromWorkingDirectory: %s advice (2 places)":                  "this command's own advice, a literal",
	"selectors.fromWorkingDirectory: %s remote.what()":                      "a remote as Remote.what names it",
	"selectors.fromWorkingDirectory: %s why":                                "a reason built from literals",
	"selectors.fromWorkingDirectory: %v err":                                "a git read's error, which PrintRefusal passes through the URL rule",
}

// No refusal of this package formats a person's value itself: every place in its non-test files
// that quotes or formats a value without Shown (ghroutetest.FormatSites) is one formattedValues
// holds, with the reason its value is none a person typed, and every place that table holds is
// still in the source (ghroutetest.RequireReviewed). A place added to the code, such as a
// cannotPlace(strconv.Quote(word), …) of a search word, fails here naming its function and its
// argument, and so does a function renamed. A source holding no such place at all is refused: the
// walk would have read nothing.
func TestNoRefusalFormatsAPersonsValueItself(t *testing.T) {
	t.Parallel()
	ghroutetest.RequireReviewed(t, ghroutetest.FormatSites(ghroutetest.SourceCalls(t, "*.go")), formattedValues, "formattedValues")
}
