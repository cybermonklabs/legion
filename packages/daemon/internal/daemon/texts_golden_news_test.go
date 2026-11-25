package daemon

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// designNews adds the news of an approved design in each of its forms (workflow.RenderNews): a
// holder's news with every kind of section and block row; a planner's news of a first approved
// version beside a pair Dispatch named nothing of; the news of an issue its architect holds in
// accepting beside a pair that holds the same text; the root architect's summary with members in
// every state; that summary sent after a later version closed the gate again; a holder's news of an
// item whose review history could not be read; one of a pair whose change list could not be read;
// and a holder owed the opening, sent while the gate stands open and after a later version closed
// it.
func (b *goldenBuild) designNews(r goldenRendering) {
	tree := r.key(70)
	specArtifact, checksArtifact := "artifact-"+r.id("news/spec"), "artifact-"+r.id("news/checks")
	specTo, checksTo := 5+r.n, 3+2*r.n
	open := func(step record.DesignStep, artifact string, to int, seq int64) record.DesignGate {
		return record.DesignGate{Issue: tree, Step: step, ArtifactID: artifact, LatestVersion: to, ApprovedVersion: new(to),
			ApprovedMark: record.EventMark{Seq: seq}}
	}
	gates := map[record.DesignStep]record.DesignGate{
		record.DesignStepSpec:     open(record.DesignStepSpec, specArtifact, specTo, 41),
		record.DesignStepCriteria: open(record.DesignStepCriteria, checksArtifact, checksTo, 40),
	}
	closed := map[record.DesignStep]record.DesignGate{record.DesignStepCriteria: gates[record.DesignStepCriteria]}
	closedSpec := gates[record.DesignStepSpec]
	closedSpec.LatestVersion = specTo + 1
	closed[record.DesignStepSpec] = closedSpec
	steps := []record.DesignStep{record.DesignStepSpec}
	specDoc, checksDoc := "dispatch://"+tree+"/spec", "dispatch://"+tree+"/artifact/"+dispatch.VerificationSlug
	// The document of each gate row as the daemon's send names it (workflow.News.Documents): both
	// rows record the tree's own documents, in gates and in closed alike.
	docs := map[record.DesignStep]string{record.DesignStepSpec: specDoc, record.DesignStepCriteria: checksDoc}
	word := func(label string, n int) string { return r.words("news/"+label, n) }
	change := func(step record.DesignStep, artifact string, from, to int) workflow.NewsChange {
		return workflow.NewsChange{Step: step, Changes: record.DesignChanges{VersionPair: record.VersionPair{ArtifactID: artifact, From: from, To: to},
			State: record.DesignChangesRead}}
	}
	headingA, headingB, headingC := word("heading-a", 14), word("heading-b", 11), word("heading-c", 9)
	idA, idB, idC, otherType := "r-"+word("id-a", 6), "note-"+word("id-b", 5), "c-"+word("id-c", 7), word("type", 8)
	specChanges := func(from int) workflow.NewsChange {
		news := change(record.DesignStepSpec, specArtifact, from, specTo)
		changes := &news.Changes
		changes.Sections = []record.SectionChange{
			{Heading: "", Change: "moved", Place: dispatch.PlaceBeforeFirstSection},
			{Heading: headingA, Change: "changed"},
			{Heading: "", Change: "added", Place: dispatch.PlaceAfterSection, AfterHeading: headingA},
			{Heading: headingB, Occurrence: 1, Change: "removed"},
			{Heading: "", Occurrence: 1, Change: "changed"},
			{Heading: "", Change: "changed"},
		}
		changes.Blocks = []record.BlockChange{{ID: idA, Type: "requirement", Change: "changed"}, {ID: idB, Type: "callout", Change: "added"},
			{ID: idB + "-x", Type: otherType, Change: "removed"}, {ID: idC, Type: "check", Change: "changed"}}
		return news
	}
	checksFrom := checksTo - 1
	checksChanges := func() workflow.NewsChange {
		news := change(record.DesignStepCriteria, checksArtifact, checksFrom, checksTo)
		changes := &news.Changes
		changes.Sections = []record.SectionChange{{Heading: headingC, Change: "changed"}}
		changes.Blocks = []record.BlockChange{{ID: idC, Type: "check", Change: "added"}, {ID: idA, Type: "requirement", Change: "changed"}}
		return news
	}
	item := func(want map[string]string, i int, step record.DesignStep, document, artifact string, version int, taken string, changes string) {
		at := fmt.Sprintf("items[%d].", i)
		want[at+"step"], want[at+"document"], want[at+"artifact"] = string(step), document, artifact
		want[at+"version"], want[at+"taken"], want[at+"changes"] = fmt.Sprint(version), taken, changes
	}
	holder := func(issue string, p phase.Phase, items ...workflow.NewsChange) workflow.News {
		return workflow.News{Audience: record.AudienceHolder, Tree: tree, Issue: issue, Phase: p, Steps: steps, Gates: gates, Documents: docs, Items: items}
	}
	add := func(variant string, reader claim.Role, news workflow.News, want map[string]string) {
		issue := news.Issue
		if news.Audience == record.AudienceArchitect {
			issue = news.Tree
		}
		want["issue"], want["tree"] = issue, tree
		b.taskText(r, texts.DesignNews, variant, reader, issue, workflow.RenderNews(news), want)
	}

	// A holder's news: the spec's sections at every place and its typed blocks but its checks, and
	// the Verification document's checks alone.
	{
		p := []phase.Phase{phase.Implementing, phase.Reviewing}[r.n-1]
		want := goldenWant("audience", texts.NewsToHolder, "phase", p,
			"sections[1].heading", headingA, "sections[2].after_heading", headingA, "sections[3].heading", headingB,
			"sections[3].nth", 2, "sections[4].nth", 1, "sections[5].unit", texts.NewsUnitOutsideSections,
			"blocks[0].id", idA, "blocks[2].type", texts.NewsBlockOther, "blocks[2].other_type", otherType, "blocks[3].id", idC,
			"blocks[3].item", 1)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-2), texts.NewsChangesListed)
		item(want, 1, record.DesignStepCriteria, checksDoc, checksArtifact, checksTo, fmt.Sprint(checksFrom), texts.NewsChangesListed)
		add("holder", phase.RoleFor(p), holder(r.key(71), p, specChanges(specTo-2), checksChanges()), want)
	}
	// A planner's news of the first approved version of the spec, beside a pair of the Verification
	// document Dispatch named nothing of.
	{
		first := change(record.DesignStepSpec, specArtifact, 0, specTo)
		named := change(record.DesignStepCriteria, checksArtifact, checksFrom, checksTo)
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Planning)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, "0", texts.NewsChangesNoPair)
		item(want, 1, record.DesignStepCriteria, checksDoc, checksArtifact, checksTo, fmt.Sprint(checksFrom), texts.NewsChangesNoneNamed)
		add("planner", claim.RolePlanner, holder(r.key(72), phase.Planning, first, named), want)
	}
	// The news of an issue its architect holds in accepting, beside a pair that holds the same text.
	{
		same := change(record.DesignStepCriteria, checksArtifact, checksFrom, checksTo)
		same.Changes.Unchanged = true
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Accepting, "sections[1].heading", headingA)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-1), texts.NewsChangesListed)
		item(want, 1, record.DesignStepCriteria, checksDoc, checksArtifact, checksTo, fmt.Sprint(checksFrom), texts.NewsChangesSameText)
		add("accepting", claim.RoleArchitect, holder(r.key(73), phase.Accepting, specChanges(specTo-1), same), want)
	}
	// The root architect's summary: its own pairs, one of decision blocks alone, then every member in
	// each state, one held in accepting by its own architect and one by the architect of the issue
	// above it.
	members := func(n int) []workflow.NewsMember {
		owed := []workflow.NewsChange{specChanges(specTo - 1)}
		return []workflow.NewsMember{
			{Issue: tree, Phase: phase.AwaitingPerson, State: workflow.NewsAwaitingPerson},
			{Issue: r.key(n + 1), Phase: phase.Implementing, State: workflow.NewsSentNow, Items: owed},
			{Issue: r.key(n + 2), Phase: phase.Testing, State: workflow.NewsWaitsForTask, Items: owed},
			{Issue: r.key(n + 3), Phase: phase.Reviewing, State: workflow.NewsAtNextStart, Items: owed},
			{Issue: r.key(n + 4), Phase: phase.Accepting, State: workflow.NewsYouAnswer, Items: owed},
			{Issue: r.key(n + 5), Phase: phase.Accepting, State: workflow.NewsSentNow, Items: owed, Architect: r.key(n + 5)},
			{Issue: r.key(n + 6), Phase: phase.Accepting, State: workflow.NewsWaitsForTask, Items: owed, Architect: r.key(n + 1)},
			{Issue: r.key(n + 7), Phase: phase.Testing, State: workflow.NewsWorkUnchanged},
			{Issue: r.key(n + 8), Phase: phase.Implementing, State: workflow.NewsNotConcerned},
			{Issue: r.key(n + 9), Phase: phase.Planning, State: workflow.NewsAlreadyTaken},
			{Issue: r.key(n + 10), Phase: phase.Admitted, State: workflow.NewsAdmitted},
			{Issue: r.key(n + 11), Phase: phase.Held, State: workflow.NewsStopped},
			{Issue: r.key(n + 12), Phase: phase.Done, State: workflow.NewsDone},
			{Issue: r.key(n + 13), Phase: phase.Implementing, State: workflow.NewsOutOfWorkflow},
			{Issue: r.key(n + 14), Phase: phase.Merging, State: workflow.NewsMerged},
			{Issue: r.key(n + 15), Phase: phase.Implementing, State: workflow.NewsSentNow, Opening: true},
			{Issue: r.key(n + 16), Phase: phase.Testing, State: workflow.NewsAtNextStart, Items: owed, Opening: true, Unnamed: r.n},
		}
	}
	{
		decisions := change(record.DesignStepCriteria, checksArtifact, checksFrom, checksTo)
		decisions.Changes.DecisionOnly = true
		news := workflow.News{Audience: record.AudienceArchitect, Tree: tree, Steps: steps, Gates: gates, Documents: docs,
			Items: []workflow.NewsChange{specChanges(specTo - 2), decisions}, Members: members(80)}
		want := goldenWant("audience", texts.NewsToRootArchitect, "items[0].yours", true, "items[2].yours", false,
			"members[0].issue", tree, "members[0].told", texts.NewsToldNothing, "members[0].why", texts.NewsWhyWaitsOnPerson,
			"members[0].opening", false, "members[0].unnamed", 0,
			"members[1].issue", r.key(81), "members[1].phase", phase.Implementing, "members[1].told", texts.NewsToldNow,
			"members[1].holder", claim.RoleImplementer, "members[5].architect_of", r.key(85), "members[6].architect_of", r.key(81),
			"members[14].why", texts.NewsWhyMerged, "members[15].issue", r.key(95), "members[15].opening", true, "members[15].unnamed", 0,
			"members[16].issue", r.key(96), "members[16].opening", true, "members[16].unnamed", r.n,
			"owed[0].issue", r.key(81), "owed[0].item", 2)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-2), texts.NewsChangesListed)
		item(want, 1, record.DesignStepCriteria, checksDoc, checksArtifact, checksTo, fmt.Sprint(checksFrom), texts.NewsChangesDecisionBlocksOnly)
		item(want, 2, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-1), texts.NewsChangesListed)
		add("summary", claim.RoleArchitect, news, want)
	}
	// That summary sent after a later version of the spec closed the gate again: the architect is
	// told the gate as it stands, and a member owed news is told nothing yet.
	{
		news := workflow.News{Audience: record.AudienceArchitect, Tree: tree, Steps: steps, Gates: closed, Documents: docs, OpeningOwed: true,
			Items: []workflow.NewsChange{specChanges(specTo - 2)},
			Members: []workflow.NewsMember{{Issue: tree, Phase: phase.AwaitingPerson, State: workflow.NewsAwaitingPerson},
				{Issue: r.key(75), Phase: phase.Testing, State: workflow.NewsGateClosed}}}
		want := goldenWant("audience", texts.NewsToRootArchitect, "gate", texts.GateClosed, "gate_step", "spec", "gate_document", specDoc,
			"gate_artifact", specArtifact, "gate_version", specTo+1, "merge", texts.MergeHeld,
			"members[1].issue", r.key(75), "members[1].told", texts.NewsToldNothingYet, "members[1].why", texts.NewsWhyGateClosed)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-2), texts.NewsChangesListed)
		add("summary-gate-closed", claim.RoleArchitect, news, want)
	}
	// A holder's news of an item whose review history could not be read: no version taken, and
	// Dispatch's reason.
	{
		reason := []string{"Dispatch failed 3 reads of the document's review history, the last with: dispatch: " + word("history", 30),
			"dispatch holds no such document: " + word("history", 40)}[r.n-1]
		unread := workflow.NewsChange{Step: record.DesignStepSpec, HistoryUnread: &record.DesignHistoryUnread{
			DesignHistoryQuestion: record.DesignHistoryQuestion{ArtifactID: specArtifact, To: specTo}, Reason: reason, ReadAt: r.at(60)}}
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Testing, "items[0].reason", reason,
			"items[0].reason@by", texts.ByDaemon, "items[0].reason@at", r.at(60))
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, "null", texts.NewsChangesNoPair)
		add("history-unread", claim.RoleTester, holder(r.key(76), phase.Testing, unread), want)
	}
	// A holder's news of a pair whose change list Dispatch could not answer for, with its reason.
	{
		reason := []string{"dispatch has no changes route: GET /api/v1/artifacts/" + specArtifact + "/changes answered 404 Not Found",
			"Dispatch failed 3 reads of it, the last with: dispatch: " + word("changes", 30)}[r.n-1]
		unavailable := change(record.DesignStepSpec, specArtifact, specTo-1, specTo)
		unavailable.Changes.State, unavailable.Changes.Reason, unavailable.Changes.ReadAt = record.DesignChangesUnavailable, reason, r.at(61)
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Merging, "items[0].reason", reason, "items[0].reason@by", texts.ByDaemon)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-1), texts.NewsChangesUnavailable)
		add("changes-unavailable", claim.RoleMerger, holder(r.key(77), phase.Merging, unavailable), want)
	}
	// A holder owed the opening: told it while the gate stands open, with a version of decision
	// blocks alone, and told the gate closed again when a later version closed it before the send.
	{
		decisions := change(record.DesignStepSpec, specArtifact, specTo-1, specTo)
		decisions.Changes.DecisionOnly = true
		news := holder(r.key(78), phase.Merging, decisions)
		news.OpeningOwed = true
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Merging, "gate", texts.GateOpened, "gate_step", "spec",
			"gate_document", specDoc, "gate_artifact", specArtifact, "gate_version", specTo, "merge", texts.MergeReleased)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-1), texts.NewsChangesDecisionBlocksOnly)
		add("gate-open", claim.RoleMerger, news, want)
	}
	{
		news := holder(r.key(79), phase.Implementing, specChanges(specTo-2))
		news.OpeningOwed, news.Gates = true, closed
		want := goldenWant("audience", texts.NewsToHolder, "phase", phase.Implementing, "gate", texts.GateClosed, "gate_step", "spec",
			"gate_document", specDoc, "gate_artifact", specArtifact, "gate_version", specTo+1, "merge", texts.MergeHeld)
		item(want, 0, record.DesignStepSpec, specDoc, specArtifact, specTo, fmt.Sprint(specTo-2), texts.NewsChangesListed)
		add("gate-closed", claim.RoleImplementer, news, want)
	}
}
