package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var (
	_ runtime.IssuePodReplacer = (*Runtime)(nil)
	_ runtime.ActReader        = (*Runtime)(nil)
)

// A running issue pod whose configuration has drifted is not replaced where it is found: every
// role of the issue runs in that one pod, and ending it ends each of their processes mid-turn and
// deletes the disk they work on, with everything unpushed on it. The daemon opens an update round
// over such a pod instead. The round asks each role in it to do the steps of its update message
// (supervise.UpdateTask) and answer (api's claims/ready-for-update), and replaces the pod once
// every asked role has answered, no turn runs, no take whose turn ended still owes its report and
// no claim's activity report holds it (internal/daemon's roundEnd.holding), or at the round's bound
// (pod_update_grace_minutes) whatever the roles answered and whatever runs, a holding suspension
// excepted. PodDrift is what the round is opened on, and ReplacePod what ends the round, or
// ReplaceIssuePod where no role process is left in the pod to name it through; the round itself,
// its ask and its sweep are the daemon's (internal/daemon).
//
// A launch never replaces a live pod a round covers for one of these reasons (relaunch,
// roundCovers): it keeps the pod and starts its role in it, since the round, not the launch, is
// what ends the roles already running there. The one exception is the round's own: where the
// round over the pod is over and no role process runs in it (Options.RoundPodEnds), the launch
// replaces the pod as that round's replacement, under the round's mark, rather than start a role
// the round would end a moment later. A pod no round covers has no such round to wait for, so its
// launch replaces it for every kind of drift, as every launch did before the round existed — save
// a launch installing a stale-plugin pair, which replaces such a pod for its shape only where the
// pod was built on another image (podImageDrift).

// roundCovers is whether an update round can be opened over a pod of kind: the one predicate that
// says who replaces such a pod when it has drifted, read by both sides of that answer. PodDrift
// answers nothing for a pod it excludes, so no round is ever opened over one; a launch into a live
// pod it excludes replaces that pod for the facets its roles' addresses carry, for the
// ServiceAccount it runs as and for the shape its template was built from (relaunch, movedInPod,
// accountDrift, shapeDrift), where a launch into a pod it covers keeps the pod — for the account
// and the image as for every other kind of drift — and leaves it to its round, unless that round
// is over with no role process in the pod, when the launch makes the round's own replacement
// (Options.RoundPodEnds). A launch installing a stale-plugin pair reads one key of an excluded
// pod's shape alone, the image (podImageDrift): it replaces a controller pod built on another
// image, and keeps a controller pod whose other facets alone drifted — its reservation, an
// operator volume or mount, its disk — and installs the pair into it. A drifted pod a round
// covers, every issue pod, it keeps as every launch does, whatever of the shape moved, the image
// included, and installs the pair into it; where that pod's round is over with no role process
// in it, it makes the round's replacement as every launch does, and installs nothing.
//
// A round covers every issue pod and nothing else. The project controller's pod is the one kind it
// excludes: the controller holds no issue workspace and has nothing to push, so there is nobody in
// it to task and no disk to protect, and a drift neither its launch nor a round replaced would
// leave it in the pod it was created in for ever — with no broker, so the token its slack_post is
// granted never reaches it, with none of the files role_config_files.controller names, and writing
// its session against the store that pod projects, while nothing said so.
func roundCovers(kind podKind) bool {
	_, controller := kind.(controllerPod)
	return !controller
}

// podShapeAnnotation records on a pod template's metadata the configuration the pod was built
// from (podShape). It is written where the template is built (podTemplate) and reaches the cluster
// with it, so every write of a template records the shape that template has, and no later sweep
// has to rebuild a launch to learn it: a launch's inputs are an issue's state and freshly minted
// GitHub App tokens, which a sweep over every pod of the fleet must not mint.
const podShapeAnnotation = "legion.dev/pod-shape"

// podShapeRecord is what podShapeAnnotation holds: the configuration a pod is built from that no
// running pod can be changed to carry, so a pod whose own differs from the runtime's now must be
// replaced to run as a pod created now does. Each field is the operator's or the deployment's own
// value, never the daemon's own derivation of it, which the daemon changes with its own releases:
// volumes and mounts are runtime.kubernetes.pod's alone (Options.Pod), and resources the pod
// kind's one reservation. Sandbox is where a microVM pod runs (sandboxShape), and nil for a gVisor
// pod, which so records exactly what it recorded before the sandbox setting existed.
//
// The field order is fixed and encoding/json's own: Go sorts a map's keys, so a ResourceList
// encodes the same whatever order it was built in, and a resource.Quantity marshals as its own
// canonical string. Two equal configurations must never encode differently — every pod of the
// fleet would read as drifted at once — which TestPodShapeEncodesEqualConfigurationsIdentically
// holds this to.
type podShapeRecord struct {
	Image     string                      `json:"image"`
	Resources corev1.ResourceRequirements `json:"resources"`
	Volumes   []corev1.Volume             `json:"volumes,omitempty"`
	Mounts    []corev1.VolumeMount        `json:"mounts,omitempty"`
	Disk      string                      `json:"disk"`
	Sandbox   *sandboxShape               `json:"sandbox,omitempty"`
}

