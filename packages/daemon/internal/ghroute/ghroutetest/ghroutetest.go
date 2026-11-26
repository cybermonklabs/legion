// Package ghroutetest holds what the tests of internal/ghroute and the tests of `legion gh`
// (cmd/legion) both read of gh calls: gh's saved help, answered as the real gh answers it
// (SavedHelp), and that help edited as another gh would print it (EditedHelp); the routing of a
// bundle that holds every owner's directory and no rename (OwnersOwnDirectories); the git remotes
// of a working directory that is no repository (NoGitRepository); the resolver tables that read no
// git remote (targets.go), which ghroute's tests hold TargetOf to and `legion gh`'s tests resolve
// through its keep of gh's help; the calls that write nothing and the calls that do (calls.go),
// which ghroute's tests hold its read rule (ghroute.Target.WritesNothing) to and `legion gh`'s
// tests run end to end; a port of the pinned shared check's own read of a call
// (PinnedCheckReads), which ghroute's tests prove the rule against and `legion gh`'s stand-in
// check answers with; and the helper processes both packages' help-read rows hold their work with
// (process.go).
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
