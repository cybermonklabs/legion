package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// podKind is which of the runtime's two pods a Sandbox runs: an issue pod (issuePod), which runs
// every workflow role of one issue on a disk its own pod owns, or its Sandbox owns where the pod
// keeps its volume (keptVolume), and provisions the issue's workspace, or the project controller's
// pod (controllerPod, `controller: daemon`), which runs the controller alone on the volume its
// Sandbox owns and provisions nothing. prepare decides a
// launch's kind once, from its claim, and a stored Sandbox's kind is the one its labels name
// (podKindOf). Building a pod asks the launch's kind for every facet the two build differently;
// supervision — relaunch's check of the running pod, the launchers, Probe, Suspend and the
// address records — asks a kind only its roles.
type podKind interface {
	// roles are the pod's launcher roles, each with a launcher token and role Secret of its own.
	roles() []claim.Role
	// claimToken is the claim of a launcher of role, one of roles, on a pod of this kind of project,
	// issue being the issue key its Secrets carry (secretAnnotations).
	claimToken(project, issue string, role claim.Role) (claim.Token, error)
	// prepare resolves what the kind decides of a launch, its working directory above all,
	// refusing a spec it cannot honour.
	prepare(l *launch) error
	// labels are the pod's resource labels, on its Sandbox, pod template and Secrets, which name
	// its kind (podKindOf).
	labels(project string, l launch) map[string]string
	// disk is the pod's one disk, mounted under the fixed name issueVolume: the claim templates the
	// kind's Sandbox owns, which outlive its pods, and the source the pod mounts the disk from.
	// They are returned together so that a template and the claim name that refers to it are
	// decided in one place: the one `issue` template and the claim its Sandbox makes from it, for
	// the controller and for an issue that keeps its volume (keptVolume), or no template at all
	// and an ephemeral volume of the pod's own for every other issue.
	disk(r *Runtime, l launch) (templates []volumeClaimTemplate, source corev1.VolumeSource)
	// secretAnnotations are the annotations each of the pod's Secrets carries.
	secretAnnotations(l launch) map[string]string
	// resources are the reservation every container of the pod carries, its init containers'
	// included (Options.Resources).
	resources(r *Runtime) corev1.ResourceRequirements
	// asksExtendedRunTime is whether a pod of this kind, under the deployment's reservations, asks
	// GKE Autopilot for extended run time, which is the one thing that decides whether its
	// template carries safeToEvictAnnotation (podTemplate). The controller's pod asks always; an
	// issue pod only while its reservation is Guaranteed, so that an operator who gives an issue
	// pod room to burst gets a pod that shares a node (issuePod.asksExtendedRunTime). Karpenter's
	// doNotDisruptAnnotation is no kind's to decide: every pod carries it.
	asksExtendedRunTime(r *Runtime) bool
	// initContainers are the pod's init containers, and initVolumes the volumes only they mount,
	// besides the pod's own volume and config home.
	initContainers(r *Runtime, l launch) []corev1.Container
	initVolumes(l launch) []corev1.Volume
	// container is the name of the pod's one container, which runs `legion launcher` for every
	// role of roles, and launcherContainer that container: its command, the mounts of every role's
	// own volumes and the pod's shared ones, and the kubelet-resolved environment (resolved).
	// mainVolumes are the volumes that container mounts beyond each role's own, the pod's volume,
	// the config home, the agent-secrets volumes, the providers and the operator's, for a pod of
	// launch l.
	container() string
	launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container
	mainVolumes(r *Runtime, l launch) []corev1.Volume
	// agentEnv is what each agent is told of its kind, mainEnvironment's one block of its own.
	agentEnv(l launch) []corev1.EnvVar
	// provision readies what a new pod needs before relaunch writes its launcher Secrets and sets
	// its Sandbox, s, running, under nothing but the pod's own launch turn: each pod works on a
	// volume of its own, so no pod's provisioning waits on another's.
	provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error
}

// podKindOf is the kind a Sandbox's labels name: an issue pod's carry legion.dev/issue, the project
// controller's legion.dev/role=controller and no tree. Any other labelling names no kind and is
// refused, so a pod is never judged against a role list its labels do not state.
func podKindOf(labels map[string]string) (podKind, error) {
	switch {
	case labels[labelIssue] != "":
		return issuePod{}, nil
	case labels[labelRole] == string(claim.RoleController) && labels[labelTree] == "":
		return controllerPod{}, nil
	}
	return nil, fmt.Errorf("its labels name neither an issue nor the project controller (%s=%q, %s=%q, %s=%q)",
		labelIssue, labels[labelIssue], labelTree, labels[labelTree], labelRole, labels[labelRole])
}

// keptVolume is the one rule that decides whether a launch keeps its volume: an issue pod (kind)
// that runs in a microVM (microVM, launch.microVM) under keep_volume (MicroVM.KeepVolume). Its
// disk is then a claim that its Sandbox owns and that outlives the pod (issuePod.disk), and its
// shape records the setting (podShape). A gVisor pod keeps the ephemeral volume of its own. The
// controller's pod ignores the setting: its volume is its Sandbox's in every case
// (controllerPod.disk).
func keptVolume(kind podKind, microVM *MicroVM) bool {
	_, issue := kind.(issuePod)
	return issue && microVM != nil && microVM.KeepVolume
}

// keepsVolume is whether l's pod keeps its volume (keptVolume).
func (l launch) keepsVolume() bool { return keptVolume(l.kind, l.microVM) }

