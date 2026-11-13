package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What an operator does when an issue pod goes away is decided by three facts this release
// changed, and docs/kubernetes.md is where that operator reads them: which pods carry the
// extended-run-time annotation, what the daemon keeps of a workspace outside its pod, and which
// pod ends are continued uncharged. A document that still states the annotation as unconditional,
// or that names the copy without its period, sends the operator to look for a node of its own
// that no longer exists, or to wait for work that no copy holds.
//
// Each check reads one region of the document, bounded by its first and last words, and fails by
// naming the marker it could not find. That bound is the whole structure. The reclaim paragraph's
// own checks (reclaim_doc_test.go) were written after an earlier version of them passed over a
// paragraph that was wrong, because a word it searched for happened to appear elsewhere in a
// document of thousands of lines; a search of the whole file for "no death charged" would pass
// here the same way, with the annotation bullet saying the opposite of what it says now.
//
// Where a region is a list, it is read as a list: the two readings that earn an uncharged
// continue are two bullets, and the three kinds of end are three, each held to the facts of its
// own bullet. A fact proved from the region's flat text alone would let the deletion-timestamp
// reading borrow the cluster's three reasons, or let the close's bullet stand on a sentence
// written for the release.
//
// The annotation keys, the three cluster reasons and the reason that needs the condition come
// from the code they document (safeToEvictAnnotation, doNotDisruptAnnotation,
// clusterEndedReasons, evictedReason), never from a list written beside the check: a reason added
// to the set is then a reason the document must name, and nobody has to remember to edit this
// file. The two facts of the document that are internal/daemon's constants — the running copy's
// 5-minute period and the line a copy that was not stored writes — are checked there, against the
// constants themselves (internal/daemon's TestKubernetesDocStatesTheRunningCopysConstants).
//
// What these checks cannot do: they read what the document says, never whether it is true. A
// reader is still the only check on that.
const (
	annotationRuleStart = "the project controller's pod additionally carries"
	annotationRuleEnd   = "with no death charged"

	copyCadenceStart = "The daemon keeps a copy of each issue workspace's unpushed work outside the pod while the pod runs"
	copyCadenceEnd   = "and the next period tries again."

	copyKindsStart = "An issue therefore holds up to two kinds of copy"
	copyKindsEnd   = "and both stay through a linger and a park."

	podEndReadingsStart = "The daemon reads what ended each watched pod once"
	podEndReadingsEnd   = "would be relaunched for ever."

	endsApartStart = "Three ends differ in what reaches the issue's next pod"
	endsApartEnd   = "and each role's session row."

	karpenterStart = "`" + doNotDisruptAnnotation + "` is the AWS scheduler's annotation"
	karpenterEnd   = "and no repacking of its pods."
)

// docFact is one thing a region of docs/kubernetes.md must state: the fact a reader would lose,
// and the words that say it. The fact is what the failure reports, because a reader who meets
// this check has to know what to write, not only which string is missing.
type docFact struct {
	fact   string
	phrase string
}

// The issue pod carries cluster-autoscaler.kubernetes.io/safe-to-evict only while its reservation
// is Guaranteed (podKind.asksExtendedRunTime), and the "Anatomy of a Sandbox pod" bullet is where
// an operator reads that rule and what a Burstable issue pod gives up for it.
func TestKubernetesDocStatesWhichPodsCarryTheExtendedRunTimeAnnotation(t *testing.T) {
	const what = `docs/kubernetes.md's annotation bullet of "Anatomy of a Sandbox pod"`
	_, region := kubernetesDocRegion(t, what, annotationRuleStart, annotationRuleEnd)
	requireDocFacts(t, what, region, []docFact{
		{"the annotation the rule is about", safeToEvictAnnotation},
		{"that the controller's pod carries it whatever its reservation", "whatever its reservation"},
		{"the rule itself, which is the reservation", "an issue pod carries it only while its cpu request equals its cpu limit and its memory request equals its memory limit"},
		{"the reservation that rule names", "`Guaranteed`"},
		{"that a Burstable issue pod carries it not at all", "is `Burstable` and carries the annotation not at all"},
		{"what such a pod asks GKE Autopilot for", "no extended run time"},
		{"that such a pod shares a node", "shares a node"},
		{"what such a pod gives up", "the seven-day protection"},
		{"what such a pod keeps, and why", "copy of each issue workspace outside the pod"},
	})
}