// sandboxShape is the sandbox a microVM pod runs in, as its shape records it: the RuntimeClass
// (microVMRuntimeClass), the ComputeClass whose nodes run it, the StorageClass its disk is on
// (diskSpec), whether it keeps its volume (keptVolume), and the metadata endpoint its launcher
// serves, nil for none — each runtime.kubernetes.sandbox.microvm's own (MicroVM). The StorageClass
// is a microVM issue pod's alone: the controller's disk stays on runtime.kubernetes.storage_class
// in either sandbox (diskSpec), so the controller's shape records none, and the key is left out of
// its encoding (omitempty), which changes no issue pod's, whose class is never empty (New refuses
// an empty one while an issue pod runs in a microVM). A gVisor pod records none
// (podShapeRecord.Sandbox), so every gVisor shape encodes byte for byte as it did before the
// setting existed and the setting's arrival opens no round over a gVisor pod, and the gVisor pods'
// storage class stays out of every shape as it always was. KeepVolume is written only when it is
// true, so a shape with keep_volume off, and the controller's, which ignores the setting
// (keptVolume), encodes byte for byte as it did before that setting existed. A change of the
// microVM block's storage class, keep_volume or metadata endpoint opens a round over each microVM
// issue pod and no other, and a change of an issue's sandbox one over that issue's pod, from null
// to a microVM or back; a change of the block's metadata endpoint or compute class, or of the
// project's type, has the controller's next launch that installs nothing replace its pod, and a
// change of the block's storage class or keep_volume alone does not (relaunch, roundCovers).
type sandboxShape struct {
	RuntimeClass string                `json:"runtime_class"`
	ComputeClass string                `json:"compute_class"`
	StorageClass string                `json:"storage_class,omitempty"`
	KeepVolume   bool                  `json:"keep_volume,omitempty"`
	Metadata     *sandboxMetadataShape `json:"metadata,omitempty"`
}

// sandboxMetadataShape is the metadata endpoint a microVM pod's launcher serves, as its shape
// records it (MetadataEndpoint).
type sandboxMetadataShape struct {
	WorkloadPool     string `json:"workload_pool"`
	IdentityProvider string `json:"identity_provider"`
}

// podShape is the configuration a pod of kind is built from now, as its template's annotation
// records it: the image every container runs, the kind's own reservation (podKind.resources), the
// operator's volumes and mounts, the size of the pod's disk, and, for a pod that runs in a
// microVM, where that microVM runs and whether the pod keeps its volume (microVM, launch.microVM;
// nil for a gVisor pod, which records no sandbox), its disk's class an issue pod's alone
// (microVMDiskClass). An encoding failure is unreachable — every field is an API object, a string
// or a bool — and is logged and recorded as no shape, compared by image alone.
//
// Every field is settled when the runtime is built and never written again, so the runtime calls
// this once per kind and sandbox, as it is built, and keeps what it answers (podShapes,
// r.podShapes): the pod template that records a shape and the sweep that compares one read that
// string.
func podShape(r *Runtime, kind podKind, microVM *MicroVM) string {
	var where *sandboxShape
	if microVM != nil {
		where = &sandboxShape{
			RuntimeClass: microVMRuntimeClass, ComputeClass: microVM.ComputeClass, StorageClass: microVMDiskClass(kind, microVM),
			KeepVolume: keptVolume(kind, microVM),
		}
		if endpoint := microVM.Metadata; endpoint != nil {
			where.Metadata = &sandboxMetadataShape{WorkloadPool: endpoint.WorkloadPool, IdentityProvider: endpoint.IdentityProvider}
		}
	}
	encoded, err := json.Marshal(podShapeRecord{
		Image:     r.image,
		Resources: kind.resources(r),
		Volumes:   r.pod.Volumes,
		Mounts:    r.pod.VolumeMounts,
		Disk:      r.volumeSize.String(),
		Sandbox:   where,
	})
	if err != nil {
		r.log.Error("sandbox runtime: the pod shape cannot be encoded; pods will be compared by image alone", "error", err)
		return ""
	}
	return string(encoded)
}