// keptTemplate is the one rule of the stored side: whether a claim template, by its name and the
// value of its labelVolume label ("" for none), is an issue's kept volume. That is the one `issue`
// template that issuePod.disk writes for a launch that keeps its volume, labelled volumeKept. Only
// that write sets the label, so only an issue Sandbox owns such a template. The controller's
// `issue` template and the one of an issue Sandbox of the release before the pod-owned disk carry
// no such label, so neither reads as kept. Every reader of a stored Sandbox asks it, through
// holdsKeptVolume (ownsKeptVolume) or storedOwnsKeptVolume.
func keptTemplate(name, volume string) bool { return name == issueVolume && volume == volumeKept }

// ownsKeptVolume is whether the decoded Sandbox s owns an issue's kept volume (keptTemplate).
func ownsKeptVolume(s *sandbox) bool { return holdsKeptVolume(s.Spec.VolumeClaimTemplates) }

// holdsKeptVolume is whether one of the claim templates is an issue's kept volume (keptTemplate).
func holdsKeptVolume(templates []volumeClaimTemplate) bool {
	return slices.ContainsFunc(templates, func(t volumeClaimTemplate) bool {
		return keptTemplate(t.Metadata.Name, t.Metadata.Labels[labelVolume])
	})
}

// storedOwnsKeptVolume is ownsKeptVolume of a Sandbox as the API server or the informer holds it,
// read without a decode, for a reader that has no decoded Sandbox: decoding a whole Sandbox costs
// tens of kilobytes and hundreds of allocations (IssueSandbox). It reads each field in place
// (NestedFieldNoCopy, never NestedSlice, which copies every template), and a field of another type
// reads as absent.
func storedOwnsKeptVolume(u *unstructured.Unstructured) bool {
	field, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "volumeClaimTemplates")
	templates, _ := field.([]any)
	for _, template := range templates {
		object, ok := template.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(object, "metadata", "name")
		volume, _, _ := unstructured.NestedString(object, "metadata", "labels", labelVolume)
		if keptTemplate(name, volume) {
			return true
		}
	}
	return false
}

// issuePod is an issue's pod: every workflow role of the issue, working in the issue's workspace
// on the issue's disk (disk), in one container whose PID 1 is one `legion launcher` holding a
// connection of each role's own (launcherContainer). The disk is the pod's own generic ephemeral
// volume, created with the pod and deleted with it, or, where the pod keeps its volume
// (keptVolume), a claim that the issue's Sandbox owns, which outlives the pod. Two init containers
// provision that workspace from the issue's repository.
type issuePod struct{}

func (issuePod) roles() []claim.Role { return claim.Roles }

func (issuePod) container() string { return runtime.IssuePodContainer }

// resources is the one issue-pod reservation.
func (issuePod) resources(r *Runtime) corev1.ResourceRequirements { return r.resources.Issue }

// asksExtendedRunTime is whether this deployment's issue pod asks GKE Autopilot for extended run
// time: only while its reservation is Guaranteed (guaranteedReservation). A deployment that gives
// an issue pod a cpu or memory request below its limit has asked for a pod that bursts, and
// Autopilot gives a pod carrying safeToEvictAnnotation a node of its own, which is the opposite of
// that: the pod would hold a whole node at its limits and burst into nothing. So the request
// decides, and no legion.yaml key does — a deployment that had set one could not go back to an
// earlier daemon release without editing its file first, which is the one action it takes when
// this change goes wrong. What a Burstable issue pod gives up is the seven-day protection: the
// cluster may end it to repack a node, taking the work its roles did not push unless its disk is
// a kept volume, and the daemon continues each of its roles in the issue's next pod, but for the
// ends it counts as the pod's own (evictedOverRequest).
func (k issuePod) asksExtendedRunTime(r *Runtime) bool {
	return guaranteedReservation(k.resources(r))
}

// guaranteedReservation reports whether resources is the Guaranteed reservation the kubelet reads:
// its cpu request equal to its cpu limit and its memory request equal to its memory limit. It
// compares quantities (resource.Quantity.Cmp), so a request of 1 under a limit of 1000m is equal,
// as config.Reservation.Burstable compares the same two pairs of the configuration's strings. The
// two are one rule written on each side of a package boundary — this package reads no
// configuration, and the configuration depends on no runtime implementation — and
// TestTheSafeToEvictAnnotationsGuaranteedRuleAgreesWithTheConfigurations holds them to one answer,
// so the pod an operator's `legion config check` calls Guaranteed is the pod that carries the
// annotation.
// Ephemeral storage is not read, and never should be: every pod of this deployment requests less
// of the node's disk than its limit allows it to write (defaultResources), so reading it would
// make every pod Burstable here while the kubelet, which reads cpu and memory alone, calls them
// Guaranteed.
//
// A resource with a request and no limit, with a limit and no request, or with neither, reads as
// not Guaranteed: an absent quantity is zero, so a missing limit is no pair to be equal to, and a
// pod with no cpu or memory limit at all is the kubelet's BestEffort, never its Guaranteed. No
// configuration reaches this: New refuses a reservation whose cpu or memory request or limit is
// missing or not positive (checkRequirements), and config.Reservation.Burstable, which reads the
// same configuration, panics on a quantity that is not there rather than reading it as Guaranteed.
func guaranteedReservation(resources corev1.ResourceRequirements) bool {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := resources.Requests[name], resources.Limits[name]
		if limit.Sign() <= 0 || request.Cmp(limit) != 0 {
			return false
		}
	}
	return true
}