// The copy's cadence: both triggers, one copy of an issue in flight, no turn waiting for one, the
// role it is asked of, where it is stored, and what a copy that fails costs. Its 5-minute bound and
// the line a copy that was not stored writes are internal/daemon's constants, checked there.
func TestKubernetesDocStatesTheWorkspaceCopyCadence(t *testing.T) {
	const what = `docs/kubernetes.md's cadence paragraphs of "The copy of a running pod's workspace"`
	_, region := kubernetesDocRegion(t, what, copyCadenceStart, copyCadenceEnd)
	requireDocFacts(t, what, region, []docFact{
		{"the first trigger, a turn's end", "the end of each turn of the issue"},
		{"that the period runs from the end of the copy before it", "from the end of the copy before it"},
		{"that one copy of an issue is in flight at a time", "One copy of an issue is in flight at a time"},
		{"that no turn waits for a copy", "No turn waits for a copy"},
		{"the role the copy is asked of", "the first live role of the issue that has a running process"},
		{"the call it is taken through, which an idle release makes too", "`BundleWorkspace`"},
		{"the table it is stored in", "workspace_running_copies"},
		{"that it is never stored in the idle release's row", "never in the `workspace_bundles` row"},
		{"that the line carries the reason by name", "with the reason by name"},
		{"that a failed copy leaves the issue the copy it had", "the issue keeps the copy it had"},
	})
}

// The two kinds of copy: which a new pod restores, what becomes of the older, which of the two
// holds a pod, and how the operator's psql recipe reads the running copy's own table.
func TestKubernetesDocStatesTheTwoKindsOfWorkspaceCopy(t *testing.T) {
	const what = `docs/kubernetes.md's two-kinds paragraph of "The copy of a running pod's workspace"`
	_, region := kubernetesDocRegion(t, what, copyKindsStart, copyKindsEnd)
	requireDocFacts(t, what, region, []docFact{
		{"the release bundle's table", "workspace_bundles"},
		{"the running copy's table", "workspace_running_copies"},
		{"which copy a new pod restores", "is sent the newer of the two"},
		{"that the restore happens before any role starts", "before any role starts"},
		{"what becomes of the older copy", "The older is closed unsent"},
		{"that an unrestored release bundle holds the issue's pod", "a release bundle no pod has restored holds the issue's pod"},
		{"that a running copy holds no pod", "a running copy holds no pod at all"},
		{"that an issue holding a running copy is still released when it is idle", "released when it is idle"},
		{"the psql recipe for the running copy's table", "select encode(bundle, 'base64') from workspace_running_copies"},
		{"that the column is bundle there too", "the column is `bundle` there too"},
	})
}

