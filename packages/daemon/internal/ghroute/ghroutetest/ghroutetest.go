// Package ghroutetest holds what the tests of internal/ghroute and the tests of `legion gh`
// (cmd/legion) read of gh calls: gh's saved help, answered as the real gh answers it
// (SavedHelp), and that help edited as another gh would print it (EditedHelp), the edited helps
// the resolver tables read rows under among them (EditedHelps), which `legion gh`'s tests hold its
// help keep to answer as each does; the routing of a bundle that holds every owner's directory and
// no rename (OwnersOwnDirectories); the git remotes of a working directory that is no repository
// (NoGitRepository); the resolver tables that read no git remote and the bundle some of them are
// routed over (targets.go), which ghroute's tests hold TargetOf to; the calls that write nothing
// and the calls that do, and the GraphQL calls (calls.go), which ghroute's tests hold its read
// rules (ghroute.Target.WritesNothing, graphQLQueryRead) to and `legion gh`'s tests run end to end,
// but for the gh api cross products and the GraphQL calls, which ghroute's tests alone read; a port
// of the pinned shared check's own read of a call (PinnedCheckReads), which ghroute's tests prove
// the rule against and `legion gh`'s stand-in check answers with; the helper processes ghroute's
// help-read rows and the signal, git-read and fork-read rows of cmd/legion hold their work with
// (process.go); the walk of each package's source that finds the places formatting a value without
// ghroute.Shown, which each holds to a reviewed table of its own, and the refusal paths, the calls
// of ghroute.Shown (sourcewalk.go); and the canary rows of internal/ghroute's refusal paths that
// ghroute.TargetOf reaches (canary.go), which ghroute's tests hold its source to and `legion gh`'s
// tests run end to end.
//
// It is an ordinary package rather than a _test.go file of one of them because cmd/legion is
// package main, which can import no other package's test-only helpers (internal/leakguard's
// leakguardtest says the same of itself). It imports ghroute not at all, so that ghroute's own
// in-package tests can use it: a file of package ghroute importing a package that imported
// ghroute would be an import cycle.
package ghroutetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// SavedHelp answers gh's help of a command path from dir, which holds the real gh's help saved
// one file per path, its words joined by "_", and gh's own help (`gh --help`) as gh.txt
// (cmd/legion's testdata/gh-help), as the real gh does: a path it has no file for prints the help
// of the nearest parent it has (gh's answer to a subcommand it does not know), and one with no
// such parent at all is an error (gh's `unknown command`, exit 1).
func SavedHelp(dir string) func(path []string) (string, error) {
	return func(path []string) (string, error) {
		if len(path) == 0 {
			data, err := os.ReadFile(filepath.Join(dir, "gh.txt"))
			return string(data), err
		}
		for p := path; len(p) > 0; p = p[:len(p)-1] {
			data, err := os.ReadFile(filepath.Join(dir, strings.Join(p, "_")+".txt"))
			if err == nil {
				return string(data), nil
			}
		}
		return "", fmt.Errorf("unknown command %q for \"gh\"", path[0])
	}
}

// OwnersOwnDirectories is the routing of a bundle under /owners that holds a directory for every
// owner and configures no rename (ghroute.DirFor's answer over such a bundle), for a test that
// reads a call as ghroute.TargetOf does: every repository, and every owner named alone, is
// answered from its owner's own directory, so a command naming two owners routes to two
// directories.
func OwnersOwnDirectories(owner, _ string) (string, error) {
	return ghconfig.Dir("/owners", owner), nil
}