// podShapeKey is what picks the shape a pod is built from and compared with (r.podShapes): its
// kind, and the sandbox it runs in. An issue pod has one shape per sandbox, and so has the
// controller's pod, whose sandbox is the project's type (SandboxConfig.Controller).
type podShapeKey struct {
	kind    podKind
	sandbox SandboxType
}

// podShapes are the shapes the runtime's pods are built from now, each encoded once (podShape):
// an issue pod's in gVisor and, where the configuration names a microvm block, in a microVM, and
// the controller's pod's in gVisor and, where the project's type is microvm
// (SandboxConfig.Controller), in a microVM. Without that last key a microVM controller's template
// would record no shape, which podShapeDrift reads as no drift, so no launch would ever move the
// controller's pod into or out of a microVM.
func podShapes(r *Runtime) map[podShapeKey]string {
	shapes := map[podShapeKey]string{
		{kind: issuePod{}, sandbox: SandboxGVisor}:      podShape(r, issuePod{}, nil),
		{kind: controllerPod{}, sandbox: SandboxGVisor}: podShape(r, controllerPod{}, nil),
	}
	if microVM := r.sandboxConfig.MicroVM; microVM != nil {
		shapes[podShapeKey{kind: issuePod{}, sandbox: SandboxMicroVM}] = podShape(r, issuePod{}, microVM)
		if r.sandboxConfig.Controller() == SandboxMicroVM {
			shapes[podShapeKey{kind: controllerPod{}, sandbox: SandboxMicroVM}] = podShape(r, controllerPod{}, microVM)
		}
	}
	return shapes
}

// shapeKey is the key of the shape l's pod is built from: its kind, in the sandbox prepare decided
// for it (launch.microVM), an issue pod's from its issue and the controller's from the project's
// type.
func (l launch) shapeKey() podShapeKey {
	if l.microVM != nil {
		return podShapeKey{kind: l.kind, sandbox: SandboxMicroVM}
	}
	return podShapeKey{kind: l.kind, sandbox: SandboxGVisor}
}

// storedShapeKey is the key of the shape the issue pod of the stored Sandbox u is compared with
// now: the issue pod's in the sandbox the configuration now gives the issue the Sandbox is labelled
// with (labelIssue, SandboxConfig.ofIssueLabel), the issue a launch decides its pod's sandbox from
// (prepare). PodDrift, its one caller, asks it only of a pod a round covers (roundCovers), an issue
// pod.
func (r *Runtime) storedShapeKey(u *unstructured.Unstructured) podShapeKey {
	label, _, _ := unstructured.NestedString(u.Object, "metadata", "labels", labelIssue)
	return podShapeKey{kind: issuePod{}, sandbox: r.sandboxConfig.ofIssueLabel(label)}
}

// ofIssueLabel is Of for the issue whose legion.dev/issue label holds label. The label holds an
// issue key as labelValue writes it, which past a label value's 63 characters is a hashed form.
// So the canary list is matched through that same form, and a key of any length reads as its
// launch decides it (prepare). Each reader of a stored issue Sandbox that asks where its issue's
// pod runs asks it: the shape that its pod is compared with (storedShapeKey), and the disk that the
// boot census compares it with (IssueDisks.templates).
func (c SandboxConfig) ofIssueLabel(label string) SandboxType {
	issue := label
	if i := slices.IndexFunc(c.CanaryIssues, func(key string) bool { return labelValue(key) == label }); i >= 0 {
		issue = c.CanaryIssues[i]
	}
	return c.Of(issue)
}

