package sandbox

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testdoc"
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

// The issue pod carries cluster-autoscaler.kubernetes.io/safe-to-evict only while its reservation
// is Guaranteed (podKind.asksExtendedRunTime), and the "Anatomy of a Sandbox pod" bullet is where
// an operator reads that rule and what a Burstable issue pod gives up for it.
func TestKubernetesDocStatesWhichPodsCarryTheExtendedRunTimeAnnotation(t *testing.T) {
	const what = `docs/kubernetes.md's annotation bullet of "Anatomy of a Sandbox pod"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", what, annotationRuleStart, annotationRuleEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the annotation the rule is about", Phrase: safeToEvictAnnotation},
		{Fact: "that the controller's pod carries it whatever its reservation", Phrase: "whatever its reservation"},
		{Fact: "the rule itself, which is the reservation", Phrase: "an issue pod carries it only while its cpu request equals its cpu limit and its memory request equals its memory limit"},
		{Fact: "the reservation that rule names", Phrase: "`Guaranteed`"},
		{Fact: "that a Burstable issue pod carries it not at all", Phrase: "is `Burstable` and carries the annotation not at all"},
		{Fact: "what such a pod asks GKE Autopilot for", Phrase: "no extended run time"},
		{Fact: "that such a pod shares a node", Phrase: "shares a node"},
		{Fact: "what such a pod gives up", Phrase: "the seven-day protection"},
		{Fact: "what such a pod keeps, and why", Phrase: "copy of each issue workspace outside the pod"},
	})
}

// The copy's cadence: both triggers, one copy of an issue in flight, no turn waiting for one, the
// role it is asked of, where it is stored, and what a copy that fails costs. Its 5-minute bound and
// the line a copy that was not stored writes are internal/daemon's constants, checked there.
func TestKubernetesDocStatesTheWorkspaceCopyCadence(t *testing.T) {
	const what = `docs/kubernetes.md's cadence paragraphs of "The copy of a running pod's workspace"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", what, copyCadenceStart, copyCadenceEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the first trigger, a turn's end", Phrase: "the end of each turn of the issue"},
		{Fact: "that the period runs from the end of the copy before it", Phrase: "from the end of the copy before it"},
		{Fact: "that one copy of an issue is in flight at a time", Phrase: "One copy of an issue is in flight at a time"},
		{Fact: "that no turn waits for a copy", Phrase: "No turn waits for a copy"},
		{Fact: "the role the copy is asked of", Phrase: "the first live role of the issue that has a running process"},
		{Fact: "the call it is taken through, which an idle release makes too", Phrase: "`BundleWorkspace`"},
		{Fact: "the table it is stored in", Phrase: "workspace_running_copies"},
		{Fact: "that it is never stored in the idle release's row", Phrase: "never in the `workspace_bundles` row"},
		{Fact: "that the line carries the reason by name", Phrase: "with the reason by name"},
		{Fact: "that a failed copy leaves the issue the copy it had", Phrase: "the issue keeps the copy it had"},
	})
}

// The two kinds of copy: which a new pod restores, what becomes of the older, which of the two
// holds a pod, and how the operator's psql recipe reads the running copy's own table.
func TestKubernetesDocStatesTheTwoKindsOfWorkspaceCopy(t *testing.T) {
	const what = `docs/kubernetes.md's two-kinds paragraph of "The copy of a running pod's workspace"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", what, copyKindsStart, copyKindsEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the release bundle's table", Phrase: "workspace_bundles"},
		{Fact: "the running copy's table", Phrase: "workspace_running_copies"},
		{Fact: "which copy a new pod restores", Phrase: "is sent the newer of the two"},
		{Fact: "that the restore happens before any role starts", Phrase: "before any role starts"},
		{Fact: "what becomes of the older copy", Phrase: "The older is closed unsent"},
		{Fact: "that an unrestored release bundle holds the issue's pod", Phrase: "a release bundle no pod has restored holds the issue's pod"},
		{Fact: "that a running copy holds no pod", Phrase: "a running copy holds no pod at all"},
		{Fact: "that an issue holding a running copy is still released when it is idle", Phrase: "released when it is idle"},
		{Fact: "the psql recipe for the running copy's table", Phrase: "select encode(bundle, 'base64') from workspace_running_copies"},
		{Fact: "that the column is bundle there too", Phrase: "the column is `bundle` there too"},
	})
}

// The two readings that earn an uncharged continue, the end reasons the second covers, the reason
// that needs the condition, the run of such ends that is charged all the same, and the ends that
// are still charged as a death.
func TestKubernetesDocStatesWhichPodEndsAreContinuedUnchargedAndWhichIsCharged(t *testing.T) {
	const what = `docs/kubernetes.md's "An end from outside the pod" paragraph`
	lines, region := testdoc.Region(t, "docs/kubernetes.md", what, podEndReadingsStart, podEndReadingsEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the line one pod end writes", Phrase: podEndLine},
		{Fact: "that each role is continued in its own session", Phrase: "in that role's own session"},
		{Fact: "that neither a death nor a launch failure is charged", Phrase: "charges no death and no launch failure"},
		{Fact: "that each role is sent the task it owed again", Phrase: "sends each role the task it owed again"},
		{Fact: "that the third such end in a row is charged", Phrase: "The third such end in a row is charged all the same"},
		{Fact: "that only a completed turn between the ends keeps them apart", Phrase: "with no turn of that claim completed between those ends"},
		{Fact: "the limit the run is counted to", Phrase: "`launch_failure_limit`"},
		{Fact: "why the condition cannot answer that run", Phrase: "can drive its own node into pressure"},
		{Fact: "that a completed turn starts the count again", Phrase: "A completed turn starts the count again"},
		{Fact: "where the count is kept, so a restart keeps it", Phrase: "`claims.pod_ends_not_ours`"},
		{Fact: "the end that is still charged", Phrase: "An end of neither reading is charged as a death"},
		{Fact: "that the kubelet's eviction with no condition is charged", Phrase: "`" + evictedReason + "` with no `DisruptionTarget` condition is such an end"},
		{Fact: "the three limits that eviction is for", Phrase: "its own ephemeral-storage limit, a container's ephemeral-storage limit or an `emptyDir`'s `sizeLimit`"},
		{Fact: "a reason outside the three, which is charged", Phrase: "UnexpectedAdmissionError"},
	})

	bullets := testdoc.RegionBullets(lines)
	if len(bullets) != 2 {
		t.Fatalf("%s lists %d readings, want the 2 that earn an uncharged continue: %q", what, len(bullets), bullets)
	}
	if deletion, ok := testdoc.BulletNaming(t, what, bullets, "deletion timestamp"); ok {
		testdoc.RequireDocFacts(t, what+", its deletion-timestamp bullet", deletion, []testdoc.Fact{
			{Fact: "that no act of the daemon's own accounts for the deletion", Phrase: "no act of the daemon's own accounts for"},
		})
	}
	cluster, ok := testdoc.BulletNaming(t, what, bullets, "DisruptionTarget")
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
	testdoc.RequireDocFacts(t, what+", its DisruptionTarget bullet", cluster, []testdoc.Fact{
		{Fact: "that the kubelet's eviction reason earns the continue only with the condition",
			Phrase: "`" + evictedReason + "` is such an end only with the condition"},
	})
}

