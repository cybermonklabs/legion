package specgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// TestReads gives the paths and globs that `go test <pkgs>` run in workdir reads, in the three
// forms of a Tie, with env ("NAME=VALUE" entries) set for the go commands.
type TestReads func(workdir string, pkgs []string, env []string) ([]string, error)

// ScriptTestReads is the TestReads of the repository at repoRoot: its
// .github/scripts/go-test-reads.sh, which computes the closure that check-ci-filters.sh holds
// each CI filter to, with the reads and go commands that ci_job_reads.py records for each test.
// So a tie is what the repository's own filter check says a test reads, and no second reader of
// it exists. The script prints one JSON object, {"reads": [...], "problems": [...]}. Its exit 1,
// a problem that leaves the closure unknown, is an error holding the problems; any other exit, an
// answer that is no such object and an exit 0 with a problem are errors too. environ is the
// script's whole environment, as exec.Cmd.Env takes it, and nil is the process's own: the script
// lists the repository's files with git ls-files, so a caller that runs it in a tree that is no
// git work tree, as spec-check's checkout of a base commit, gives it git's view of that tree there.
func ScriptTestReads(repoRoot string, environ []string) TestReads {
	script := filepath.Join(repoRoot, ".github", "scripts", "go-test-reads.sh")
	return func(workdir string, pkgs []string, env []string) ([]string, error) {
		args := make([]string, 0, 2*len(env)+1+len(pkgs))
		for _, entry := range env {
			args = append(args, "--env", entry)
		}
		args = append(append(args, workdir), pkgs...)
		cmd := exec.Command(script, args...)
		cmd.Dir = repoRoot
		cmd.Env = environ
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		code := 0
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			return nil, fmt.Errorf("specgraph: %s: %w", script, err)
		}
		what := fmt.Sprintf("go test %s in %s", strings.Join(pkgs, " "), workdir)
		var answer struct {
			Reads    *[]string `json:"reads"`
			Problems []string  `json:"problems"`
		}
		decodeErr := json.Unmarshal(out, &answer)
		switch {
		case code == 1 && decodeErr == nil && len(answer.Problems) > 0:
			return nil, fmt.Errorf("specgraph: what %s reads is unknown: %s", what, strings.Join(answer.Problems, "; "))
		case code != 0:
			return nil, fmt.Errorf("specgraph: %s exited %d for %s: %s", script, code, what, strings.TrimSpace(stderr.String()))
		case decodeErr != nil:
			return nil, fmt.Errorf("specgraph: %s printed no reads object for %s: %w", script, what, decodeErr)
		case len(answer.Problems) > 0:
			return nil, fmt.Errorf("specgraph: %s exited 0 with problems for %s: %s", script, what, strings.Join(answer.Problems, "; "))
		case answer.Reads == nil:
			return nil, fmt.Errorf("specgraph: %s printed no reads for %s", script, what)
		}
		return *answer.Reads, nil
	}
}

// GoTies gives the tie of each program check (R18: "The code that each check runs, from the test
// that the check names"): for runner go and a selector that ParseSelector takes, what `go test`
// of the selector's package, run in workdir with env, reads by reads, as NewTie takes it. A check
// whose tie cannot be derived gets Known false and a Why that names the cause: no test part, a
// runner other than go, a selector ParseSelector refuses, an error of reads, or an answer that
// NewTie refuses (R18: "A tie that Dispatch cannot derive reads as unknown, not as none"). Checks
// of one package ask reads once. In production workdir is "." at the repository root, where the
// root go.work takes the envoy and daemon import paths, and env is empty. A symbolic link in a tie
// whose target is outside it is the same case as a test that reads a file outside its package: the
// tie holds what the rows of ci_job_reads.py record, and a row for the target names it.
func GoTies(checks []Check, workdir string, env []string, reads TestReads) map[string]Tie {
	type answer struct {
		tie Tie
		err error
	}
	asked := map[string]answer{}
	ties := map[string]Tie{}
	for _, c := range checks {
		if c.Procedure != ProcedureProgram {
			continue
		}
		if c.Test == nil {
			ties[c.ID] = Tie{Why: "the check names no test"}
			continue
		}
		if c.Test.Runner != "go" {
			ties[c.ID] = Tie{Why: fmt.Sprintf("runner %q has no tie reader", c.Test.Runner)}
			continue
		}
		path, _, err := ParseSelector(c.Test.Selector)
		if err != nil {
			ties[c.ID] = Tie{Why: err.Error()}
			continue
		}
		a, ok := asked[path]
		if !ok {
			var paths []string
			if paths, a.err = reads(workdir, []string{path}, env); a.err == nil {
				a.tie, a.err = NewTie(paths)
			}
			asked[path] = a
		}
		if a.err != nil {
			ties[c.ID] = Tie{Why: a.err.Error()}
			continue
		}
		ties[c.ID] = a.tie
	}
	return ties
}