// podShapeDrift names what the pod of the Sandbox read as s was built from that the runtime would
// build a pod from otherwise now (want, podShape), one short phrase per key: `image`, `resources`,
// `volumes`, `mounts`, `disk` or `sandbox`, with what the pod holds and would be handed now.
// `sandbox` is where a microVM pod runs (sandboxShape) and null for a gVisor pod, so a pod whose
// issue moved from one sandbox to the other reads null on one side.
// Nil means nothing drifted.
//
// It reads the stored Sandbox's template, never the pod: GKE Autopilot rewrites a pod's
// requirements at admission, so a pod need not match the template it was made from, which is why
// the repository cache's own check reads the template too (templateResized).
//
// A template carrying no shape at all was written before this release, and is compared by image
// alone, exactly as imageDrift compares a pod: such a Sandbox never reads as having drifted
// resources, volumes or mounts, whatever its pod holds, since nothing recorded what it was built
// from and a guess would replace pods the operator never changed.
//
// Equality is semantic, not textual: a quantity spelled otherwise (1024Mi for 1Gi) is the same
// reservation and the same disk, and a configuration that encodes differently but means the same
// is no drift.
func podShapeDrift(s *sandbox, want string) []string {
	held := s.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
	if held == want {
		return nil
	}
	var now podShapeRecord
	if err := json.Unmarshal([]byte(want), &now); err != nil {
		return nil
	}
	if held == "" {
		return templateImageDrift(s, now.Image)
	}
	var was podShapeRecord
	if err := json.Unmarshal([]byte(held), &was); err != nil {
		return []string{fmt.Sprintf("its recorded shape %q cannot be read: %v", held, err)}
	}
	var drifted []string
	named := func(key string, inPod, handed any) {
		if equality.Semantic.DeepEqual(inPod, handed) {
			return
		}
		drifted = append(drifted, key+" "+shapeValue(inPod)+", now "+shapeValue(handed))
	}
	if was.Image != now.Image {
		drifted = append(drifted, "image "+was.Image+", now "+now.Image)
	}
	named("resources", was.Resources, now.Resources)
	named("volumes", was.Volumes, now.Volumes)
	named("mounts", was.Mounts, now.Mounts)
	if !sameQuantity(was.Disk, now.Disk) {
		drifted = append(drifted, "disk "+was.Disk+", now "+now.Disk)
	}
	named("sandbox", was.Sandbox, now.Sandbox)
	return drifted
}

// shapeValue is one part of a shape as a drift reason names it: its own JSON, which is how the
// annotation holds it, so a reason names exactly what differs. An unencodable value is
// unreachable, since the value was decoded from that same JSON.
func shapeValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// sameQuantity is whether two spellings of a quantity are the same amount, so a disk written
// 1024Mi and one written 1Gi are no drift. A spelling neither Kubernetes nor this daemon can parse
// is compared as text.
func sameQuantity(held, want string) bool {
	if held == want {
		return true
	}
	heldQuantity, heldErr := resource.ParseQuantity(held)
	wantQuantity, wantErr := resource.ParseQuantity(want)
	return heldErr == nil && wantErr == nil && heldQuantity.Cmp(wantQuantity) == 0
}

// templateImageDrift is how a Sandbox written before this release is compared: the first container
// of its stored template — an init container or the pod's own — running an image other than the
// one this runtime launches every container with, as one reason. A pod is built whole from one
// image, so one container's drift is the pod's.
func templateImageDrift(s *sandbox, image string) []string {
	if other, drifted := imageOtherThan(&s.Spec.PodTemplate.Spec, image); drifted {
		return []string{"image " + other + ", now " + image}
	}
	return nil
}

// shapeDrift is why the pod of the Sandbox read as s, a pod of the kind and sandbox key names, was
// built from a configuration other than the one such a pod is built from now: podShapeDrift
// against the shape the runtime computed for that key when it was built (r.podShapes).
//
// It is the one reading of a pod's shape, which both sides of a drifted pod's replacement take:
// the round names its reasons in the task it opens with (PodDrift, storedShapeKey), and a launch
// into a live pod no round covers that installs nothing replaces that pod when it answers anything
// (relaunch, launch.shapeKey, roundCovers). So the two can never disagree about whether a pod's
// shape has moved, or about which kind's shape, in which sandbox, it is compared with. A launch
// installing a stale-plugin pair into such a pod reads the image key of that same shape alone
// (podImageDrift).
func (r *Runtime) shapeDrift(s *sandbox, key podShapeKey) []string {
	return podShapeDrift(s, r.podShapes[key])
}

// podImageDrift is whether the pod of the Sandbox read as s was built from an image other than the
// one want names, want being the shape a pod of its kind is built from now (podShape,
// r.podShapes): the image its recorded shape holds, or, for a Sandbox whose template records no
// shape, the image a container of that template runs, read as templateImageDrift reads it
// (imageOtherThan). It is podShapeDrift's image key, read from the shape as data and never from
// the phrase podShapeDrift names it in.
//
// It is what a launch installing a stale-plugin pair into a live pod no round covers, the project
// controller's, replaces that pod for (relaunch). The install puts the release's plugins under the
// agent and nothing more: a pod built on another image would go on running another release's
// legion launcher as its process 1 and another release's Oh My Pi, where a fresh pod on the
// configured image carries the release's own launcher, Oh My Pi and plugins. A pod whose other
// facets alone drifted answers false, and that launch keeps it and installs into it.
//
// A recorded shape this daemon cannot read answers false as well: it names no image, and this
// reading guesses none. Such a launch then goes on as it did before this was asked: keptBy reads
// that same record, cannot read it either, and the launch replaces the pod as it replaces any pod
// whose template says nothing it can read.
func podImageDrift(s *sandbox, want string) bool {
	held := s.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
	if held == want {
		return false
	}
	var now podShapeRecord
	if err := json.Unmarshal([]byte(want), &now); err != nil {
		return false
	}
	if held == "" {
		_, drifted := imageOtherThan(&s.Spec.PodTemplate.Spec, now.Image)
		return drifted
	}
	var was podShapeRecord
	if err := json.Unmarshal([]byte(held), &was); err != nil {
		return false
	}
	return was.Image != now.Image
}

