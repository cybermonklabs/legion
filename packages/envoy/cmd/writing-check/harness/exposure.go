package harness

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Exposure is one line of the exposure log (INFRA-3189's, R3 here): one process that read the
// held-out part. The held-out part is scored once at a commit for each cell of the grid: a change
// with a candidate, or a change that calls none (cells). So the held-out run at a commit is every
// line at that commit with no reason, one for each process, each of its own cells; a line with a
// reason is a second run of its cells, and a continuation goes on with the run it names.
type Exposure struct {
	// Commit is the commit the run ran at, and Time when it started (RFC 3339, UTC).
	Commit string `json:"commit"`
	Time   string `json:"time"`
	// Changes are the changes the run scored, and Models the models it called by role: under
	// "candidate", each candidate the run scored the change with, comma-separated, as a lockstep
	// run (score --models) names them.
	Changes []string          `json:"changes"`
	Models  map[string]string `json:"models"`
	// Reason is the --new-run reason of a second run of a cell at one commit, empty otherwise.
	Reason string `json:"reason,omitempty"`
	// Out is the run's output directory, an absolute path.
	Out string `json:"out"`
	// Continues is the Time of the line that began the run this one continues: the run at the
	// same commit, in the same Out, of the same cells, which stopped before it scored every case.
	// Retried counts the results whose line held an error that this process scores again, each
	// model's counted.
	Continues string `json:"continues,omitempty"`
	Retried   int    `json:"retried,omitempty"`
}

// cells are the cells of the grid the line's run scored: its changes with each of its
// candidates, or with none.
func (e Exposure) cells() []string {
	change := strings.Join(e.Changes, "+")
	var out []string
	for _, m := range strings.Split(e.Models[RoleCandidate], ",") {
		out = append(out, change+" "+strings.TrimSpace(m))
	}
	slices.Sort(out)
	return out
}

// DefaultExposureLog is the exposure log's path: $TMPDIR/infra-3231-impl/harness/exposure.log.
func DefaultExposureLog() string {
	return filepath.Join(os.Getenv("TMPDIR"), "infra-3231-impl", "harness", "exposure.log")
}

// ReadExposures reads the exposure log at path; a log that does not exist holds no run.
func ReadExposures(path string) ([]Exposure, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Exposure
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Exposure
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

// ClaimHeldOut appends e to the exposure log at path just before a run scores a case of the
// held-out part, and answers e as the log holds it. It refuses what checkHeldOut refuses, reading
// the log again, so a line another process appended since the run's own check counts too.
func ClaimHeldOut(path string, e Exposure, finished bool) (Exposure, error) {
	e, err := checkHeldOut(path, e, finished)
	if err != nil {
		return e, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return e, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return e, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return e, err
	}
	return e, f.Close()
}

// checkHeldOut decides whether e may read the held-out part, and answers e as ClaimHeldOut would
// log it; it writes nothing, so a run checks first and claims only once nothing it refuses
// itself is left. A run with no reason is refused where an earlier line at e's commit scored one
// of e's cells, unless e continues that run: the latest such line is in e's directory, of e's
// very cells, and the run has a case left to score (finished is false: a case with no line, or
// one whose line holds an error). Then e names the run it continues. A continuation scores no
// case that holds a result; the cases it retries hold no score, and at one commit nothing that
// decides a score has changed. checkHeldOut also refuses e with no commit.
func checkHeldOut(path string, e Exposure, finished bool) (Exposure, error) {
	if strings.TrimSpace(e.Commit) == "" {
		return e, errors.New("a run that reads the held-out part names its commit (--commit)")
	}
	logged, err := ReadExposures(path)
	if err != nil {
		return e, err
	}
	if strings.TrimSpace(e.Reason) == "" {
		cells := e.cells()
		var last *Exposure
		for i, prior := range logged {
			if prior.Commit == e.Commit && slices.ContainsFunc(prior.cells(), func(c string) bool { return slices.Contains(cells, c) }) {
				last = &logged[i]
			}
		}
		switch {
		case last == nil:
		case last.Out != e.Out || !slices.Equal(last.cells(), cells):
			return e, fmt.Errorf("the exposure log %s already holds a run of the held-out part at commit %s that scored change %s with %s (%s, in %s): give --new-run '<reason>' to run it again",
				path, last.Commit, strings.Join(last.Changes, ","), cmp.Or(last.Models[RoleCandidate], "no candidate"), last.Time, last.Out)
		case finished:
			return e, fmt.Errorf("the run of the held-out part at commit %s in %s (begun %s) holds a result for every case: there is nothing to continue; give --new-run '<reason>' to run it again",
				last.Commit, last.Out, cmp.Or(last.Continues, last.Time))
		default:
			e.Continues = cmp.Or(last.Continues, last.Time)
		}
	}
	return e, nil
}

// headCommit is the commit git names HEAD in the working directory: in a jj workspace colocated
// with git, the parent of the working-copy commit, which is the named commit of a held-out run
// made on a clean working copy.
func headCommit() (string, error) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// OutsideRepository refuses an output path inside a repository: a directory at or above it that
// holds a .git or a .jj. Every file the harness and the dataset steps write holds Dispatch or
// session text, which goes under $TMPDIR and never into a working copy.
func OutsideRepository(flag, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for d := abs; ; d = filepath.Dir(d) {
		for _, marker := range []string{".git", ".jj"} {
			if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
				return fmt.Errorf("%s %s is inside the repository at %s: its files hold Dispatch text, so give a path under $TMPDIR", flag, path, d)
			}
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}