func (issuePod) claimToken(project, issue string, role claim.Role) (claim.Token, error) {
	return claim.NewToken(project, issue, role)
}

// prepare refuses a spec with no repository, which the workspace is provisioned from, and resolves
// the workspace's place on the issue's disk. It runs at every launch and expects nothing of that
// disk. The pod's own disk is created with the pod and deleted with it (INFRA-80). A kept volume
// (keptVolume) outlives the pod, so a new pod of the issue finds on it what the pods before it
// left. Only a new or replacement pod provisions its workspace (provision, which relaunch runs
// only when it replaces the pod), and a role launched into the live pod finds the workspace the
// roles before it left. docs/kubernetes.md, "The disk dies with its pod", states which launch is
// which.
func (issuePod) prepare(l *launch) error {
	spec := l.spec
	if spec.Repository.IsZero() {
		return errors.New("no repository: a pod's init container provisions the issue's workspace from one")
	}
	dir, err := WorkspaceDir(spec.Repository, spec.Issue)
	if err != nil {
		return err
	}
	l.workspace = dir
	return nil
}

// WorkspaceDir is the directory of issue's workspace of repository in its pod: its place on the
// issue's disk (workspace.Location under TreeRoot), the working directory of every role container
// of the pod (issuePod.prepare), and so the directory each agent of the issue starts in. The
// workflow engine names it in every task it composes (workflow.Config.WorkspaceDir, which the
// daemon reads from here).
func WorkspaceDir(repository ghrepo.Repository, issue string) (string, error) {
	working, err := workspace.Location(TreeRoot, repository, issue)
	if err != nil {
		return "", err
	}
	return working.Dir, nil
}

// labels carry the pod's tree and issue and no role: a role belongs to a process locator, not to a
// shared issue pod.
func (issuePod) labels(project string, l launch) map[string]string {
	return map[string]string{
		labelProject: project,
		labelTree:    labelValue(l.spec.Tree),
		labelIssue:   labelValue(l.spec.Issue),
	}
}

// disk is one of two shapes, and keptVolume decides which (launch.keepsVolume).
//
// A launch that keeps no volume has no claim template: the issue's Sandbox owns no volume, so
// nothing of the issue's disk outlives a pod (INFRA-80). The disk itself is the pod's own, a
// generic ephemeral volume: the kubelet creates its claim when the pod starts and deletes it when
// the pod goes, so a suspended, relaunched or swept issue leaves no volume behind and every pod
// starts on a workspace its init containers provision afresh. Its claim template carries no
// metadata at all: the pinned Sandbox CRD declares the template's `metadata` a bare object with no
// properties and no preserve-unknown-fields, so a label set there is pruned by the API server
// without a word (TestManifestMatchesTheSandboxCRD).
//
// A launch that keeps a volume (a microVM pod under keep_volume) gets what controllerPod.disk
// gives: the one `issue` template (keptVolumeTemplates), which the issue's Sandbox owns, and the
// claim that the Agent Sandbox controller makes from it (IssueClaimName), which the pod mounts.
// The claim outlives the pod and goes with the Sandbox.
//
// Either claim is on the launch's class (diskSpec: a microVM issue pod's is the microVM block's).
func (issuePod) disk(r *Runtime, l launch) ([]volumeClaimTemplate, corev1.VolumeSource) {
	if l.keepsVolume() {
		templates := keptVolumeTemplates(r.project, labelValue(l.spec.Issue), r.diskSpec(l))
		return templates, corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: IssueClaimName(l.spec.Claim),
		}}
	}
	return nil, ephemeralDisk(r.diskSpec(l))
}

// ephemeralDisk is a generic ephemeral volume whose claim is spec: the kubelet creates the claim
// when the pod starts and deletes it when the pod goes. Its template carries no metadata (disk).
func ephemeralDisk(spec corev1.PersistentVolumeClaimSpec) corev1.VolumeSource {
	return corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: spec},
	}}
}

// keptVolumeTemplates are the claim templates of an issue's kept volume, with spec as its claim:
// the one `issue` template, labelled with project, the issue (issueLabel, the issue key as
// labelValue writes it) and labelVolume=volumeKept, which marks the volume as kept
// (keptTemplate). They are not the pod's own labels (labels), which carry the tree: a launch
// relabels the Sandbox for a new tree (ensureSandbox), and the daemon's restricted identity has no
// PVC verb to relabel the claim. The Sandbox CRD keeps the labels of a Sandbox's own claim
// template. A launch writes them (issuePod.disk), and the boot census renders them
// (IssueDisks.templates).
func keptVolumeTemplates(project, issueLabel string, spec corev1.PersistentVolumeClaimSpec) []volumeClaimTemplate {
	return []volumeClaimTemplate{{
		Metadata: volumeClaimMetadata{Name: issueVolume, Labels: map[string]string{
			labelProject: project,
			labelIssue:   issueLabel,
			labelVolume:  volumeKept,
		}},
		Spec: spec,
	}}
}

// secretAnnotations carry the exact issue key (annotationIssue), from which the launcher resolver
// derives a role's claim (claimToken): the issue label holds the key as a label value, which past
// 63 characters is not the key.
func (issuePod) secretAnnotations(l launch) map[string]string {
	return map[string]string{annotationIssue: l.spec.Issue}
}