// PodDrift satisfies runtime.Runtime: why the pod k's process runs in no longer matches the
// configuration a pod created now is built from, each reason a phrase an operator reads in a task
// and in a log line, nil when nothing drifted.
//
// It is the configuration a running pod cannot be changed to carry: what its template was built
// from (podShapeDrift — the image, the kind's reservation, the operator's volumes and mounts, the
// disk, and the sandbox its issue's pod runs in — and leakCheckDrift, the leak check digest its
// install is held to), the ServiceAccount the pod runs as
// (accountDrift), and what driftedInPod reads of the pod itself whose worker can be asked to push
// first — the operator's volume and mount holding its role's overlays, the secrets broker's
// enrollment, the session store, and the GitHub owners its
// gh volume projects a directory for. The two facets whose worker cannot be
// reached at all stay a launch's business and are never a round's (movedInPod).
//
// It answers nothing for a claim with no process — such a claim records no pod, so no pod of it
// is gone — and nothing for a pod no round covers (roundCovers), today the project controller's:
// there is nobody in such a pod to task, so its drift is its launch's to replace rather than a
// round's.
//
// It answers runtime.ErrPodGone where the Sandbox or the pod is absent, where the pod running
// now is not the one the locator records, and where the Sandbox is suspended for anything but
// this round's own replacement: a pod that is not there, and a pod on its way out, need no round,
// and neither has drifted from nothing. That is the whole difference between a round that ends
// over the pod it was opened on and a round whose pod went with the work on its disk, which only
// the caller can tell its workers of (runtime.ErrPodGone, internal/daemon's groupDrift).
//
// The one suspended Sandbox that is not gone is the one this round's own ReplacePod suspended: it
// names the locator's own pod under the mark only that act writes (roundDisplacedAnnotation,
// displacing) and it is waiting that pod out. Such a pod is answered for as a pod that is
// there and has not drifted — no reasons and no error — so a sweep that reads the Sandbox between
// the suspension and the pod's end ends the round as the replacement it is. Any other suspended
// Sandbox is some other act's: an operator's suspension, a tree moved on, or a launch replacing
// the pod, which names it as displaced too (displacedAnnotation) and takes its disk with whatever
// the workers have not pushed. Nothing the round decides over such a pod is true, and the round
// that was opened on it ends as a round whose pod went.
//
// The sweep asks this of every claim of the deployment at every tick, so the steady state — a
// Sandbox running and a pod built from the configuration the runtime still has — costs one read
// of the operating mode, one of the issue the Sandbox is labelled with and one of the annotation
// the stored template records, against the shape the runtime holds for an issue pod in that
// issue's sandbox (r.podShapes, storedShapeKey), all strings of the object this call already
// holds. The Sandbox itself is decoded only where the two shapes differ, which is the pod a round
// is about to be opened over.
func (r *Runtime) PodDrift(_ context.Context, k runtime.Known) ([]string, error) {
	if err := k.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift: %w", err)
	}
	if k.Locator == nil {
		return nil, nil
	}
	if err := r.checkLocator(*k.Locator); err != nil {
		return nil, err
	}
	role, _ := k.Locator.Claim.Role()
	kind := podKindFor(role)
	if !roundCovers(kind) {
		return nil, nil
	}
	name := k.Locator.Sandbox.Name
	u, err := r.storedObject(name)
	if err != nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, err)
	}
	if u == nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
	}
	pod := r.storedPod(name)
	if !ownedBy(pod, u.GetUID()) || string(pod.UID) != k.Locator.Sandbox.PodUID {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
	}
	if mode, _, _ := unstructured.NestedString(u.Object, "spec", "operatingMode"); mode == modeSuspended {
		round, _, _ := unstructured.NestedString(u.Object, "metadata", "annotations", roundDisplacedAnnotation)
		if round != k.Locator.Sandbox.PodUID {
			return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
		}
		// This round's own replacement, between its suspension and the pod's end: the pod is
		// there, and what it was built from is what the replacement is about to build again.
		return nil, nil
	}
	var drifted []string
	shape := r.storedShapeKey(u)
	if held, _, _ := unstructured.NestedString(u.Object, "spec", "podTemplate", "metadata", "annotations", podShapeAnnotation); held != r.podShapes[shape] {
		// Only a template whose recorded shape differs is decoded, which is the pod a round is
		// about to be opened over; shapeDrift reads that same recorded shape against that same
		// key's and answers nothing where the two are equal, so the skipped decode hides no reason.
		s, err := decodeSandbox(u)
		if err != nil {
			return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, err)
		}
		drifted = r.shapeDrift(s, shape)
	}
	// The leak check digest the pod records, read from the same stored template's annotations and,
	// where the template records none, from the Sandbox's record of the check this daemon pushed
	// into this very pod (recordedLeakChecker, leakCheckDrift): a pod that records another digest
	// than the setting's is replaced by its round, and its workers are told the digest it records
	// while they push.
	annotations, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "annotations")
	drifted = append(drifted, r.leakCheckDrift(recordedLeakChecker(annotations, u.GetAnnotations(), string(pod.UID)))...)
	// The account the pod runs as is read from the live pod this call already holds, and both
	// sides go through serviceAccountOr, so a configured account left unset matches a pod on
	// `default` rather than reading every pod of the deployment as drifted (accountDrift). The
	// kind gate above (!roundCovers) is what keeps the controller's pod out of this reason, as
	// it does of every other. Its reason opens on runtime.BreakingDrift, which is set here, where
	// the account's drift is decided: no role can keep working in a pod on an account the operator
	// replaced, so the round over it replaces the pod after the read of its workspace whatever the
	// read finds (R7 of #5285, spec v23).
	if account, accountDrifted := accountDrift(pod, r.pod.ServiceAccount); accountDrifted {
		drifted = append(drifted, runtime.BreakingDrift+"account "+account+", now "+serviceAccountOr(r.pod.ServiceAccount))
	}
	// The exchange identity's reason (movedExchangeIdentity) opens on the mark only where a launch
	// into this pod is refused until the pod's round replaces it (spec v26): the deployment sets
	// dispatch_token_broker, names no dispatch_token_transition_file, and the pod carries no
	// identity the exchange trades for the claim's role (awaitsPodRound over tradesIn of the live
	// pod, the condition dispatchCredential refuses on). A round that kept such a pod for work on
	// its disk would leave a claim that waits on the pod, where it is the issue's only claim, with
	// no way to run again (runtime.ErrAwaitingPodRound). Any other drift of the exchange identity
	// is one the roles can keep working under: a token of another lifetime still trades
	// (carriesExchangeIdentity), and where the identity is missing and no launch is refused, a role
	// keeps the credential it had or carries the transition token. No other reason carries the mark.
	for _, moved := range r.driftedInPod(pod, role) {
		reason := moved.String()
		if moved.where == agentSecretsExchangeVolume && r.awaitsPodRound(r.tradesIn(&pod.Spec, role)) {
			reason = runtime.BreakingDrift + reason
		}
		drifted = append(drifted, reason)
	}
	return drifted, nil
}

