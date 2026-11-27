package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghCanaryRows are the rows of every refusal path of this package's gh*.go (ghOwnSource), each
// one call with ghroutetest.Canary in its slots, keyed as ghroutetest.ShownPaths keys a call of
// ghroute.Shown: the function that makes it and the source of the place it names the value by.
// internal/ghroute's paths have rows of their own, which its test of this name holds to its
// source: ghroutetest.CanaryRows, and its canaryRuns for a path ghroute.TargetOf does not reach.
var ghCanaryRows = map[string][]ghroutetest.CanaryRun{
	`withGitHubUseHTTPPath: "a value that is not printed"`: {
		{Run: func() error {
			_, err := withGitHubUseHTTPPath([]string{"GIT_CONFIG_COUNT=" + ghroutetest.Canary})
			return err
		}, Names: "GIT_CONFIG_COUNT holds a value that is not printed, which is no count"},
	},
}

// ghOwnSource is this package's gh*.go, the non-test files of legion gh itself, which both of its
// source walks read alone, its refusal paths (TestNoRefusalPathPrintsACanary) and its formatting
// places (TestNoRefusalFormatsAPersonsValueItself): internal/ghroute holds its own source to
// tables of its own, by its own tests of those names.
const ghOwnSource = "gh*.go"

// Every refusal path of legion gh's own gh*.go that puts a value a person gave it into a refusal
// — every call of ghroute.Shown that source holds (ghroutetest.ShownPaths) — has rows in
// ghCanaryRows, and every row's path is one the source holds: a new path fails this until it
// joins the table. Each row puts ghroutetest.Canary in every slot of its call and reaches its
// path, which names the value by its place (the row's Names), and its refusal printed as legion gh
// prints it (ghroute.PrintRefusal) holds no part of the canary. A source holding no path at all is
// refused: the table would test nothing. ghroutetest.RequireCanaryRows makes this check, as it
// does for internal/ghroute's paths by its own test of this name; here every row of
// ghroutetest.CanaryRows, a ghroute.TargetOf call, is also run as `legion gh` itself, as a pane
// runs it, with the saved help's stand-in gh, whose own output the help read never prints, and
// must exit 2 with the canary on neither stdout nor stderr and stderr naming the row's words.
func TestNoRefusalPathPrintsACanary(t *testing.T) {
	paths, own := ghroutetest.RequireCanaryRows(t, ghOwnSource, ghCanaryRows, ghPrinted, "ghCanaryRows")
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
	ran := 0
	for _, key := range slices.Sorted(maps.Keys(ghroutetest.CanaryRows)) {
		for i, row := range ghroutetest.CanaryRows[key] {
			ran++
			t.Run("legion gh/"+key+"/"+strconv.Itoa(i), func(t *testing.T) {
				env := maps.Clone(pane)
				maps.Copy(env, row.Env)
				code, stdout, stderr := legionGh(t, env, row.Argv...)
				if code != 2 || strings.Contains(stdout, ghroutetest.Canary) || strings.Contains(stderr, ghroutetest.Canary) || !strings.Contains(stderr, row.Names) {
					t.Errorf("the path %s: legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 naming %q, the canary in neither", key, row.Argv, code, stdout, stderr, row.Names)
				}
			})
		}
	}
	t.Logf("%d refusal paths of legion gh's own, %d rows; %d rows of internal/ghroute's run as legion gh", paths, own, ran)
}

// ghFormattedValues are the places in this package's gh*.go files (ghOwnSource) that quote or
// format a value themselves, without ghroute.Shown, each keyed as ghroutetest.FormatSites keys it
// and given the reason it is no value a person typed in a slot a refusal names: a name this
// command's tables gave, a configured path, a value git or the clock gave. A new such place fails
// TestNoRefusalFormatsAPersonsValueItself until it joins this table, which is where a reviewer
// reads it. internal/ghroute's places are held to its own table, by its own test of that name.
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
}

// No refusal of legion gh formats a person's value itself: every place in this package's gh*.go
// that quotes or formats a value without ghroute.Shown (ghroutetest.FormatSites) is one
// ghFormattedValues holds, with the reason its value is none a person typed, and every place that
// table holds is still in the source (ghroutetest.RequireReviewed). A place added to the code, such
// as an fmt.Errorf("%q", arg) of an argument, fails here naming its function and its argument. A
// source holding no such place at all is refused: the walk would have read nothing.
func TestNoRefusalFormatsAPersonsValueItself(t *testing.T) {
	ghroutetest.RequireReviewed(t, ghroutetest.FormatSites(ghroutetest.SourceCalls(t, ghOwnSource)), ghFormattedValues, "ghFormattedValues")
}