// The two readings that earn an uncharged continue, the end reasons the second covers, the reason
// that needs the condition, the run of such ends that is charged all the same, and the ends that
// are still charged as a death.
func TestKubernetesDocStatesWhichPodEndsAreContinuedUnchargedAndWhichIsCharged(t *testing.T) {
	const what = `docs/kubernetes.md's "An end from outside the pod" paragraph`
	lines, region := kubernetesDocRegion(t, what, podEndReadingsStart, podEndReadingsEnd)
	requireDocFacts(t, what, region, []docFact{
		{"the line one pod end writes", podEndLine},
		{"that each role is continued in its own session", "in that role's own session"},
		{"that neither a death nor a launch failure is charged", "charges no death and no launch failure"},
		{"that each role is sent the task it owed again", "sends each role the task it owed again"},
		{"that the third such end in a row is charged", "The third such end in a row is charged all the same"},
		{"that only a completed turn between the ends keeps them apart", "with no turn of that claim completed between those ends"},
		{"the limit the run is counted to", "`launch_failure_limit`"},
		{"why the condition cannot answer that run", "can drive its own node into pressure"},
		{"that a completed turn starts the count again", "A completed turn starts the count again"},
		{"where the count is kept, so a restart keeps it", "`claims.pod_ends_not_ours`"},
		{"the end that is still charged", "An end of neither reading is charged as a death"},
		{"that the kubelet's eviction with no condition is charged", "`" + evictedReason + "` with no `DisruptionTarget` condition is such an end"},
		{"the three limits that eviction is for", "its own ephemeral-storage limit, a container's ephemeral-storage limit or an `emptyDir`'s `sizeLimit`"},
		{"a reason outside the three, which is charged", "UnexpectedAdmissionError"},
	})

	bullets := regionBullets(lines)
	if len(bullets) != 2 {
		t.Fatalf("%s lists %d readings, want the 2 that earn an uncharged continue: %q", what, len(bullets), bullets)
	}
	if deletion, ok := bulletNaming(t, what, bullets, "deletion timestamp"); ok {
		requireDocFacts(t, what+", its deletion-timestamp bullet", deletion, []docFact{
			{"that no act of the daemon's own accounts for the deletion", "no act of the daemon's own accounts for"},
		})
	}
	cluster, ok := bulletNaming(t, what, bullets, "DisruptionTarget")
	if !ok {
		return
	}
	// The reasons come from the code this bullet documents, so a reason added to the set is a
	// reason the document must name from that moment, with nobody reminded to edit either file.
	for _, reason := range clusterEndedReasons {
		if !strings.Contains(cluster, "`"+reason+"`") {
			t.Errorf("%s's bullet for what the cluster says ended a pod does not name the reason %q, which is one of the "+
				"reasons that earn an uncharged continue (clusterEndedReasons): an operator reading that reason in a pod's "+
				"status is left to guess whether the roles were charged for it. The bullet is: %q", what, reason, cluster)
		}
	}
	requireDocFacts(t, what+", its DisruptionTarget bullet", cluster, []docFact{
		{"that the kubelet's eviction reason earns the continue only with the condition",
			"`" + evictedReason + "` is such an end only with the condition"},
	})
}

// What a pod loses and what it keeps at each kind of end, read as the three bullets it is: a
// reader tells an end the cluster made from an idle release and from an issue's close by them.
func TestKubernetesDocStatesWhatEachKindOfPodEndLosesAndKeeps(t *testing.T) {
	const what = `docs/kubernetes.md's "What each kind of end loses and keeps" paragraph`
	lines, _ := kubernetesDocRegion(t, what, endsApartStart, endsApartEnd)
	bullets := regionBullets(lines)
	if len(bullets) != 3 {
		t.Fatalf("%s lists %d kinds of end, want the 3 a reader tells apart: %q", what, len(bullets), bullets)
	}
	for _, end := range []struct {
		name  string
		facts []docFact
	}{
		{"an end the cluster made", []docFact{
			{"that the disk goes with the pod", "The disk goes with the pod"},
			{"that the newer saved copy comes back", "the newer of the issue's two saved copies"},
			{"that the roles keep their sessions and are charged nothing", "charged no death"},
			{"what no copy holds", "What no copy holds goes with the disk"},
		}},
		{"an idle release", []docFact{
			{"that the bundle is saved before any role stops", "saves the release bundle first"},
			{"that the workspace comes back whole", "comes back whole at the next wake"},
			{"what each role keeps", "suspended `released` until a wake"},
			{"that the issue keeps its slot", "The issue keeps its admission slot"},
		}},
		{"an issue's close", []docFact{
			{"that both saved copies are deleted", "both saved copies of the workspace are deleted"},
			{"that nothing of the disk is kept", "nothing of that disk is kept"},
			{"what the issue keeps", "What the issue keeps is what it pushed"},
		}},
	} {
		if bullet, ok := bulletNaming(t, what, bullets, end.name); ok {
			requireDocFacts(t, what+", its bullet for "+end.name, bullet, end.facts)
		}
	}
}

// Karpenter's annotation is unchanged by this release, which is what tells an AWS deployment that
// it gains the copy and the uncharged continue and pays no repacking for them.
func TestKubernetesDocStatesKarpentersAnnotationStaysOnEveryPod(t *testing.T) {
	const what = `docs/kubernetes.md's AWS paragraph of "The copy of a running pod's workspace"`
	_, region := kubernetesDocRegion(t, what, karpenterStart, karpenterEnd)
	requireDocFacts(t, what, region, []docFact{
		{"the annotation that stays", doNotDisruptAnnotation},
		{"that every pod carries it whatever its reservation", "every Legion pod carries it whatever its reservation"},
		{"that Karpenter disrupts no node a Legion pod runs on", "no node a Legion pod runs on"},
		{"what an AWS deployment gains", "gains the copy and the uncharged resume"},
		{"what it does not pay for", "no repacking"},
	})
}