// ReplacePod satisfies runtime.Runtime: it ends the pod k's process runs in, once its update round
// is over, and waits until the cluster no longer has it. It is what a launch does to a live pod it
// replaces, and nothing more — under the pod's launch turn, the Suspended patch that names the
// replaced pod on the Sandbox in the same write (displacing, suspendOver), then the wait for the
// pod's end — so every role still running there is reported ended by the daemon's own act
// (Displaced) and resumed uncharged on its session, as it is for a pod a launch replaces. That one
// patch names the pod under the round's own mark as well (roundDisplacedAnnotation,
// roundReplacement), which no launch writes for a reason of its own: it is what a sweep reading
// the Sandbox before the pod ends tells this replacement by, where a launch's replacement of the
// same pod is a pod gone (PodDrift).
//
// It creates and provisions nothing: the first of those resumes finds the Sandbox Suspended and
// provisions a fresh pod from the current configuration, which is the launch path a replacement
// has always taken. It is a no-op when the Sandbox is absent or already suspended, or when the pod
// the locator records is no longer the Sandbox's own: there is then no pod of that round to end.
//
// The pod may show that an init container failed on the Sandbox's kept volume for a cause local
// to it (volumeFault, localInitFailure). The same patch then writes the reset mark on the Sandbox
// (volumeResetAnnotation, suspendOver), from the pod as it stood before the suspension. The launch
// after the round reads no pod, and it resets the volume on the mark (volumeReset). Every other
// pod is suspended over with the two names alone.
//
// due is asked last of all, under the pod's launch turn and one statement before the suspension,
// and its error is returned having written nothing. That is where it belongs rather than at the
// top: taking the launch turn waits out whatever launch holds it, which under a fleet-wide roll
// is a pod boot of minutes, and a round the caller found complete before that wait is exactly a
// round a worker can start a turn in during it. Asked here, the caller reads the claims one
// statement before the write that ends their processes, and nothing it refuses is ever cut.
func (r *Runtime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("sandbox runtime: replace pod: %w", err)
	}
	if k.Locator == nil {
		return nil
	}
	if err := r.checkLocator(*k.Locator); err != nil {
		return err
	}
	return r.replaceRoundPod(ctx, k.Locator.Sandbox.Name, k.Locator.Sandbox.PodUID, due, "claim", string(k.Claim))
}

