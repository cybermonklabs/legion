package main

import (
	"flag"
	"io"
	"strings"
)

// ids is the value of a flag that can be given more than once, each value in the order given.
type ids []string

func (v *ids) String() string      { return strings.Join(*v, " ") }
func (v *ids) Set(id string) error { *v = append(*v, id); return nil }

// runPrint is "spec-check print --approved <approved.json> --id <id> [--id <id>...]": it writes
// the current approved block of each node named, and after a statement the block of each current
// check that serves it, each block once, separated by one blank line, and answers 0; 2 when the
// approved spec does not hold an id or removed it, for a file it cannot read or arguments it does
// not take, with nothing on stdout.
//
// The blocks do not pass through runnerlog.Say: a change copies them into the files byte for byte
// (R2: "A change copies that output into the files, and no agent types it"). Each is its node's
// block as the approved spec holds it, which ReadApproved has checked is the block Print writes.
// So the output of print must not reach a CI log: a line of a block that starts with "::" would be
// a workflow command there.
func runPrint(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("print", flag.ContinueOnError)
	approvedFlag := flags.String("approved", "", "the approved spec, in the form of approved.json")
	var named ids
	flags.Var(&named, "id", "a node of the approved spec to print; give it once for each node")
	if code, ok := parse(flags, args, stderr, "approved", "id"); !ok {
		return code
	}
	path, a, ok := readApproved(stderr, *approvedFlag)
	if !ok {
		return 2
	}
	refused := false
	for _, id := range named {
		switch n, held := a.Nodes[id]; {
		case !held:
			say(stderr, "%s: the approved spec holds no node %q", path, id)
			refused = true
		case *n.Removed: // ReadApproved refused a node that does not set it
			say(stderr, "%s: the approved spec removed node %q, so it has no current text", path, id)
			refused = true
		}
	}
	if refused {
		return 2
	}

	current, _ := a.Current() // ReadApproved refused every block that would be a problem here
	var blocks []string
	printed := map[string]bool{}
	add := func(id string) {
		if !printed[id] {
			printed[id] = true
			versions := a.Nodes[id].Versions
			blocks = append(blocks, versions[len(versions)-1].Block)
		}
	}
	for _, id := range named {
		add(id)
		if _, statement := current.StatementByID(id); statement {
			for _, c := range current.ChecksOf(id) {
				add(c.ID)
			}
		}
	}
	_, _ = io.WriteString(stdout, strings.Join(blocks, "\n"))
	return 0
}
