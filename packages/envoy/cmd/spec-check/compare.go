package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// ciVariable is the variable whose value true says that a process runs in a CI job. Each CI job of
// this repository is a GitHub Actions job, and GitHub sets GITHUB_ACTIONS=true in each step of each
// job. The command does not read CI: an agent's shell sets CI=true too, and an agent's step is no
// CI job (R12).
const ciVariable = "GITHUB_ACTIONS"

// runCompare is "spec-check compare --dir <dir> --approved <approved.json> [--allow-empty]": it
// compares the files of dir with the approved spec and answers 0 when the approved spec held each
// text that they hold, 1 when it did not, and 2 for files with a problem of Parse that can lose a
// node or change one (its Effect is specgraph.LosesNode), a file that is not as print writes it
// among them, a directory or a file it cannot read, a directory that holds no spec file without
// --allow-empty, or arguments it does not take. The printed form is the one form that each YAML
// library reads alike, so compare trusts no reading of another form, and the not-printed line names
// the way out, spec-check print. A problem of Parse that leaves each node as a printed file writes
// it (specgraph.KeepsEachNode), a value that the schema refuses and the node keeps or an empty
// file, is tree's to judge, and compare compares those files. For files with a problem that it
// refuses (refusedProblems), it writes one line on stderr for each such problem,
// "<dir>/<file>:<line>: <node>: <detail>", then "<dir>: the files do not parse, or are not as
// print writes them, so no comparison was made".
//
// It answers 2, before it reads an argument, in a CI job: R2 gives the comparison at each push to
// Dispatch, and no CI job compares the files. No route of the Dispatch server calls
// specgraph.Compare yet. The refusal is the command's and not specgraph's, since the tests of
// specgraph run in CI. It reads the environment of its own process, so it covers each route that
// passes the runner's environment on to the command: a step, a package script, a make target, a
// script in any directory, or a link to one. It does not cover a route that drops that
// environment, such as sudo with its default env_reset or a container that docker run starts
// without -e GITHUB_ACTIONS: there the command compares (TestTheBuiltBinary, "compare compares
// behind a boundary that drops the environment"). spec-check guard names such a line in the
// workflows, actions and scripts that it reads, when the command's name and compare stand next to
// each other there.
func runCompare(args []string, stdout, stderr io.Writer) int {
	if os.Getenv(ciVariable) == "true" {
		say(stderr, "compare does not run in a CI job (%s=true): R2 gives the comparison of the files with the approved spec at each push to Dispatch", ciVariable)
		return 2
	}
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	dirFlag := flags.String("dir", "", "the directory of spec files")
	approvedFlag := flags.String("approved", "", "the approved spec, in the form of approved.json")
	allowEmpty := flags.Bool("allow-empty", false, "read a directory that does not exist or holds no spec file as the empty tree")
	if code, ok := parse(flags, args, stderr, "dir", "approved"); !ok {
		return code
	}
	dir, ok := absolute(stderr, *dirFlag)
	if !ok {
		return 2
	}
	_, a, ok := readApproved(stderr, *approvedFlag)
	if !ok {
		return 2
	}
	j, err := read(stderr, dir)
	what, empty := noSpecFile(j, err)
	lost := refusedProblems(j.problems)
	switch {
	case empty:
		// An instrument that read no file has not passed: every text of no file is approved. But
		// git keeps no empty directory, so a revert of the change that added the last spec file
		// leaves none (R2: the revert "takes it out of the files"), and the caller can ask for the
		// empty tree.
		if !*allowEmpty {
			say(stderr, "%s: %s, and compare reads it as the empty tree only with --allow-empty", dir, what)
			return 2
		}
		say(stderr, "%s: %s, so compare reads it as the empty tree", dir, what)
	case err != nil:
		return cannotJudge(stderr, dir, err)
	case len(lost) > 0:
		// The tree misses the text of a document that the reader does not read into its node, or of a
		// value that its node drops, or holds yaml.v3's reading of a file that another library can
		// read as another text, so a comparison of the tree would pass that text.
		judged{dir: dir, problems: lost}.sayProblems(stderr)
		say(stderr, "%s: the files do not parse, or are not as print writes them, so no comparison was made", dir)
		return 2
	}

	c := specgraph.Compare(j.tree, a)
	if len(c.Problems) > 0 {
		judged{dir: dir, problems: c.Problems}.sayProblems(stderr)
		say(stdout, "%s: the comparison fails on %s", dir, count(len(c.Problems), "node"))
		return 1
	}
	current, _ := a.Current() // ReadApproved refused every document that would be a problem here
	others, held := notCurrentLines(current, c, a)
	all := len(current.Statements)
	// The verb and its pronoun take the number of held. The noun is in the singular only for 1 of
	// 1, so a verb in the plural never follows a noun in the singular: "0 of 1 approved statements
	// are", "1 of 3 approved statements is".
	noun, verb, pronoun := "statements", "are", "their"
	if held == 1 {
		verb, pronoun = "is", "its"
		if all == 1 {
			noun = "statement"
		}
	}
	// The files may hold an earlier approved text or a text of a removed node, so the line says
	// that the approved spec held each text, not that each text is approved now.
	say(stdout, "%s: the approved spec held every text that the files hold; %d of %d approved %s %s in the files at %s current text",
		dir, held, all, noun, verb, pronoun)
	for _, line := range others {
		say(stdout, "%s", line)
	}
	for _, line := range removedLines(j.tree, c.Removed) {
		say(stdout, "%s", line)
	}
	return 0
}