// What a pod loses and what it keeps at each kind of end, read as the three bullets it is: a
// reader tells an end the cluster made from an idle release and from an issue's close by them.
func TestKubernetesDocStatesWhatEachKindOfPodEndLosesAndKeeps(t *testing.T) {
	const what = `docs/kubernetes.md's "What each kind of end loses and keeps" paragraph`
	lines, _ := testdoc.Region(t, "docs/kubernetes.md", what, endsApartStart, endsApartEnd)
	bullets := testdoc.RegionBullets(lines)
	if len(bullets) != 3 {
		t.Fatalf("%s lists %d kinds of end, want the 3 a reader tells apart: %q", what, len(bullets), bullets)
	}
	for _, end := range []struct {
		name  string
		facts []testdoc.Fact
	}{
		{"an end the cluster made", []testdoc.Fact{
			{Fact: "that the disk goes with the pod", Phrase: "The disk goes with the pod"},
			{Fact: "that the newer saved copy comes back", Phrase: "the newer of the issue's two saved copies"},
			{Fact: "that the roles keep their sessions and are charged nothing", Phrase: "charged no death"},
			{Fact: "what no copy holds", Phrase: "What no copy holds goes with the disk"},
		}},
		{"an idle release", []testdoc.Fact{
			{Fact: "that the bundle is saved before any role stops", Phrase: "saves the release bundle first"},
			{Fact: "that the workspace comes back whole", Phrase: "comes back whole at the next wake"},
			{Fact: "what each role keeps", Phrase: "suspended `released` until a wake"},
			{Fact: "that the issue keeps its slot", Phrase: "The issue keeps its admission slot"},
		}},
		{"an issue's close", []testdoc.Fact{
			{Fact: "that both saved copies are deleted", Phrase: "both saved copies of the workspace are deleted"},
			{Fact: "that nothing of the disk is kept", Phrase: "nothing of that disk is kept"},
			{Fact: "what the issue keeps", Phrase: "What the issue keeps is what it pushed"},
		}},
	} {
		if bullet, ok := testdoc.BulletNaming(t, what, bullets, end.name); ok {
			testdoc.RequireDocFacts(t, what+", its bullet for "+end.name, bullet, end.facts)
		}
	}
}

// Karpenter's annotation is unchanged by this release, which is what tells an AWS deployment that
// it gains the copy and the uncharged continue and pays no repacking for them.
func TestKubernetesDocStatesKarpentersAnnotationStaysOnEveryPod(t *testing.T) {
	const what = `docs/kubernetes.md's AWS paragraph of "The copy of a running pod's workspace"`
	_, region := testdoc.Region(t, "docs/kubernetes.md", what, karpenterStart, karpenterEnd)
	testdoc.RequireDocFacts(t, what, region, []testdoc.Fact{
		{Fact: "the annotation that stays", Phrase: doNotDisruptAnnotation},
		{Fact: "that every pod carries it whatever its reservation", Phrase: "every Legion pod carries it whatever its reservation"},
		{Fact: "that Karpenter disrupts no node a Legion pod runs on", Phrase: "no node a Legion pod runs on"},
		{Fact: "what an AWS deployment gains", Phrase: "gains the copy and the uncharged resume"},
		{Fact: "what it does not pay for", Phrase: "no repacking"},
	})
}
