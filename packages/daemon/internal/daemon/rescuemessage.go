package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// rescuedBranch is one rescue branch of the check as the shim reports it, the repository of the
// workspace that first reports it (rescue.RepositoryOf), and its kind (rescueKindOf).
type rescuedBranch struct {
	runtime.RescuedBranch
	repository string
	kind       rescueKind
}

// rescueKind is which of four kinds a reported rescue branch is. The issue's message
// (rescueSummary, rescueBranchSummary) and the document it links (rescueList) each tell every
// kind in words of their own.
type rescueKind int

const (
	// olderShimBranch is a per-pod rescue branch of a shim that predates the issue's one rescue
	// branch. Such a shim reports the branch's name and tip alone: no commits and no tip before.
	olderShimBranch rescueKind = iota
	// createdBranch is the issue's one rescue branch of a repository, which this check pushed and
	// which the remote did not have before the push.
	createdBranch
	// movedBranch is the issue's one rescue branch of a repository, which this check pushed
	// forward from the tip it had before the push (Previous).
	movedBranch
	// heldBranch is the issue's one rescue branch of a repository, which this check did not push
	// because it already holds work the issue branch does not have. Its commits are the ones
	// only that branch holds.
	heldBranch
)

// rescueKindOf is the kind of b. A shim of this daemon reports at least one commit for every
// branch it reports (the commit it pushed, or the commits only the branch holds), and an older
// shim reports none (runtime.RescuedBranch), so a branch with no commits is an older shim's.
func rescueKindOf(b runtime.RescuedBranch) rescueKind {
	switch {
	case len(b.Commits) == 0:
		return olderShimBranch
	case !b.Pushed:
		return heldBranch
	case b.Previous == "":
		return createdBranch
	default:
		return movedBranch
	}
}

// branches is every rescue branch the check reports, in the order reported, each once for each
// repository, with its kind. Each repository of the issue has a `legion/<KEY>-rescue` of its own,
// so the name alone would hide all but the first. An older shim reports a branch of its pod again
// from every workspace whose remote holds it, so the workspace alone would name one branch twice.
func (p pushCheck) branches() []rescuedBranch {
	type key struct{ repository, name string }
	var branches []rescuedBranch
	seen := map[key]bool{}
	for _, w := range p.workspaces {
		repository := rescue.RepositoryOf(w.Dir)
		for _, b := range w.Branches {
			if k := (key{repository, b.Name}); !seen[k] {
				seen[k] = true
				branches = append(branches, rescuedBranch{RescuedBranch: b, repository: repository, kind: rescueKindOf(b)})
			}
		}
	}
	return branches
}

// rescueListed is how many commit ids of a branch, or branches of an older shim's report, the
// rescue's message names before it says how many more there are. The document the message links
// lists every one, so the message fits in one block of Dispatch's whatever the check found: a
// notice that named each branch whole was 34,828 characters, and Dispatch refused it
// (dispatch://INFRA-2704).
const rescueListed = 20

// rescueMessage is what the issue is told after its pod's replacement when the push check found
// work the issue branch does not have, or could not check the pod, and an empty post when it found
// nothing. The issue's branch does not hold rescued work, and the next pod provisions from that
// branch, so somebody must bring it in. The body is a summary (rescueSummary) that links a document
// of every branch and commit (rescueList), which the outbox puts on the issue before it posts the
// body. A summary Dispatch would refuse all the same is made again with no ids in it. Another agent
// reads it, so it follows ASD-STE100.
func rescueMessage(issue, round string, checked pushCheck) record.MessagePost {
	if checked.unchecked != "" {
		return record.MessagePost{Body: fmt.Sprintf("The daemon replaced the pod of this issue (update round %s). It did not check the pod's workspaces for work that was not pushed: %s. Work that a worker did not push is lost with the pod.", round, checked.unchecked)}
	}
	branches := checked.branches()
	if len(branches) == 0 {
		return record.MessagePost{}
	}
	artifact := &record.MessageArtifact{Name: rescueArtifactName(round), Content: rescueList(issue, round, branches)}
	link := "dispatch://" + issue + "/artifact/" + artifact.Name
	body := rescueSummary(round, branches, link, rescueListed)
	if dispatchbody.Check(record.LongestPost(body)) != nil {
		body = rescueSummary(round, branches, link, 0)
	}
	return record.MessagePost{Body: body, Artifact: artifact}
}