// kubernetesDocRegion is the part of docs/kubernetes.md between the first and the last words of
// one region, as its lines and as one flattened string. Every check reads a region and never the
// document: a phrase found anywhere in its 6000 lines would answer a check about a paragraph that
// says the opposite, which is the miss these checks exist against.
//
// It fails by name rather than skipping or passing. A marker that cannot be found is a region
// that was reworded, and a check that cannot find what it bounds is a check that stopped
// checking, which reads exactly like a pass.
//
// The markers are prose and the document is hard-wrapped, so they straddle line ends: they are
// found in a flattened copy and carried back to the lines they cover, which is what a list read
// as a list needs (regionBullets). The last line is the one the end marker finishes on, so a
// marker that wraps takes its whole last line with it.
func kubernetesDocRegion(t *testing.T, what, start, end string) ([]string, string) {
	t.Helper()
	// The document is at the repository root, five directories above this package.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	flat, lineOf := flattenKeepingLines(lines)
	startOffset := strings.Index(flat, start)
	if startOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q, so %s cannot be found: the region was reworded or removed, and this "+
			"check stops checking the moment it cannot find it", start, what)
	}
	if strings.Contains(flat[startOffset+len(start):], start) {
		t.Fatalf("docs/kubernetes.md has %q more than once, so it no longer says where %s starts: the check would read "+
			"whichever comes first and cannot tell that it read the wrong one", start, what)
	}
	endOffset := strings.Index(flat[startOffset:], end)
	if endOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q after %q, so the end of %s moved: an unbounded search would pass on a "+
			"sentence anywhere in the document", end, start, what)
	}
	last := startOffset + endOffset + len(end) - 1
	return lines[lineOf[startOffset] : lineOf[last]+1], flat[startOffset : last+1]
}

// requireDocFacts holds text, which is one region or one bullet of it, to every fact it must
// state. Each miss is its own failure naming the fact, since a reader who has to fix this knows
// what to write from the fact and not from the string.
func requireDocFacts(t *testing.T, what, text string, facts []docFact) {
	t.Helper()
	for _, f := range facts {
		if !strings.Contains(text, f.phrase) {
			t.Errorf("%s does not state %s: it carries no %q. A reader of that part of the document is left to guess, "+
				"and a sentence elsewhere in the document is no answer, since this is the part they read", what, f.fact, f.phrase)
		}
	}
}

// regionBullets reads the markdown bullets of a region, each as its whole content: the `- ` line
// and the indented lines under it, joined. A fact is then held to the bullet that carries it,
// which is what stops the sentence written for one kind of end from standing in for another's.
func regionBullets(region []string) []string {
	var bullets []string
	// The loop advances i itself over a wrapped bullet's continuation lines, so it stays explicit.
	for i := 0; i < len(region); i++ {
		line := strings.TrimRight(region[i], " ")
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		for i+1 < len(region) && strings.HasPrefix(region[i+1], " ") && strings.TrimSpace(region[i+1]) != "" {
			i++
			bullet += " " + strings.TrimSpace(region[i])
		}
		bullets = append(bullets, bullet)
	}
	return bullets
}

// bulletNaming is the one bullet of a region that names what the check is about. Two bullets
// naming it, or none, is a failure of its own: the facts below it would otherwise be read against
// a bullet nobody chose.
func bulletNaming(t *testing.T, what string, bullets []string, name string) (string, bool) {
	t.Helper()
	var found []string
	for _, bullet := range bullets {
		if strings.Contains(bullet, name) {
			found = append(found, bullet)
		}
	}
	if len(found) != 1 {
		t.Errorf("%s has %d bullets naming %q, want exactly 1: the facts a reader takes from that bullet have no one "+
			"bullet to be read against. The bullets are: %q", what, len(found), name, bullets)
		return "", false
	}
	return found[0], true
}
