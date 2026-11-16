package sandbox

import (
	"os"
	"path"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The reclaim paragraph of docs/kubernetes.md is an operator's whole remedy for an issue volume
// near full, and its claim is an accounting one: every directory this layout puts on the volume is
// on its delete list or in one of its exemptions. A path the renderer creates and neither list
// carries is the failure this guards — the operator deletes the five paths, the volume is still
// full, and the bytes are in a directory nobody wrote down. Six fixes to this paragraph in one
// review were each a rendered path missing from it, found by a reader rather than by a check.
//
// This half of that guarantee does not read the guide. It reads reclaim_doc_extract.txt, this
// package's own copy of the paragraph's two lists, which internal/textcheck's
// TestTheReclaimExtractIsTheGuidesReclaimParagraph holds equal to the paragraph itself. The split
// is what takes docs/kubernetes.md out of this job's filter: a declared read applies to every
// compiled source file of a run, so a test that reads the guide starts the whole daemon Go lane on
// a documentation-only edit however the run selects it, and only a test that does not read it at
// all stops doing that.
//
// Reading the paragraph's shape — which bullet is a path the operator removes, which word inside
// an exemption's reason is not a path on any list, where the paragraph ends — is that other test's
// job, on the guide itself. That reading is not fastidiousness: an earlier version of this test
// asked only whether the paragraph mentioned a path anywhere between its markers, and it passed
// with /legion/uv/cache struck from the delete list, because an exemption's reason happens to
// mention it. None of it is parsed here. This file reads a list of paths under two headings, which
// is all the extract is.
//
// A delete-list path covers what sits under it, which is how `/legion/go` accounts for GOCACHE and
// GOMODCACHE, and that coverage is the hole a coarsened bullet used to slip through: `/legion/uv`
// in place of `/legion/uv/cache` accounted for every rendered path while telling the operator to
// delete `/legion/uv/python/<issue>`, the interpreters an exemption exists to keep. The check that
// closes that one, TestTheReclaimParagraphNeverDeletesWhatItExempts, is a property of the two
// lists rather than of a rendered directory, so it went to internal/textcheck with them.
//
// What this cannot do: it reads the lists, not their grounds. It can say that /legion/scratch/data
// is accounted for; it cannot say the sentence giving the reason is true, and an earlier head
// passed a check like this one carrying an exemption whose stated reason the code refuted. It also
// does not care which of the two lists carries a path, so a path moved from the delete list to the
// exemptions, or the reverse, still passes. Both are a reader's job.
const (
	// reclaimExtract is read from this package's own directory, where the file sits beside the
	// code it is about and needs no declared read of its own.
	reclaimExtract = "reclaim_doc_extract.txt"
	// The two lines of it that open a list. The same two open the extract's lists for
	// internal/textcheck's TestTheReclaimExtractIsTheGuidesReclaimParagraph, which keeps a parse
	// of its own over this same file. The copy stands rather than being shared: that package may
	// not import this one - doing so would put the Kubernetes runtime, and every file this
	// package's tests read, back in the text-check job's filter, which is what splitting the
	// check took out - and a package both could import would be a new package of this module
	// carrying nine lines. The two parses differ in one point, below: this one refuses a line
	// outside TreeRoot, where that one takes it and lets the comparison with the guide refuse it.
	reclaimDeleteList = "delete:"
	reclaimExemptList = "exempt:"
)

// Every directory roleEnvironment and the pod's uv variables put on the issue's volume is on the
// delete list or in an exemption of the reclaim paragraph, as reclaim_doc_extract.txt copies the
// two. The rendered set comes from the renderer, never from a list written beside it: a list would
// have to be edited by whoever adds a cache, which is exactly the edit that gets forgotten.
func TestTheReclaimParagraphNamesEveryDirectoryTheRendererPutsOnTheIssueVolume(t *testing.T) {
	const issue = "LEGION-1234"
	const role = claim.RoleImplementer

	deleted, exempt := reclaimExtractLists(t)
	for _, dir := range issueVolumeDirectories(issue, role) {
		// A per-role or per-issue directory reaches the prose as a placeholder, since no document can
		// name every role and every issue. Rewrite the rendered value's last segment to the form the
		// paragraph uses rather than matching it loosely, so an unrelated path that happens to share a
		// prefix cannot stand in for it.
		documented := dir
		switch dir {
		case roleStateHome(role):
			documented = path.Dir(dir) + "/<role>"
		case uvPythonDir(issue):
			documented = path.Dir(dir) + "/<issue>"
		}
		onDeleteList := namesPathOrAncestor(deleted, documented)
		isExempt := namesPathOrAncestor(exempt, documented)
		switch {
		case onDeleteList && isExempt:
			t.Errorf("the reclaim paragraph both deletes and exempts %s: the operator is "+
				"told to remove it and to leave it, so one of the two bullets is wrong", documented)
		case !onDeleteList && !isExempt:
			t.Errorf("the reclaim paragraph accounts for %s in neither list, though the "+
				"renderer puts it on the issue's volume: it belongs on the delete list or in an exemption, "+
				"and a mention inside an exemption's reason is neither", documented)
		}
	}
}

// issueVolumeDirectories is what a role and the pod are told to write under the issue's
// volume, taken from the two renderers that tell them: roleEnvironment, which carries the role's
// temporary directory, its XDG homes and the three tool caches, and the issue pod's agentEnv, which
// carries uv's cache and Pythons. A value under TreeRoot is on that volume, since TreeRoot is where
// the issue's claim is mounted (issueVolume); XDG_CONFIG_HOME, under the pod's in-memory home, is
// outside it and drops out here without being listed.
func issueVolumeDirectories(issue string, role claim.Role) []string {
	l := launch{
		spec:      runtime.SpawnSpec{Tree: "legion", Issue: issue},
		workspace: TreeRoot + "/workspaces/owner/repo/" + issue,
	}
	var dirs []string
	for _, env := range append(roleEnvironment(role), issuePod{}.agentEnv(l)...) {
		// LEGION_WORKSPACE is on the volume but is not a directory this layout creates to absorb what a
		// tool writes: it is the issue's checkout, the work itself, and the remedy deletes nothing in
		// it. Every other variable pointing under TreeRoot is scratch and belongs in the accounting.
		if env.Name == "LEGION_WORKSPACE" || !strings.HasPrefix(env.Value, TreeRoot+"/") {
			continue
		}
		dirs = append(dirs, env.Value)
	}
	return dirs
}

// reclaimExtractLists reads the extract's two lists: the paths the delete list tells the operator
// to remove, and the paths the exemptions hold back. A line ending in a colon opens a list, every
// other line is one path, and the blank lines and the `#` header saying what the file is are passed
// over. The extract is written to be read that way, so this parse is a split and a switch and
// shares no code with the markdown reading that holds the file to the guide: two parsers that agree
// with each other while both drift are what a copy of a document is most at risk of.
func reclaimExtractLists(t *testing.T) (deleted, exempt map[string]bool) {
	t.Helper()
	// Read it and fail when it cannot be read: a skip here would be an absence that reads exactly
	// like a pass, and this check exists because a silent absence is what the paragraph kept
	// shipping.
	text, err := os.ReadFile(reclaimExtract)
	if err != nil {
		t.Fatalf("read %s: %v", reclaimExtract, err)
	}
	deleted, exempt = map[string]bool{}, map[string]bool{}
	var into map[string]bool
	for i, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch line {
		case reclaimDeleteList:
			into = deleted
			continue
		case reclaimExemptList:
			into = exempt
			continue
		}
		if into == nil {
			t.Fatalf("%s:%d: %q stands under neither %q nor %q, so which of the two lists it is on "+
				"cannot be read", reclaimExtract, i+1, line, reclaimDeleteList, reclaimExemptList)
		}
		// TreeRoot itself is a path a list may name: it covers no rendered directory
		// (namesPathOrAncestor stops short of it), but a delete list naming it swallows every
		// exemption, which is a reading the lists must be held to.
		if !strings.HasPrefix(line, TreeRoot+"/") && line != TreeRoot {
			t.Fatalf("%s:%d: %q is no path under %s, where this layout mounts the issue's volume: the "+
				"lists are the paths the remedy names and nothing else", reclaimExtract, i+1, line, TreeRoot)
		}
		into[line] = true
	}
	// An empty list is this check failing open: the file is still there, the parse matched nothing,
	// and every rendered path would then be reported against a list that is not being read.
	if len(deleted) == 0 {
		t.Fatalf("%s names no path under %q: its copy of the delete list is gone, and nothing is "+
			"being held to it", reclaimExtract, reclaimDeleteList)
	}
	if len(exempt) == 0 {
		t.Fatalf("%s names no path under %q: its copy of the exemptions is gone, and nothing is "+
			"being held to it", reclaimExtract, reclaimExemptList)
	}
	return deleted, exempt
}

// namesPathOrAncestor accepts a directory a list covers by naming a parent of it, which is how the
// remedy handles the Go pair: it deletes /legion/go, and GOCACHE and GOMODCACHE are under it.
// TreeRoot itself is not an ancestor that counts, since deleting the tree root is not a remedy this
// document offers and every path in the prose starts with it.
func namesPathOrAncestor(named map[string]bool, dir string) bool {
	for p := dir; strings.HasPrefix(p, TreeRoot+"/"); p = path.Dir(p) {
		if named[p] {
			return true
		}
	}
	return false
}