// initContainers are workspace-fetch and workspace-init, so the provisioning token never shares a
// process with anything an agent of the issue can write (Stage 4b Task 4b.6b): workspace-fetch, the
// one container of the pod that holds the token, mounts the provisioning Secret, its own TMPDIR, and
// the feed alone of the issue's volume (its feedSubPath, read-write), and fills the feed — from the
// repository's cache when one is ready (fetchEnvironment), then from GitHub the branches an
// issue's clone is made from, or from GitHub alone; workspace-init
// mounts the issue's volume, the feed over it read-only, and the config home, and does all the
// volume's work from the feed, with no credential, making the issue's clone shared with the feed
// and starting an issue with no branch at the repository's default branch the spec names. The
// issue's volume is the pod's disk (disk). A new pod's own ephemeral volume is empty, and a kept
// volume (keptVolume) holds the feed and the clone that the issue's earlier pods left. Each takes
// the pod's one reservation, which adds nothing to the pod's: the scheduler counts a pod's request
// as the larger of its main containers' sum and its largest init container.
func (k issuePod) initContainers(r *Runtime, l launch) []corev1.Container {
	legion, resources := r.tools.Legion, k.resources(r)
	return []corev1.Container{{
		Name:       fetchContainer,
		Image:      r.image,
		Command:    []string{legion, "workspace-init", "fetch", "--repo", l.spec.Repository.String(), "--feed", FeedDir},
		Env:        fetchEnvironment(l),
		WorkingDir: FeedDir,
		VolumeMounts: []corev1.VolumeMount{
			{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
			{Name: tempVolume, MountPath: initTempDir},
			{Name: issueVolume, MountPath: FeedDir, SubPath: feedSubPath},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}, {
		Name:  initContainer,
		Image: r.image,
		Command: []string{
			legion, "workspace-init", "provision", "--issue", l.spec.Issue, "--repo", l.spec.Repository.String(),
			"--default-branch", l.spec.DefaultBranch, "--root", TreeRoot,
			"--credential-helper", workspace.GitHubCredentialHelper, "--feed", FeedDir,
		},
		Env:        r.initEnvironment(l),
		WorkingDir: TreeRoot,
		VolumeMounts: []corev1.VolumeMount{
			{Name: issueVolume, MountPath: TreeRoot},
			readOnlyFeed(),
			{Name: configVolume, MountPath: xdgConfigHome},
		},
		Resources:       resources,
		SecurityContext: restrictedContainer(),
	}}
}

// readOnlyFeed is the issue volume's feed mounted read-only at FeedDir, over the volume's own mount
// at TreeRoot: what every container of an issue pod but workspace-fetch sees of the feed.
func readOnlyFeed() corev1.VolumeMount {
	return corev1.VolumeMount{Name: issueVolume, MountPath: FeedDir, SubPath: feedSubPath, ReadOnly: true}
}

// initVolumes are the provisioning material only the init containers mount: the provisioning
// Secret, with the provisioning token and the repository cache's bearer token, and
// workspace-fetch's in-memory TMPDIR. The feed is the issue volume's own (FeedDir).
func (issuePod) initVolumes(l launch) []corev1.Volume {
	return []corev1.Volume{
		{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName(l.name),
			Items: []corev1.KeyToPath{
				{Key: provisionTokenKey, Path: provisionTokenKey}, {Key: cacheTokenKey, Path: cacheTokenKey},
			},
			DefaultMode: new(int32(0o440)),
		}}},
		{Name: tempVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
}

// launcherContainer is an issue pod's one container, `roles`: `legion launcher` in its roles-dir
// form for every role of l.roles (rolesDirLayout), which reads each role's launcher token under
// its directory of RolesDir and holds a connection of its own per role, and starts each role's
// generation in namespaces of its own (`legion launcher role-init`), where the role sees its own
// gh, private, state and agent-secrets key directories at the fixed paths a role reads them at and
// none of its siblings'. So each role's own volumes mount under its directory of RolesDir, its
// launcher token and gh files read-only, and none at those fixed paths; the role homes volume
// (roleHomesVolume) at RoleHomesDir holds each role's home overlay, temporary directories and own
// config home. The pod's shared mounts are mounted once: the issue's disk, the config home
// workspace-init writes, the feed read-only (so no agent writes what workspace-fetch, the token's
// holder, reads), the providers, the session writer token, the broker's token and the exchange's
// (podTokenMounts), and the operator's. Every
// one of them that belongs at or under the image's home — the config home, an operator's mount
// there — is mounted under launcher.HomeMountsDir at the same relative path instead
// (homeMountsView), so the home carries no mount: each role's namespace overlays the home, which
// Linux refuses over a lower layer with mounts under it, and then binds each of them back at its
// own path, but the shared config home, whose place gets the role's own copy of it
// (launcher.ConfigHomeDir). The values the kubelet resolves are the same for every role (the
// downward API's POD_UID), so they are set once. A microVM pod's launcher is told so
// (microVMLauncher), the metadata endpoint's token mounted at WorkloadIdentityDir, under RolesDir,
// where no role's namespace shows it.
func (k issuePod) launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container {
	shared := []corev1.VolumeMount{
		{Name: issueVolume, MountPath: TreeRoot},
		{Name: configVolume, MountPath: xdgConfigHome},
		readOnlyFeed(),
	}
	var flags []string
	var extra []corev1.VolumeMount
	if r.testPostgres {
		// The launcher serves the pod's own test database (launcher's testpg.go) and the roles
		// container keeps its cluster here; no role's namespace shows this mount. Where the
		// cluster is on a disk of its own (mainVolumes), the launcher is told so
		// (--test-postgres-disk), since the free bytes it reads there are then the cluster's alone.
		flags = append(flags, "--test-postgres")
		if r.testPostgresDisk != nil {
			flags = append(flags, "--test-postgres-disk")
		}
		extra = append(extra, corev1.VolumeMount{Name: testPGVolume, MountPath: TestPGDir})
	}
	microVMFlags, tokenMounts := microVMLauncher(l, WorkloadIdentityDir)
	flags = append(flags, microVMFlags...)
	extra = append(extra, tokenMounts...)
	return r.rolesDirLauncher(k, l, resolved, providersMounts, shared, flags, extra)
}

// rolesDirLauncher is the one container of a pod whose launcher runs the roles-dir form for the
// roles of l (rolesDirLayout): an issue pod's `roles`, and the controller's `controller` where the
// session database names a writer audience (controllerPod.rolesDirContainer). Both kinds build it
// here, so the tokens no role may read (podTokenMounts) and the home's mounts (homeMountsView) are
// laid out the same way in each. The launcher's command is the stream's address and the
// Sandbox's name, the form's own words, the kind's flags, then the stop grace and the session
// proxy's flags. Its mounts are shared, each role's own volumes, the kind's extra mounts, then the
// tokens, the providers and the operator's mounts, in that order.
func (r *Runtime) rolesDirLauncher(k podKind, l launch, resolved []corev1.EnvVar, providersMounts, shared []corev1.VolumeMount, flags []string, extra []corev1.VolumeMount) corev1.Container {
	roles, own := r.rolesDirLayout(l.roles)
	return corev1.Container{
		Name:  k.container(),
		Image: r.image,
		Command: slices.Concat([]string{r.tools.Legion, "launcher", connectFlag, r.streamURL, "--sandbox", l.name}, roles, flags,
			[]string{"--stop-grace", r.terminationGrace.String()}, r.sessionProxyFlags()),
		Env:             resolved,
		WorkingDir:      l.workspace,
		VolumeMounts:    homeMountsView(slices.Concat(shared, own, extra, r.podTokenMounts(), providersMounts, r.pod.VolumeMounts)),
		Resources:       k.resources(r),
		SecurityContext: restrictedContainer(),
	}
}

// rolesDirLayout is the launcher's roles-dir form for roles, the form an issue pod's container runs
// for every workflow role, and the controller's pod for the controller where the session database
// names a writer audience (controllerPod.launcherContainer). command is the words that name the
// form: --roles-dir RolesDir, then one --role per role. mounts are each role's own volumes under
// its directory of RolesDir (roleDir), at the names of launcher's layout: its launcher token and
// its gh files read-only, its private and state directories, and its agent-secrets key directory
// where the pod enrolls with the secrets broker; then the role homes volume at RoleHomesDir
// (roleHomesVolumes), where role-init keeps each role's home overlay, temporary directories and
// own config home. Nothing is mounted at the fixed paths a role reads its own at (GHConfigDir and
// its siblings): role-init covers RolesDir in each role's namespaces and binds the role's own
// back there, all but the launcher token, which no role's process sees.
func (r *Runtime) rolesDirLayout(roles []claim.Role) (command []string, mounts []corev1.VolumeMount) {
	command = []string{"--roles-dir", RolesDir}
	for _, role := range roles {
		command = append(command, "--role", string(role))
		dir := roleDir(role)
		mounts = append(mounts,
			corev1.VolumeMount{Name: roleVolume("launcher", role), MountPath: dir + "/" + launcher.RoleLauncherDir, ReadOnly: true},
			corev1.VolumeMount{Name: roleVolume(ghVolume, role), MountPath: dir + "/" + launcher.RoleGHDir, ReadOnly: true},
			corev1.VolumeMount{Name: roleVolume("private", role), MountPath: dir + "/" + launcher.RolePrivateDir},
			corev1.VolumeMount{Name: roleVolume(stateVolume, role), MountPath: dir + "/" + launcher.RoleStateDir},
		)
		if r.agentSecrets != nil {
			mounts = append(mounts, corev1.VolumeMount{Name: roleVolume(agentSecretsKeyVolume, role), MountPath: dir + "/" + launcher.RoleAgentSecretsDir})
		}
	}
	return command, append(mounts, corev1.VolumeMount{Name: roleHomesVolume, MountPath: RoleHomesDir})
}

// microVMLauncher is what a pod's launcher of either kind is told of the microVM it runs in, and
// what it mounts for it: nothing in gVisor (l.microVM nil); --microvm in a microVM; and, where the
// block names the metadata endpoint, the endpoint's pool and identity provider, with the
// projected token it exchanges mounted read-only at tokenDir, the kind's own place for it.
func microVMLauncher(l launch, tokenDir string) (flags []string, mounts []corev1.VolumeMount) {
	if l.microVM == nil {
		return nil, nil
	}
	flags = []string{"--microvm"}
	endpoint := l.microVM.Metadata
	if endpoint == nil {
		return flags, nil
	}
	flags = append(flags, "--metadata-workload-pool", endpoint.WorkloadPool, "--metadata-identity-provider", endpoint.IdentityProvider)
	return flags, []corev1.VolumeMount{{Name: workloadIdentityTokenVolume, MountPath: tokenDir, ReadOnly: true}}
}

// mainVolumes is the role homes volume (roleHomesVolumes). Where the deployment serves the pod's
// own test database, the testpg volume its cluster lives on follows, its disk claimed for launch l.
func (issuePod) mainVolumes(r *Runtime, l launch) []corev1.Volume {
	volumes := roleHomesVolumes()
	if !r.testPostgres {
		return volumes
	}
	// The test database's cluster, on a disk for the same reason the role homes are: a memory
	// volume would charge its data directory and every test's database to the pod's memory. It is
	// created with the pod and deleted with it either way, so each issue starts on an empty
	// cluster. Where the deployment sizes a disk for it (runtime.kubernetes.test_postgres_disk) it
	// is a generic ephemeral volume of that size, claimed on the issue disk's class (claimSpec) with
	// no metadata, as the issue disk's is (disk): the kubelet's ephemeral-storage limit counts the
	// pod's emptyDir volumes and not a volume a claim provides, so a test database that fills it
	// fills its own disk and cannot get the pod evicted. The pod's FSGroup makes the new disk's
	// root writable to the pod's user, as it makes the issue disk's (podTemplate). Otherwise it is
	// an emptyDir on the node's disk with no SizeLimit, as the role homes volume carries none: the
	// pod's ephemeral-storage limit is what bounds both, and a test that fills the node's disk
	// through either is evicted with every role of the pod.
	cluster := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	if size := r.testPostgresDisk; size != nil {
		cluster = ephemeralDisk(claimSpec(r.storageClass, *size, l.kind, l.microVM))
	}
	return append(volumes, corev1.Volume{Name: testPGVolume, VolumeSource: cluster})
}

// roleHomesVolumes is the role homes volume of a pod whose launcher runs the roles-dir form
// (rolesDirLayout): an emptyDir on the node's disk, charged to the pod's ephemeral storage as a
// container's own filesystem is, where each role's namespace keeps its home overlay's writable
// layer, its /tmp and /var/tmp, and its own config home (`legion launcher role-init`). An
// overlay's writable layer cannot be the container's own root filesystem, and a memory one would
// charge a role's /tmp to the pod's memory.
func roleHomesVolumes() []corev1.Volume {
	return []corev1.Volume{{Name: roleHomesVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
}

// agentEnv tells each agent its tree, issue and workspace, the issue key it stamps into its
// commits where the project names its repository (LEGION_ISSUE_TRAILER, left out of an agent that
// stamps none), where its gh reads its credentials, and uv's directories on the issue's volume.
func (issuePod) agentEnv(l launch) []corev1.EnvVar {
	var trailer []corev1.EnvVar
	if l.spec.IssueTrailer != "" {
		trailer = []corev1.EnvVar{{Name: "LEGION_ISSUE_TRAILER", Value: l.spec.IssueTrailer}}
	}
	return slices.Concat(
		[]corev1.EnvVar{
			{Name: "LEGION_TREE", Value: l.spec.Tree},
			{Name: "LEGION_ISSUE", Value: l.spec.Issue},
		},
		trailer,
		[]corev1.EnvVar{{Name: "LEGION_WORKSPACE", Value: l.workspace}},
		ghAgentEnv(l),
		[]corev1.EnvVar{
			{Name: "UV_PYTHON_INSTALL_DIR", Value: uvPythonDir(l.spec.Issue)},
			{Name: "UV_CACHE_DIR", Value: uvCacheDir},
			{Name: "UV_LINK_MODE", Value: uvLinkMode},
		},
	)
}

// ghAgentEnv is the six variables that point an agent's gh at its own gh bundle (GHConfigDir), so
// its gh — the image's `legion gh`, first on PATH, which runs the real gh at LEGION_GH_PATH — and
// the clone's `legion git-credential` helper (workspace.GitHubCredentialHelper) read the token of
// the owner a command names from the owner's directory under LEGION_GH_OWNERS_DIR, and every other
// command's from GH_CONFIG_DIR, the launch's default owner's (gitHubDefault). The helper also reads
// a repository of an owner with no directory from GitHub with GH_CONFIG_DIR's token, and answers a
// fork with the token of its parent's or its source's owner. GH_TOKEN,
// GITHUB_TOKEN and GH_HOST are set to the empty string, which gh ignores, so no value the image or
// an operator's rc file could leave in the environment outranks the files: a non-empty GH_TOKEN
// would. No other tool path or helper is told: the agent's git and jj are the image's on PATH, and
// the helper is the clone's own configuration. Every launcher role, the controller's included,
// reads its own App's gh bundle at GHConfigDir, so this is appended once for every kind.
func ghAgentEnv(l launch) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "GH_CONFIG_DIR", Value: ghconfig.Dir(GHConfigDir, l.gitHubDefault)},
		{Name: "GH_TOKEN", Value: ""},
		{Name: "GITHUB_TOKEN", Value: ""},
		{Name: "GH_HOST", Value: ""},
		{Name: "LEGION_GH_OWNERS_DIR", Value: GHConfigDir},
		{Name: "LEGION_GH_PATH", Value: l.gh},
	}
}

// provision is what a new issue pod needs before it starts: it mints the provisioning token for
// the repository, makes sure the repository has its cache (EnsureRepositoryCache: one a
// sweep has not made yet is made now), since a fresh feed comes from that cache alone, waits until
// the cache's Service is its own (awaitCacheService), since the fetch presents the cache's token
// at that Service's name, and writes the token to the pod's init-only provisioning Secret with the
// cache's bearer token. It waits on no other pod, the cache's included: the fetch waits for the
// cache to be ready itself, and the issue's clone lives on the pod's own disk, which no other pod
// mounts. A cache that cannot be made, or whose Service is not its own by the boot timeout, fails
// the launch, with nothing written: the fetch could fill no fresh feed without it.
//
// The repository the token is minted for is the one the launch names, and the token source is
// what reads a configured rename off it (ProvisionTokens): every address below — the cache, its
// Service and the fetch's own `--repo` — stays this recorded repository.
func (issuePod) provision(ctx context.Context, r *Runtime, l *launch, s *sandbox) error {
	minting, cancel := call(ctx)
	token, err := r.tokens.Token(minting, l.spec.Repository)
	cancel()
	if err != nil {
		return fmt.Errorf("mint the provisioning token for %s: %w", l.spec.Repository, err)
	}
	cache, err := r.ensureRepositoryCache(ctx, l.spec.Repository)
	if err != nil {
		return err
	}
	if err := r.awaitCacheService(ctx, l.spec.Repository, cache); err != nil {
		return err
	}
	bearer := r.cacheBearer(cache.Name)
	if bearer == "" {
		return fmt.Errorf("the repository cache %s of %s holds no token", cache.Name, l.spec.Repository)
	}
	if err := r.upsertSecret(ctx, corev1.Secret{
		ObjectMeta: r.secretMeta(s, *l, secretName(s.Name)),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{provisionTokenKey: []byte(token), cacheTokenKey: []byte(bearer)},
	}); err != nil {
		return fmt.Errorf("write its provisioning secret: %w", err)
	}
	return nil
}

// controllerPod is the project controller's pod (`controller: daemon`): the controller's launcher
// alone, working in the root of a volume of its own, which its Sandbox owns, so the working root a
// pod leaves is the one its successor finds. It belongs to no tree and holds the review App's
// GitHub credential like any verdict-only role (appauth.AppRoleFor). Its pod runs no init
// container: it provisions no workspace, and the sessions directory that was the last thing such a
// container made on its volume is gone, its sessions now rows in the session database
// (initContainers). It runs in the project's sandbox (SandboxConfig.Controller, prepare): in a
// microVM where the project's type is microvm, placed, labelled and told so as a microVM issue pod
// is, and in gVisor otherwise, whatever the canary list says; its disk stays on
// runtime.kubernetes.storage_class in either (diskSpec).
type controllerPod struct{}

func (controllerPod) roles() []claim.Role { return []claim.Role{claim.RoleController} }

func (controllerPod) container() string { return runtime.ControllerPodContainer }

// resources is the controller's pod's reservation.
func (controllerPod) resources(r *Runtime) corev1.ResourceRequirements { return r.resources.Controller }

// asksExtendedRunTime is always: the controller's pod asks for extended run time whatever its
// reservation. One pod runs the project's controller, every tree's work passes through it, and
// nothing else can take over the turn it is in, so a deployment that gives the controller room to
// burst must not thereby lose the controller's protection from a repacking autoscaler. What an
// issue pod gives up in exchange for bursting — being ended and continued elsewhere — is an issue
// pod's alone: an issue's roles continue in the issue's next pod, and nothing else can run the
// controller's turn.
func (controllerPod) asksExtendedRunTime(*Runtime) bool { return true }

func (controllerPod) claimToken(project, _ string, _ claim.Role) (claim.Token, error) {
	return claim.ControllerToken(project), nil
}

// prepare works in the root of the controller's own volume.
func (controllerPod) prepare(l *launch) error {
	l.workspace = TreeRoot
	return nil
}

// labels carry the controller role and no tree or issue: the controller belongs to no tree, so no
// tree's cleanup lists its Sandbox (CleanupTree), and the labels name its kind (podKindOf).
func (controllerPod) labels(project string, _ launch) map[string]string {
	return map[string]string{labelProject: project, labelRole: string(claim.RoleController)}
}

// disk is the one `issue` volume the controller's Sandbox owns, so the working root the controller
// runs in (TreeRoot, prepare) outlives its pods. Its template's labels are the pod's own (labels),
// which carry the project and the controller role and no tree: the controller belongs to none, and
// the daemon's restricted identity has no PVC verb, so a label put on the claim at creation is the
// label it keeps for life. The pod mounts the claim of its own Sandbox (IssueClaimName), the name
// the Agent Sandbox controller gives the claim it makes from that template. The claim is on
// runtime.kubernetes.storage_class whatever sandbox the pod runs in (diskSpec), so the launch that
// moves the pod to another sandbox rebuilds the pod and keeps the Sandbox and its volume.
func (controllerPod) disk(r *Runtime, l launch) ([]volumeClaimTemplate, corev1.VolumeSource) {
	templates := []volumeClaimTemplate{{
		Metadata: volumeClaimMetadata{Name: issueVolume, Labels: r.labels(l)},
		Spec:     r.diskSpec(l),
	}}
	return templates, corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: IssueClaimName(l.spec.Claim),
	}}
}

