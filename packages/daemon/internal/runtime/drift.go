package runtime

import (
	"slices"
	"strings"
)

// DriftKind is one facet of a pod's configuration that PodDrift reads, and so one kind of reason a
// running pod no longer matches what a pod made now would be built from. Each kind is declared
// once (DriftDeclarations) as one the pod's roles keep working under, which drains, or one they
// cannot, which breaks. The daemon opens an update round over a pod at once for a kind that breaks,
// and never for a kind that drains: such a pod keeps working on its old configuration until a
// release of it (the idle release, a close) replaces it, and the next pod is built from the
// configuration then. A kind no declaration names breaks, since nothing shows its roles can keep
// working under it.
type DriftKind string

// The kinds of drift, one per facet PodDrift reads (the Sandbox runtime's podShapeDrift,
// leakCheckDrift, accountDrift and driftedInPod), one for a recorded shape that cannot be read, and
// one for the floor of the daemon contracts whose pods this daemon can still run roles in.
const (
	DriftImage            DriftKind = "image"
	DriftResources        DriftKind = "resources"
	DriftVolumes          DriftKind = "volumes"
	DriftMounts           DriftKind = "mounts"
	DriftDisk             DriftKind = "disk"
	DriftSandbox          DriftKind = "sandbox"
	DriftUnreadableShape  DriftKind = "unreadable_shape"
	DriftLeakCheck        DriftKind = "leak_check"
	DriftAccount          DriftKind = "service_account"
	DriftRoleOverlays     DriftKind = "role_overlays"
	DriftEnrollment       DriftKind = "enrollment"
	DriftExchangeIdentity DriftKind = "exchange_identity"
	DriftSessionStore     DriftKind = "session_store"
	DriftGhOwner          DriftKind = "gh_owner"
	DriftGhRepositoryDir  DriftKind = "gh_repository_dir"
	DriftGhNoPairs        DriftKind = "gh_no_pairs"
	DriftFloor            DriftKind = "floor"
)

// DriftDeclaration is one kind's answer, whether a pod's roles keep working under it, with the
// reason that answers it.
type DriftDeclaration struct {
	Kind   DriftKind
	Drains bool
	Reason string
}

// driftDeclarations is every kind's answer, each with its reason beside it. A kind drains where
// the old pod keeps what it was made with and its roles keep working, pushing and answering under
// it; a kind breaks where a role can lose a credential or a store it writes.
var driftDeclarations = []DriftDeclaration{
	{DriftImage, true, "A pod is built whole from one image, and its launcher, shim, workspace-init, " +
		"gh wrapper and Oh My Pi run that image's release for the pod's life. The stale-plugin lane " +
		"moves an Oh My Pi claim onto the daemon's plugins in the same pod, and a release whose daemon " +
		"can no longer serve an older pod's processes raises the floor (DriftFloor) instead."},
	{DriftResources, true, "The reservation bounds the pod's CPU and memory and nothing a role reads: " +
		"the roles keep the bound they started under, and the memory guard holds them to it."},
	{DriftVolumes, true, "An operator's volumes are the pod's own, fixed when it was made, and the old " +
		"pod keeps the ones it was made with. On 2026-11-23 one added operator volume and mount " +
		"opened 172 rounds over one project's pods, asking 595 claims to push and stop for a file " +
		"none of their work needed."},
	{DriftMounts, true, "An operator's mounts are the pod's own, fixed when it was made, and the old " +
		"pod keeps the ones it was made with, as it keeps its volumes."},
	{DriftDisk, true, "The disk's size bounds how much the workspace holds and nothing a role reads: " +
		"the old disk holds what it held, and the volume trimmer and its notices guard its fill."},
	{DriftSandbox, true, "The sandbox class is where the pod runs, fixed when it was made: the old pod " +
		"keeps the sandbox, the disk class and the metadata endpoint it was made with, and its roles " +
		"keep working in it."},
	{DriftUnreadableShape, false, "A recorded shape this daemon cannot read says nothing it can " +
		"compare, so nothing shows the pod's roles can keep working."},
	{DriftLeakCheck, true, "A pod keeps the leak check its install was made under, and a kept pod's " +
		"roles are told that digest (keptPod.checker), so every push stays checked."},
	{DriftAccount, false, "The ServiceAccount is the identity every call of the pod's roles carries, " +
		"so a pod on another account holds a grant the deployment has moved away from."},
	{DriftRoleOverlays, true, "A role's overlays are files its Oh My Pi reads when it starts: a running " +
		"role keeps the files it started with, and a role started in the kept pod starts on the ones " +
		"the pod was made with and can still push and answer."},
	{DriftEnrollment, false, "The secrets broker's enrollment is the token a role's agent-secrets " +
		"calls carry, and the shim refuses to start a role without its token file."},
	{DriftExchangeIdentity, false, "The exchange identity is the token a role trades for its " +
		"credentials, so a role started in a pod without the new one trades nothing."},
	{DriftSessionStore, false, "The session store is where each role writes its conversation, so a " +
		"pod on another store writes where the daemon no longer reads."},
	{DriftGhOwner, true, "An owner the daemon newly serves and the pod lacks leaves only the gh calls " +
		"for that owner without a token: every repository the pod already works keeps its token, " +
		"which the kubelet refreshes in place, and a directory the pod holds that the daemon no longer " +
		"serves costs nothing."},
	{DriftGhRepositoryDir, false, "A repository directory in the gh volume is the layout from before " +
		"contract 152 (version.go's entry 152): its token reaches that one repository where an owner's " +
		"directory reaches every repository of the installation, so the pod's pushes to the rest fail."},
	{DriftGhNoPairs, false, "A gh Secret projection with no pairs file is the layout from before " +
		"contract 152 (version.go's entry 152): its wrappers answer a moved repository with the old " +
		"owner's token, a push GitHub refuses, and the deployment repository records pods with no " +
		"renames file whose pushes failed with 403."},
	{DriftFloor, false, "A pod made by a daemon below the floor runs a launcher, a shim, a " +
		"workspace-init, a gh wrapper or an Oh My Pi this daemon can no longer serve."},
}

