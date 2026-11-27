package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// codegraphMemoryCap is the most memory a warm-up build's process group may hold, read as the
// launcher's memory guard reads a process (memoryguard.GroupReader), before runCodegraph ends the
// group with SIGKILL. It was measured on 2026-11-27 in a Job of an issue pod's shape (2 CPUs,
// 8Gi, gVisor, the worker image with CodeGraph 1.6.4), by `codegraph init` of a fresh checkout
// read every 250 ms: cybermonklabs/legion peaked at 2,792 MiB at 1 parse worker (2,760 MiB on a
// repeat), and a second repository of 5,412 indexed files at 2,264 MiB (2,171). The cap is the
// larger peak at the count the warm-up sets, plus a quarter for the variance between runs and the
// repositories' growth, rounded up to a multiple of 256 MiB, 3,584 MiB, unless that is above the
// guard's 85% mark at 8Gi (6,963 MiB) less the median of the guard's hourly reading_high in
// INFRA's issue pods over the last day; then it is the largest multiple of 256 MiB under that
// bound. Over the 24 hours to 2026-11-27T08:17Z the median was 3,616 MiB, so the bound is
// 3,347 MiB and the cap 3,328 MiB, 19% above the larger peak. So a build at the cap on a pod whose
// roles hold their median leaves the pod under the mark (INFRA-5717 R1). It is a variable so that
// a test can lower it.
var codegraphMemoryCap uint64 = 3328 << 20

// codegraphMemoryPoll is how often runCodegraph reads its build's group against the cap: a build
// past it is ended within one poll and one read.
const codegraphMemoryPoll = 250 * time.Millisecond

// codegraphParseWorkers is the size of CodeGraph's parse pool in a warm-up build
// (CODEGRAPH_PARSE_WORKERS, codegraphEnvironment), the count the cap was measured at. CodeGraph's
// own default is clamp(max(3, CPUs) − 1, 1, 8), 2 in a 2-CPU pod; on CodeGraph 1.6.4 each build's
// peak at 1 worker was 39 to 56 MiB lower than at 2, legion's builds took the same time (374 s
// mean against 379 s), and the second repository's 4 s longer (108 s against 104 s).
const codegraphParseWorkers = "1"

// warmBuildName is the record of the last warm-up build under `.codegraph/`, beside the lease
// (warmLeaseName): on the issue's disk, so it outlives a container restart and a role's relaunch,
// and goes with the workspace. CodeGraph's own `.gitignore` there keeps it out of the repository.
const warmBuildName = "legion-warm.build"

// The outcomes a build record holds. A build that exited 0 leaves no record, and neither does one
// whose warm-up's own context ended it, nor one that never started. Every other outcome is a
// build that would end the same way again, and "exit N" names a non-zero status.
const (
	warmOutcomeRunning   = "running"
	warmOutcomeKilled    = "killed"
	warmOutcomeMemoryCap = "memory cap"
	warmOutcomeTimedOut  = "timed out"
)

// warmBuildRecordLimit bounds how much of the record a warm-up reads: one line of a few hundred
// bytes. A file an agent made larger is no record of a build.
const warmBuildRecordLimit = 4096

// warmBuild is the record of a warm-up build (INFRA-5717 R2): which subcommand it ran, when it
// started, how it ended, and the CodeGraph and the cap it ran under. A warm-up writes it
// `running` before the build starts and rewrites it with the build's end, so a record still
// `running` while a warm-up holds the lease is a build that died unseen: a pod that ended, the
// kernel's out-of-memory kill, a shim killed at its stop grace.
type warmBuild struct {
	Subcommand string    `json:"subcommand"`
	Started    time.Time `json:"started"`
	Outcome    string    `json:"outcome"`
	CodeGraph  string    `json:"codegraph"`
	Cap        uint64    `json:"cap_bytes"`
	Peak       uint64    `json:"peak_bytes,omitempty"`
}

// holds is whether the record holds a warm-up from building again: the image's CodeGraph
// (version) and the cap are the ones the recorded build ran under, and the workspace is the one it
// ran in, which its creation record (created, createdOK: Created) tells where provisioning kept
// one. A workspace created after the recorded build started is a new one. Another CodeGraph, an
// unread one included, or another cap allows one new attempt, whose own end is recorded in turn.
func (rec warmBuild) holds(version string, capBytes uint64, created time.Time, createdOK bool) bool {
	if createdOK && created.After(rec.Started) {
		return false
	}
	return rec.CodeGraph == version && rec.Cap == capBytes
}

// describe is the record as one line names it: the build, its start and how it ended.
func (rec warmBuild) describe() string {
	return fmt.Sprintf("codegraph %s started %s, ended %q", rec.Subcommand, rec.Started.UTC().Format(time.RFC3339), rec.Outcome)
}

// exitOutcome is the outcome of a build that exited with code, a non-zero status.
func exitOutcome(code int) string { return "exit " + strconv.Itoa(code) }

// readWarmBuild reads the record at path. found is false with a nil error where there is none.
// A record that cannot be opened, read or parsed answers the error: the caller reads it as none,
// and says so, since a record that says nothing it can read holds no build. It is opened without
// following a link and read only up to warmBuildRecordLimit, since every agent of the issue can
// write the directory it lives in.
func readWarmBuild(path string) (rec warmBuild, found bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return warmBuild{}, false, nil
	}
	if err != nil {
		return warmBuild{}, false, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return warmBuild{}, false, err
	} else if !info.Mode().IsRegular() {
		return warmBuild{}, false, fmt.Errorf("%s is no regular file", path)
	}
	body, err := io.ReadAll(io.LimitReader(f, warmBuildRecordLimit+1))
	if err != nil {
		return warmBuild{}, false, err
	}
	if len(body) > warmBuildRecordLimit {
		return warmBuild{}, false, fmt.Errorf("%s holds more than %d bytes", path, warmBuildRecordLimit)
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return warmBuild{}, false, fmt.Errorf("%s does not parse: %w", path, err)
	}
	if rec.Subcommand == "" || rec.Outcome == "" || rec.Started.IsZero() {
		return warmBuild{}, false, fmt.Errorf("%s names no build: %q", path, body)
	}
	return rec, true, nil
}

// writeWarmBuild writes rec as the record at path, one JSON line, and syncs it before it returns:
// the record `running` must be on the disk before the build starts, since the build's death can
// take the pod with it. It replaces a file at path, never follows a link there.
func writeWarmBuild(path string, rec warmBuild) error {
	rec.Started = rec.Started.UTC()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removeWarmBuild removes the record at path, which a build that exited 0, an orderly stop, a
// build that never started and a complete index each leave none of.
func removeWarmBuild(dir, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s could not remove its build record: %s\n", dir, err)
	}
}

// codegraphVersion is the version of the CodeGraph at codegraphPath, read from the package.json of
// the bundle it is in (`/opt/codegraph/package.json` for the image's `/opt/codegraph/bin/codegraph`),
// never by running codegraph: the warm-up runs `status` and its build alone. "" where it cannot
// be read, which a record compares as one more version.
func codegraphVersion(codegraphPath string) string {
	resolved, err := filepath.EvalSymlinks(codegraphPath)
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(resolved)), "package.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &manifest) != nil {
		return ""
	}
	return manifest.Version
}