// secretAnnotations are none: the controller's claim is the project's (claimToken).
func (controllerPod) secretAnnotations(launch) map[string]string { return nil }

// initContainers are none: the controller's volume needs no preparing before its launcher starts.
// The launcher works in the volume's root, which the mount itself makes, owned by the pod's group
// (fsGroup, podTemplate); the scratch directories its agent is told are made by the shim
// (podsafety.EnsureScratch); and its session is a row in the session database, which the role
// launcher looks up before it starts the child (internal/launcher), not a file on the volume.
func (controllerPod) initContainers(*Runtime, launch) []corev1.Container { return nil }

// launcherContainer is the controller's pod's one container, `controller`. Where the session
// database names no writer audience it is `legion launcher` in single-role mode, with the
// controller's own launcher token, private directory, state directory and gh volume mounted at the
// fixed paths it reads them at, the gh volume read-only, its key directory and the broker's token
// when the pod enrolls, beside the pod's volume, the config home, the providers and the operator's
// mounts. A microVM pod's launcher is told so (microVMLauncher), the metadata endpoint's token
// mounted at ControllerWorkloadIdentityDir, the single-role launcher's fixed path: the pod has no
// role namespaces, so its agent can read the token too, which gives it no identity the endpoint
// does not already serve it. The flags go before --stop-grace, as an issue pod's launcher's do.
// Where the database names a writer audience, the container runs the roles-dir form instead
// (rolesDirContainer).
func (k controllerPod) launcherContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container {
	if r.sessionWriterAudience() != "" {
		return k.rolesDirContainer(r, l, resolved, providersMounts)
	}
	role := claim.RoleController
	command := []string{
		r.tools.Legion, "launcher", connectFlag, r.streamURL, "--token-file", LauncherDir + "/" + LauncherTokenFile,
		"--sandbox", l.name, "--role", string(role), "--private-dir", LauncherPrivateDir,
	}
	mounts := []corev1.VolumeMount{
		{Name: issueVolume, MountPath: TreeRoot},
		{Name: roleVolume("launcher", role), MountPath: LauncherDir, ReadOnly: true},
		{Name: roleVolume("private", role), MountPath: LauncherPrivateDir},
		{Name: roleVolume(stateVolume, role), MountPath: StateDir},
		{Name: configVolume, MountPath: xdgConfigHome},
		{Name: roleVolume(ghVolume, role), MountPath: GHConfigDir, ReadOnly: true},
	}
	flags, tokenMounts := microVMLauncher(l, ControllerWorkloadIdentityDir)
	command = append(command, flags...)
	mounts = append(mounts, tokenMounts...)
	return corev1.Container{
		Name:            k.container(),
		Image:           r.image,
		Command:         slices.Concat(command, []string{"--stop-grace", r.terminationGrace.String()}, r.sessionProxyFlags()),
		Env:             resolved,
		WorkingDir:      l.workspace,
		VolumeMounts:    slices.Concat(mounts, providersMounts, agentSecretsMounts(r.agentSecrets, role), r.pod.VolumeMounts),
		Resources:       k.resources(r),
		SecurityContext: restrictedContainer(),
	}
}