// rescueArtifactName is the name of the document a rescue's message links: `rescue-<round>`, with
// each character that is no lowercase ASCII letter or digit made a dash, each run of dashes made
// one, and no dash at either end. Dispatch files a document of that name under the name itself
// (its artifactSlug), so the message links it by its name before it exists, and a second run of
// the message's row finds the document the first one put there.
func rescueArtifactName(round string) string {
	var name strings.Builder
	dash := false
	for _, r := range "rescue-" + round {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			if dash && name.Len() > 0 {
				name.WriteByte('-')
			}
			name.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	return name.String()
}

// rescueSummary is the body of a rescue's message, which names at most listed commit ids of each
// branch and listed branches of an older shim's report, and ends with the link to the full list.
// Each branch of every kind but olderShimBranch is the issue's one rescue branch of a repository,
// with what this check pushed to it or the commits only that branch holds, and has an item of its
// own (rescueBranchSummary). An older shim's per-pod branches, which that shim reports as it
// always did (runtime.RescuedBranch), are told together: how many there are, and the first listed.
// Such a shim pushed every local branch, visible head, tag, stash entry and other worktree of the
// pod that the remote lacked, so its branches can hold another session's commits and another
// issue's: the summary tells the reader to take only the commits it needs from them, and gives
// the warning the round's ask gives too (supervise.RescueBranchWarning).
func rescueSummary(round string, branches []rescuedBranch, link string, listed int) string {
	var carried, older []rescuedBranch
	for _, b := range branches {
		if b.kind == olderShimBranch {
			older = append(older, b)
		} else {
			carried = append(carried, b)
		}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Before the daemon replaced the pod of this issue (update round %s), its push check found work that the issue branch does not have. The new pod starts from the issue branch.", round)
	if len(carried) > 0 {
		var names []string
		text.WriteString("\n")
		for _, b := range carried {
			text.WriteString("\n- " + rescueBranchSummary(b, listed))
			if name := "`" + b.Name + "`"; !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		fmt.Fprintf(&text, "\n\nWhen this work is necessary, merge %s into the issue branch. The branch keeps each earlier rescue of this issue.", rescueJoin(names, len(names)))
	}
	if len(older) > 0 {
		fmt.Fprintf(&text, "\n\nThe pod's shim is older than this daemon. It reported %s of this pod. It did not report their commits.", rescueCount(len(older), "rescue branch", "rescue branches"))
		if listed > 0 {
			text.WriteString("\n")
			for _, b := range older[:min(listed, len(older))] {
				fmt.Fprintf(&text, "\n- `%s` (commit %s) in `%s`", b.Name, b.Commit, b.repository)
			}
			if more := len(older) - listed; more > 0 {
				fmt.Fprintf(&text, "\n- and %d more", more)
			}
		}
		text.WriteString("\n\nTake the commits that you need from each of these branches into the issue branch, or delete the branch if its work is not necessary.\n\n" + supervise.RescueBranchWarning)
	}
	text.WriteString("\n\nThe full list of branches and commits is in this document: " + link)
	return text.String()
}

// rescueBranchSummary is the summary's item for one branch of the kind createdBranch, movedBranch
// or heldBranch: its repository, its name and its tip, then what this check pushed to it and the
// tip before, or, for a branch it pushed nothing to, the commits only that branch holds. It names
// at most listed ids. That the branch already holds the work says nothing of which pod put it
// there: the same report comes from a later check of this pod and from a new pod whose put-back
// restored work only the branch holds.
func rescueBranchSummary(b rescuedBranch, listed int) string {
	var text strings.Builder
	before := ""
	switch b.kind {
	case heldBranch:
		fmt.Fprintf(&text, "In the repository `%s`, the branch `%s` already holds this work from an earlier rescue. Its tip is `%s`. The issue branch does not have this work. %s only on this branch",
			b.repository, b.Name, b.Commit, rescueCount(len(b.Commits), "commit is", "commits are"))
		if listed > 0 {
			text.WriteString(": " + rescueIDs(b.Commits, listed))
		}
		text.WriteString(".")
		return text.String()
	case createdBranch:
		before = " This is a new branch."
	case movedBranch:
		before = " The tip before the push was `" + b.Previous + "`."
	}
	fmt.Fprintf(&text, "In the repository `%s`, the push check pushed %s to the branch `%s`. The tip of the branch is now `%s`.%s",
		b.repository, rescueCount(len(b.Commits), "commit", "commits"), b.Name, b.Commit, before)
	if listed > 0 {
		commits := "commits are"
		if len(b.Commits) == 1 {
			commits = "commit is"
		}
		fmt.Fprintf(&text, " The %s %s.", commits, rescueIDs(b.Commits, listed))
	}
	return text.String()
}

// rescueIDs is the first listed of commits' ids, each as code, and how many more there are.
func rescueIDs(commits []runtime.RescuedCommit, listed int) string {
	ids := make([]string, len(commits))
	for i, c := range commits {
		ids[i] = "`" + c.ID + "`"
	}
	return rescueJoin(ids, listed)
}

// rescueJoin is the first listed of items, joined as a sentence joins them, with how many more
// there are when there are more.
func rescueJoin(items []string, listed int) string {
	if more := len(items) - listed; more > 0 {
		return strings.Join(items[:listed], ", ") + " and " + strconv.Itoa(more) + " more"
	}
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// rescueCount is n with the word for one or for many.
func rescueCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// rescueList is the document a rescue's message links: every branch the check reported, with its
// repository, its tip and its tip before, and every commit each carries with its subject. The list
// is one fenced code block, so no subject a commit was given is read as markdown, and the fence is
// longer than any run of backticks inside it, so no subject closes it. A list longer than one
// Dispatch document can be (record.MaxMessageArtifactBytes) stops where it would pass that bound,
// and says how many lines it leaves out, so the message is never refused for its document.
func rescueList(issue, round string, branches []rescuedBranch) string {
	var lines []string
	for i, b := range branches {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "repository: "+b.repository, "branch: "+b.Name, "tip: "+b.Commit)
		switch b.kind {
		case olderShimBranch:
			// Such a shim reports no commits, so there are none to count or to list.
			lines = append(lines, "tip before: not reported, because the pod's shim is older than the daemon")
			continue
		case heldBranch:
			lines = append(lines, "tip before: the push check pushed nothing, and these commits are only on this branch")
		case createdBranch:
			lines = append(lines, "tip before: none, because this is a new branch")
		case movedBranch:
			lines = append(lines, "tip before: "+b.Previous)
		}
		lines = append(lines, "commits: "+strconv.Itoa(len(b.Commits)))
		for _, c := range b.Commits {
			lines = append(lines, strings.TrimRight(c.ID+" "+c.Subject, " "))
		}
	}
	header := fmt.Sprintf("Before the daemon replaced the pod of %s (update round %s), its push check found this work. The issue branch does not have it.\n\n", issue, round)
	listing := strings.Join(lines, "\n")
	fence := strings.Repeat("`", max(3, longestRun(listing, '`')+1))
	// stops is the sentence after a list cut at the bound, with the newline on each side of it.
	stops := func(left int) string {
		return fmt.Sprintf("\nThe list stops here. It leaves out %d more lines, because one Dispatch document holds at most %d bytes.\n", left, record.MaxMessageArtifactBytes)
	}
	// What the header, the two fences and that sentence take of the bound. The sentence is
	// measured at its longest: a cut leaves out at most every line, so no count it names is
	// longer than len(lines).
	room := record.MaxMessageArtifactBytes - len(header) - 2*(len(fence)+1) - len(stops(len(lines)))
	left := ""
	if len(listing) > room {
		kept, size := 0, 0
		for kept < len(lines) && size+len(lines[kept])+1 <= room {
			size += len(lines[kept]) + 1
			kept++
		}
		listing = strings.Join(lines[:kept], "\n")
		left = stops(len(lines) - kept)
	}
	return header + fence + "\n" + listing + "\n" + fence + "\n" + left
}

// longestRun is the length of the longest run of c in text.
func longestRun(text string, c byte) int {
	longest, run := 0, 0
	for i := range len(text) {
		if text[i] != c {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}