// DriftDeclarations is every kind's declaration, in its declared order.
func DriftDeclarations() []DriftDeclaration {
	return append([]DriftDeclaration(nil), driftDeclarations...)
}

// Drains is whether a pod's roles keep working under drift of kind k: true only for a kind
// declared to drain.
func (k DriftKind) Drains() bool {
	for _, d := range driftDeclarations {
		if d.Kind == k {
			return d.Drains
		}
	}
	return false
}

// DriftReason is one reason a pod has drifted: its kind, and the phrase an operator reads in a task
// and in a log line.
type DriftReason struct {
	Kind DriftKind
	Text string
}

// DriftReasons is every reason one read of a pod's drift gave.
type DriftReasons []DriftReason

// Breaks is whether any reason is of a kind that breaks.
func (rs DriftReasons) Breaks() bool {
	for _, r := range rs {
		if !r.Kind.Drains() {
			return true
		}
	}
	return false
}

// Kinds is each reason's kind, in order, once.
func (rs DriftReasons) Kinds() []DriftKind {
	var kinds []DriftKind
	for _, r := range rs {
		if !slices.Contains(kinds, r.Kind) {
			kinds = append(kinds, r.Kind)
		}
	}
	return kinds
}

// Texts is each reason's phrase, in order.
func (rs DriftReasons) Texts() []string {
	texts := make([]string, len(rs))
	for i, r := range rs {
		texts[i] = r.Text
	}
	return texts
}

// BreakingDrift starts each reason of a kind that breaks in the reasons a round records
// (DriftReasons.String, supervise.Claim.UpdateReasons), and no reason of a kind that drains carries
// it. A round records its reasons as one string at its open, so the mark is what tells a reader of
// that string a round over a drift that breaks the pod from one its roles could keep working
// under. Nothing in this package or internal/daemon reads it yet: dispatch://INFRA-3271's R7 is the
// reader (internal/daemon's roundBreaksPod in cybermonklabs/legion#5285, which defines the same
// constant with the same value; whichever of the two lands second keeps one).
const BreakingDrift = "[breaks the pod] "

// String is the reasons as a round's terms record them, each phrase in order, each reason of a kind
// that breaks starting with BreakingDrift.
func (rs DriftReasons) String() string {
	texts := rs.Texts()
	for i, r := range rs {
		if !r.Kind.Drains() {
			texts[i] = BreakingDrift + texts[i]
		}
	}
	return strings.Join(texts, ", ")
}