// ReplaceIssuePod satisfies runtime.IssuePodReplacer: it ends the pod podUID of issue's Sandbox,
// once the update round over that pod is over and no role process runs in it, and waits until
// the cluster no longer has it. It is ReplacePod's act keyed by the issue rather than by a
// claim's locator: the round's claims record no process in the pod, so no locator names it, and
// the Sandbox is the issue's one Sandbox (issueSandboxName). The act is ReplacePod's own
// (replaceRoundPod): the launch turn, the round's own displaced-pod mark, the reset mark of a pod
// that failed on its kept volume, the wait for the pod's end, and due asked last. So a sweep that
// reads the Sandbox before the pod ends tells this replacement from a launch's, as it tells
// ReplacePod's (PodDrift).
//
// It is a no-op when the issue has no Sandbox, when the Sandbox is already suspended, and when
// the pod the Sandbox holds is not podUID: there is then no pod of that round to end. An issue
// key no Sandbox name can be built from is an error.
func (r *Runtime) ReplaceIssuePod(ctx context.Context, issue, podUID string, due func(context.Context) error) error {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("sandbox runtime: replace the pod of %s: %w", issue, err)
	}
	return r.replaceRoundPod(ctx, name, podUID, due, "issue", issue)
}

// ReadAct satisfies runtime.ActReader: whether a recorded act on issue's pod was made, read from
// the API server under the pod's launch turn (GPT-6.1 Sol's point 2 on #5285 at fa9deb04,
// the Legion reviewer's point A). The turn is taken without waiting (tryLockPod): a launch, a
// replacement or a release that holds it may be making the act now, so ReadAct answers
// runtime.ErrTurnBusy and reads nothing. Under the turn no act of this daemon on the pod runs, and
// the Sandbox and its pod are read from the API server, never from the stores: a store whose feed
// fails holds what it last heard, a Sandbox Running over a pod whose suspension landed among it.
// decide runs under the turn. A Sandbox name no issue key builds, and a read that fails, are
// errors, and decide is not called.
//
// An act that takes the pod (runtime.TakesPod, the pod's uid its target) is made where the pod is
// taken (podTaken): the issue has no Sandbox, its Sandbox is Suspended — the suspension landed,
// whether or not the pod is still there to see — or the Sandbox's own pod is another or none; it is
// not made where the Sandbox runs the target as its own pod. ReplacePod writes its suspension before it waits for the
// pod to go (replaceRoundPod), so a call whose wait ran out has taken the pod all the same.
//
// An act that takes the kept volume (runtime.TakesVolume, the uid of the Sandbox that owns it its
// target) is made only where no Sandbox of that uid is left: the delete is foreground, so the
// Sandbox's absence is the API server's confirmation that the volume it owned is gone
// (deleteSandbox; GPT-6.1 Sol's point 4 on #5285 at fa9deb04). A Suspended Sandbox of that uid, or
// one that runs no pod of its own, says only that the pod was taken, and the act reads uncertain.
// A Sandbox of that uid Running with a live pod of its own, which is not being deleted, uses the
// volume again: the act was not made.
func (r *Runtime) ReadAct(ctx context.Context, issue string, act runtime.RecordedAct, decide func(context.Context, runtime.ActState) error) error {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("sandbox runtime: read an act on the pod of %s: %w", issue, err)
	}
	release, free := r.tryLockPod(name)
	if !free {
		return runtime.ErrTurnBusy
	}
	defer release()
	state, err := r.actState(ctx, name, act)
	if err != nil {
		return fmt.Errorf("sandbox runtime: read an act on the pod of %s: %w", issue, err)
	}
	return decide(ctx, state)
}