// rolesDirContainer is the controller's container where the session database names a writer
// audience: `legion launcher` in the roles-dir form for the controller alone (rolesDirLayout),
// laid out as an issue pod lays out one of its roles. The single-role form runs its agent in the
// container's own namespaces, where the writer token would be in the agent's view, so the launcher
// refuses the writer audience there. In the roles-dir form role-init starts the controller's
// generation in namespaces of its own, which cover RolesDir, and the writer token at
// SessionWriterDir and the controller's launcher token are both under it, so the agent reads
// neither. role-init binds the controller's own gh, private, state and agent-secrets key
// directories back at the fixed paths the single-role form mounts them at, so this container
// mounts nothing at those paths, and the agent sees each where it always has. A microVM pod's
// launcher is told so (microVMLauncher), the metadata endpoint's token mounted at
// WorkloadIdentityDir, under RolesDir, as an issue pod's is, where no role's namespace shows it.
// The pod's volume, the config home, the tokens (podTokenMounts), the providers and the
// operator's mounts are mounted once, and every one of them at or under the image's home through
// homeMountsView, as in an issue pod: both kinds build the container in rolesDirLauncher.
func (k controllerPod) rolesDirContainer(r *Runtime, l launch, resolved []corev1.EnvVar, providersMounts []corev1.VolumeMount) corev1.Container {
	flags, tokenMounts := microVMLauncher(l, WorkloadIdentityDir)
	return r.rolesDirLauncher(k, l, resolved, providersMounts, []corev1.VolumeMount{
		{Name: issueVolume, MountPath: TreeRoot},
		{Name: configVolume, MountPath: xdgConfigHome},
	}, flags, tokenMounts)
}

