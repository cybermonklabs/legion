package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// citationsJob is the job of the workflow that runs citations: the jobs it needs are the jobs whose
// reports it can read, since only they have ended when it starts.
const citationsJob = "spec-citations"

// reportPrefix starts the name of each report artifact, spec-report--<job> or
// spec-report--<job>--<leg>, a directory that holds the job's report.jsonl.
const reportPrefix = "spec-report--"

// citationFault is one entry of the --json file: the machine form of the value at the commit of a
// check's state input Input.Uncited, which is by commit, then by check; its reader files the
// entries under the commit that citations judged.
type citationFault struct {
	Check  string `json:"check"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// runCitations is "spec-check citations --dir <dir> --repo <root> --workflow <file> (--changed
// <file> | --all-changed) --reports <dir> --needs <file> [--ties <file>...] [--json <file>]
// [--allow-empty]": it names each program check of the spec files that CI refuses at the commit
// (specgraph.Citations, R48), and answers 1 when it names one, 0 when none, and 2 for an input it
// cannot read, a root that is not the top of its git clone (isCloneTop) or arguments it does not
// take. Each answer 2 but a --json file it cannot write comes before it writes that file, so a spec
// file that does not parse, an input it cannot read and a --needs that does not name exactly the
// jobs spec-citations needs write no --json file.
func runCitations(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("citations", flag.ContinueOnError)
	dir := flags.String("dir", "", "the directory of spec files")
	repo := flags.String("repo", "", "the top of the git clone of the repository at the commit")
	workflow := flags.String("workflow", "", "the workflow that holds the spec-citations job")
	changedFile := flags.String("changed", "", "what spec-check changed printed: the base line, then the paths that changed since the base")
	allChanged := flags.Bool("all-changed", false, "the commit has no base commit, so every file counts as changed")
	reportsDir := flags.String("reports", "", "the directory of the report artifacts of the run")
	needsFile := flags.String("needs", "", "toJSON(needs) of the spec-citations job")
	var tiesFiles ids
	flags.Var(&tiesFiles, "ties", "a file of ties by commit, {\"<commit>\":{\"<check>\":[…]}}; may be given more than once")
	jsonFile := flags.String("json", "", "the file to write the faults to as JSON")
	allowEmpty := flags.Bool("allow-empty", false, "read a directory that does not exist or holds no spec file as no check to cite")
	if code, ok := parse(flags, args, stderr, "dir", "repo", "workflow", "reports", "needs"); !ok {
		return code
	}
	switch {
	case *changedFile == "" && !*allChanged:
		return usageFault(stderr, "citations: give --changed <file> or --all-changed")
	case *changedFile != "" && *allChanged:
		return usageFault(stderr, "citations: give --changed <file> or --all-changed, not both")
	}
	paths := []*string{dir, repo, workflow, changedFile, reportsDir, needsFile, jsonFile}
	for i := range tiesFiles {
		paths = append(paths, &tiesFiles[i])
	}
	for _, path := range paths {
		if *path == "" {
			continue
		}
		abs, ok := absolute(stderr, *path)
		if !ok {
			return 2
		}
		*path = abs
	}
	if !isDirectory(stderr, *repo) || !isCloneTop(stderr, *repo) {
		return 2
	}

	content, err := os.ReadFile(*workflow)
	if err != nil {
		say(stderr, "%s: cannot be read: %s", *workflow, err)
		return 2
	}
	jobs, err := specgraph.ReadWorkflowJobs(content, citationsJob)
	if err != nil {
		say(stderr, "%s: %s", *workflow, err)
		return 2
	}
	ran, err := readNeeds(*needsFile, jobs.Awaited)
	if err != nil {
		say(stderr, "%s", err)
		return 2
	}
	reports, err := readReports(stderr, *reportsDir, jobs.Awaited, ran)
	if err != nil {
		say(stderr, "%s", err)
		return 2
	}
	var changed []string
	var baseCommit string
	if *changedFile != "" {
		if baseCommit, changed, err = readChanged(*changedFile); err != nil {
			say(stderr, "%s", err)
			return 2
		}
	}
	// The tie of a check at the commit, and at the base commit of --changed; Citations asks for each
	// only for a check whose test did not run.
	var tie, baseTie func(specgraph.Check) specgraph.Tie
	if len(tiesFiles) > 0 {
		files, err := readTiesFiles(tiesFiles)
		if err != nil {
			say(stderr, "%s", err)
			return 2
		}
		// The ties at the commit are the files' ties of the commit that the repository is at.
		head, ok := commitName(*repo, "HEAD")
		if !ok {
			say(stderr, "%s: git cannot read its commit HEAD, whose ties --ties gives", *repo)
			return 2
		}
		tie = func(c specgraph.Check) specgraph.Tie { return fileTie(files, head, c.ID) }
		if *changedFile != "" {
			baseTie = func(c specgraph.Check) specgraph.Tie { return fileTie(files, baseCommit, c.ID) }
		}
	} else {
		tie = newDerivedTies(*repo, nil).tie
		if *changedFile != "" {
			if head, ok := commitName(*repo, "HEAD"); ok && head == baseCommit {
				// The base is the commit itself, whose tie is the one at the commit (R8: a result counts
				// at its own commit), so no checkout is made.
				baseTie = tie
			} else {
				base := &baseCheckout{repo: *repo, commit: baseCommit}
				defer base.close()
				baseTie = base.tie
			}
		}
	}

	j, err := read(stderr, *dir)
	switch {
	case errors.Is(err, notes.ErrNoDirectory) && *allowEmpty:
		// Git keeps no empty directory, so a repository with no spec file yet has no directory.
	case err != nil:
		return cannotJudge(stderr, *dir, err)
	case len(j.problems) > 0:
		// A block that does not parse is missing from the tree, so a check in it would go uncited.
		j.sayProblems(stderr)
		return 2
	case j.files == 0 && !*allowEmpty:
		say(stderr, "%s: holds no spec file", *dir)
		return 2
	}
	if j.files == 0 {
		if !writeFaults(stderr, *jsonFile, nil) {
			return 2
		}
		say(stdout, "%s: holds no spec file; no check to cite", *dir)
		return 0
	}

	faults := specgraph.Citations(j.tree, specgraph.CitationInput{Code: &specgraph.GoCode{Dir: *repo}, Jobs: jobs, Reports: reports,
		Changed: changed, AllChanged: *allChanged, Tie: tie, BaseTie: baseTie})
	if !writeFaults(stderr, *jsonFile, faults) {
		return 2
	}
	for _, f := range faults {
		say(stderr, "%s:%d: %s: %s", filepath.Join(*dir, f.Pos.File), f.Pos.Line, f.Check, f.Detail)
	}
	programs := 0
	for _, c := range j.tree.Checks {
		if c.Procedure == specgraph.ProcedureProgram {
			programs++
		}
	}
	if len(faults) > 0 {
		say(stdout, "%s: %s; %d refused", *dir, count(programs, "program check"), len(faults))
		return 1
	}
	say(stdout, "%s: %s; each is cited", *dir, count(programs, "program check"))
	return 0
}

// usageFault writes the line that names a fault of the arguments, then the usage, and answers 2.
func usageFault(stderr io.Writer, format string, args ...any) int {
	say(stderr, format, args...)
	fmt.Fprintln(stderr, usage)
	return 2
}

// readNeeds reads toJSON(needs) of the citations job, {"<job>":{"result":…,"outputs":…}}, and
// answers whether each awaited job ran: its result is success or failure. The file must name
// exactly the awaited jobs, each with a result, or the workflow read is not the one that ran.
func readNeeds(path string, awaited map[string]bool) (map[string]bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot be read: %w", path, err)
	}
	var needs map[string]struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(content, &needs); err != nil {
		return nil, fmt.Errorf("%s: cannot be read: %w", path, err)
	}
	for _, job := range slices.Sorted(maps.Keys(awaited)) {
		if _, held := needs[job]; !held {
			return nil, fmt.Errorf("%s: holds no result of job %q, which %s waits for", path, job, citationsJob)
		}
	}
	ran := make(map[string]bool, len(needs))
	for _, job := range slices.Sorted(maps.Keys(needs)) {
		switch result := needs[job].Result; {
		case !awaited[job]:
			return nil, fmt.Errorf("%s: holds job %q, which %s does not wait for", path, job, citationsJob)
		case result == "":
			return nil, fmt.Errorf("%s: job %q has no result", path, job)
		default:
			ran[job] = result == "success" || result == "failure"
		}
	}
	return ran, nil
}

// readReports reads the report artifacts under dir, one directory spec-report--<job> or
// spec-report--<job>--<leg> each, with the go test -json report report.jsonl, and answers one
// JobReport for each leg of an awaited job that left one, and one with no report for each awaited
// job that left none. A directory that does not exist holds no report: download-artifact makes none
// when no artifact matches. A report of a job that the citations job does not wait for is passed
// over with a warning, since that job may still be running.
func readReports(stderr io.Writer, dir string, awaited, ran map[string]bool) ([]specgraph.JobReport, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: cannot be read: %w", dir, err)
	}
	var reports []specgraph.JobReport
	reported := map[string]bool{}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		rest, artifact := strings.CutPrefix(entry.Name(), reportPrefix)
		if !artifact || !entry.IsDir() {
			return nil, fmt.Errorf("%s: is no directory named %s<job> or %s<job>--<leg>", path, reportPrefix, reportPrefix)
		}
		job, leg, ok := reportJob(rest, awaited)
		if !ok {
			say(stderr, "warning: %s: names no job that %s waits for; passed over", path, citationsJob)
			continue
		}
		file := filepath.Join(path, "report.jsonl")
		tests, err := readReport(file)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot be read: %w", file, err)
		}
		reports = append(reports, specgraph.JobReport{Job: job, Leg: leg, Ran: ran[job], HasReport: true, Tests: tests})
		reported[job] = true
	}
	for _, job := range slices.Sorted(maps.Keys(awaited)) {
		if !reported[job] {
			reports = append(reports, specgraph.JobReport{Job: job, Ran: ran[job]})
		}
	}
	return reports, nil
}

// reportJob is the awaited job and the leg that the rest of an artifact's name, after
// spec-report--, names: "<job>" or "<job>--<leg>", the longest job that fits; ok is false when it
// names none.
func reportJob(rest string, awaited map[string]bool) (job, leg string, ok bool) {
	for candidate := range awaited {
		if len(candidate) <= len(job) {
			continue
		}
		if rest == candidate {
			job, leg, ok = candidate, "", true
		} else if l, cut := strings.CutPrefix(rest, candidate+"--"); cut && l != "" {
			job, leg, ok = candidate, l, true
		}
	}
	return job, leg, ok
}

// readReport reads one go test -json report.
func readReport(path string) ([]specgraph.TestOutcome, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return specgraph.ReadGoTestJSON(f)
}

// readChanged reads --changed, in the one form that changed prints: the base line "base <object
// name>", then one path a line. It refuses a list that it cannot read for certain: one with no
// base line, whose paths would be changes since no known commit, or with an empty line.
func readChanged(path string) (base string, changed []string, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("%s: cannot be read: %w", path, err)
	}
	first, rest, _ := strings.Cut(string(content), "\n")
	base, ok := strings.CutPrefix(first, baseLine)
	if !ok || !objectName(base) {
		return "", nil, fmt.Errorf("%s:1: no base line %q, which spec-check changed writes first", path, baseLine+"<commit>")
	}
	n := 1
	for line := range strings.Lines(rest) {
		n++
		text := strings.TrimSuffix(line, "\n")
		if text == "" {
			return "", nil, fmt.Errorf("%s:%d: an empty line, where a path was due", path, n)
		}
		changed = append(changed, text)
	}
	return base, changed, nil
}

// writeFaults writes the faults to path as one line of JSON with printJSON, [] for none, making its
// directory, and answers whether it could; with no path it writes nothing. It is the value at the
// commit of the input of the state of each check, Input.Uncited, which is by commit, then by
// check, so its reader files it under that commit.
func writeFaults(stderr io.Writer, path string, faults specgraph.CitationFaults) bool {
	if path == "" {
		return true
	}
	entries := make([]citationFault, len(faults))
	for i, f := range faults {
		entries[i] = citationFault{Check: f.Check, Rule: f.Rule, Detail: f.Detail}
	}
	var content bytes.Buffer
	if printJSON(&content, stderr, path, entries) != 0 {
		return false
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path, content.Bytes(), 0o644)
	}
	if err != nil {
		say(stderr, "%s: cannot be written: %s", path, err)
		return false
	}
	return true
}