// actState is what the API server holds now of act on the Sandbox named name (ReadAct).
func (r *Runtime) actState(ctx context.Context, name string, act runtime.RecordedAct) (runtime.ActState, error) {
	s, err := r.readSandbox(ctx, name)
	if err != nil {
		return runtime.ActUncertain, fmt.Errorf("read sandbox %s: %w", name, err)
	}
	if s == nil {
		return runtime.ActMade, nil
	}
	switch act.Takes {
	case runtime.TakesPod:
		var pod *corev1.Pod
		if s.mode() != modeSuspended {
			if pod, err = r.apiPod(ctx, name); err != nil {
				return runtime.ActUncertain, err
			}
		}
		if podTaken(s, pod, act.Target) {
			return runtime.ActMade, nil
		}
		return runtime.ActNotMade, nil
	case runtime.TakesVolume:
		if string(s.UID) != act.Target {
			return runtime.ActMade, nil
		}
		if s.mode() == modeSuspended || s.DeletionTimestamp != nil {
			return runtime.ActUncertain, nil
		}
		pod, err := r.apiPod(ctx, name)
		if err != nil {
			return runtime.ActUncertain, err
		}
		if ownedBy(pod, s.UID) && pod.DeletionTimestamp == nil {
			return runtime.ActNotMade, nil
		}
		return runtime.ActUncertain, nil
	}
	return runtime.ActUncertain, fmt.Errorf("no act takes %q", act.Takes)
}

// podTaken is whether the pod of uid is taken from the issue's Sandbox s, read from the stores or
// from the API server: there is no Sandbox, the Sandbox is Suspended — a suspension that landed
// takes the pod, whether or not the pod is still there to see — or the pod the Sandbox owns is
// another or none (pod, a same-named pod the Sandbox does not own being none). It is the one test of
// a taken pod: a round's replacement makes nothing of a pod that is taken (replaceRoundPod), and a
// recorded act that takes the pod is made once the pod is taken (actState).
func podTaken(s *sandbox, pod *corev1.Pod, uid string) bool {
	return s == nil || s.mode() == modeSuspended || !ownedBy(pod, s.UID) || string(pod.UID) != uid
}

// apiPod is the pod named name as the API server holds it now, nil where it holds none.
func (r *Runtime) apiPod(ctx context.Context, name string) (*corev1.Pod, error) {
	pod, err := r.kube.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pod %s: %w", name, err)
	}
	return pod, nil
}

// replaceRoundPod is the act ReplacePod and ReplaceIssuePod share: under the launch turn of the
// Sandbox named name, it ends that Sandbox's pod podUID as a finished update round's own
// replacement and waits until the cluster no longer has it. It answers nil having written nothing
// for a Sandbox that is absent or already suspended, and for a Sandbox whose own pod is not
// podUID. due is asked last, one statement before the suspension, and its error is returned
// having written nothing. key and of name what the caller replaces the pod for, the claim or the
// issue, in an error and on the line.
func (r *Runtime) replaceRoundPod(ctx context.Context, name, podUID string, due func(context.Context) error, key, of string) error {
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return fmt.Errorf("replace %s's pod: take its pod's launch turn: %w", of, err)
	}
	defer release()
	s, err := r.storedSandbox(name)
	if err != nil {
		return fmt.Errorf("replace %s's pod: %w", of, err)
	}
	// The pod the Sandbox owns, as every other reader of the two together takes it (viewOf): a
	// same-named pod it does not own is some earlier Sandbox's, and no pod of this round.
	v := r.viewOf(name, s)
	if podTaken(s, v.pod, podUID) {
		return nil
	}
	if err := due(ctx); err != nil {
		return err
	}
	if _, err := r.suspendOver(ctx, s, v.pod, displacing(v.pod.UID, roundReplacement)); err != nil {
		return fmt.Errorf("replace %s's pod: suspend its sandbox: %w", of, err)
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return fmt.Errorf("replace %s's pod: wait out its pod: %w", of, err)
	}
	r.log.Info("sandbox runtime: the pod of a finished update round is replaced", "sandbox", name, "pod", v.pod.UID, key, of)
	return nil
}

// podKindFor is the kind of pod a claim of role runs in, the one decision a launch makes from its
// claim (prepare) and a round makes again from the locator it holds: the project controller's pod
// for the controller's claim, and the issue's pod for every workflow role.
func podKindFor(role claim.Role) podKind {
	if role == claim.RoleController {
		return controllerPod{}
	}
	return issuePod{}
}