// refusedProblems is each problem of Parse in problems for which compare refuses the files: each
// one whose Effect is not specgraph.KeepsEachNode. So a problem with no Effect is refused, as one
// that loses a node is, and no problem reads as one that keeps each node by default.
// Only compare reads the Effect: tree and print (judgeFiles), and state (specgraph.ReadScenario),
// refuse each problem of Parse whatever its Effect.
func refusedProblems(problems []specgraph.Problem) []specgraph.Problem {
	return slices.DeleteFunc(problems, func(p specgraph.Problem) bool { return p.Effect == specgraph.KeepsEachNode })
}

// notCurrentLines is one line for each node of current, the current approved spec, that the files
// do not hold at its current text, of each kind (R2: the change that lands a statement writes it
// and its checks into the files): "<kind> <id>: not in the files", or "<kind> <id>: the files hold
// approved version 1, and version 2 is current", in the order of specgraph.Nodes, each id cut. held
// is the number of statements that the files hold at their current text.
func notCurrentLines(current specgraph.Tree, c specgraph.Comparison, a specgraph.Approved) (lines []string, held int) {
	for _, n := range specgraph.Nodes(current) {
		id, kind := n.NodeID(), n.KindName()
		h := c.Holds[id]
		switch {
		case !h.Present:
			lines = append(lines, kind+" "+specgraph.Cut(id)+": not in the files")
		case !h.Current:
			versions := a.Nodes[id].Versions
			lines = append(lines, fmt.Sprintf("%s %s: the files hold approved version %d, and version %d is current", kind, specgraph.Cut(id), h.Version, versions[len(versions)-1].Version))
		case kind == "statement":
			held++
		}
	}
	return lines, held
}

// removedLines is one line for each node of tree whose id is in removed, the nodes that the
// approved spec removed and the files still hold (R28: "the files hold one that is no longer
// approved"): "<kind> <id>: the files hold it, and the approved spec removed it", in the order of
// kinded, and an id once, cut.
func removedLines(tree specgraph.Tree, removed []string) []string {
	var lines []string
	named := map[string]bool{} // an id the files hold two times is one line
	for _, n := range specgraph.Nodes(tree) {
		if id := n.NodeID(); slices.Contains(removed, id) && !named[id] {
			named[id] = true
			lines = append(lines, n.KindName()+" "+specgraph.Cut(id)+": the files hold it, and the approved spec removed it")
		}
	}
	return lines
}

// readApproved reads the approved spec at path with specgraph.ReadApproved, and answers the path
// made absolute and whether it could read it; when not, it has written the line that names the
// file and the fault.
func readApproved(stderr io.Writer, path string) (string, specgraph.Approved, bool) {
	path, ok := absolute(stderr, path)
	if !ok {
		return path, specgraph.Approved{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		say(stderr, "%s: cannot be read: %s", path, err)
		return path, specgraph.Approved{}, false
	}
	defer f.Close()
	a, err := specgraph.ReadApproved(f)
	if err != nil {
		say(stderr, "%s: %s", path, err)
		return path, specgraph.Approved{}, false
	}
	return path, a, true
}
