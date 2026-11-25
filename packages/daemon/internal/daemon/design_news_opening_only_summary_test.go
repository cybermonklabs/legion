package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A member of the tree whose holder row owes the gate's opening and no item is told by a task of
// its own: record.OwedNews at main 8966cf45^2 (record/design_news.go) owes it "that opening, with
// no item where it is owed none", since "no role message told this opening, and a task of its own
// does (MemberNews with no Items)". Main's summary said of such a member only what was true,
// "owed news of the approved design" (workflow/design_news.go, memberWords), and main's
// architect prompt "An issue owed news is told by a task now, by a task that waits until its
// holder takes the task it already holds, or at the next start of its phase" (go/architect-common.md).
//
// At the head the member's row names how it is told and no `owed` row, and the root architect's
// prompt reads that as "an issue told by a task with no row of `owed` is owed news the daemon
// cannot name yet". For this member that is a wrong fact: nothing waits to be named, and the only
// news it is owed is the opening. dispatch://INFRA-175's R5, which dispatch://INFRA-3686's R8
// keeps, has "The root architect's message names each issue with what changed for it", and R2 has
// a text state "the state that the reader acts on that its event has". So the summary, read with
// the prompt its reader loads, must not say that such a member is owed news the daemon cannot name
// yet: either the member's row names the opening, or the prompt's sentence on a member with no
// `owed` row names the opening among its cases.
func TestTheRootArchitectsSummaryOfAMemberOwedOnlyTheOpeningSaysNoNewsWaitsToBeNamed(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.told(reconcileChild, record.AudienceHolder, 5)
	r.owesOpening(reconcileChild, record.AudienceHolder)
	r.record(reconcilePair, "")
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.holder(reconcileChild, claim.RoleImplementer, supervise.StateWorking, "", 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	var summary *entryStartRow
	starts := r.newsStarts()
	for i, start := range starts {
		if start.row.Issue == reconcileRoot && start.request.Role == claim.RoleArchitect {
			summary = &starts[i]
		}
	}
	if summary == nil {
		t.Fatalf("the news-only starts = %v, want the root architect's summary", startsTo(starts))
	}
	machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, summary.row); err != nil {
		t.Fatalf("run the root architect's news-only start: %v", err)
	}
	task, _ := pendingNews(t, machine)
	t.Logf("the root architect's task:\n%s", task)
	rec := newsRecordOf(t, task, "root_architect", reconcileRoot)

	line := rec.member(reconcileChild)
	if line == "" || strings.Contains(line, "told=nothing") || strings.Contains(line, "told=not_yet") {
		t.Fatalf("the summary's row for %s = %q, want it told by a task: its holder row owes the opening", reconcileChild, line)
	}
	for _, owed := range rec.owed() {
		if strings.HasPrefix(owed, reconcileChild+": ") {
			t.Fatalf("the summary owes %s an item (%q), want none: its holder took version 5", reconcileChild, owed)
		}
	}
	rowNamesOpening := false
	for _, row := range rec.rows["members"] {
		if row["issue"] != reconcileChild {
			continue
		}
		for column, value := range row {
			if strings.Contains(column, "opening") && value != "" && value != "false" {
				rowNamesOpening = true
			}
		}
	}

	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(goPrompts), "architect-common.md"))
	if err != nil {
		t.Fatalf("read the root architect's prompt part: %v", err)
	}
	var reading string
	for _, paragraph := range strings.Split(string(body), "\n") {
		if !strings.Contains(paragraph, "`audience: root_architect`") {
			continue
		}
		for _, sentence := range strings.SplitAfter(paragraph, ". ") {
			if strings.Contains(sentence, "no row of `owed`") {
				reading = sentence
			}
		}
	}
	if reading == "" {
		t.Fatalf("go/architect-common.md says nothing of a member told by a task with no row of `owed`")
	}
	if strings.Contains(reading, "cannot name yet") && !strings.Contains(reading, "opening") && !rowNamesOpening {
		t.Errorf("the summary's row %q has no `owed` row, and the root architect's prompt reads it as %q; "+
			"but %s is owed the gate's opening alone, which the daemon names, and nothing waits to be named", line, strings.TrimSpace(reading), reconcileChild)
	}
}