// mainVolumes are none beyond the controller's own in the single-role form: its launcher has no
// namespaces to keep homes in, and the pod's own test database is an issue pod's
// (issuePod.mainVolumes). Where the session database names a writer audience its launcher runs the
// roles-dir form (rolesDirContainer), whose role homes volume holds the controller's home overlay,
// temporary directories and own config home, as an issue pod's holds each of its roles'.
func (controllerPod) mainVolumes(r *Runtime, _ launch) []corev1.Volume {
	if r.sessionWriterAudience() == "" {
		return nil
	}
	return roleHomesVolumes()
}

// initVolumes are none: the controller's pod runs no init container.
func (controllerPod) initVolumes(launch) []corev1.Volume { return nil }

// agentEnv tells the agent LEGION_CONTROLLER=1, its pane marker, where its gh reads its
// credentials (ghAgentEnv), and nothing a tree agent alone needs: no tree, issue or workspace, and
// none of uv's directories on an issue's volume.
func (controllerPod) agentEnv(l launch) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "LEGION_CONTROLLER", Value: "1"}}
	return append(env, ghAgentEnv(l)...)
}

// provision readies nothing: the controller's pod provisions no workspace and mints no token.
func (controllerPod) provision(context.Context, *Runtime, *launch, *sandbox) error { return nil }
