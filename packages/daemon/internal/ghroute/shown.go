package ghroute

import (
	"fmt"
	"io"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// Shown is place, the words that name value, a value a person gave `legion gh` or the answer
// loop's gh tool (an argument, an option's value, a selector, an environment value, a search
// query), by its kind and where it stands ("a -R value", "a git flag after --", "an option the
// command's help does not list"). A refusal of either prints no such value in any form: any of it
// can be a credential a person put there. value is passed so that each refusal path says which
// value its words stand for.
//
// Every call of Shown is a refusal path, read from the source by ghroutetest.ShownPaths and keyed
// by the function that makes it and the source of its place argument (a literal, or a variable
// such as repositoryTarget's kind). This package's TestNoRefusalPathPrintsACanary (shown_test.go)
// holds every call in its source to rows that drive it with a canary in every slot
// (ghroutetest.CanaryRows, and its own canaryRuns for a path TargetOf does not reach) and prints
// each refusal through PrintRefusal, so a call added with no rows, or a function here holding one
// that is renamed, fails `./internal/ghroute`. cmd/legion's test of that name holds its gh*.go
// files' calls to rows of its own and runs every row of ghroutetest.CanaryRows as `legion gh`
// itself. This package's TestNoRefusalFormatsAPersonsValueItself holds every other place of its
// source that formats a value to a reviewed table, each with the reason its value is none a person
// typed unchecked, as cmd/legion's test of that name holds its gh*.go files' places; all of them
// read the source through ghroutetest's walk (SourceCalls).
func Shown(value, place string) string {
	return place
}

// PrintRefusal writes err to stderr as `legion gh: <err>`, through redact.URLCredentials as
// cmd/legion's printRefusal writes every other command's refusal, a URL a person put in a selector
// (GH_REPO, -R), a git config key or a remote's marker included: `legion gh`'s every refusal, and
// the reentry refusal ReadHelp reads back from gh's error output (ReentryRefusal).
func PrintRefusal(stderr io.Writer, err error) {
	fmt.Fprintf(stderr, "legion gh: %s\n", redact.URLCredentials(err.Error()))
}
