package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/daemon"
)

// realGHCandidates are the gh programs a test that drives the real gh may run, in the order it
// tries them: LEGION_GH_PATH alone when it is set (named), as in a Legion pane or pod, whose `gh`
// on PATH is the wrapper; otherwise every gh on PATH, first to last, but a script. A script is a
// wrapper of the machine's own (a gh shim that hands each call to the next gh on PATH), as boot
// refuses one for the real gh (daemon.RefuseScriptGH). The first gh on PATH need not be the real
// one: a machine's own gh wrapper can head PATH with the real gh further down it, as on a devbox
// whose dotfiles shim comes first, then mise's shim, then mise's install of gh.
func realGHCandidates() (candidates []string, named bool) {
	if path := os.Getenv("LEGION_GH_PATH"); path != "" {
		return []string{path}, true
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// A relative PATH entry is skipped, as exec.LookPath("gh") refuses one (exec.ErrDot).
		if !filepath.IsAbs(dir) {
			continue
		}
		path, err := exec.LookPath(filepath.Join(dir, "gh"))
		if err != nil || slices.Contains(candidates, path) || daemon.RefuseScriptGH(path) != nil {
			continue
		}
		candidates = append(candidates, path)
	}
	return candidates, false
}

// sentWithoutCredentials says what a control run sent by host and path, and of each credential
// only whether there was one: a gh the control refuses is a machine's own wrapper, which may carry
// a real credential, and a test log is no place for it.
func sentWithoutCredentials(requests []capturedRequest) string {
	if len(requests) == 0 {
		return "nothing"
	}
	sent := make([]string, 0, len(requests))
	for _, r := range requests {
		credential := "no credential"
		if r.auth != "" {
			credential = "another credential"
		}
		sent = append(sent, r.host+r.path+" with "+credential)
	}
	return strings.Join(sent, ", ")
}
